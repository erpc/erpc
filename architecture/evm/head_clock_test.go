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
// the next Ask; asked again, the trailing voter has the block. A timed
// chain (mintEvery) mints its blocks on a clock instead.
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
	// A timed chain's: block k is minted at t0 + k·mint; heads counts
	// the view heads, sampled is the third, the block time's first
	// sample, and came each block's first answer.
	mint    time.Duration
	t0      time.Time
	heads   int
	sampled uint64
	came    map[uint64]time.Time
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
	// polled: its full poll fetched just now (justAsked), so its turn is
	// never due; else only the clock's fetches date its turn.
	polled bool
}

// justAsked is a poller whose full poll fetched just now: no turn of it
// is due, so every clock fetch of it is the block's one fetch.
type justAsked struct{ common.EvmStatePoller }

func (justAsked) LatestAskAt() time.Time { return time.Now() }

func newClockNet(t *testing.T, id string, voters ...laneVoter) *clockNet {
	n := &clockNet{servedChain: newServedChain(t, id), rtt: map[string]time.Duration{}, trail: map[string]time.Duration{}, asks: map[string]int{}}
	for _, v := range voters {
		var p common.EvmStatePoller = common.NewFakeEvmStatePoller(0, 0)
		if v.polled {
			p = justAsked{p}
		}
		u := common.NewFakeUpstream(v.id, common.WithEvmStatePoller(p))
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

// EvmBlockTime is 500 ms; a timed chain's is unknown (0) until its third
// view head, the network's first block time sample (spec §6.7).
func (n *clockNet) EvmBlockTime() time.Duration {
	n.mu.Lock()
	defer n.mu.Unlock()
	switch {
	case n.mint == 0:
		return 500 * time.Millisecond
	case n.sampled == 0:
		return 0
	}
	return n.mint
}

func (n *clockNet) EvmViewHeadMoved(number, _ int64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.heads++; n.heads == 3 {
		n.sampled = uint64(number)
	}
}

// sampledAt is the view head the block time was first sampled at, 0
// before.
func (n *clockNet) sampledAt() uint64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.sampled
}

// mintEvery makes the chain timed: block 1 is minted now and one more
// every bt, each Ask answers the newest block minted, and every block it
// reached is served by number and hash.
func (n *clockNet) mintEvery(bt time.Duration) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.mint, n.t0, n.came = bt, time.Now().Add(-bt), map[uint64]time.Time{}
}

// mintedAt is when a timed chain minted block k.
func (n *clockNet) mintedAt(k uint64) time.Time {
	return n.t0.Add(time.Duration(k) * n.mint)
}

// lag is how long after its mint a timed chain's block k first came in an
// answer; ok false when it never did.
func (n *clockNet) lag(k uint64) (time.Duration, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	at, ok := n.came[k]
	return at.Sub(n.mintedAt(k)), ok
}

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
		n.c.observeSource(id, h, sent, HeaderPolled)
	}
	return raw
}

