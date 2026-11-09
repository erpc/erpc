package consensus

import (
	"context"
	"errors"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// Empty-as-slot-vote must only arm when the request names a concrete block —
// a policy-wide empty-waiver opt-in alone is not enough, or a hash lookup
// would keep a fast null that the waiver can never prove.
func TestKeepEmptyConsensusSlot_RequiresRequestBlock(t *testing.T) {
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

	t.Run("request with a block number keeps empty", func(t *testing.T) {
		req := common.NewNormalizedRequestFromJsonRpcRequest(&common.JsonRpcRequest{
			Method: "eth_getBlockByNumber",
			Params: []interface{}{"0x10", false},
		})
		req.SetUpstreams(ups)
		inner := func(ctx context.Context, r *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			require.True(t, r.ExecState().KeepEmptyConsensusSlot())
			return nil, probeErr
		}
		_, err := ex.Run(context.Background(), req, inner)
		require.ErrorIs(t, err, probeErr)
	})

	t.Run("hash lookup rejects empty keep", func(t *testing.T) {
		req := common.NewNormalizedRequestFromJsonRpcRequest(&common.JsonRpcRequest{
			Method: "eth_getTransactionByHash",
			Params: []interface{}{"0x5c504ed432cb51138bcf09aa5e8a410dd4a1e204ef84bfed1be16dfba1b22060"},
		})
		req.SetUpstreams(ups)
		inner := func(ctx context.Context, r *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			require.False(t, r.ExecState().KeepEmptyConsensusSlot(),
				"empty keep must not arm for requests that name no block")
			return nil, probeErr
		}
		_, err := ex.Run(context.Background(), req, inner)
		require.ErrorIs(t, err, probeErr)
	})
}
