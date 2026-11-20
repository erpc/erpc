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

// The composition of erpc/erpc#1475: two pruned "type:internal" validators
// (ledger floor 500) and two archive "type:external" providers (floor 0), each
// tier holding a minAgreement quota, at a pool tip of 1000. The tests drive
// old-slot reads through the real SVM handler and consensus executor.

const (
	svmWaiverSlot = 100
	svmWaiverSig  = "5VERv8NMvzbJMEkV8xnrLkEaWRtSz9CosKDYjCJjBRnbJLgp8uirBgmQpjKhoR4tjF3ZpRzrFmBV6UjKdiSZkQUW"
	svmWaiverHash = "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d"
)

// svmWaiverUpstream is one upstream of the composition: its tier tag, its
// observed ledger floor, and its reply per method. calls counts the answers
// per method.
type svmWaiverUpstream struct {
	id, tier string
	floor    int64
	replies  map[string]string
	calls    map[string]*atomic.Int32
}

func svmWaiverResult(result string) string {
	return `{"jsonrpc":"2.0","id":1,"result":` + result + `}`
}

func svmWaiverBlock(signatures ...string) string {
	quoted := make([]string, len(signatures))
	for i, s := range signatures {
		quoted[i] = `"` + s + `"`
	}
	return svmWaiverResult(fmt.Sprintf(
		`{"blockHeight":%d,"blockTime":1600000000,"blockhash":%q,"parentSlot":%d,"previousBlockhash":"4sGjMW1sUnHzSxGspuhpqLDx6wiyjNtZAMdL4VZHirAn","signatures":[%s],"transactions":[]}`,
		svmWaiverSlot-1, svmWaiverHash, svmWaiverSlot-1, strings.Join(quoted, ",")))
}

func svmWaiverTx() string {
	return svmWaiverResult(fmt.Sprintf(
		`{"slot":%d,"blockTime":1600000000,"meta":{"err":null,"fee":5000},"transaction":{"signatures":[%q],"message":{"accountKeys":[],"instructions":[],"recentBlockhash":"4sGjMW1sUnHzSxGspuhpqLDx6wiyjNtZAMdL4VZHirAn"}}}`,
		svmWaiverSlot, svmWaiverSig))
}

const svmWaiverCleanedUp = `{"jsonrpc":"2.0","id":1,"error":{"code":-32001,"message":"Block 100 cleaned up, does not exist on node. First available block: 500"}}`

// setupSvmWaiverNetwork mocks ups (in pinned routing order) and builds a
// consensus network over them with the given quotas.
func setupSvmWaiverNetwork(t *testing.T, ctx context.Context, name string, maxParticipants int, quotas []*common.ConsensusRequiredParticipant, ups []*svmWaiverUpstream) *Network {
	t.Helper()
	util.ResetGock()
	t.Cleanup(util.ResetGock)
	var cfgs []*common.UpstreamConfig
	var ids []string
	for _, u := range ups {
		host := fmt.Sprintf("svm-waiver-%s-%s.localhost", name, u.id)
		util.SetupMocksForSvmStatePollerWithFirstAvailable(host, 1000, 990, 1000, u.floor)
		u.calls = map[string]*atomic.Int32{}
		for method, reply := range u.replies {
			c := &atomic.Int32{}
			u.calls[method] = c
			needle := `"method":"` + method + `"`
			gock.New("http://" + host).
				Post("").
				Persist().
				Filter(func(r *http.Request) bool {
					if r.URL.Host != host || !strings.Contains(util.SafeReadBody(r), needle) {
						return false
					}
					c.Add(1)
					return true
				}).
				Reply(200).
				BodyString(reply)
		}
		cfg := svmUpstreamConfig(u.id, host)
		cfg.Tags = []string{u.tier}
		cfgs = append(cfgs, cfg)
		ids = append(ids, u.id)
	}
	preferNonEmpty := true
	net := setupTestSvmNetworkWithConfig(t, ctx, func(nc *common.NetworkConfig) {
		nc.Failsafe = []*common.FailsafeConfig{{
			MatchMethod: "*",
			Consensus: &common.ConsensusPolicyConfig{
				MaxParticipants:         maxParticipants,
				AgreementThreshold:      2,
				PreferNonEmpty:          &preferNonEmpty,
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
				RequiredParticipants:    quotas,
			},
		}}
	}, cfgs)
	net.PinUpstreamOrderForTest(ids...)
	return net
}

