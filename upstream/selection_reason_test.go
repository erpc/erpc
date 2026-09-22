package upstream

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
)

// Locks down the documented reason taxonomy and its precedence
// (consensus_slot > hedge > sweep > retry > primary): the outermost
// fan-out cause wins; IsHedge/IsRetry on the attempt record preserve the
// inner mechanics. See docs/pages/operation/directives.mdx.
func TestDeriveSelectionReason(t *testing.T) {
	bare := context.Background()
	slot := common.WithConsensusSlot(bare)
	sweep := common.WithSweepIteration(bare)

	cases := []struct {
		name    string
		ctx     context.Context
		isHedge bool
		retries int
		want    common.UpstreamSelectionReason
	}{
		{"first pick of a plain execution", bare, false, 0, common.SelectionReasonPrimary},
		{"first pick of a retry round", bare, false, 1, common.SelectionReasonRetry},
		{"hedge attempt", bare, true, 0, common.SelectionReasonHedge},
		{"hedge inside a retry round", bare, true, 2, common.SelectionReasonHedge},
		{"sweep past the first upstream", sweep, false, 0, common.SelectionReasonSweep},
		{"sweep within a retry round", sweep, false, 1, common.SelectionReasonSweep},
		{"hedged execution sweeping", sweep, true, 0, common.SelectionReasonHedge},
		{"consensus participant slot", slot, false, 0, common.SelectionReasonConsensusSlot},
		{"consensus slot that retried", slot, false, 3, common.SelectionReasonConsensusSlot},
		{"consensus slot that hedged", slot, true, 0, common.SelectionReasonConsensusSlot},
		{"consensus slot sweeping", common.WithSweepIteration(slot), false, 0, common.SelectionReasonConsensusSlot},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, deriveSelectionReason(tc.ctx, tc.isHedge, tc.retries))
		})
	}
}
