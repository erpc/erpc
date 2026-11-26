package consensus

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// Empty-as-slot-vote must only arm when the waiver can get a block to judge:
// one the request names, or one a hash lookup's answer names. A policy-wide
// empty-waiver opt-in alone is not enough, or a request whose answer names no
// block would keep a fast null that the waiver can never prove.
func TestKeepEmptyConsensusSlot_RequiresWaiverBlock(t *testing.T) {
	lg := zerolog.Nop()
	cfg := &config{
		maxParticipants:    1,
		agreementThreshold: 1,
		requiredParticipants: []*common.ConsensusRequiredParticipant{
			{
				Tag:                                   "type:internal",
				MinParticipants:                       1,
				MinAgreement:                          1,
				WaiveAgreementOnEmptyOutsideRetention: true,
				RetentionBlocks:                       100,
			},
		},
	}
	ex := &executor{consensusPolicy: &consensusPolicy{logger: &lg, config: cfg}}

	ups := []common.Upstream{common.NewFakeUpstream("u1", common.WithTags("type:internal"))}
	probeErr := errors.New("probe-done")
	txHash := "0x5c504ed432cb51138bcf09aa5e8a410dd4a1e204ef84bfed1be16dfba1b22060"

	cases := []struct {
		name   string
		method string
		params []interface{}
		keep   bool
	}{
		{"request names a block", "eth_getBlockByNumber", []interface{}{"0x10", false}, true},
		{"tx hash lookup whose answer names a block", "eth_getTransactionByHash", []interface{}{txHash}, true},
		{"block hash lookup", "eth_getBlockByHash", []interface{}{txHash, false}, true},
		{"tx hash whose answer names no block", "debug_traceTransaction", []interface{}{txHash}, false},
		{"tag request", "eth_getBalance", []interface{}{"0x00000000219ab540356cbb839cbe05303d7705fa", "latest"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := common.NewNormalizedRequestFromJsonRpcRequest(&common.JsonRpcRequest{Method: tc.method, Params: tc.params})
			req.SetUpstreams(ups)
			// The participant runs on its own goroutine, where a failing
			// require would hang the round instead of failing the test.
			var keep atomic.Bool
			inner := func(ctx context.Context, r *common.NormalizedRequest) (*common.NormalizedResponse, error) {
				keep.Store(r.ExecState().KeepEmptyConsensusSlot())
				return nil, probeErr
			}
			_, err := ex.Run(context.Background(), req, inner)
			require.ErrorIs(t, err, probeErr)
			require.Equal(t, tc.keep, keep.Load())
		})
	}
}
