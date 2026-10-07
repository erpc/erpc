package erpc

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"
)

func init() { util.ConfigureTestLogger() }

// fakeChain is a deterministic chain producing one block per blockTime, with
// timestamps in whole seconds like a real chain.
type fakeChain struct {
	start     time.Time
	blockTime time.Duration
	base      int64
	calls     atomic.Int64
	fail      atomic.Bool
}

func (c *fakeChain) headAt(now time.Time) (int64, int64) {
	n := c.base + int64(now.Sub(c.start)/c.blockTime)
	ts := c.start.Add(time.Duration(n-c.base) * c.blockTime).Unix()
	return n, ts
}

func (c *fakeChain) poll(_ context.Context, _ bool) (*headObservation, error) {
	c.calls.Add(1)
	if c.fail.Load() {
		return nil, fmt.Errorf("upstream down")
	}
	n, ts := c.headAt(time.Now())
	return &headObservation{Number: n, Timestamp: ts, Hash: fmt.Sprintf("0x%064x", n)}, nil
}

func newTestSSR(t *testing.T, ctx context.Context) data.SharedStateRegistry {
	t.Helper()
	r, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
		ClusterKey: fmt.Sprintf("ht-%d", time.Now().UnixNano()),
		Connector: &common.ConnectorConfig{Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{MaxItems: 1000, MaxTotalSize: "10MB"}},
	})
	require.NoError(t, err)
	return r
}

func newTestTracker(ssr data.SharedStateRegistry, cfg *common.EvmHeadTrackerConfig, deps headTrackerDeps) *headTracker {
	if cfg == nil {
		cfg = &common.EvmHeadTrackerConfig{Enabled: true}
	}
	cfg.SetDefaults()
	return newHeadTracker("p", "evm:1", "n", cfg, ssr, deps, &log.Logger)
}

func TestHeadTracker_AdaptiveWait(t *testing.T) {
	ssr := newTestSSR(t, t.Context())
	bt := 2 * time.Second
	ht := newTestTracker(ssr, nil, headTrackerDeps{blockTime: func() time.Duration { return bt }})
	ts := int64(1_700_000_000)
	blockAt := time.Unix(ts, 0)

	// Observed 300ms after the block: next poll at the next block's timestamp.
	w := ht.newHeadWait(&headObservation{Number: 10, Timestamp: ts}, blockAt.Add(300*time.Millisecond), bt)
	require.Equal(t, 1700*time.Millisecond, w)

	// Observed late (1.8s): never sooner than half a block.
	w = ht.newHeadWait(&headObservation{Number: 10, Timestamp: ts}, blockAt.Add(1800*time.Millisecond), bt)
	require.Equal(t, time.Second, w)

	// Mean propagation delay is added to the target (and the cap).
	ht.delays = []time.Duration{time.Second, time.Second}
	w = ht.newHeadWait(&headObservation{Number: 10, Timestamp: ts}, blockAt.Add(300*time.Millisecond), bt)
	require.Equal(t, 2700*time.Millisecond, w)
	ht.delays = nil

	// Stale result before the next block is due: wait for it.
	ht.prev = &headObservation{Number: 10, Timestamp: ts}
	w = ht.staleWait(&headObservation{Number: 10, Timestamp: ts}, blockAt.Add(time.Second), bt)
	require.Equal(t, time.Second, w)
	// Stale after the next block was due: retry at blockTime/2 (floor 500ms).
	w = ht.staleWait(&headObservation{Number: 10, Timestamp: ts}, blockAt.Add(3*time.Second), bt)
	require.Equal(t, time.Second, w)
	w = newTestTracker(ssr, nil, headTrackerDeps{blockTime: func() time.Duration { return 12 * time.Second }}).
		staleWait(&headObservation{Number: 10}, blockAt, 12*time.Second)
	require.Equal(t, 6*time.Second, w, "retry is blockTime/2 before visibility cadence warms")

	// Sub-second chains are floored at 500ms.
	w = ht.newHeadWait(&headObservation{Number: 10, Timestamp: ts}, blockAt, 400*time.Millisecond)
	require.Equal(t, common.MinHeadTrackerPollWait, w)
}

