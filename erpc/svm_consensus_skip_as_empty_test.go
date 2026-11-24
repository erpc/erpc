package erpc

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSvm_Consensus_PermanentSkip_AgreesAsNativeError drives a skipped-slot
// getBlock through the real SVM handler + consensus with the mixConsensus shape
// used on solana-mainnet staging: two type:internal + two type:external, each
// with minAgreement:1, agreementThreshold:2, and NO waiveAgreementOnMissingData.
//
// -32007 / -32009 are ExecutionException (not MissingData, not Empty): they
// agree as svm:slot_skipped and the client receives a native Solana code.
func TestSvm_Consensus_PermanentSkip_AgreesAsNativeError(t *testing.T) {
	const slot = 500281501

	skips := map[string]string{
		"internal-skip": fmt.Sprintf(
			`{"code":-32007,"message":"Slot %d was skipped, or missing due to ledger jump to recent snapshot"}`, slot),
		"external-skip": fmt.Sprintf(
			`{"code":-32009,"message":"Slot %d was skipped, or missing in long-term storage"}`, slot),
	}

	cases := []struct {
		name         string
		internalBody string
		externalBody string
	}{
		{
			name:         "all permanent skips",
			internalBody: skips["internal-skip"],
			externalBody: skips["external-skip"],
		},
		{
			name:         "uniform -32007",
			internalBody: skips["internal-skip"],
			externalBody: skips["internal-skip"],
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			util.ResetGock()
			t.Cleanup(util.ResetGock)

			tag := strings.NewReplacer(" ", "-").Replace(tc.name)
			hosts := map[string]string{
				"internal-blue":  "svm-skip-int-blue-" + tag + ".localhost",
				"internal-green": "svm-skip-int-green-" + tag + ".localhost",
				"archive-a":      "svm-skip-ext-a-" + tag + ".localhost",
				"archive-b":      "svm-skip-ext-b-" + tag + ".localhost",
			}
			calls := map[string]*atomic.Int32{}
			var ups []*common.UpstreamConfig
			for _, id := range []string{"internal-blue", "internal-green", "archive-a", "archive-b"} {
				host := hosts[id]
				util.SetupMocksForSvmStatePoller(host, 500_300_000, 500_299_000)
				c := &atomic.Int32{}
				calls[id] = c

				body := tc.externalBody
				tier := "type:external"
				if strings.HasPrefix(id, "internal") {
					body = tc.internalBody
					tier = "type:internal"
				}
				reply := `{"jsonrpc":"2.0","id":1,"error":` + body + `}`

				gock.New("http://" + host).
					Post("").
					Persist().
					Filter(func(r *http.Request) bool {
						if r.URL.Host != host || !strings.Contains(util.SafeReadBody(r), `"method":"getBlock"`) {
							return false
						}
						c.Add(1)
						return true
					}).
					Reply(200).
					BodyString(reply)

				u := svmUpstreamConfig(id, host)
				u.Tags = []string{tier}
				ups = append(ups, u)
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			preferNonEmpty := true
			net := setupTestSvmNetworkWithConfig(t, ctx, func(nc *common.NetworkConfig) {
				nc.Failsafe = []*common.FailsafeConfig{{
					MatchMethod: "*",
					Consensus: &common.ConsensusPolicyConfig{
						MaxParticipants:         4,
						AgreementThreshold:      2,
						PreferNonEmpty:          &preferNonEmpty,
						DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
						LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
						RequiredParticipants: []*common.ConsensusRequiredParticipant{
							{Tag: "type:internal", MinParticipants: 2, MinAgreement: 1},
							{Tag: "type:external", MinParticipants: 2, MinAgreement: 1},
						},
					},
				}}
			}, ups)

			req := common.NewNormalizedRequest([]byte(fmt.Sprintf(
				`{"jsonrpc":"2.0","id":1,"method":"getBlock","params":[%d,{"commitment":"finalized","maxSupportedTransactionVersion":0}]}`, slot)))
			waived := func() float64 {
				return testutil.ToFloat64(telemetry.MetricConsensusCompositionWaived.WithLabelValues(
					net.ProjectId(), net.Label(), "type:internal", "missing_data"))
			}
			before := waived()
			resp, err := svmProjectForward(ctx, net, req)

			for id, c := range calls {
				assert.LessOrEqual(t, c.Load(), int32(1), "%s must not be re-asked", id)
			}
			var answered int32
			for _, c := range calls {
				answered += c.Load()
			}
			assert.GreaterOrEqual(t, answered, int32(2), "agreementThreshold requires at least two skip votes")
			// Consensus settles on the skip ExecutionException — surfaced as the
			// response error (or forward err) with a native Solana code.
			var jre *common.ErrJsonRpcExceptionInternal
			if err != nil {
				require.ErrorAs(t, err, &jre, "forward error must carry native skip code, got: %v", err)
			} else {
				require.NotNil(t, resp)
				jrr, jerr := resp.JsonRpcResponse()
				require.NoError(t, jerr)
				require.NotNil(t, jrr.Error, "skip must reach the client as a JSON-RPC error, not null")
				require.ErrorAs(t, jrr.Error, &jre)
			}
			assert.Contains(t, []common.JsonRpcErrorNumber{-32007, -32009}, jre.NormalizedCode(),
				"client must see native -32007 or -32009, got %v", jre.NormalizedCode())
			assert.Equal(t, 0.0, waived()-before,
				"waiveAgreementOnMissingData must stay unused for permanent skips")
		})
	}
}

