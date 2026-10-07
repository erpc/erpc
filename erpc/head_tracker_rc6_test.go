package erpc

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"
)

func init() { util.ConfigureTestLogger() }

// Unlike cold-start coverage, begin with a warm but wrong six-second EMA.
// Drive the real tick and health EMA: sparse observations must recover
// rather than validate their own spacing.
func TestHeadTracker_UnderPolledLeaderConverges(t *testing.T) {
	for _, bt := range []time.Duration{time.Second, 2 * time.Second} {
		t.Run(bt.String(), func(t *testing.T) {
			start := time.Unix(1_700_000_000, 0)
			tr := health.NewTracker(&log.Logger, "p", time.Minute)
			for i := int64(0); i <= 4; i++ {
				tr.ObserveNetworkHead("evm:1", "n", 996+i, start.Unix()-24+6*i)
			}
			require.Equal(t, 6*time.Second, tr.GetNetworkBlockTime("evm:1"))
			c := regularChain(start, bt, 400*time.Millisecond, 1000)
			var steadyPolls int
			var first, last int64
			polls, heads, lag, _ := runSimLeader(t, c, func() time.Duration { return tr.GetNetworkBlockTime("evm:1") }, start, 2*time.Minute, func(ht *headTracker) {
				ht.deps.onAccepted = func(_ context.Context, obs *headObservation) {
					tr.ObserveNetworkHead("evm:1", "n", obs.Number, obs.Timestamp)
				}
				poll := ht.deps.poll
				ht.deps.poll = func(ctx context.Context, full bool) (*headObservation, error) {
					obs, err := poll(ctx, full)
					if ht.deps.now().Sub(start) >= time.Minute {
						if steadyPolls == 0 {
							first = obs.Number
						}
						steadyPolls++
						last = obs.Number
					}
					return obs, err
				}
			})
			perBlock := float64(steadyPolls-1) / float64(last-first)
			t.Logf("%s seeded at 6s: %d polls, %d blocks, steady %.3f polls/block, mean lag %.3f, EMA %s", bt, polls, heads, perBlock, lag, tr.GetNetworkBlockTime("evm:1"))
			require.InDelta(t, 1, perBlock, 0.2, "must converge within one minute")
			require.LessOrEqual(t, lag, 1.0)
			require.InDelta(t, float64(bt), float64(tr.GetNetworkBlockTime("evm:1")), float64(bt)/10)
		})
	}
}

// rc.6 regressions found on staging (rc.5, 2026-10-06):
//   - a single poll slower than one block time forked a second, permanently
//     offset poll schedule (1-2s chains: 1.0 -> 1.3-1.8 polls/block);
//   - fresh pods polled at the 1s cold default until their own EMA warmed;
//   - nibiru (bursty head visibility) polled 1.71 times per block.

// simChain is a deterministic chain on a virtual clock. visible(now) returns
// the head (index into ts) an RPC node answers at `now`.
type simChain struct {
	ts      []int64
	visible func(now time.Time) int
}

// runSimLeader drives one leader's tick loop on a virtual clock for `total`,
// and returns polls, heads advanced, and the mean and maximum lag (blocks)
// behind what was visible after the startup window.
func runSimLeader(t *testing.T, c *simChain, blockTime func() time.Duration, start time.Time, total time.Duration, prep func(*headTracker)) (polls int, heads int64, meanLag float64, maxLag int64) {
	t.Helper()
	ctx := t.Context()
	ssr := newTestSSR(t, ctx)
	now := start
	ht := newTestTracker(ssr, nil, headTrackerDeps{
		now: func() time.Time { return now }, blockTime: blockTime,
		poll: func(context.Context, bool) (*headObservation, error) {
			polls++
			n := c.visible(now)
			return &headObservation{Number: 1000 + int64(n), Timestamp: c.ts[n], Hash: fmt.Sprintf("0x%x", n)}, nil
		},
	})
	ht.leaseDeadlineNs.Store(start.Add(24 * time.Hour).UnixNano())
	if prep != nil {
		prep(ht)
	}
	first := int64(-1)
	lagSum, lagN := 0.0, 0
	next := start
	for now.Sub(start) < total {
		if !now.Before(next) {
			w, err := ht.tick(ctx)
			require.NoError(t, err)
			next = now.Add(w)
			if first < 0 {
				first = ht.Head()
			}
		}
		if now.Sub(start) > 30*time.Second {
			lag := 1000 + int64(c.visible(now)) - ht.Head()
			maxLag = max(maxLag, lag)
			lagSum += float64(lag)
			lagN++
		}
		now = now.Add(20 * time.Millisecond)
	}
	return polls, ht.Head() - first, lagSum / float64(max(lagN, 1)), maxLag
}

