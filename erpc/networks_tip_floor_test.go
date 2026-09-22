package erpc

import (
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
)

// newTipFloorTestNetwork builds the minimal Network needed by
// applyDeliveredHeadFloor: only the logger and networkId are read. With no
// metricsTracker the bound falls back to defaultMaxRetryableBlockDistance.
func newTipFloorTestNetwork() *Network {
	logger := zerolog.Nop()
	return &Network{
		logger:    &logger,
		networkId: "evm:1234",
	}
}

func TestDeliveredHeadFloor_NoDeliveriesServesComputedHeadAsIs(t *testing.T) {
	n := newTipFloorTestNetwork()

	assert.Equal(t, int64(100), n.applyDeliveredHeadFloor(100, 100))
	// The computed head is never remembered: a later, lower pick (fail-open,
	// small reorg) passes straight through.
	assert.Equal(t, int64(95), n.applyDeliveredHeadFloor(95, 95))
	assert.Equal(t, int64(0), n.applyDeliveredHeadFloor(0, 0), "an unknown head stays unknown")
}

func TestDeliveredHeadFloor_SmallLagIsFlooredToDeliveredHead(t *testing.T) {
	n := newTipFloorTestNetwork()

	// A head already delivered on a subscription sits one block ahead of
	// what the pollers corroborate.
	n.NoteObservedLatestBlock(nil, 1001)
	assert.Equal(t, int64(1001), n.applyDeliveredHeadFloor(1000, 1001),
		"a computed head just below the delivered head must be floored")
	assert.Equal(t, int64(1005), n.applyDeliveredHeadFloor(1005, 1005),
		"once the computed head passes the floor it is served as computed")
}

func TestDeliveredHeadFloor_BoundIsMeasuredAgainstFreshestLiveHead(t *testing.T) {
	n := newTipFloorTestNetwork()

	// A stalled sibling drags the corroborated (second-highest) head far
	// below the freshest upstream. The delivered head came from that
	// freshest upstream, so it must still floor the tip.
	n.NoteObservedLatestBlock(nil, 9001)
	assert.Equal(t, int64(9001), n.applyDeliveredHeadFloor(1000, 9000),
		"a floor within reach of the freshest live head is honoured")

	// A stale or poisoned floor that no live upstream comes close to must
	// not pin the tip, even with a stalled sibling.
	n.deliveredLatestBlock.Store(9000 + defaultMaxRetryableBlockDistance + 1)
	assert.Equal(t, int64(1000), n.applyDeliveredHeadFloor(1000, 9000))

	// Exactly at the bound is still honoured.
	n.deliveredLatestBlock.Store(9000 + defaultMaxRetryableBlockDistance)
	assert.Equal(t, int64(9000+defaultMaxRetryableBlockDistance), n.applyDeliveredHeadFloor(1000, 9000))
}

func TestDeliveredHeadFloor_NoLiveHeadAdoptsDeliveredHead(t *testing.T) {
	n := newTipFloorTestNetwork()

	// Pollers cold (no head yet): a head already streamed to a subscriber is
	// the best available answer, regardless of distance.
	n.NoteObservedLatestBlock(nil, 5_000_000)
	assert.Equal(t, int64(5_000_000), n.applyDeliveredHeadFloor(0, 0))
}

func TestNoteObservedLatestBlock_IsMonotonic(t *testing.T) {
	n := newTipFloorTestNetwork()
	n.NoteObservedLatestBlock(nil, 200)
	n.NoteObservedLatestBlock(nil, 150)
	n.NoteObservedLatestBlock(nil, 0)
	assert.Equal(t, int64(200), n.deliveredLatestBlock.Load())
}