func TestHeadTracker_BlockTimeSource(t *testing.T) {
	ssr := newTestSSR(t, t.Context())
	var ema time.Duration
	ht := newTestTracker(ssr, nil, headTrackerDeps{blockTime: func() time.Duration { return ema }})
	require.Equal(t, common.DefaultHeadTrackerColdInterval, ht.blockTime(), "cold start")
	ema = 400 * time.Millisecond
	require.Equal(t, ema, ht.blockTime(), "EMA by default")
	require.Equal(t, headTrackerMinStale, ht.localStaleAfter())
	require.Equal(t, headTrackerColdStale, ht.staleAfter(), "nothing published yet: conservative cold window")
	ema = 12 * time.Second
	require.Equal(t, 36*time.Second, ht.localStaleAfter(), "stale = 3 block times")

	ht = newTestTracker(ssr, &common.EvmHeadTrackerConfig{Enabled: true, Interval: &common.BlockTimeAdaptiveDuration{BlockTimeMultiplier: 0.5, Fallback: common.Duration(3 * time.Second)}},
		headTrackerDeps{blockTime: func() time.Duration { return ema }})
	require.Equal(t, 6*time.Second, ht.blockTime())
	ema = 0
	require.Equal(t, 3*time.Second, ht.blockTime(), "override fallback on cold start")
}

func TestHeadTracker_RegressionAndFutureGuards(t *testing.T) {
	ssr := newTestSSR(t, t.Context())
	bt := time.Second
	now := time.Unix(1_700_000_100, 0)
	var verified atomic.Int64
	chainOk := true
	ht := newTestTracker(ssr, nil, headTrackerDeps{
		blockTime: func() time.Duration { return bt },
		verifyChainId: func(context.Context, common.Upstream) (bool, error) {
			verified.Add(1)
			return chainOk, nil
		},
		majorMove: func() int64 { return 60 },
	})
	up := common.NewFakeUpstream("u1")
	ctx := t.Context()

	require.Empty(t, ht.reject(ctx, &headObservation{Number: 1000, Timestamp: now.Unix()}, 0, now, bt))
	require.Equal(t, "regression", ht.reject(ctx, &headObservation{Number: 1000}, 1000+common.DefaultToleratedBlockHeadRollback+1, now, bt))
	require.Empty(t, ht.reject(ctx, &headObservation{Number: 990}, 1000, now, bt), "small regressions are not rejected (the counter ignores them)")
	require.Equal(t, "far_future", ht.reject(ctx, &headObservation{Number: 1001, Timestamp: now.Add(2 * time.Minute).Unix()}, 1000, now, bt))

	ht.prev = &headObservation{Number: 1000, Timestamp: now.Unix() - 10}
	require.Empty(t, ht.reject(ctx, &headObservation{Number: 1010, Timestamp: now.Unix()}, 1000, now, bt))
	require.Equal(t, "far_future", ht.reject(ctx, &headObservation{Number: 1500, Timestamp: now.Unix()}, 1000, now, bt),
		"500 blocks in 10s of a 1s chain is impossible")

	ht.prev = nil
	require.Empty(t, ht.reject(ctx, &headObservation{Number: 1100, Upstream: up}, 1000, now, bt))
	require.Equal(t, int64(1), verified.Load(), "major jump verifies chain id")
	chainOk = false
	require.Equal(t, "chain_id", ht.reject(ctx, &headObservation{Number: 1100, Upstream: up}, 1000, now, bt))
	ht.prev = &headObservation{Number: 1000}
	require.Empty(t, ht.reject(ctx, &headObservation{Number: 1010, Upstream: up}, 1000, now, bt), "small move needs no verification")
}

func TestHeadTracker_TickPublishesAndCallsBack(t *testing.T) {
	ssr := newTestSSR(t, t.Context())
	chain := &fakeChain{start: time.Now().Add(-10 * time.Second), blockTime: time.Second, base: 100}
	var accepted []int64
	var publishedAtWrite int64 = -1
	ht := newTestTracker(ssr, nil, headTrackerDeps{
		poll:      chain.poll,
		blockTime: func() time.Duration { return time.Second },
	})
	ht.deps.onAccepted = func(_ context.Context, o *headObservation) {
		accepted = append(accepted, o.Number)
		publishedAtWrite = ht.Head()
	}
	ht.leaseDeadlineNs.Store(time.Now().Add(time.Hour).UnixNano())
	_, err := ht.tick(t.Context())
	require.NoError(t, err)
	require.GreaterOrEqual(t, ht.Head(), int64(110))
	require.Equal(t, []int64{ht.Head()}, accepted)
	require.Zero(t, publishedAtWrite, "S1: the block is written BEFORE the head is published")
	require.Equal(t, ht.Head(), ht.FreshHead())

	// Same head again: no second callback.
	ht.head.TryUpdate(t.Context(), ht.Head()+1)
	before := len(accepted)
	_, err = ht.tick(t.Context())
	require.NoError(t, err)
	require.Len(t, accepted, before)

	chain.fail.Store(true)
	w, err := ht.tick(t.Context())
	require.Error(t, err)
	require.Equal(t, common.MinHeadTrackerPollWait, w, "errors retry at max(500ms, blockTime/4), no backoff to the slow poller")
}

