package evm

import (
	"context"
	"errors"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakePollerUpstream is a minimal common.Upstream implementation whose Forward
// simply counts invocations and fails, which is enough to observe how many
// poll loops are live without standing up a real JSON-RPC endpoint.
type fakePollerUpstream struct {
	cfg             *common.UpstreamConfig
	logger          zerolog.Logger
	forwards        atomic.Int64
	syncingForwards atomic.Int64
}

func (f *fakePollerUpstream) Id() string                     { return f.cfg.Id }
func (f *fakePollerUpstream) VendorName() string             { return "test" }
func (f *fakePollerUpstream) NetworkId() string              { return "evm:123" }
func (f *fakePollerUpstream) NetworkLabel() string           { return "evm:123" }
func (f *fakePollerUpstream) Config() *common.UpstreamConfig { return f.cfg }
func (f *fakePollerUpstream) Logger() *zerolog.Logger        { return &f.logger }
func (f *fakePollerUpstream) Vendor() common.Vendor          { return nil }
func (f *fakePollerUpstream) Tracker() common.HealthTracker  { return nil }
func (f *fakePollerUpstream) Forward(ctx context.Context, nq *common.NormalizedRequest, byPassMethodExclusion, isHedgeAttempt bool) (*common.NormalizedResponse, error) {
	f.forwards.Add(1)
	if m, err := nq.Method(); err == nil && m == "eth_syncing" {
		f.syncingForwards.Add(1)
	}
	return nil, errors.New("fake upstream: forward always fails")
}
func (f *fakePollerUpstream) Cordon(method string, reason string)   {}
func (f *fakePollerUpstream) Uncordon(method string, reason string) {}
func (f *fakePollerUpstream) IgnoreMethod(method string)            {}
func (f *fakePollerUpstream) ShouldHandleMethod(method string) (bool, error) {
	return true, nil
}

var _ common.Upstream = (*fakePollerUpstream)(nil)

func newTestStatePoller(t *testing.T, interval, debounce time.Duration) (*EvmStatePoller, *fakePollerUpstream, context.Context) {
	t.Helper()
	poller, up, _, ctx := newTestStatePollerWithInterval(t, common.FixedDuration(interval), debounce)
	return poller, up, ctx
}

func newTestStatePollerWithInterval(t *testing.T, interval *common.BlockTimeAdaptiveDuration, debounce time.Duration) (*EvmStatePoller, *fakePollerUpstream, *health.Tracker, context.Context) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	logger := zerolog.Nop()
	up := &fakePollerUpstream{
		logger: logger,
		cfg: &common.UpstreamConfig{
			Id:   "test-upstream",
			Type: common.UpstreamTypeEvm,
			Evm: &common.EvmUpstreamConfig{
				ChainId:             123,
				StatePollerInterval: interval,
				StatePollerDebounce: common.Duration(debounce),
			},
		},
	}

	ssr, err := data.NewSharedStateRegistry(ctx, &logger, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: "memory",
			Memory: &common.MemoryConnectorConfig{
				MaxItems:     100_000,
				MaxTotalSize: "1GB",
			},
		},
	})
	require.NoError(t, err)

	tracker := health.NewTracker(&logger, "test", 2*time.Second)
	poller := NewEvmStatePoller("test", ctx, &logger, up, tracker, ssr)
	return poller, up, tracker, ctx
}

// seedBlockTime feeds the tracker enough head observations for its block-time
// estimate to settle at blockTime (whole seconds only: timestamps are seconds).
func seedBlockTime(t *testing.T, tracker *health.Tracker, up common.Upstream, blockTime time.Duration) {
	t.Helper()
	secs := int64(blockTime / time.Second)
	require.Positive(t, secs)
	for i := int64(1); i <= 5; i++ {
		tracker.SetLatestBlockNumber(up, 1000+i, 1_700_000_000+i*secs)
	}
	require.Equal(t, blockTime, tracker.GetNetworkBlockTime(up.NetworkId()))
}

// countGoroutinesStable samples runtime.NumGoroutine after letting transient
// goroutines (Poll's short-lived workers) finish.
func countGoroutinesStable() int {
	time.Sleep(150 * time.Millisecond)
	runtime.GC()
	return runtime.NumGoroutine()
}

func TestEvmStatePoller_RepeatedBootstrapDoesNotLeakTickerGoroutines(t *testing.T) {
	// Long interval: the ticker never fires during the test, so any goroutine
	// growth is the ticker goroutines themselves.
	poller, _, ctx := newTestStatePoller(t, time.Hour, time.Millisecond)

	before := countGoroutinesStable()

	// Simulate what a retried/re-executed bootstrap task does: call Bootstrap
	// over and over on the same poller. Poll errors are irrelevant here.
	for range 25 {
		_ = poller.Bootstrap(ctx)
	}

	after := countGoroutinesStable()

	// Exactly one ticker goroutine must exist (allow small scheduler noise).
	// Without the started-guard this grows by one goroutine per Bootstrap.
	assert.LessOrEqual(t, after-before, 3,
		"expected a single ticker goroutine after 25 Bootstrap calls, got %d new goroutines", after-before)
}

func TestEvmStatePoller_RepeatedBootstrapDoesNotMultiplyPollRate(t *testing.T) {
	// Short interval and tiny debounce so every tick reaches Forward.
	poller, up, ctx := newTestStatePoller(t, 40*time.Millisecond, time.Millisecond)

	_ = poller.Bootstrap(ctx)

	base1 := up.forwards.Load()
	time.Sleep(600 * time.Millisecond)
	singleTickerCalls := up.forwards.Load() - base1
	require.Greater(t, singleTickerCalls, int64(0), "single ticker should be polling")

	// Re-bootstrap several times; with the guard this must not add tickers.
	for range 4 {
		_ = poller.Bootstrap(ctx)
	}

	base2 := up.forwards.Load()
	time.Sleep(600 * time.Millisecond)
	afterRebootstrapCalls := up.forwards.Load() - base2

	// With five tickers the second window would see ~5x the calls of the
	// first. Allow generous scheduling slack, but reject any multiplication.
	assert.Less(t, afterRebootstrapCalls, singleTickerCalls*2+8,
		"poll rate multiplied after repeated Bootstrap calls: first window %d calls, second window %d calls",
		singleTickerCalls, afterRebootstrapCalls)
}

