package consensus

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every participant slot must execute under a consensus-slot-marked context:
// that marker is the only way "this attempt exists because of consensus
// fan-out" crosses from the consensus executor down to the upstream attempt
// recorder (Reason = consensus_slot in the attempt log, the
// X-ERPC-Upstreams trace, and erpc_upstream_selection_total). Without it,
// consensus participants are indistinguishable from primary/retry attempts.
func TestConsensus_ParticipantContextsAreMarkedAsConsensusSlots(t *testing.T) {
	logger := zerolog.New(zerolog.NewTestWriter(t))

	pol := NewConsensusPolicyBuilder().
		WithMaxParticipants(3).
		WithAgreementThreshold(3).
		WithLogger(&logger).
		Build()

	req := newTestRequest()
	ctx := context.WithValue(context.Background(), common.RequestContextKey, req)

	var total, marked atomic.Int32
	resp, err := pol.Run(ctx, req, func(slotCtx context.Context, _ *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		total.Add(1)
		if common.IsConsensusSlot(slotCtx) {
			marked.Add(1)
		}
		return validResponse(), nil
	})
	require.NoError(t, err)
	require.NotNil(t, resp)

	assert.GreaterOrEqual(t, total.Load(), int32(3), "all participants should run")
	assert.Equal(t, total.Load(), marked.Load(),
		"every participant slot must carry the consensus-slot marker")

	// The marker is slot-scoped — the caller's own context stays unmarked.
	assert.False(t, common.IsConsensusSlot(ctx))
}
