package erpc

import (
	"context"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
)

// newTipFloorTestNetwork builds the minimal Network applyDeliveredHeadFloor
// needs. With no metricsTracker the bound is defaultMaxRetryableBlockDistance.
func newTipFloorTestNetwork() *Network {
	logger := zerolog.Nop()
	return &Network{logger: &logger}
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
	n.NoteObservedLatestBlock(context.Background(), 1001)
	assert.Equal(t, int64(1001), n.applyDeliveredHeadFloor(1000, 1001),
		"a computed head just below the delivered head must be floored")
	assert.Equal(t, int64(1005), n.applyDeliveredHeadFloor(1005, 1005),
		"once the computed head passes the floor it is served as computed")
}

func TestDeliveredHeadFloor_BoundIsMeasuredAgainstFreshestLiveHead(t *testing.T) {
	n := newTipFloorTestNetwork()

	// A stalled upstream drags the corroborated head far below the freshest
	// one, which the delivered head came from.
	n.NoteObservedLatestBlock(context.Background(), 9001)
	assert.Equal(t, int64(9001), n.applyDeliveredHeadFloor(1000, 9000),
		"a floor within reach of the freshest live head is honoured")

	// A floor no live upstream comes close to is ignored.
	n.deliveredLatestBlock.Store(9000 + defaultMaxRetryableBlockDistance + 1)
	assert.Equal(t, int64(1000), n.applyDeliveredHeadFloor(1000, 9000))

	// Exactly at the bound is still honoured.
	n.deliveredLatestBlock.Store(9000 + defaultMaxRetryableBlockDistance)
	assert.Equal(t, int64(9000+defaultMaxRetryableBlockDistance), n.applyDeliveredHeadFloor(1000, 9000))
}

func TestDeliveredHeadFloor_NoLiveHeadAdoptsDeliveredHead(t *testing.T) {
	n := newTipFloorTestNetwork()

	// With no known head, the delivered head is used regardless of distance.
	n.NoteObservedLatestBlock(context.Background(), 5_000_000)
	assert.Equal(t, int64(5_000_000), n.applyDeliveredHeadFloor(0, 0))
}

func TestNoteObservedLatestBlock_IsMonotonic(t *testing.T) {
	n := newTipFloorTestNetwork()
	n.NoteObservedLatestBlock(context.Background(), 200)
	n.NoteObservedLatestBlock(context.Background(), 150)
	n.NoteObservedLatestBlock(context.Background(), 0)
	assert.Equal(t, int64(200), n.deliveredLatestBlock.Load())
}