func TestEvmStatePoller_IgnoreSyncingCheck_ImmediatelyNotSyncing(t *testing.T) {
	// ignoreSyncingCheck: true must set syncingState = NotSyncing without ever
	// calling eth_syncing (i.e. without any Forward calls for the syncing check).
	poller, up, ctx := newTestStatePoller(t, 40*time.Millisecond, time.Millisecond)

	skipSyncing := true
	up.cfg.Evm.SkipSyncingCheck = &skipSyncing

	_ = poller.Bootstrap(ctx)

	// Give the poller a couple of ticks to run.
	time.Sleep(120 * time.Millisecond)

	assert.Equal(t, common.EvmSyncingStateNotSyncing, poller.SyncingState(),
		"ignoreSyncingCheck should mark upstream as NotSyncing immediately")
	assert.Equal(t, int64(0), up.syncingForwards.Load(),
		"skipSyncingCheck should not call eth_syncing on the upstream")
}

func TestEvmStatePoller_BootstrapWithZeroIntervalStartsNothing(t *testing.T) {
	poller, up, ctx := newTestStatePoller(t, 0, 0)

	before := countGoroutinesStable()
	require.NoError(t, poller.Bootstrap(ctx))
	require.NoError(t, poller.Bootstrap(ctx))
	after := countGoroutinesStable()

	assert.LessOrEqual(t, after-before, 0, "interval=0 must not start any poll loop")
	assert.Equal(t, int64(0), up.forwards.Load(), "interval=0 must not poll the upstream")
	assert.False(t, poller.Enabled)
}

func TestEvmStatePoller_ResolvePollInterval(t *testing.T) {
	scaled := &common.BlockTimeAdaptiveDuration{
		BlockTimeMultiplier: 2,
		Min:                 common.Duration(5 * time.Second),
		Max:                 common.Duration(20 * time.Second),
		Fallback:            common.Duration(10 * time.Second),
	}

	t.Run("FixedIgnoresBlockTime", func(t *testing.T) {
		poller, up, tracker, _ := newTestStatePollerWithInterval(t, common.FixedDuration(7*time.Second), 0)
		seedBlockTime(t, tracker, up, 12*time.Second)
		assert.Equal(t, 7*time.Second, poller.resolvePollInterval(up.cfg.Evm.StatePollerInterval))
	})

	t.Run("FallbackUntilBlockTimeKnown", func(t *testing.T) {
		poller, _, _, _ := newTestStatePollerWithInterval(t, scaled, 0)
		assert.Equal(t, 10*time.Second, poller.resolvePollInterval(scaled))
	})

	t.Run("DefaultWithoutFallback", func(t *testing.T) {
		noFallback := &common.BlockTimeAdaptiveDuration{BlockTimeMultiplier: 2}
		poller, _, _, _ := newTestStatePollerWithInterval(t, noFallback, 0)
		assert.Equal(t, common.DefaultEvmStatePollerInterval, poller.resolvePollInterval(noFallback))
	})

	for _, tc := range []struct {
		blockTime time.Duration
		want      time.Duration
	}{
		{1 * time.Second, 5 * time.Second},   // 2s raised to min
		{4 * time.Second, 8 * time.Second},   // within bounds
		{12 * time.Second, 20 * time.Second}, // 24s capped at max
	} {
		t.Run("ScalesWithBlockTime/"+tc.blockTime.String(), func(t *testing.T) {
			poller, up, tracker, _ := newTestStatePollerWithInterval(t, scaled, 0)
			seedBlockTime(t, tracker, up, tc.blockTime)
			assert.Equal(t, tc.want, poller.resolvePollInterval(scaled))
		})
	}
}

// TestEvmStatePoller_IntervalFollowsBlockTime drives the real poll loop: with a
// block-time-scaled interval the loop polls at the scaled cadence once block
// time is known, and at the (here very long) fallback while it is not.
func TestEvmStatePoller_IntervalFollowsBlockTime(t *testing.T) {
	// 1s block time * 0.05 = 50ms interval; fallback 1h never fires in-test.
	interval := &common.BlockTimeAdaptiveDuration{
		BlockTimeMultiplier: 0.05,
		Fallback:            common.Duration(time.Hour),
	}

	t.Run("KnownBlockTime", func(t *testing.T) {
		poller, up, tracker, ctx := newTestStatePollerWithInterval(t, interval, time.Millisecond)
		seedBlockTime(t, tracker, up, time.Second)
		_ = poller.Bootstrap(ctx)

		base := up.forwards.Load()
		time.Sleep(600 * time.Millisecond)
		assert.GreaterOrEqual(t, up.forwards.Load()-base, int64(5),
			"expected the loop to poll every ~50ms once block time is known")
	})

	t.Run("UnknownBlockTimeUsesFallback", func(t *testing.T) {
		poller, up, _, ctx := newTestStatePollerWithInterval(t, interval, time.Millisecond)
		_ = poller.Bootstrap(ctx)

		base := up.forwards.Load()
		time.Sleep(300 * time.Millisecond)
		assert.Equal(t, int64(0), up.forwards.Load()-base,
			"without a block-time estimate the 1h fallback must apply")
	})
}