func TestHeadTracker_FallbackWhenStale(t *testing.T) {
	ssr := newTestSSR(t, t.Context())
	ht := newTestTracker(ssr, nil, headTrackerDeps{blockTime: func() time.Duration { return 100 * time.Millisecond }})
	require.Zero(t, ht.FreshHead(), "no head yet")
	require.True(t, ht.inFallback.Load())
	ht.head.TryUpdate(t.Context(), 42)
	require.Zero(t, ht.FreshHead(), "the first value a replica learns is not proof of freshness (could be an hours-old counter)")
	ht.head.TryUpdate(t.Context(), 43)
	require.Equal(t, int64(43), ht.FreshHead())
	require.False(t, ht.inFallback.Load())
}

// S5: freshness is the LOCAL receipt time of an advance, never the remote
// writer's timestamp, so a skewed clock on the leader cannot make followers
// treat a stale head as fresh (or a fresh one as stale).
func TestHeadTracker_FreshnessUsesLocalClock(t *testing.T) {
	ssr := newTestSSR(t, t.Context())
	now := time.Now()
	ht := newTestTracker(ssr, nil, headTrackerDeps{
		blockTime: func() time.Duration { return time.Second },
		now:       func() time.Time { return now },
	})
	ht.head.TryUpdate(t.Context(), 100)
	ht.head.TryUpdate(t.Context(), 101)
	require.Equal(t, int64(101), ht.FreshHead())
	// Local time moves past the staleness window with no advance: stale,
	// whatever timestamps the shared counter carries.
	now = now.Add(ht.staleAfter() + time.Second)
	require.Zero(t, ht.FreshHead())
	// S7: the fallback floor is the last fresh head, for a bounded time
	// measured from when it was last served fresh.
	now = now.Add(-ht.staleAfter())
	require.Equal(t, int64(101), ht.FallbackFloor())
	now = now.Add(headTrackerFallbackFloorTTL + time.Second)
	require.Zero(t, ht.FallbackFloor(), "the floor fails open after its TTL")
}

// S2: same height, different hash: the cached head block is rewritten.
func TestHeadTracker_SameHeightReorgRewritesBlock(t *testing.T) {
	ssr := newTestSSR(t, t.Context())
	hash := "0xaa"
	var written []string
	ht := newTestTracker(ssr, nil, headTrackerDeps{
		blockTime: func() time.Duration { return time.Second },
		poll: func(context.Context, bool) (*headObservation, error) {
			return &headObservation{Number: 500, Hash: hash, Timestamp: time.Now().Unix()}, nil
		},
		onAccepted: func(_ context.Context, o *headObservation) { written = append(written, o.Hash) },
	})
	ht.leaseDeadlineNs.Store(time.Now().Add(time.Hour).UnixNano())
	_, err := ht.tick(t.Context())
	require.NoError(t, err)
	_, err = ht.tick(t.Context())
	require.NoError(t, err)
	require.Equal(t, []string{"0xaa"}, written, "same block twice: written once")
	hash = "0xbb"
	_, err = ht.tick(t.Context())
	require.NoError(t, err)
	require.Equal(t, []string{"0xaa", "0xbb"}, written, "reorged head block is rewritten")
	require.Equal(t, int64(500), ht.Head())
}

// S3: the first head is verified (chain id), and a poisoned published head
// is recovered from after headTrackerRecoverAfter consistent polls.
func TestHeadTracker_BogusFirstHeadAndRecovery(t *testing.T) {
	ssr := newTestSSR(t, t.Context())
	up := common.NewFakeUpstream("u1")
	wrongChain := true
	n := int64(1000)
	ht := newTestTracker(ssr, nil, headTrackerDeps{
		blockTime: func() time.Duration { return time.Second },
		poll: func(context.Context, bool) (*headObservation, error) {
			return &headObservation{Number: n, Upstream: up, Timestamp: time.Now().Unix()}, nil
		},
		verifyChainId: func(context.Context, common.Upstream) (bool, error) { return !wrongChain, nil },
	})
	ht.leaseDeadlineNs.Store(time.Now().Add(time.Hour).UnixNano())
	_, _ = ht.tick(t.Context())
	require.Zero(t, ht.Head(), "a first head from a wrong-chain upstream is not published")

	// A poisoned counter (another chain's height published earlier).
	wrongChain = false
	ht.head.TryUpdate(t.Context(), 50_000_000)
	for i := 0; i < headTrackerRecoverAfter-1; i++ {
		_, _ = ht.tick(t.Context())
		require.Equal(t, int64(50_000_000), ht.Head(), "a single regressing poll is not enough")
		n++
	}
	_, _ = ht.tick(t.Context())
	require.Equal(t, n, ht.Head(), "consistent polls recover from the poisoned head")
}

