package data

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/failsafe"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// seedLatency feeds n identical samples (in seconds) into the tracker so
// every quantile of the window resolves to ~sec.
func seedLatency(ex *cacheExecutor, sec float64, n int) {
	for range n {
		ex.latency.Add(sec)
	}
}

func TestCacheExecutor_DynamicHedge_ResolvesFromSeededQuantile(t *testing.T) {
	logger := zerolog.New(io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := &common.FailsafeConfig{
		Hedge: &common.HedgePolicyConfig{
			Delay:    &common.AdaptiveDuration{Quantile: 0.95, Max: common.Duration(300 * time.Millisecond)},
			MaxCount: 1,
		},
	}
	ex, err := NewCacheExecutor(ctx, cfg, &logger)
	require.NoError(t, err)
	require.NotNil(t, ex.latency, "quantile-driven hedge must create the latency tracker")

	// Empty tracker: adaptive falls back to Min (0 here), so the spec
	// resolves to Base(0)+Min(0) = 0 per the AdaptiveDuration contract.
	assert.Equal(t, time.Duration(0), cfg.Hedge.Delay.Resolve(ex.latency),
		"empty tracker with zero Base/Min must resolve to 0")

	// Cold-start Base+Min semantics with explicit values.
	cold := &common.AdaptiveDuration{
		Base:     common.Duration(20 * time.Millisecond),
		Quantile: 0.95,
		Min:      common.Duration(30 * time.Millisecond),
	}
	assert.Equal(t, 50*time.Millisecond, cold.Resolve(nil),
		"cold tracker must resolve to Base+Min exactly")

	// Seeded tracker: p95 of uniform 50ms samples is ~50ms (DDSketch
	// relative accuracy is a few percent).
	seedLatency(ex, 0.050, 20)
	resolved := cfg.Hedge.Delay.Resolve(ex.latency)
	assert.GreaterOrEqual(t, resolved, 40*time.Millisecond, "resolved hedge delay must track the seeded p95")
	assert.LessOrEqual(t, resolved, 100*time.Millisecond, "resolved hedge delay must track the seeded p95")
}

func TestCacheFailsafe_DynamicHedge_FiresOnSlowPrimary(t *testing.T) {
	logger := zerolog.New(io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mc := NewMockConnector("test")
	// Primary blocks well past the resolved (~50ms) hedge delay.
	mc.On("Get", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			cctx := args.Get(0).(context.Context)
			select {
			case <-cctx.Done():
			case <-time.After(150 * time.Millisecond):
			}
		}).
		Return(nil, context.Canceled).Once()
	// Hedged attempt returns immediately.
	mc.On("Get", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return([]byte("hedge"), nil).Once()

	fc, err := NewFailsafeConnector(ctx, &logger, mc, []*common.FailsafeConfig{
		{
			Hedge: &common.HedgePolicyConfig{
				Delay:    &common.AdaptiveDuration{Quantile: 0.95, Max: common.Duration(300 * time.Millisecond)},
				MaxCount: 1,
			},
		},
	}, nil)
	require.NoError(t, err)

	// Seed the executor's own tracker so the quantile resolves small (~50ms).
	seedLatency(fc.getExecutors[0], 0.050, 20)

	result, err := fc.Get(context.Background(), "", "pk", "rk", nil)
	require.NoError(t, err)
	assert.Equal(t, []byte("hedge"), result, "hedge should win over the slow primary")
	mc.AssertNumberOfCalls(t, "Get", 2)
}

func TestCacheFailsafe_DynamicTimeout_ClampsAtMax(t *testing.T) {
	logger := zerolog.New(io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mc := NewMockConnector("test")
	mc.On("Get", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			cctx := args.Get(0).(context.Context)
			select {
			case <-cctx.Done():
			case <-time.After(5 * time.Second):
			}
		}).
		Return(nil, context.DeadlineExceeded)

	fc, err := NewFailsafeConnector(ctx, &logger, mc, []*common.FailsafeConfig{
		{
			Timeout: &common.TimeoutPolicyConfig{
				Duration: &common.AdaptiveDuration{
					Base:     common.Duration(100 * time.Millisecond),
					Quantile: 0.9,
					Max:      common.Duration(250 * time.Millisecond),
				},
			},
		},
	}, nil)
	require.NoError(t, err)

	// Slow samples: unclamped resolve would be ~100ms + 1s; Max caps at 250ms.
	seedLatency(fc.getExecutors[0], 1.0, 10)

	start := time.Now()
	_, err = fc.Get(context.Background(), "", "pk", "rk", nil)
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeFailsafeTimeoutExceeded),
		"dynamic timeout expiry must surface as failsafe timeout, got: %v", err)
	assert.GreaterOrEqual(t, elapsed, 200*time.Millisecond,
		"timeout must not fire before the 250ms Max clamp")
	assert.Less(t, elapsed, 1*time.Second,
		"Max clamp must cap the timeout well below the seeded ~1.1s resolve")
}

