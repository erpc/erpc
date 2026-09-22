package erpc

import (
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
)

// upstream helper: a FakeUpstream wired with a FakeEvmStatePoller at the
// given latest block. id encodes both identity and the original score
// position so test assertions can read order directly.
func upWithLatest(id string, latest int64) common.Upstream {
	poller := common.NewFakeEvmStatePoller(latest, 0)
	return common.NewFakeUpstream(id, common.WithEvmStatePoller(poller))
}

// upWithoutPoller returns a FakeUpstream with no state poller — the
// partition should treat it as "doesn't have the block" without panicking.
func upWithoutPoller(id string) common.Upstream {
	return common.NewFakeUpstream(id)
}

func ids(ups []common.Upstream) []string {
	out := make([]string, len(ups))
	for i, u := range ups {
		out[i] = u.Id()
	}
	return out
}

func TestPartitionUpstreamsByLatestBlock_AllHaveTheBlock_NoReorder(t *testing.T) {
	in := []common.Upstream{
		upWithLatest("a", 100),
		upWithLatest("b", 105),
		upWithLatest("c", 110),
	}
	got := partitionUpstreamsByLatestBlock(in, 100)
	assert.Equal(t, []string{"a", "b", "c"}, ids(got), "no upstream lags the requested block; partition is identity")
}

func TestPartitionUpstreamsByLatestBlock_NoneHaveTheBlock_NoReorder(t *testing.T) {
	in := []common.Upstream{
		upWithLatest("a", 50),
		upWithLatest("b", 60),
		upWithLatest("c", 70),
	}
	got := partitionUpstreamsByLatestBlock(in, 100)
	assert.Equal(t, []string{"a", "b", "c"}, ids(got), "no upstream has the requested block; partition is identity (caller can still try them)")
}

func TestPartitionUpstreamsByLatestBlock_SplitsAndPreservesIntraGroupOrder(t *testing.T) {
	// Mixed: a (behind), b (has), c (behind), d (has), e (behind).
	// Expected: b, d, a, c, e. Within "has" group: b before d (input order).
	// Within "lags" group: a, c, e (input order).
	in := []common.Upstream{
		upWithLatest("a", 95),
		upWithLatest("b", 110),
		upWithLatest("c", 90),
		upWithLatest("d", 105),
		upWithLatest("e", 88),
	}
	got := partitionUpstreamsByLatestBlock(in, 100)
	assert.Equal(t, []string{"b", "d", "a", "c", "e"}, ids(got))
}

func TestPartitionUpstreamsByLatestBlock_EqualLatestIsTreatedAsHavingBlock(t *testing.T) {
	// Boundary: an upstream at exactly bn has the block.
	in := []common.Upstream{
		upWithLatest("a", 99),
		upWithLatest("b", 100),
		upWithLatest("c", 100),
	}
	got := partitionUpstreamsByLatestBlock(in, 100)
	assert.Equal(t, []string{"b", "c", "a"}, ids(got))
}

func TestPartitionUpstreamsByLatestBlock_UpstreamWithoutPollerSortsAsLagging(t *testing.T) {
	in := []common.Upstream{
		upWithoutPoller("a"),
		upWithLatest("b", 110),
		upWithoutPoller("c"),
	}
	got := partitionUpstreamsByLatestBlock(in, 100)
	assert.Equal(t, []string{"b", "a", "c"}, ids(got), "poller-less upstreams keep input order, partition keeps them after the upstream that demonstrably has the block")
}

func TestPartitionUpstreamsByLatestBlock_ZeroOrNegativeBlockIsNoOp(t *testing.T) {
	in := []common.Upstream{
		upWithLatest("a", 100),
		upWithLatest("b", 50),
	}
	assert.Equal(t, in, partitionUpstreamsByLatestBlock(in, 0), "no block reference means no per-block routing preference")
	assert.Equal(t, in, partitionUpstreamsByLatestBlock(in, -1), "negative block numbers are nonsensical; skip the partition rather than mis-sort")
}

func TestPartitionUpstreamsByLatestBlock_SingleUpstreamIsNoOp(t *testing.T) {
	in := []common.Upstream{upWithLatest("a", 50)}
	got := partitionUpstreamsByLatestBlock(in, 100)
	assert.Equal(t, in, got, "no other upstream to prefer; partition is a no-op")
}
