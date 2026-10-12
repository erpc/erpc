package erpc

import (
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
)

// upWithLatest returns a FakeUpstream whose poller reports latest.
func upWithLatest(id string, latest int64) common.Upstream {
	poller := common.NewFakeEvmStatePoller(latest, 0)
	return common.NewFakeUpstream(id, common.WithEvmStatePoller(poller))
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
	in := []common.Upstream{
		upWithLatest("a", 99),
		upWithLatest("b", 100),
		upWithLatest("c", 100),
	}
	got := partitionUpstreamsByLatestBlock(in, 100)
	assert.Equal(t, []string{"b", "c", "a"}, ids(got))
}

func TestPartitionUpstreamsByLatestBlock_UnknownHeadKeepsItsPlace(t *testing.T) {
	in := []common.Upstream{
		common.NewFakeUpstream("a"),
		upWithLatest("b", 90),
		upWithLatest("c", 110),
		upWithLatest("d", 0),
	}
	got := partitionUpstreamsByLatestBlock(in, 100)
	assert.Equal(t, []string{"a", "c", "d", "b"}, ids(got), "only a known head below the block moves back")
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

func TestPreferTipLeaderForNearTipGetBlock(t *testing.T) {
	const method = "eth_getBlockByNumber"

	tied := []common.Upstream{upWithLatest("c", 1000), upWithLatest("b", 1000), upWithLatest("a", 1000)}
	assert.Equal(t, []string{"c", "b", "a"}, ids(preferTipLeaderForNearTipGetBlock(tied, method, 1000)),
		"tied heads keep the selection policy's order")

	ahead := []common.Upstream{upWithLatest("c", 999), upWithLatest("b", 999), upWithLatest("a", 1000)}
	assert.Equal(t, []string{"a", "c", "b"}, ids(preferTipLeaderForNearTipGetBlock(ahead, method, 1001)),
		"a strict leader goes first for its next block")
	assert.Equal(t, []string{"c", "b", "a"}, ids(preferTipLeaderForNearTipGetBlock(ahead, method, 998)),
		"blocks every candidate has keep the policy order")
	assert.Equal(t, []string{"c", "b", "a"}, ids(preferTipLeaderForNearTipGetBlock(ahead, "eth_call", 1000)))
}