func svmWaived(net *Network, reason string) float64 {
	return testutil.ToFloat64(telemetry.MetricConsensusCompositionWaived.WithLabelValues(
		net.ProjectId(), net.Label(), "type:internal", reason))
}

func svmWaiverForward(t *testing.T, ctx context.Context, net *Network, body string) (*common.NormalizedResponse, error) {
	t.Helper()
	return svmProjectForward(ctx, net, common.NewNormalizedRequest([]byte(body)))
}

func requireSvmWaiverResult(t *testing.T, resp *common.NormalizedResponse, err error, want string) {
	t.Helper()
	require.NoError(t, err)
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	require.Nil(t, jrr.Error)
	assert.Contains(t, string(jrr.GetResultBytes()), want)
}

// requireSvmWaiverDispute asserts the round failed closed: the pruned tier's
// empty votes stand against the archives' answer.
func requireSvmWaiverDispute(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeConsensusDispute, common.ErrCodeConsensusCompositionDispute),
		"the unwaived internal quota must fail the round, got: %v", err)
}

// A positional getBlock that the pruned tier answers with an empty result is
// released by waiveAgreementOnEmptyOutsideRetention only when the slot sits
// deeper than the tier's retention below the corroborated tip.
func TestSvm_Consensus_EmptyOutsideRetention_PositionalGetBlock(t *testing.T) {
	for _, tc := range []struct {
		name      string
		retention int64
		waived    bool
	}{
		{"slot outside retention", 500, true},
		{"slot inside retention", 950, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			block := svmWaiverBlock(svmWaiverSig)
			ups := []*svmWaiverUpstream{
				{id: "internal-blue", tier: "type:internal", floor: 500, replies: map[string]string{"getBlock": svmWaiverResult("null")}},
				{id: "internal-green", tier: "type:internal", floor: 500, replies: map[string]string{"getBlock": svmWaiverResult("null")}},
				{id: "archive-a", tier: "type:external", replies: map[string]string{"getBlock": block}},
				{id: "archive-b", tier: "type:external", replies: map[string]string{"getBlock": block}},
			}
			net := setupSvmWaiverNetwork(t, ctx, fmt.Sprintf("getblock-%d", tc.retention), 4, []*common.ConsensusRequiredParticipant{
				{Tag: "type:internal", MinParticipants: 2, MinAgreement: 1, WaiveAgreementOnEmptyOutsideRetention: true, RetentionBlocks: tc.retention},
				{Tag: "type:external", MinParticipants: 2, MinAgreement: 1},
			}, ups)

			before := svmWaived(net, "empty_outside_retention")
			resp, err := svmWaiverForward(t, ctx, net, fmt.Sprintf(
				`{"jsonrpc":"2.0","id":1,"method":"getBlock","params":[%d,{"commitment":"finalized","maxSupportedTransactionVersion":0}]}`, svmWaiverSlot))
			if !tc.waived {
				requireSvmWaiverDispute(t, err)
				assert.Equal(t, 0.0, svmWaived(net, "empty_outside_retention")-before)
				return
			}
			requireSvmWaiverResult(t, resp, err, svmWaiverHash)
			assert.Equal(t, 1.0, svmWaived(net, "empty_outside_retention")-before)
		})
	}
}

