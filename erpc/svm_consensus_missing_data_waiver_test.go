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

// TestSvm_Consensus_MissingDataWaiver_PrunedGetBlock drives an old-slot
// getBlock through the real SVM handler and consensus executor with the
// composition of erpc/erpc#1475: two pruned "type:internal" nodes and two
// archive "type:external" providers, each tier holding a minAgreement quota.
// The pruned nodes answer agave's own errors for a slot below their ledger
// floor; the SVM normalizer maps both to ErrEndpointMissingData, so
// waiveAgreementOnMissingData must release the internal quota and the archive
// block wins. Without the flag the same round must fail closed.
func TestSvm_Consensus_MissingDataWaiver_PrunedGetBlock(t *testing.T) {
	const (
		slot      = 100
		blockHash = "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d"
	)
	block := fmt.Sprintf(`{"blockHeight":%d,"blockTime":1600000000,"blockhash":%q,"parentSlot":%d,"previousBlockhash":"4sGjMW1sUnHzSxGspuhpqLDx6wiyjNtZAMdL4VZHirAn","transactions":[]}`,
		slot-1, blockHash, slot-1)

	// Agave's wire answers for a slot this node no longer holds
	// (rpc.rs check_slot_cleaned_up / check_bigtable_result).
	pruned := map[string]string{
		"-32001 block cleaned up":      fmt.Sprintf(`{"code":-32001,"message":"Block %d cleaned up, does not exist on node. First available block: 5000"}`, slot),
		"-32009 long-term storage gap": fmt.Sprintf(`{"code":-32009,"message":"Slot %d was skipped, or missing in long-term storage"}`, slot),
	}

	for name, prunedErr := range pruned {
		for _, waive := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/waive=%v", name, waive), func(t *testing.T) {
				util.ResetGock()
				t.Cleanup(util.ResetGock)

				tag := strings.NewReplacer(" ", "-", "=", "").Replace(fmt.Sprintf("%s-%v", name[:6], waive))
				hosts := map[string]string{
					"internal-blue":  "svm-waive-int-blue" + tag + ".localhost",
					"internal-green": "svm-waive-int-green" + tag + ".localhost",
					"archive-a":      "svm-waive-ext-a" + tag + ".localhost",
					"archive-b":      "svm-waive-ext-b" + tag + ".localhost",
				}
				calls := map[string]*atomic.Int32{}
				var ups []*common.UpstreamConfig
				for _, id := range []string{"internal-blue", "internal-green", "archive-a", "archive-b"} {
					host := hosts[id]
					util.SetupMocksForSvmStatePoller(host, 1000, 990)
					c := &atomic.Int32{}
					calls[id] = c
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
								{Tag: "type:internal", MinParticipants: 2, MinAgreement: 1, WaiveAgreementOnMissingData: waive},
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
					assert.GreaterOrEqual(t, c.Load(), int32(1), "%s must take part in the round", id)
				}
				if !waive {
					require.Error(t, err, "without the waiver the internal quota must hold and the round fail closed")
					assert.True(t, common.HasErrorCode(err, common.ErrCodeConsensusCompositionDispute),
						"the internal minAgreement quota must be what fails the round, got: %v", err)
					assert.Equal(t, 0.0, waived()-before)
					return
				}
				require.NoError(t, err, "pruned internal nodes must not block the archive answer")
				jrr, err := resp.JsonRpcResponse()
				require.NoError(t, err)
				require.Nil(t, jrr.Error)
				assert.Contains(t, string(jrr.GetResultBytes()), blockHash)
				assert.Equal(t, 1.0, waived()-before,
					"erpc_consensus_composition_waived_total{reason=missing_data} must count the waiver")
			})
		}
	}
}