func (n *clockNet) EvmAskHead(_ context.Context, id string, _ common.HeadNeed, _ time.Duration, _ time.Time, reason common.AskReason) common.HeadAnswer {
	sent := time.Now().Add(-n.rtt[id])
	n.mu.Lock()
	if reason == common.AskClock {
		n.asks[id]++
	}
	if n.mint > 0 {
		now := time.Now()
		num := uint64(now.Sub(n.t0) / n.mint)
		for k := n.top + 1; k <= num; k++ {
			n.came[k] = now
			n.serve(k, testHash("c", k), testHeader(k, testHash("c", k), testHash("c", k-1)))
		}
		n.top = max(n.top, num)
		n.mu.Unlock()
		raw := n.vote(id, num, sent)
		return common.HeadAnswer{Outcome: common.AskFetched, At: sent, Next: sent.Add(time.Hour), Header: askedHeader(raw)}
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

// early is the clock's last rungs and tail rungs that answered early.
func (n *clockNet) early() float64 {
	var sum float64
	for _, kind := range []string{"probe", "retry"} {
		sum += testutil.ToFloat64(telemetry.MetricHeadClockTotal.WithLabelValues(n.c.projectId(), n.c.networkLabel(), n.c.laneLabel(), kind, "early"))
	}
	return sum
}

// The block clock's target (spec §6.8): a voter's poll interval is its
// operator's read budget for its head. Voters whose turn is due take the
// block's fetches as the ladder's rungs; with none due, the fastest voter
// takes the block's one fetch. Each voter the clock has not measured
// takes one fetch, which measures its round trip.
func TestBlockClockTarget(t *testing.T) {
	t.Run("voters polled every block take its turns, not the faster one polled every 10 s", func(t *testing.T) {
		n := newClockNet(t, "evm:clock-turns",
			laneVoter{id: "vendor-a", interval: 10 * time.Second, rtt: 100 * time.Millisecond, polled: true},
			laneVoter{id: "node-a", interval: 500 * time.Millisecond, rtt: 300 * time.Millisecond},
			laneVoter{id: "node-b", interval: 500 * time.Millisecond, rtt: 350 * time.Millisecond},
		)
		got := n.clockAsks(t, 20)
		assert.LessOrEqual(t, got["vendor-a"], 3, "a voter polled every 10 s takes about no fetch: %v", got)
		assert.GreaterOrEqual(t, got["node-a"], 7, "each voter polled every block takes its turns: %v", got)
		assert.GreaterOrEqual(t, got["node-b"], 7, "each voter polled every block takes its turns: %v", got)
	})

	t.Run("with no turn due the fastest voter takes the block's one fetch", func(t *testing.T) {
		n := newClockNet(t, "evm:clock-fallback",
			laneVoter{id: "vendor-a", interval: 10 * time.Second, rtt: 200 * time.Millisecond, polled: true},
			laneVoter{id: "vendor-b", interval: 10 * time.Second, rtt: 600 * time.Millisecond, polled: true},
		)
		got := n.clockAsks(t, 10)
		assert.LessOrEqual(t, got["vendor-b"], 1, "the slower voter only takes its measuring fetch: %v", got)
		assert.GreaterOrEqual(t, got["vendor-a"], 9, "the fastest voter takes the fetches: %v", got)
	})

	// node-a has the lowest round trip but gets each block 0.3 block after
	// node-b: its answers come early, and node-b's fetch sent before node-a's
	// brought the block, so node-a lagged whichever answer arrived first.
	t.Run("a voter trailing by 0.3 block is marked late and stops taking the block's fetch", func(t *testing.T) {
		n := newClockNet(t, "evm:clock-trail",
			laneVoter{id: "node-a", interval: 10 * time.Second, rtt: 100 * time.Millisecond, trail: 150 * time.Millisecond, polled: true},
			laneVoter{id: "node-b", interval: 10 * time.Second, rtt: 120 * time.Millisecond, polled: true},
		)
		got := n.clockAsks(t, 14)
		assert.LessOrEqual(t, got["node-a"], 4, "the trailing voter stops taking fetches once marked late: %v", got)
		assert.GreaterOrEqual(t, got["node-b"], 10, "the voter that does not trail takes them: %v", got)
		assert.LessOrEqual(t, n.early(), 2.0, "early answers stop with the trailing target: %v", got)
	})
}

// A cold instance reads the block time as health.ColdBlockTime (1 s)
// until the network's first sample (spec §6.7). Under eth_blockNumber
// reads from the start, each block after the sample comes within a block
// of its mint. Laid and planned by the cold 1 s, a 3 s chain's first
// sights came two slots late and the sample put the next fetch three
// blocks out; a 0.25 s chain's plans went seconds out (#1426: 12 s
// blocks, 145 s).
func TestBlockClockColdStart(t *testing.T) {
	for _, tc := range []struct {
		bt     time.Duration
		blocks uint64 // checked after the sample
	}{
		{3 * time.Second, 2},
		{250 * time.Millisecond, 8},
	} {
		t.Run(tc.bt.String()+" blocks", func(t *testing.T) {
			n := newClockNet(t, "evm:clock-cold-"+tc.bt.String(),
				laneVoter{id: "node-a", interval: 30 * time.Second, rtt: 30 * time.Millisecond},
				laneVoter{id: "node-b", interval: 30 * time.Second, rtt: 30 * time.Millisecond},
			)
			n.mintEvery(tc.bt)
			var last uint64
			for end := time.Now().Add(15 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
				n.c.LatestRead(t.Context(), true)
				if s := n.sampledAt(); s > 0 {
					last = s + tc.blocks
					if _, ok := n.lag(last); ok {
						break
					}
				}
			}
			require.NotZero(t, last, "the block time was sampled")
			for k := last - tc.blocks + 1; k <= last; k++ {
				lag, ok := n.lag(k)
				require.True(t, ok, "block %d never came: the clock fetched nothing after the sample", k)
				assert.Less(t, lag, tc.bt, "block %d came %v after its mint", k, lag)
			}
		})
	}
}