// NEW-2: a stuck node on the right chain, far (>1024 blocks) behind the real
// head, polls consistently and passes the chain-id check. Its old block
// timestamp must prevent it from rolling the published head back.
func TestHeadTracker_StuckNodeDoesNotTriggerRollback(t *testing.T) {
	ssr := newTestSSR(t, t.Context())
	up := common.NewFakeUpstream("u1")
	stuckTs := time.Now().Add(-2 * time.Hour).Unix()
	ht := newTestTracker(ssr, nil, headTrackerDeps{
		blockTime: func() time.Duration { return time.Second },
		poll: func(context.Context, bool) (*headObservation, error) {
			return &headObservation{Number: 1000, Upstream: up, Timestamp: stuckTs}, nil
		},
		verifyChainId: func(context.Context, common.Upstream) (bool, error) { return true, nil },
	})
	ht.leaseDeadlineNs.Store(time.Now().Add(time.Hour).UnixNano())
	ht.head.TryUpdate(t.Context(), 1000+common.DefaultToleratedBlockHeadRollback+500)
	for i := 0; i < 3*headTrackerRecoverAfter; i++ {
		_, _ = ht.tick(t.Context())
	}
	require.Equal(t, int64(1000+common.DefaultToleratedBlockHeadRollback+500), ht.Head(), "no rollback to a stale head")
}

// S4: a leader past its hard lease deadline never publishes.
func TestHeadTracker_NoPublishPastLeaseDeadline(t *testing.T) {
	ssr := newTestSSR(t, t.Context())
	ht := newTestTracker(ssr, nil, headTrackerDeps{
		blockTime: func() time.Duration { return time.Second },
		poll: func(context.Context, bool) (*headObservation, error) {
			return &headObservation{Number: 77, Timestamp: time.Now().Unix()}, nil
		},
	})
	ht.leaseDeadlineNs.Store(time.Now().Add(-time.Millisecond).UnixNano())
	_, _ = ht.tick(t.Context())
	require.Zero(t, ht.Head())
}

// Several replicas share one shared state: exactly one polls, ~1 call per
// block, and when the leader stops another takes over within ~TTL + 1 block
// and the head keeps advancing.
func TestHeadTracker_ElectionAndFailover(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ssr := newTestSSR(t, ctx)
	bt := time.Second
	chain := &fakeChain{start: time.Now(), blockTime: bt, base: 1000}

	const replicas = 4
	trackers := make([]*headTracker, replicas)
	var mu sync.Mutex
	pollsBy := map[int]int{}
	for i := 0; i < replicas; i++ {
		i := i
		cfg := &common.EvmHeadTrackerConfig{Enabled: true, LeaseTtl: common.Duration(time.Second)}
		trackers[i] = newTestTracker(ssr, cfg, headTrackerDeps{
			blockTime: func() time.Duration { return bt },
			poll: func(ctx context.Context, full bool) (*headObservation, error) {
				mu.Lock()
				pollsBy[i]++
				mu.Unlock()
				return chain.poll(ctx, full)
			},
		})
		trackers[i].Start(ctx)
	}
	defer func() {
		for _, tr := range trackers {
			tr.Stop()
		}
	}()

	leader := func() int {
		idx := -1
		for i, tr := range trackers {
			if tr.IsLeader() {
				require.Equal(t, -1, idx, "two leaders at once")
				idx = i
			}
		}
		return idx
	}
	require.Eventually(t, func() bool { return leader() >= 0 }, 3*time.Second, 10*time.Millisecond)

	// Sample leadership continuously over the window: never two at once.
	stopSampling := make(chan struct{})
	var maxLeaders atomic.Int32
	go func() {
		tk := time.NewTicker(5 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stopSampling:
				return
			case <-tk.C:
				var c int32
				for _, tr := range trackers {
					if tr.IsLeader() {
						c++
					}
				}
				if c > maxLeaders.Load() {
					maxLeaders.Store(c)
				}
			}
		}
	}()
	defer close(stopSampling)

	window := 5 * time.Second
	startCalls := chain.calls.Load()
	time.Sleep(window)
	blocks := float64(window / bt)
	calls := float64(chain.calls.Load() - startCalls)
	t.Logf("calls per block across %d replicas: %.2f", replicas, calls/blocks)
	require.LessOrEqual(t, calls/blocks, 2.0, "about one poll per block in total")
	require.GreaterOrEqual(t, calls/blocks, 0.8, "the leader keeps up with every block")
	mu.Lock()
	pollers := 0
	for _, n := range pollsBy {
		if n > 0 {
			pollers++
		}
	}
	mu.Unlock()
	require.Equal(t, 1, pollers, "only the leader polls")
	require.LessOrEqual(t, maxLeaders.Load(), int32(1), "never two concurrent leaders")
	expected, _ := chain.headAt(time.Now())
	for _, tr := range trackers {
		require.GreaterOrEqual(t, tr.Head(), expected-2, "every replica sees the head within ~1 block")
	}

	// Kill the leader without releasing (simulated crash: lease must expire).
	old := leader()
	crashed := trackers[old]
	crashed.abandonLease.Store(true)
	headBefore := crashed.Head()
	killedAt := time.Now()
	// Stop its loop but keep the lease held so takeover depends on TTL.
	crashed.Stop()
	require.Eventually(t, func() bool {
		l := leader()
		return l >= 0 && l != old
	}, 3*time.Second, 10*time.Millisecond)
	took := time.Since(killedAt)
	t.Logf("takeover after %s", took)
	require.LessOrEqual(t, took, time.Second+500*time.Millisecond, "within TTL + a renew check")
	require.Eventually(t, func() bool {
		for i, tr := range trackers {
			if i != old && tr.Head() <= headBefore+1 {
				return false
			}
		}
		return true
	}, 3*time.Second, 10*time.Millisecond, "head keeps advancing after failover (within ~TTL + 1 block)")
}