// getTransaction names no slot; its answer does. The pruned tier answers null
// and cannot serve the answer's block either, so only an upstream outside the
// round can confirm the signature sits in that slot. Without one the round
// fails closed. An outsider whose block lacks the signature contradicts it.
func TestSvm_Consensus_EmptyOutsideRetention_GetTransactionConfirmedOutsideRound(t *testing.T) {
	for _, tc := range []struct {
		name     string
		outsider string // the out-of-round archive's getBlock reply; "" = none
		waived   bool
	}{
		{"outsider confirms", svmWaiverBlock("1111111111111111111111111111111111111111111111111111111111111111", svmWaiverSig), true},
		{"no outsider", "", false},
		{"outsider contradicts", svmWaiverBlock("1111111111111111111111111111111111111111111111111111111111111111"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			pruned := map[string]string{"getTransaction": svmWaiverResult("null"), "getBlock": svmWaiverCleanedUp}
			archive := map[string]string{"getTransaction": svmWaiverTx()}
			ups := []*svmWaiverUpstream{
				{id: "internal-blue", tier: "type:internal", floor: 500, replies: pruned},
				{id: "internal-green", tier: "type:internal", floor: 500, replies: pruned},
				{id: "archive-a", tier: "type:external", replies: archive},
				{id: "archive-b", tier: "type:external", replies: archive},
			}
			var outsider *svmWaiverUpstream
			if tc.outsider != "" {
				outsider = &svmWaiverUpstream{id: "archive-c", tier: "type:external", replies: map[string]string{
					"getTransaction": svmWaiverTx(), "getBlock": tc.outsider,
				}}
				ups = append(ups, outsider)
			}
			net := setupSvmWaiverNetwork(t, ctx, "gettx-"+strings.ReplaceAll(tc.name, " ", "-"), 4, []*common.ConsensusRequiredParticipant{
				{Tag: "type:internal", MinParticipants: 2, MinAgreement: 1, WaiveAgreementOnEmptyOutsideRetention: true, RetentionBlocks: 500},
				{Tag: "type:external", MinParticipants: 2, MinAgreement: 1},
			}, ups)

			before := svmWaived(net, "empty_outside_retention")
			resp, err := svmWaiverForward(t, ctx, net, fmt.Sprintf(
				`{"jsonrpc":"2.0","id":1,"method":"getTransaction","params":[%q,{"commitment":"finalized","maxSupportedTransactionVersion":0}]}`, svmWaiverSig))
			if outsider != nil {
				assert.Equal(t, int32(0), outsider.calls["getTransaction"].Load(), "the outsider takes no part in the round")
				assert.Equal(t, int32(1), outsider.calls["getBlock"].Load(), "the outsider is probed for the answer's slot once")
			}
			if !tc.waived {
				requireSvmWaiverDispute(t, err)
				assert.Equal(t, 0.0, svmWaived(net, "empty_outside_retention")-before)
				return
			}
			requireSvmWaiverResult(t, resp, err, svmWaiverSig)
			assert.Equal(t, 1.0, svmWaived(net, "empty_outside_retention")-before)
		})
	}
}

// A pruned validator's getSignaturesForAddress page ends at its ledger floor.
// Under waiveAgreementOnMissingData its short page abstains toward the deeper
// archives of the same round, and the archives' longer page wins. Without the
// waiver the short page stays a data vote, as before.
func TestSvm_Consensus_MissingData_ShortSignaturePageAbstains(t *testing.T) {
	for _, waive := range []bool{true, false} {
		t.Run(fmt.Sprintf("waive=%v", waive), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			short := map[string]string{"getSignaturesForAddress": svmSigPage(2)}
			deep := map[string]string{"getSignaturesForAddress": svmSigPage(4)}
			ups := []*svmWaiverUpstream{
				{id: "internal-blue", tier: "type:internal", floor: 500, replies: short},
				{id: "internal-green", tier: "type:internal", floor: 500, replies: short},
				{id: "archive-a", tier: "type:external", replies: deep},
				{id: "archive-b", tier: "type:external", replies: deep},
			}
			net := setupSvmWaiverNetwork(t, ctx, fmt.Sprintf("sigs-%v", waive), 4, []*common.ConsensusRequiredParticipant{
				{Tag: "type:internal", MinParticipants: 2, MinAgreement: 1, WaiveAgreementOnMissingData: waive},
				{Tag: "type:external", MinParticipants: 2, MinAgreement: 1},
			}, ups)

			before := svmWaived(net, "missing_data")
			resp, err := svmWaiverForward(t, ctx, net,
				`{"jsonrpc":"2.0","id":1,"method":"getSignaturesForAddress","params":["addr",{"limit":5}]}`)
			for _, u := range ups {
				assert.Equal(t, int32(1), u.calls["getSignaturesForAddress"].Load(), "%s answers its own slot once", u.id)
			}
			if !waive {
				require.Error(t, err, "without the waiver the short pages vote data against the deeper page")
				assert.Equal(t, 0.0, svmWaived(net, "missing_data")-before)
				return
			}
			requireSvmWaiverResult(t, resp, err, `"sig3"`)
			assert.Equal(t, 1.0, svmWaived(net, "missing_data")-before)
		})
	}
}
