package consensus

import (
	"context"
	"errors"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// Empty-as-slot-vote must only arm when the current method has
// blockEvidenceFields — a policy-wide empty-waiver opt-in alone is not
// enough, or eth_getBalance would keep a fast null just because
// eth_getTransactionByHash is waiver-eligible.
func TestKeepEmptyConsensusSlot_RequiresMethodBlockEvidence(t *testing.T) {
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
		blockEvidenceFields: map[string][]string{
			"eth_getTransactionByHash": {"blockNumber"},
		},
	}
	ex := &executor{consensusPolicy: &consensusPolicy{logger: &lg, config: cfg}}

	ups := []common.Upstream{common.NewFakeUpstream("u1", common.WithTags("type:internal"))}
	probeErr := errors.New("probe-done")

	t.Run("method with evidence keeps empty", func(t *testing.T) {
		req := common.NewNormalizedRequestFromJsonRpcRequest(&common.JsonRpcRequest{
			Method: "eth_getTransactionByHash",
		})
		req.SetUpstreams(ups)
		inner := func(ctx context.Context, r *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			require.True(t, r.ExecState().KeepEmptyConsensusSlot())
			return nil, probeErr
		}
		_, err := ex.Run(context.Background(), req, inner)
		require.ErrorIs(t, err, probeErr)
	})

	t.Run("method without evidence rejects empty keep", func(t *testing.T) {
		req := common.NewNormalizedRequestFromJsonRpcRequest(&common.JsonRpcRequest{
			Method: "eth_getBalance",
		})
		req.SetUpstreams(ups)
		inner := func(ctx context.Context, r *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			require.False(t, r.ExecState().KeepEmptyConsensusSlot(),
				"empty keep must not arm for methods without blockEvidenceFields")
			return nil, probeErr
		}
		_, err := ex.Run(context.Background(), req, inner)
		require.ErrorIs(t, err, probeErr)
	})
}