// rc.2 dev flapping: on slow / irregular chains the head legitimately stops
// moving while every leader poll succeeds. Followers must stay fresh.
// Simulated on a virtual clock with two replicas sharing state: the leader
// polls on its own cadence (real block time), the follower's block-time EMA
// is cold (1s, as on lat-dev where only the leader sees block timestamps).
func runSlowChainFreshness(t *testing.T, leaderBt time.Duration, headAt func(elapsed time.Duration) int64, total time.Duration) (staleSeconds int) {
	ctx := t.Context()
	ssr := newTestSSR(t, ctx)
	start := time.Unix(1_700_000_000, 0)
	now := start
	clock := func() time.Time { return now }
	leader := newTestTracker(ssr, nil, headTrackerDeps{
		now:       clock,
		blockTime: func() time.Duration { return leaderBt },
		poll: func(context.Context, bool) (*headObservation, error) {
			n := headAt(now.Sub(start))
			return &headObservation{Number: n, Hash: fmt.Sprintf("0x%x", n), Timestamp: now.Unix()}, nil
		},
	})
	follower := newTestTracker(ssr, nil, headTrackerDeps{now: clock, blockTime: func() time.Duration { return 0 }})
	// Both instances share one in-process registry, so the follower reads
	// the same counters; give it its own callbacks by observing values.
	leader.leaseDeadlineNs.Store(start.Add(24 * time.Hour).UnixNano())
	next := start
	for now.Sub(start) < total {
		if !now.Before(next) {
			wait, err := leader.tick(ctx)
			require.NoError(t, err)
			next = now.Add(wait)
		}
		if now.Sub(start) > 2*leaderBt && follower.FreshHead() == 0 {
			staleSeconds++
		}
		now = now.Add(time.Second)
	}
	return staleSeconds
}

func TestHeadTracker_SlowChainsStayFresh(t *testing.T) {
	t.Run("ethereum missed slots", func(t *testing.T) {
		// 12s slots, every 4th slot missed (24s gaps), 4 minutes.
		stale := runSlowChainFreshness(t, 12*time.Second, func(e time.Duration) int64 {
			slots := int64(e / (12 * time.Second))
			return 20_000_000 + slots - slots/4
		}, 4*time.Minute)
		require.Zero(t, stale, "no fallback on missed slots")
	})
	t.Run("on-demand blocks", func(t *testing.T) {
		// Nominal 5s blocks, then no block at all for 90s while every poll
		// succeeds (redbelly/rootstock/saga style), then blocks again.
		stale := runSlowChainFreshness(t, 5*time.Second, func(e time.Duration) int64 {
			switch {
			case e < 30*time.Second:
				return 1000 + int64(e/(5*time.Second))
			case e < 120*time.Second:
				return 1006
			default:
				return 1006 + int64((e-120*time.Second)/(5*time.Second))
			}
		}, 3*time.Minute)
		require.Zero(t, stale, "a chain that stops producing blocks is not a tracker failure")
	})
	t.Run("filecoin 30s with a cold follower EMA", func(t *testing.T) {
		stale := runSlowChainFreshness(t, 30*time.Second, func(e time.Duration) int64 {
			return 6_000_000 + int64(e/(30*time.Second))
		}, 5*time.Minute)
		require.Zero(t, stale)
	})
}