// Transient pruned answers without waiveAgreementOnMissingData must still fail
// closed under the same mixConsensus quotas — only skips leave MissingData.
func TestSvm_Consensus_TransientPruned_StillNeedsWaiver(t *testing.T) {
	const slot = 100
	util.ResetGock()
	t.Cleanup(util.ResetGock)

	prunedErr := fmt.Sprintf(
		`{"code":-32001,"message":"Block %d cleaned up, does not exist on node. First available block: 5000"}`, slot)
	block := `{"blockHeight":99,"blockTime":1600000000,"blockhash":"5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d","parentSlot":99,"previousBlockhash":"4sGjMW1sUnHzSxGspuhpqLDx6wiyjNtZAMdL4VZHirAn","transactions":[]}`

	hosts := map[string]string{
		"internal-blue":  "svm-transient-int-blue.localhost",
		"internal-green": "svm-transient-int-green.localhost",
		"archive-a":      "svm-transient-ext-a.localhost",
		"archive-b":      "svm-transient-ext-b.localhost",
	}
	var ups []*common.UpstreamConfig
	for _, id := range []string{"internal-blue", "internal-green", "archive-a", "archive-b"} {
		host := hosts[id]
		util.SetupMocksForSvmStatePoller(host, 1000, 990)
		reply := `{"jsonrpc":"2.0","id":1,"result":` + block + `}`
		tier := "type:external"
		if strings.HasPrefix(id, "internal") {
			reply = `{"jsonrpc":"2.0","id":1,"error":` + prunedErr + `}`
			tier = "type:internal"
		}
		gock.New("http://" + host).
			Post("").
			Persist().
			Filter(func(r *http.Request) bool {
				return r.URL.Host == host && strings.Contains(util.SafeReadBody(r), `"method":"getBlock"`)
			}).
			Reply(200).
			BodyString(reply)
		u := svmUpstreamConfig(id, host)
		u.Tags = []string{tier}
		ups = append(ups, u)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	preferNonEmpty := true
	net := setupTestSvmNetworkWithConfig(t, ctx, func(nc *common.NetworkConfig) {
		nc.Failsafe = []*common.FailsafeConfig{{
			MatchMethod: "*",
			Consensus: &common.ConsensusPolicyConfig{
				MaxParticipants:         4,
				AgreementThreshold:      2,
				PreferNonEmpty:          &preferNonEmpty,
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
				RequiredParticipants: []*common.ConsensusRequiredParticipant{
					{Tag: "type:internal", MinParticipants: 2, MinAgreement: 1},
					{Tag: "type:external", MinParticipants: 2, MinAgreement: 1},
				},
			},
		}}
	}, ups)

	req := common.NewNormalizedRequest([]byte(fmt.Sprintf(
		`{"jsonrpc":"2.0","id":1,"method":"getBlock","params":[%d,{"commitment":"finalized","maxSupportedTransactionVersion":0}]}`, slot)))
	_, err := svmProjectForward(ctx, net, req)
	require.Error(t, err, "transient MissingData without waiver must fail closed")
	assert.True(t, common.HasErrorCode(err, common.ErrCodeConsensusCompositionDispute),
		"internal minAgreement on MissingData must hold, got: %v", err)
}
