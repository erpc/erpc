package evm

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// clockNet is a lane of voters the block clock fetches, with 500 ms
// blocks. Each head Ask of a voter that does not trail brings the next
// block, voted through intake as the network does, dated its voter's
// round trip before it returned. A trailing voter asked for a new block
// answers the one before: a voter that does not trail (lead) fetched the
// new block with a fetch sent the trail before it, whose vote arrives at
// the next Ask; asked again, the trailing voter has the block.
type clockNet struct {
	*servedChain
	ups   []common.Upstream
	rtt   map[string]time.Duration
	trail map[string]time.Duration
	lead  string
	c     *ChainTracker
	mu    sync.Mutex
	top   uint64
	owed  *owedVote
	asks  map[string]int // clock Asks per voter
}

type owedVote struct {
	id   string
	num  uint64
	sent time.Time
}

type laneVoter struct {
	id       string
	interval time.Duration // statePollerInterval
	rtt      time.Duration
	trail    time.Duration
}

// justAsked is a poller whose full poll fetched just now: no clock turn is
// due, so every clock fetch is a probe's choice.
type justAsked struct{ common.EvmStatePoller }

func (justAsked) LatestAskAt() time.Time { return time.Now() }

func newClockNet(t *testing.T, id string, voters ...laneVoter) *clockNet {
	n := &clockNet{servedChain: newServedChain(t, id), rtt: map[string]time.Duration{}, trail: map[string]time.Duration{}, asks: map[string]int{}}
	for _, v := range voters {
		u := common.NewFakeUpstream(v.id, common.WithEvmStatePoller(justAsked{common.NewFakeEvmStatePoller(0, 0)}))
		u.Config().Evm = &common.EvmUpstreamConfig{StatePollerInterval: common.Duration(v.interval)}
		n.ups = append(n.ups, u)
		n.rtt[v.id], n.trail[v.id] = v.rtt, v.trail
		if v.trail == 0 {
			n.lead = v.id
		}
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

// vote is id's fetch, sent at sent, of block num, through intake.
func (n *clockNet) vote(id string, num uint64, sent time.Time) json.RawMessage {
	raw := testHeader(num, testHash("c", num), testHash("c", num-1))
	n.serve(num, testHash("c", num), raw)
	h, err := parseChainHeader(raw)
	if err == nil {
		n.c.observeSource(id, h, sent, false)
	}
	return raw
}

func (n *clockNet) EvmAskHead(_ context.Context, id string, _ common.HeadNeed, _ time.Duration, _ time.Time, reason common.AskReason) common.HeadAnswer {
	sent := time.Now().Add(-n.rtt[id])
	n.mu.Lock()
	if reason == common.AskClock {
		n.asks[id]++
	}
	owed := n.owed
	n.owed = nil
	switch {
	case n.trail[id] == 0:
		n.top++
	case owed == nil:
		n.top++
		n.owed = &owedVote{id: n.lead, num: n.top, sent: sent.Add(-n.trail[id])}
	}
	num := n.top
	if n.owed != nil {
		num--
	}
	n.mu.Unlock()
	if owed != nil {
		n.vote(owed.id, owed.num, owed.sent)
	}
	raw := n.vote(id, num, sent)
	return common.HeadAnswer{Outcome: common.AskFetched, At: sent, Next: sent.Add(time.Hour), Header: askedHeader(raw)}
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

// early is the clock's probes and retries that answered early.
func (n *clockNet) early() float64 {
	var sum float64
	for _, kind := range []string{"probe", "retry"} {
		sum += testutil.ToFloat64(telemetry.MetricHeadClockTotal.WithLabelValues(n.c.projectId(), n.c.networkLabel(), n.c.laneLabel(), kind, "early"))
	}
	return sum
}

// The block clock's target (spec §6.8): a voter's poll interval is its
// operator's read budget for its head. Probes go to the fastest voter
// polled at least every 2 blocks, never to a faster one polled every 10 s;
// with none polled that often, the fastest takes them. Each voter the clock
// has not measured takes one fetch, which measures its round trip.
func TestBlockClockTarget(t *testing.T) {
	t.Run("probes go to the voter polled every 500 ms, not the faster one polled every 10 s", func(t *testing.T) {
		n := newClockNet(t, "evm:clock-budget",
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
		n := newClockNet(t, "evm:clock-fallback",
			laneVoter{id: "vendor-a", interval: 10 * time.Second, rtt: 200 * time.Millisecond},
			laneVoter{id: "vendor-b", interval: 10 * time.Second, rtt: 600 * time.Millisecond},
		)
		got := n.clockAsks(t, 10)
		assert.LessOrEqual(t, got["vendor-b"], 1, "the slower voter only takes its measuring fetch: %v", got)
		assert.GreaterOrEqual(t, got["vendor-a"], 9, "the fastest voter takes the probes: %v", got)
	})

	// node-a has the lowest round trip but gets each block 0.3 block after
	// node-b: its answers come early, and node-b's fetch sent before node-a's
	// brought the block, so node-a lagged whichever answer arrived first.
	t.Run("a voter trailing by 0.3 block is marked late and stops taking probes", func(t *testing.T) {
		n := newClockNet(t, "evm:clock-trail",
			laneVoter{id: "node-a", interval: 500 * time.Millisecond, rtt: 100 * time.Millisecond, trail: 150 * time.Millisecond},
			laneVoter{id: "node-b", interval: 500 * time.Millisecond, rtt: 120 * time.Millisecond},
		)
		got := n.clockAsks(t, 14)
		assert.LessOrEqual(t, got["node-a"], 4, "the trailing voter stops taking probes once marked late: %v", got)
		assert.GreaterOrEqual(t, got["node-b"], 10, "the voter that does not trail takes them: %v", got)
		assert.LessOrEqual(t, n.early(), 2.0, "early answers stop with the trailing target: %v", got)
	})
}