// A dead leader (no successful poll) still sends followers into fallback
// within ~3 × the observed poll cadence.
func TestHeadTracker_DeadLeaderStillFallsBack(t *testing.T) {
	ctx := t.Context()
	ssr := newTestSSR(t, ctx)
	now := time.Unix(1_700_000_000, 0)
	clock := func() time.Time { return now }
	n := int64(100)
	leader := newTestTracker(ssr, nil, headTrackerDeps{now: clock, blockTime: func() time.Duration { return 12 * time.Second },
		poll: func(context.Context, bool) (*headObservation, error) {
			return &headObservation{Number: n, Timestamp: now.Unix()}, nil
		}})
	follower := newTestTracker(ssr, nil, headTrackerDeps{now: clock, blockTime: func() time.Duration { return 0 }})
	leader.leaseDeadlineNs.Store(now.Add(24 * time.Hour).UnixNano())
	for i := 0; i < 10; i++ {
		_, _ = leader.tick(ctx)
		now = now.Add(12 * time.Second)
		n++
	}
	require.NotZero(t, follower.FreshHead())
	// Leader dies: no more polls.
	now = now.Add(37 * time.Second)
	require.Zero(t, follower.FreshHead(), "stale after 3 × the 12s poll cadence")
}

// rc.3 lag: with poll latency close to the block time (CPU-throttled pods,
// fullBlocks payloads) the leader used to poll once per (latency + wait),
// i.e. far less than once per block. Cadence must stay ~1 poll per block.
func TestHeadTracker_CadenceIndependentOfPollLatency(t *testing.T) {
	for _, tc := range []struct {
		name    string
		bt      time.Duration
		latency time.Duration
	}{
		{"1s blocks, 0.9s polls", time.Second, 900 * time.Millisecond},
		{"1s blocks, 1.5s polls", time.Second, 1500 * time.Millisecond},
		{"monad-like 0.4s blocks, 0.9s polls", 400 * time.Millisecond, 900 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ssr := newTestSSR(t, ctx)
			chain := &fakeChain{start: time.Now(), blockTime: tc.bt, base: 5000}
			var inFlight, maxInFlight atomic.Int32
			ht := newTestTracker(ssr, &common.EvmHeadTrackerConfig{Enabled: true, LeaseTtl: common.Duration(2 * time.Second)}, headTrackerDeps{
				blockTime: func() time.Duration { return tc.bt },
				poll: func(ctx context.Context, full bool) (*headObservation, error) {
					c := inFlight.Add(1)
					defer inFlight.Add(-1)
					for {
						m := maxInFlight.Load()
						if c <= m || maxInFlight.CompareAndSwap(m, c) {
							break
						}
					}
					// The upstream answers with the head as of the request.
					obs, err := chain.poll(ctx, full)
					time.Sleep(tc.latency)
					return obs, err
				},
			})
			ht.Start(ctx)
			defer ht.Stop()
			require.Eventually(t, ht.IsLeader, 3*time.Second, 10*time.Millisecond)
			window := 8 * time.Second
			startCalls := chain.calls.Load()
			time.Sleep(window)
			perBlock := float64(chain.calls.Load()-startCalls) / (float64(window) / float64(tc.bt))
			head, _ := chain.headAt(time.Now())
			lag := head - ht.Head()
			t.Logf("%s: %.2f polls per block, max in flight %d, head lag %d", tc.name, perBlock, maxInFlight.Load(), lag)
			// Sub-second chains are floored at one poll per 500ms (and 2 in
			// flight): the head still advances every poll by several blocks.
			minPerBlock := min(0.85, float64(tc.bt)/float64(common.MinHeadTrackerPollWait)*0.85)
			require.GreaterOrEqual(t, perBlock, minPerBlock, "about one poll per block (or per 500ms floor)")
			require.LessOrEqual(t, perBlock, 2.0)
			require.LessOrEqual(t, maxInFlight.Load(), int32(headTrackerMaxInFlight))
			require.LessOrEqual(t, lag, int64(tc.latency/tc.bt)+3, "head stays within poll latency + ~1 poll")
		})
	}
}