func TestCacheExecutor_DynamicTimeout_MinAutoFloor(t *testing.T) {
	logger := zerolog.New(io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Quantile>0, Min unset, Base=100ms → Min auto-floors to Base/2=50ms,
	// so a cold tracker resolves to Base+Min = 150ms.
	cfg := &common.FailsafeConfig{
		Timeout: &common.TimeoutPolicyConfig{
			Duration: &common.AdaptiveDuration{
				Base:     common.Duration(100 * time.Millisecond),
				Quantile: 0.9,
				Max:      common.Duration(250 * time.Millisecond),
			},
		},
	}
	ex, err := NewCacheExecutor(ctx, cfg, &logger)
	require.NoError(t, err)
	require.NotNil(t, ex.timeoutSpec)
	assert.Equal(t, common.Duration(50*time.Millisecond), ex.timeoutSpec.Min,
		"Min must auto-floor to Base/2")
	assert.Equal(t, 150*time.Millisecond, ex.timeoutSpec.Resolve(nil),
		"cold tracker must resolve to Base + auto-floored Min")
	assert.Equal(t, common.Duration(0), cfg.Timeout.Duration.Min,
		"auto-floor must not mutate the user config")

	// Base==0 → Min auto-floors to 500ms.
	cfgNoBase := &common.FailsafeConfig{
		Timeout: &common.TimeoutPolicyConfig{
			Duration: &common.AdaptiveDuration{Quantile: 0.9},
		},
	}
	ex2, err := NewCacheExecutor(ctx, cfgNoBase, &logger)
	require.NoError(t, err)
	assert.Equal(t, 500*time.Millisecond, ex2.timeoutSpec.Resolve(nil),
		"zero Base must auto-floor Min to 500ms")
}

func TestCacheFailsafe_LatencyRecording_Classification(t *testing.T) {
	logger := zerolog.New(io.Discard)

	// Generous quantile timeout only to force the latency tracker into
	// existence; it never fires within these cases.
	quantileCfg := func() []*common.FailsafeConfig {
		return []*common.FailsafeConfig{
			{
				Timeout: &common.TimeoutPolicyConfig{
					Duration: &common.AdaptiveDuration{
						Base:     common.Duration(1 * time.Second),
						Quantile: 0.9,
					},
				},
			},
		}
	}

	newConnector := func(t *testing.T, mc *MockConnector) *FailsafeConnector {
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		fc, err := NewFailsafeConnector(ctx, &logger, mc, quantileCfg(), nil)
		require.NoError(t, err)
		require.NotNil(t, fc.getExecutors[0].latency)
		return fc
	}

	t.Run("SuccessRecordsSample", func(t *testing.T) {
		mc := NewMockConnector("test")
		mc.On("Get", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
			Return([]byte("data"), nil)
		fc := newConnector(t, mc)

		_, err := fc.Get(context.Background(), "", "pk", "rk", nil)
		require.NoError(t, err)
		assert.Greater(t, fc.getExecutors[0].latency.GetQuantile(0.5), time.Duration(0),
			"success must record a latency sample")
	})

	t.Run("RecordNotFoundRecordsSample", func(t *testing.T) {
		mc := NewMockConnector("test")
		mc.On("Get", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
			Return(nil, common.NewErrRecordNotFound("pk", "rk", "memory"))
		fc := newConnector(t, mc)

		_, err := fc.Get(context.Background(), "", "pk", "rk", nil)
		require.Error(t, err)
		assert.Greater(t, fc.getExecutors[0].latency.GetQuantile(0.5), time.Duration(0),
			"semantic miss must record a latency sample — the connector did real work")
	})

	t.Run("TransportErrorRecordsNoSample", func(t *testing.T) {
		mc := NewMockConnector("test")
		mc.On("Get", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
			Return(nil, errors.New("connection refused"))
		fc := newConnector(t, mc)

		_, err := fc.Get(context.Background(), "", "pk", "rk", nil)
		require.Error(t, err)
		assert.Equal(t, time.Duration(0), fc.getExecutors[0].latency.GetQuantile(0.5),
			"a fast-failing connector must not be crowned fast")
	})

	t.Run("InterruptedAttemptRecordsNoSample", func(t *testing.T) {
		mc := NewMockConnector("test")
		mc.On("Get", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
			Run(func(args mock.Arguments) {
				cctx := args.Get(0).(context.Context)
				<-cctx.Done()
			}).
			Return(nil, context.Canceled)
		fc := newConnector(t, mc)

		reqCtx, reqCancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(10 * time.Millisecond)
			reqCancel()
		}()
		_, err := fc.Get(reqCtx, "", "pk", "rk", nil)
		require.Error(t, err)
		assert.Equal(t, time.Duration(0), fc.getExecutors[0].latency.GetQuantile(0.5),
			"an interrupted attempt measures the canceller, not the connector")
	})
}

// funcConnector routes Get through a plain function so a test can vary
// per-call behavior (fast vs slow) without a testify expectation per
// call. Everything else falls through to the embedded mock.
type funcConnector struct {
	*MockConnector
	get func(ctx context.Context) ([]byte, error)
}

func (c *funcConnector) Get(ctx context.Context, _, _, _ string, _ interface{}) ([]byte, error) {
	return c.get(ctx)
}

// sleepOrCancel blocks for d unless ctx ends first, returning a timely
// reply or the context error — a connector that honors cancellation.
func sleepOrCancel(ctx context.Context, d time.Duration) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(d):
		return []byte("slow"), nil
	}
}

func newQuantileTimeoutConnector(t *testing.T, spec *common.AdaptiveDuration, get func(ctx context.Context) ([]byte, error)) (*FailsafeConnector, *cacheExecutor) {
	t.Helper()
	logger := zerolog.New(io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	fc, err := NewFailsafeConnector(ctx, &logger, &funcConnector{MockConnector: NewMockConnector("test"), get: get}, []*common.FailsafeConfig{
		{Timeout: &common.TimeoutPolicyConfig{Duration: spec}},
	}, nil)
	require.NoError(t, err)
	ex := fc.getExecutors[0]
	require.NotNil(t, ex.latency)
	return fc, ex
}

// An attempt the executor's own timeout kills is a right-censored
// observation: the connector's latency is "at least the budget", not the
// budget. Recording it at the budget would cap the tracked quantile at
// the current timeout and the adaptive loop could only ever tighten.
func TestCacheFailsafe_DynamicTimeout_OwnTimeoutRecordedCensored(t *testing.T) {
	hang := func(ctx context.Context) ([]byte, error) { return sleepOrCancel(ctx, 5*time.Second) }

	t.Run("AtMaxWhenSet", func(t *testing.T) {
		const maxD = 300 * time.Millisecond
		fc, ex := newQuantileTimeoutConnector(t, &common.AdaptiveDuration{
			Quantile: 0.9,
			Min:      common.Duration(20 * time.Millisecond),
			Max:      common.Duration(maxD),
		}, hang)
		require.Equal(t, 20*time.Millisecond, ex.timeoutSpec.Resolve(ex.latency), "cold budget is Min")

		_, err := fc.Get(context.Background(), "", "pk", "rk", nil)
		require.Error(t, err)
		require.True(t, common.HasErrorCode(err, common.ErrCodeFailsafeTimeoutExceeded), "got: %v", err)

		// The single sample is the window's every quantile: it must sit at
		// Max (within DDSketch's 1% relative accuracy), not at the 20ms
		// budget the attempt actually consumed.
		sample := ex.latency.GetQuantile(0.5)
		assert.InDelta(t, float64(maxD), float64(sample), float64(maxD)*0.05,
			"own-timeout must be recorded at Max, got %v", sample)
		resolved := ex.timeoutSpec.Resolve(ex.latency)
		assert.LessOrEqual(t, resolved, maxD, "a censored window must never resolve past Max")
		assert.Greater(t, resolved, 20*time.Millisecond, "a censored window must lift the budget off Min")
	})

	t.Run("AtTwiceBudgetWithoutMax", func(t *testing.T) {
		// Base=20ms, Min auto-floors to 10ms → cold budget 30ms. No Max:
		// the censored sample lands at 2× the budget it was killed at.
		fc, ex := newQuantileTimeoutConnector(t, &common.AdaptiveDuration{
			Base:     common.Duration(20 * time.Millisecond),
			Quantile: 0.9,
		}, hang)
		budget := ex.timeoutSpec.Resolve(ex.latency)
		require.Equal(t, 30*time.Millisecond, budget)

		_, err := fc.Get(context.Background(), "", "pk", "rk", nil)
		require.Error(t, err)
		require.True(t, common.HasErrorCode(err, common.ErrCodeFailsafeTimeoutExceeded), "got: %v", err)

		sample := ex.latency.GetQuantile(0.5)
		assert.InDelta(t, float64(2*budget), float64(sample), float64(budget)*0.1,
			"own-timeout without Max must be recorded at 2× the budget, got %v", sample)
		assert.Greater(t, ex.timeoutSpec.Resolve(ex.latency), budget,
			"the next budget must be able to rise above the one that timed out")
	})
}

// {quantile:0.99, min:X, max:Y} against a connector whose tail exceeds X:
// the budget starts at X (cold), the first slow read times out, and the
// censored sample lets the resolved budget rise above X — so later slow
// reads complete instead of every one of them dying at X forever.
func TestCacheFailsafe_DynamicTimeout_BudgetRisesAboveMin(t *testing.T) {
	const (
		minD  = 20 * time.Millisecond
		maxD  = 200 * time.Millisecond
		slowD = 40 * time.Millisecond // above Min, well below Max
		calls = 100
	)
	var n atomic.Int64
	fc, ex := newQuantileTimeoutConnector(t, &common.AdaptiveDuration{
		Quantile: 0.99,
		Min:      common.Duration(minD),
		Max:      common.Duration(maxD),
	}, func(ctx context.Context) ([]byte, error) {
		if n.Add(1)%10 == 0 { // 10% of reads are slow — well past the 1% tail
			return sleepOrCancel(ctx, slowD)
		}
		return []byte("fast"), nil
	})
	require.Equal(t, minD, ex.timeoutSpec.Resolve(ex.latency), "cold budget is Min")

	var slowTimedOut, slowCompleted int
	for i := 1; i <= calls; i++ {
		budget := ex.timeoutSpec.Resolve(ex.latency)
		assert.GreaterOrEqual(t, budget, minD, "call %d: budget below Min", i)
		assert.LessOrEqual(t, budget, maxD, "call %d: budget above Max", i)
		_, err := fc.Get(context.Background(), "", "pk", "rk", nil)
		if i%10 != 0 {
			require.NoError(t, err, "call %d (fast) must succeed", i)
			continue
		}
		switch {
		case err == nil:
			slowCompleted++
		case common.HasErrorCode(err, common.ErrCodeFailsafeTimeoutExceeded):
			slowTimedOut++
		default:
			t.Fatalf("call %d (slow): unexpected error %v", i, err)
		}
	}
	assert.GreaterOrEqual(t, slowTimedOut, 1, "the first slow read must die at the cold Min budget")
	assert.GreaterOrEqual(t, slowCompleted, 1,
		"the budget must loosen so slow reads complete; recording own-timeouts at face value keeps every one at Min")

	resolved := ex.timeoutSpec.Resolve(ex.latency)
	assert.Greater(t, resolved, minD, "a >1%% tail above Min must lift the p99 budget above Min")
	assert.LessOrEqual(t, resolved, maxD, "the budget must never exceed Max")
}

// A connector that always answers inside Min leaves the budget at Min:
// censoring only touches attempts the timeout killed.
func TestCacheFailsafe_DynamicTimeout_BudgetStaysAtMinWhenFast(t *testing.T) {
	const minD = 20 * time.Millisecond
	fc, ex := newQuantileTimeoutConnector(t, &common.AdaptiveDuration{
		Quantile: 0.99,
		Min:      common.Duration(minD),
		Max:      common.Duration(200 * time.Millisecond),
	}, func(context.Context) ([]byte, error) { return []byte("fast"), nil })

	for i := range 100 {
		_, err := fc.Get(context.Background(), "", "pk", "rk", nil)
		require.NoError(t, err, "call %d", i+1)
	}
	assert.Greater(t, ex.latency.GetQuantile(0.99), time.Duration(0), "fast completions are still recorded")
	assert.Equal(t, minD, ex.timeoutSpec.Resolve(ex.latency), "a fast connector's budget stays at Min")
}

// Sustained own-timeouts with Base set: Base + censored(Max) exceeds Max
// before clamping, and the resolved budget must still land exactly on
// Max — attempts are never given more than the configured ceiling.
func TestCacheFailsafe_DynamicTimeout_CensoredSamplesNeverExceedMax(t *testing.T) {
	const maxD = 80 * time.Millisecond
	fc, ex := newQuantileTimeoutConnector(t, &common.AdaptiveDuration{
		Base:     common.Duration(30 * time.Millisecond),
		Quantile: 0.99,
		Max:      common.Duration(maxD),
	}, func(ctx context.Context) ([]byte, error) { return sleepOrCancel(ctx, 5*time.Second) })
	minD := ex.timeoutSpec.Min.Duration()
	require.Equal(t, 15*time.Millisecond, minD, "Min auto-floors to Base/2")

	for i := 1; i <= 5; i++ {
		start := time.Now()
		_, err := fc.Get(context.Background(), "", "pk", "rk", nil)
		elapsed := time.Since(start)
		require.Error(t, err, "call %d", i)
		require.True(t, common.HasErrorCode(err, common.ErrCodeFailsafeTimeoutExceeded), "call %d: %v", i, err)
		assert.Less(t, elapsed, maxD+50*time.Millisecond, "call %d: attempt outlived Max", i)

		resolved := ex.timeoutSpec.Resolve(ex.latency)
		assert.Equal(t, maxD, resolved, "call %d: censored window must resolve to exactly Max", i)
		assert.GreaterOrEqual(t, resolved, minD, "call %d: budget below Min", i)
	}
}

// A cache read the executor's own timeout had to kill is pure tax — the
// caller pays the wait AND falls through anyway — so own-timeouts count
// as breaker failures with no separate slowness knob: the timeout policy
// is the sole definition of "too slow". Sustained timeouts open the
// breaker (fail-fast exclusion); a probe that completes in time re-admits
// the connector.
func TestCacheFailsafe_TimeoutBreaker_OpensAndRecovers(t *testing.T) {
	logger := zerolog.New(io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mc := NewMockConnector("test")
	// Two reads that outlive the 20ms timeout: the mock honors context
	// cancellation, so each returns when the executor's own deadline
	// fires and is translated into ErrFailsafeTimeoutExceeded.
	mc.On("Get", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			cctx := args.Get(0).(context.Context)
			<-cctx.Done()
		}).
		Return(nil, context.DeadlineExceeded).Times(2)
	// Everything afterwards completes immediately.
	mc.On("Get", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return([]byte("fast"), nil)

	fc, err := NewFailsafeConnector(ctx, &logger, mc, []*common.FailsafeConfig{
		{
			Timeout: &common.TimeoutPolicyConfig{
				Duration: common.NewStaticDuration(20 * time.Millisecond),
			},
			CircuitBreaker: &common.CircuitBreakerPolicyConfig{
				FailureThresholdCount:    2,
				FailureThresholdCapacity: 2,
				HalfOpenAfter:            common.Duration(25 * time.Millisecond),
				SuccessThresholdCount:    1,
				SuccessThresholdCapacity: 1,
			},
		},
	}, nil)
	require.NoError(t, err)
	ex := fc.getExecutors[0]
	require.NotNil(t, ex.breaker)

	// Two own-timeouts: surfaced as timeout errors, each a breaker failure.
	for i := range 2 {
		_, err := fc.Get(context.Background(), "", "pk", "rk", nil)
		require.Error(t, err, "call %d must surface the timeout", i+1)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeFailsafeTimeoutExceeded),
			"own-timeout must be translated to failsafe-timeout-exceeded, got: %v", err)
	}
	assert.Equal(t, failsafe.StateOpen, ex.breaker.State(),
		"sustained own-timeouts must open the breaker")

	// Open breaker rejects without touching the connector.
	_, err = fc.Get(context.Background(), "", "pk", "rk", nil)
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeFailsafeCircuitBreakerOpen),
		"open breaker must reject with circuit-breaker-open, got: %v", err)
	mc.AssertNumberOfCalls(t, "Get", 2)

	// After HalfOpenAfter elapses, a probe that completes in time closes
	// the breaker again.
	time.Sleep(60 * time.Millisecond)
	result, err := fc.Get(context.Background(), "", "pk", "rk", nil)
	require.NoError(t, err, "half-open probe must be permitted and succeed")
	assert.Equal(t, []byte("fast"), result)
	assert.Equal(t, failsafe.StateClosed, ex.breaker.State(),
		"an in-time successful probe must close the breaker")

	// Closed again: traffic flows normally.
	result, err = fc.Get(context.Background(), "", "pk", "rk", nil)
	require.NoError(t, err)
	assert.Equal(t, []byte("fast"), result)
	mc.AssertNumberOfCalls(t, "Get", 4)
}