// regularChain: one block per bt, visible `delay` after its (whole-second)
// timestamp.
func regularChain(start time.Time, bt, delay time.Duration, n int) *simChain {
	c := &simChain{}
	for i := 0; i < n; i++ {
		c.ts = append(c.ts, start.Add(time.Duration(i)*bt).Unix())
	}
	c.visible = func(now time.Time) int {
		k := int(now.Add(-delay).Sub(start) / bt)
		return min(max(k, 0), n-1)
	}
	return c
}

// Fresh replicas (no EMA, nothing published, the 1s cold default) on a 1s and
// a 6s chain must stay at ~1 poll per block over the first five minutes.
func TestHeadTracker_ColdStartPollsAboutOncePerBlock(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	for _, tc := range []struct {
		name  string
		bt    time.Duration
		delay time.Duration
	}{
		{"1s chain", time.Second, 400 * time.Millisecond},
		{"6s chain", 6 * time.Second, 1500 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := regularChain(start, tc.bt, tc.delay, 2000)
			// The network EMA stays cold for the whole run: the tracker must
			// not rely on it.
			polls, heads, lag, _ := runSimLeader(t, c, func() time.Duration { return 0 }, start, 5*time.Minute, nil)
			perBlock := float64(polls) / float64(heads)
			t.Logf("%s cold start: %d polls, %d heads: %.2f polls/block, mean lag %.2f blocks", tc.name, polls, heads, perBlock, lag)
			require.LessOrEqual(t, perBlock, 1.2, "cold start must not over-poll")
			require.LessOrEqual(t, lag, 1.0, "and still follows the head")
		})
	}
}

// A fresh leader after a deploy (EMA cold) must start from the block time
// the previous leader published, not the 1s cold default.
func TestHeadTracker_PublishedBlockTimeSeedsFreshReplica(t *testing.T) {
	ctx := t.Context()
	ssr := newTestSSR(t, ctx)
	start := time.Unix(1_700_000_000, 0)
	now := start
	clock := func() time.Time { return now }
	bt := 6 * time.Second
	old := newTestTracker(ssr, nil, headTrackerDeps{now: clock, blockTime: func() time.Duration { return bt },
		poll: func(context.Context, bool) (*headObservation, error) {
			n := int64(now.Sub(start) / bt)
			return &headObservation{Number: 100 + n, Timestamp: start.Unix() + n*6}, nil
		}})
	old.leaseDeadlineNs.Store(start.Add(time.Hour).UnixNano())
	_, err := old.tick(ctx)
	require.NoError(t, err)
	require.Equal(t, bt.Milliseconds(), old.blockTimeMs.GetValue(), "leader publishes its measured block time")

	fresh := newTestTracker(ssr, nil, headTrackerDeps{now: clock, blockTime: func() time.Duration { return 0 }})
	require.Equal(t, bt, fresh.blockTime(), "fresh replica starts from the published block time")
	require.False(t, fresh.blockTimeCold())

	// The published value never echoes back from a replica without its own
	// measurement, and an on-demand gap (> 120s) is not used as a cadence.
	require.Zero(t, fresh.ownBlockTime())
	other := newHeadTracker("p", "evm:2", "n2", &common.EvmHeadTrackerConfig{Enabled: true}, ssr, headTrackerDeps{now: clock}, &log.Logger)
	other.blockTimeMs.TryUpdate(ctx, (5 * time.Minute).Milliseconds())
	require.Equal(t, common.DefaultHeadTrackerColdInterval, other.blockTime())
	require.True(t, other.blockTimeCold())
}

// Before the EMA warms (4 observations), the leader estimates the block time
// from its own consecutive heads; zero or negative timestamp deltas are no
// samples.
func TestHeadTracker_QuickBlockTimeEstimate(t *testing.T) {
	ht := newTestTracker(newTestSSR(t, t.Context()), nil, headTrackerDeps{})
	p := &headObservation{Number: 100, Timestamp: 1000}
	ht.noteQuickBlockTime(p, &headObservation{Number: 101, Timestamp: 1000})
	require.Zero(t, ht.quickBtNs.Load(), "same-second blocks carry no rate")
	ht.noteQuickBlockTime(p, &headObservation{Number: 99, Timestamp: 999})
	require.Zero(t, ht.quickBtNs.Load(), "out-of-order heads carry no rate")
	ht.noteQuickBlockTime(p, &headObservation{Number: 102, Timestamp: 1012})
	require.Equal(t, 6*time.Second, time.Duration(ht.quickBtNs.Load()))
	require.Equal(t, 6*time.Second, ht.blockTime())
}