// rc.3 follower windows: a follower that just started (or just went through
// failover) on a 52s-block chain, whose leader also publishes alive on 13s
// stale-result retries, must not fall back. Its own EMA is cold; it relies on
// the window the leader publishes (and a conservative 60s floor before that).
func TestHeadTracker_FollowerUsesPublishedWindowOnSlowChain(t *testing.T) {
	ctx := t.Context()
	ssr := newTestSSR(t, ctx)
	start := time.Unix(1_700_000_000, 0)
	now := start
	clock := func() time.Time { return now }
	bt := 52 * time.Second
	leader := newTestTracker(ssr, nil, headTrackerDeps{
		now: clock, blockTime: func() time.Duration { return bt },
		poll: func(context.Context, bool) (*headObservation, error) {
			n := 9000 + int64(now.Sub(start)/bt)
			return &headObservation{Number: n, Timestamp: start.Add(time.Duration(n-9000) * bt).Unix()}, nil
		},
	})
	leader.leaseDeadlineNs.Store(start.Add(24 * time.Hour).UnixNano())
	var follower *headTracker
	stale := 0
	next := start
	for now.Sub(start) < 10*time.Minute {
		if !now.Before(next) {
			// Leader polls every bt/4 (13s): stale-result retries between
			// blocks, each publishing alive.
			_, err := leader.tick(ctx)
			require.NoError(t, err)
			next = now.Add(bt / 4)
		}
		if follower == nil && now.Sub(start) >= 3*time.Minute {
			// The follower starts mid-run (or after failover): fresh state.
			follower = newTestTracker(ssr, nil, headTrackerDeps{now: clock, blockTime: func() time.Duration { return 0 }})
			follower.seenAlive.Store(0)
			follower.alive.OnValue(func(int64) {})
		}
		if follower != nil {
			if follower.staleAfter() < 150*time.Second {
				t.Fatalf("follower window %s at %s is too short for a 52s chain", follower.staleAfter(), now.Sub(start))
			}
		}
		now = now.Add(time.Second)
	}
	_ = stale
	require.GreaterOrEqual(t, leader.publishedStaleAfter(), 156*time.Second)
}

// Leases spread across replicas: 3 replicas × 6 networks end up ~2 each
// instead of one replica leading all 6 (rc.3: all 31 on one pod).
func TestHeadTracker_LeasesSpreadAcrossReplicas(t *testing.T) {
	mr := miniredis.RunT(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		tk := time.NewTicker(50 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
				mr.FastForward(50 * time.Millisecond)
			}
		}
	}()
	mkSSR := func(id string) data.SharedStateRegistry {
		t.Setenv("INSTANCE_ID", id)
		rc := &common.RedisConnectorConfig{Addr: mr.Addr(), ConnPoolSize: 4}
		require.NoError(t, rc.SetDefaults())
		r, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
			ClusterKey: "spread", Connector: &common.ConnectorConfig{Id: id, Driver: common.DriverRedis, Redis: rc},
		})
		require.NoError(t, err)
		return r
	}
	const replicas, networks = 3, 6
	var all [replicas][]*headTracker
	for r := 0; r < replicas; r++ {
		ssr := mkSSR(fmt.Sprintf("pod-%d", r))
		for n := 0; n < networks; n++ {
			chain := &fakeChain{start: time.Now(), blockTime: time.Second, base: 100}
			ht := newHeadTracker("p", fmt.Sprintf("evm:%d", n), fmt.Sprintf("n%d", n),
				&common.EvmHeadTrackerConfig{Enabled: true, LeaseTtl: common.Duration(time.Second)}, ssr,
				headTrackerDeps{blockTime: func() time.Duration { return time.Second }, poll: chain.poll}, &log.Logger)
			all[r] = append(all[r], ht)
		}
		// Replica 0 boots first and would otherwise take every lease.
		for _, ht := range all[r] {
			ht.Start(ctx)
		}
		if r == 0 {
			time.Sleep(2 * time.Second)
		}
	}
	defer func() {
		for r := range all {
			for _, ht := range all[r] {
				ht.Stop()
			}
		}
	}()
	counts := func() (c [replicas]int, total int) {
		for r := range all {
			for _, ht := range all[r] {
				if ht.IsLeader() {
					c[r]++
					total++
				}
			}
		}
		return
	}
	require.Eventually(t, func() bool {
		c, total := counts()
		if total != networks {
			return false
		}
		for _, n := range c {
			if n > (networks+replicas-1)/replicas {
				return false
			}
		}
		return true
	}, 3*time.Minute, 200*time.Millisecond, "leases spread to <= ceil(N/replicas) per replica")
	c, _ := counts()
	t.Logf("leases per replica: %v", c)
}

