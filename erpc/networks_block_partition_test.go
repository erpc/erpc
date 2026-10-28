package erpc

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	got := (&Network{}).partitionUpstreamsByLatestBlock(in, 100)
	assert.Equal(t, []string{"a", "b", "c"}, ids(got), "no upstream lags the requested block; partition is identity")
}

func TestPartitionUpstreamsByLatestBlock_NoneHaveTheBlock_NoReorder(t *testing.T) {
	in := []common.Upstream{
		upWithLatest("a", 50),
		upWithLatest("b", 60),
		upWithLatest("c", 70),
	}
	got := (&Network{}).partitionUpstreamsByLatestBlock(in, 100)
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
	got := (&Network{}).partitionUpstreamsByLatestBlock(in, 100)
	assert.Equal(t, []string{"b", "d", "a", "c", "e"}, ids(got))
}

func TestPartitionUpstreamsByLatestBlock_EqualLatestIsTreatedAsHavingBlock(t *testing.T) {
	in := []common.Upstream{
		upWithLatest("a", 99),
		upWithLatest("b", 100),
		upWithLatest("c", 100),
	}
	got := (&Network{}).partitionUpstreamsByLatestBlock(in, 100)
	assert.Equal(t, []string{"b", "c", "a"}, ids(got))
}

func TestPartitionUpstreamsByLatestBlock_UnknownHeadKeepsItsPlace(t *testing.T) {
	in := []common.Upstream{
		common.NewFakeUpstream("a"),
		upWithLatest("b", 90),
		upWithLatest("c", 110),
		upWithLatest("d", 0),
	}
	got := (&Network{}).partitionUpstreamsByLatestBlock(in, 100)
	assert.Equal(t, []string{"a", "c", "d", "b"}, ids(got), "only a known head below the block moves back")
}

func TestPartitionUpstreamsByLatestBlock_ZeroOrNegativeBlockIsNoOp(t *testing.T) {
	in := []common.Upstream{
		upWithLatest("a", 100),
		upWithLatest("b", 50),
	}
	assert.Equal(t, in, (&Network{}).partitionUpstreamsByLatestBlock(in, 0), "no block reference means no per-block routing preference")
	assert.Equal(t, in, (&Network{}).partitionUpstreamsByLatestBlock(in, -1), "negative block numbers are nonsensical; skip the partition rather than mis-sort")
}

func TestPartitionUpstreamsByLatestBlock_SingleUpstreamIsNoOp(t *testing.T) {
	in := []common.Upstream{upWithLatest("a", 50)}
	got := (&Network{}).partitionUpstreamsByLatestBlock(in, 100)
	assert.Equal(t, in, got, "no other upstream to prefer; partition is a no-op")
}

func TestPreferTipLeaderForNearTipGetBlock(t *testing.T) {
	const method = "eth_getBlockByNumber"

	tied := []common.Upstream{upWithLatest("c", 1000), upWithLatest("b", 1000), upWithLatest("a", 1000)}
	assert.Equal(t, []string{"c", "b", "a"}, ids((&Network{}).preferTipLeaderForNearTipGetBlock(tied, method, 1000)),
		"tied heads keep the selection policy's order")

	ahead := []common.Upstream{upWithLatest("c", 999), upWithLatest("b", 999), upWithLatest("a", 1000)}
	assert.Equal(t, []string{"a", "c", "b"}, ids((&Network{}).preferTipLeaderForNearTipGetBlock(ahead, method, 1001)),
		"a strict leader goes first for its next block")
	assert.Equal(t, []string{"c", "b", "a"}, ids((&Network{}).preferTipLeaderForNearTipGetBlock(ahead, method, 998)),
		"blocks every candidate has keep the policy order")
	assert.Equal(t, []string{"c", "b", "a"}, ids((&Network{}).preferTipLeaderForNearTipGetBlock(ahead, "eth_call", 1000)))
}

// A polled head ages by up to a poll interval. Through real pollers, the real
// tracker and the real Network: "fast" is first in policy order and polls
// every 10s, so a few seconds after its poll it reads blocks behind "slow" and
// "reader", which polled since ("reader" serves no eth_getBlockByNumber).
// A tip read still goes to "fast" first while its poll is younger than its
// interval; once "fast" stops reporting past that, it is demoted again.
func TestNetwork_TipGetBlockOrdersByPollAgeCreditedHead(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	advance := pinServedTipClock(t)

	hits := map[string]*atomic.Int64{"fast": {}, "slow": {}}
	for id, n := range hits {
		for _, bn := range []int64{1003, 1010} {
			hex := fmt.Sprintf("0x%x", bn)
			gock.New("http://" + id + ".localhost").
				Post("").
				Persist().
				Filter(func(r *http.Request) bool {
					body := util.SafeReadBody(r)
					if r.URL.Host != id+".localhost" || !strings.Contains(body, "eth_getBlockByNumber") || !strings.Contains(body, `"`+hex+`"`) {
						return false
					}
					n.Add(1)
					return true
				}).
				Reply(200).
				JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"number":"` + hex + `","hash":"0x` + strings.Repeat("a", 64) + `","parentHash":"0x` + strings.Repeat("b", 64) + `","timestamp":"0x6702a8f1"}}`))
		}
	}

	const interval = 10 * time.Second
	network, ups := setupServedTipNetworkWith(t, ctx, []servedTipFixture{
		{id: "fast", chainID: 123, latestBlock: 1000, statePollerInterval: interval},
		{id: "slow", chainID: 123, latestBlock: 1000, statePollerInterval: interval},
		{id: "reader", chainID: 123, latestBlock: 1000, statePollerInterval: interval, ignoreMethods: []string{"eth_getBlockByNumber"}},
	}, nil)
	network.PinUpstreamOrderForTest("fast", "slow", "reader")
	fast, slow, reader := ups[0], ups[1], ups[2]

	head := int64(1000)
	step := func() {
		advance(2 * time.Second)
		head++
		slow.EvmStatePoller().SuggestLatestBlock(head)
		reader.EvmStatePoller().SuggestLatestBlock(head)
	}
	getBlock := func(bn int64) string {
		t.Helper()
		req := common.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x%x",false]}`, bn)))
		req.SetNetwork(network)
		resp, err := network.Forward(ctx, req)
		require.NoError(t, err)
		require.NotNil(t, resp.Upstream())
		return resp.Upstream().Id()
	}

	// 6s after its poll, "fast" reads 3 blocks behind; its poll is younger
	// than its interval, so it is presumed at the head.
	for range 3 {
		step()
	}
	require.Equal(t, int64(1000), fast.EvmStatePoller().LatestBlock())
	require.Equal(t, head, slow.EvmStatePoller().LatestBlock())
	assert.Equal(t, "fast", getBlock(head), "a head that only aged since its poll must not demote the first-ranked upstream")
	assert.Zero(t, hits["slow"].Load(), "the slow upstream must not take the tip read")

	// "fast" stops reporting past its interval while the chain goes on.
	for range 7 {
		step()
	}
	hits["fast"].Store(0)
	assert.Equal(t, "slow", getBlock(head), "a head that stopped reporting past its interval stays demoted")
	assert.Zero(t, hits["fast"].Load(), "the stale upstream must not be tried first")
}