// Nibiru-like: blocks every 1s/2s alternately (1.5s mean, the EMA is right),
// but the node exposes its head in steps every 2s of wall clock, 2.5s+
// behind. Normalize cadence per block even when heads arrive in batches.
// Same-head backoff must keep polling near one call per block and lag bounded.
func TestHeadTracker_BurstyVisibilityPollsAboutOncePerBlock(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	c := &simChain{}
	cur := start.Unix()
	for i := 0; i < 4000; i++ {
		c.ts = append(c.ts, cur)
		cur += 1 + int64(i%2)
	}
	c.visible = func(now time.Time) int {
		snap := now.Truncate(2 * time.Second).Add(-2500 * time.Millisecond).Unix()
		n := 0
		for n+1 < len(c.ts) && c.ts[n+1] <= snap {
			n++
		}
		return n
	}
	polls, heads, lag, maxLag := runSimLeader(t, c, func() time.Duration { return 1500 * time.Millisecond }, start, 10*time.Minute, nil)
	perHead := float64(polls) / float64(heads)
	perSec := float64(polls) / (10 * 60)
	t.Logf("bursty chain: %d polls, %d blocks: %.2f polls/block, %.2f polls/s, mean lag %.2f, max lag %d blocks", polls, heads, perHead, perSec, lag, maxLag)
	require.LessOrEqual(t, perHead, 1.1)
	require.LessOrEqual(t, maxLag, int64(2))
}

// One poll slower than a block time used to fork the schedule: the surplus
// result stayed queued, and every later poll was paced by its predecessor's
// proposal. Real clock (the bug lives in pollLoop's goroutines).
func TestHeadTracker_SlowPollDoesNotForkSchedule(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ssr := newTestSSR(t, ctx)
	bt := 1500 * time.Millisecond
	chain := &fakeChain{start: time.Now(), blockTime: bt, base: 5000}
	var n atomic.Int64
	ht := newTestTracker(ssr, &common.EvmHeadTrackerConfig{Enabled: true, LeaseTtl: common.Duration(2 * time.Second)}, headTrackerDeps{
		blockTime: func() time.Duration { return bt },
		poll: func(ctx context.Context, full bool) (*headObservation, error) {
			obs, err := chain.poll(ctx, full)
			if n.Add(1)%10 == 2 {
				time.Sleep(2 * bt) // occasional slow poll
			} else {
				time.Sleep(150 * time.Millisecond)
			}
			return obs, err
		},
	})
	ht.Start(ctx)
	defer ht.Stop()
	require.Eventually(t, ht.IsLeader, 3*time.Second, 10*time.Millisecond)
	time.Sleep(3 * time.Second)
	c0, h0 := chain.calls.Load(), ht.Head()
	time.Sleep(24 * time.Second)
	blocks := ht.Head() - h0
	perBlock := float64(chain.calls.Load()-c0) / float64(blocks)
	t.Logf("1.5s chain with slow polls: %.2f polls per block (%d blocks)", perBlock, blocks)
	require.LessOrEqual(t, perBlock, 1.25)
	require.GreaterOrEqual(t, blocks, int64(13))
}

func TestAwaitPollResultDiscardsOvertakenPolls(t *testing.T) {
	ch := make(chan headTrackerPollResult, 3)
	ch <- headTrackerPollResult{seq: 1, wait: time.Hour}
	ch <- headTrackerPollResult{seq: 2, wait: 7 * time.Millisecond}
	require.Equal(t, 7*time.Millisecond, awaitPollResult(t.Context(), ch, 2, time.Second))
	require.Zero(t, awaitPollResult(t.Context(), ch, 3, 10*time.Millisecond), "still running: fire the next poll")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.Negative(t, awaitPollResult(ctx, ch, 4, time.Second))
}

// The block-time EMA must take exactly one sample per distinct head from the
// head tracker: per-upstream poller heads (other upstreams' number/timestamp
// pairs at unrelated cadences) must not interleave with it while it feeds.
func TestHealthTracker_HeadTrackerIsExclusiveBlockTimeSource(t *testing.T) {
	tr := health.NewTracker(&log.Logger, "p", time.Minute)
	ts := int64(1_700_000_000)
	// Head tracker: 6s blocks.
	for i := int64(0); i < 10; i++ {
		tr.ObserveNetworkHead("evm:1", "n", 100+i, ts+6*i)
	}
	require.Equal(t, 6*time.Second, tr.GetNetworkBlockTime("evm:1"))
	// Duplicates and zero/negative deltas are not samples.
	tr.ObserveNetworkHead("evm:1", "n", 109, ts+54)
	tr.ObserveNetworkHead("evm:1", "n", 110, ts+54)
	tr.ObserveNetworkHead("evm:1", "n", 111, ts+53)
	require.Equal(t, 6*time.Second, tr.GetNetworkBlockTime("evm:1"))
}