// rc.5: an on-demand chain (a block every 60-300s, redbelly-like) with a COLD
// block-time EMA (intervals > 120s are rejected by the EMA, so it never warms)
// must not poll hundreds of times per block, and followers must not fall back.
func TestHeadTracker_OnDemandChainBacksOff(t *testing.T) {
	ctx := t.Context()
	ssr := newTestSSR(t, ctx)
	start := time.Unix(1_700_000_000, 0)
	now := start
	clock := func() time.Time { return now }
	// Block schedule: gaps of 60, 300, 120, 200, 90, 240 ... seconds.
	gaps := []time.Duration{60, 300, 120, 200, 90, 240, 75, 180}
	var blockTimes []time.Time
	at := start
	for i := 0; at.Sub(start) < 40*time.Minute; i++ {
		at = at.Add(gaps[i%len(gaps)] * time.Second)
		blockTimes = append(blockTimes, at)
	}
	headAt := func(tm time.Time) (int64, int64) {
		n, ts := int64(500), start.Unix()
		for _, b := range blockTimes {
			if b.After(tm) {
				break
			}
			n++
			ts = b.Unix()
		}
		return n, ts
	}
	polls := 0
	leader := newTestTracker(ssr, nil, headTrackerDeps{
		now: clock, blockTime: func() time.Duration { return 0 },
		poll: func(context.Context, bool) (*headObservation, error) {
			polls++
			n, ts := headAt(now)
			return &headObservation{Number: n, Hash: fmt.Sprintf("0x%x", n), Timestamp: ts}, nil
		},
	})
	leader.leaseDeadlineNs.Store(start.Add(24 * time.Hour).UnixNano())
	follower := newTestTracker(ssr, nil, headTrackerDeps{now: clock, blockTime: func() time.Duration { return 0 }})
	next := start
	stale, maxLag := 0, int64(0)
	for now.Sub(start) < 40*time.Minute {
		if !now.Before(next) {
			w, err := leader.tick(ctx)
			require.NoError(t, err)
			next = now.Add(w)
		}
		if now.Sub(start) > time.Minute {
			if follower.FreshHead() == 0 {
				stale++
			}
			n, _ := headAt(now)
			maxLag = max(maxLag, n-leader.Head())
		}
		now = now.Add(time.Second)
	}
	blocks := len(blockTimes)
	perMin := float64(polls) / 40
	t.Logf("on-demand chain: %d polls over 40 min, %d blocks: %.1f polls/block, %.2f polls/min; follower stale %ds; max head lag %d",
		polls, blocks, float64(polls)/float64(blocks), perMin, stale, maxLag)
	require.Zero(t, stale, "no fallback on an on-demand chain")
	// Steady state between blocks is one poll per 30s (cold cap); each new
	// block restarts the backoff (1,2,4,8,16s) so the next block is found
	// quickly. ~4 polls/min = ~0.07/s, vs ~3/s before (rc.4 redbelly).
	require.LessOrEqual(t, perMin, 4.5, "bounded polling between on-demand blocks")
	require.Greater(t, leader.publishedStaleAfter(), 3*30*time.Second-time.Second, "published window covers the 30s backoff cap")
	require.LessOrEqual(t, maxLag, int64(1))
}

// Regular chains are unaffected by the backoff: the streak resets on every
// new head, so a 6s chain (nibiru-like, warm EMA) polls ~1-2 times per block.
func TestHeadTracker_RegularChainPollsAboutOncePerBlock(t *testing.T) {
	ctx := t.Context()
	ssr := newTestSSR(t, ctx)
	start := time.Unix(1_700_000_000, 0)
	now := start
	polls := 0
	bt := 6 * time.Second
	leader := newTestTracker(ssr, nil, headTrackerDeps{
		now: func() time.Time { return now }, blockTime: func() time.Duration { return bt },
		poll: func(context.Context, bool) (*headObservation, error) {
			polls++
			n := int64(now.Sub(start) / bt)
			// Blocks become visible ~1s after their timestamp (propagation).
			if now.Sub(start.Add(time.Duration(n)*bt)) < time.Second {
				n--
			}
			return &headObservation{Number: 1000 + n, Timestamp: start.Add(time.Duration(n) * bt).Unix()}, nil
		},
	})
	leader.leaseDeadlineNs.Store(start.Add(24 * time.Hour).UnixNano())
	next := start
	for now.Sub(start) < 10*time.Minute {
		if !now.Before(next) {
			w, err := leader.tick(ctx)
			require.NoError(t, err)
			next = now.Add(w)
		}
		now = now.Add(100 * time.Millisecond)
	}
	perBlock := float64(polls) / float64(10*time.Minute/bt)
	t.Logf("6s chain: %.2f polls per block", perBlock)
	require.LessOrEqual(t, perBlock, 2.2)
}
