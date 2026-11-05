package evm

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// clockNet is a lane of voters the block clock fetches, with 500 ms
// blocks: each head Ask brings the next block, voted through intake as the
// network does, and answers as sent its voter's round trip before.
type clockNet struct {
	*servedChain
	ups  []common.Upstream
	rtt  map[string]time.Duration
	c    *ChainTracker
	mu   sync.Mutex
	top  uint64
	asks map[string]int // clock Asks per voter
}

type laneVoter struct {
	id       string
	interval time.Duration // statePollerInterval
	rtt      time.Duration
}

// justAsked is a poller whose full poll fetched just now: no clock turn is
// due, so every clock fetch is a probe's choice.
type justAsked struct{ common.EvmStatePoller }

func (justAsked) LatestAskAt() time.Time { return time.Now() }

func newClockNet(t *testing.T, voters ...laneVoter) *clockNet {
	n := &clockNet{servedChain: newServedChain(t, "evm:clock"), rtt: map[string]time.Duration{}, asks: map[string]int{}}
	for _, v := range voters {
		u := common.NewFakeUpstream(v.id, common.WithEvmStatePoller(justAsked{common.NewFakeEvmStatePoller(0, 0)}))
		u.Config().Evm = &common.EvmUpstreamConfig{StatePollerInterval: common.Duration(v.interval)}
		n.ups = append(n.ups, u)
		n.rtt[v.id] = v.rtt
	}
	n.c = newChainTracker(t.Context(), n, 32, 0, "", "", "", nil, newFetchScheduler(t.Context(), n))
	return n
}

func (n *clockNet) EvmAllUpstreams(context.Context) []common.Upstream { return n.ups }
func (n *clockNet) EvmBlockTime() time.Duration                       { return 500 * time.Millisecond }

func (n *clockNet) EvmHashVoters(string, time.Duration) HashVoters {
	var ids []string
	for _, u := range n.ups {
		ids = append(ids, u.Id())
	}
	return HashVoters{Voters: ids, Hash: ids, Need: 1}
}

func (n *clockNet) EvmAskHead(_ context.Context, id string, _ common.HeadNeed, _ time.Duration, _ time.Time, reason common.AskReason) common.HeadAnswer {
	n.mu.Lock()
	n.top++
	num := n.top
	if reason == common.AskClock {
		n.asks[id]++
	}
	n.mu.Unlock()
	h := testHeader(num, testHash("c", num), testHash("c", num-1))
	n.serve(num, testHash("c", num), h)
	n.c.Observe(id, h, true)
	sent := time.Now().Add(-n.rtt[id])
	return common.HeadAnswer{Outcome: common.AskFetched, At: sent, Next: sent.Add(time.Hour), Header: askedHeader(h)}
}

// clockAsks runs the clock under read demand until it sent total fetches,
// and returns them per voter.
func (n *clockNet) clockAsks(t *testing.T, total int) map[string]int {
	got := map[string]int{}
	require.Eventually(t, func() bool {
		n.c.MarkRead()
		n.mu.Lock()
		defer n.mu.Unlock()
		sum := 0
		for id, k := range n.asks {
			got[id], sum = k, sum+k
		}
		return sum >= total
	}, 30*time.Second, 10*time.Millisecond)
	return got
}

// The block clock's target (spec §6.8): a voter's poll interval is its
// operator's read budget for its head. Probes go to the fastest voter
// polled at least every 2 blocks, never to a faster one polled every 10 s;
// with none polled that often, the fastest takes them. Each voter the clock
// has not measured takes one fetch, which measures its round trip.
func TestBlockClockTarget(t *testing.T) {
	t.Run("probes go to the voter polled every 500 ms, not the faster one polled every 10 s", func(t *testing.T) {
		n := newClockNet(t,
			laneVoter{id: "vendor-a", interval: 10 * time.Second, rtt: 200 * time.Millisecond},
			laneVoter{id: "node-a", interval: 500 * time.Millisecond, rtt: 300 * time.Millisecond},
			laneVoter{id: "node-b", interval: 500 * time.Millisecond, rtt: 600 * time.Millisecond},
		)
		got := n.clockAsks(t, 10)
		assert.Zero(t, got["vendor-a"], "a voter polled every 10 s takes no probe: %v", got)
		assert.LessOrEqual(t, got["node-b"], 1, "the slower fast-polled voter only takes its measuring fetch: %v", got)
		assert.GreaterOrEqual(t, got["node-a"], 9, "the fastest fast-polled voter takes the probes: %v", got)
	})

	t.Run("with no voter polled every 2 blocks the fastest takes the probes", func(t *testing.T) {
		n := newClockNet(t,
			laneVoter{id: "vendor-a", interval: 10 * time.Second, rtt: 200 * time.Millisecond},
			laneVoter{id: "vendor-b", interval: 10 * time.Second, rtt: 600 * time.Millisecond},
		)
		got := n.clockAsks(t, 10)
		assert.LessOrEqual(t, got["vendor-b"], 1, "the slower voter only takes its measuring fetch: %v", got)
		assert.GreaterOrEqual(t, got["vendor-a"], 9, "the fastest voter takes the probes: %v", got)
	})
}
