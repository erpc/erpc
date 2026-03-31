package upstream

import (
	"context"
	"net/http"
	"testing"
	"time"

	pb "github.com/envoyproxy/go-control-plane/envoy/service/ratelimit/v3"
	"github.com/envoyproxy/ratelimit/src/config"
	"github.com/envoyproxy/ratelimit/src/limiter"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"github.com/prometheus/client_golang/prometheus"
	promUtil "github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type delayedRateLimitCache struct {
	delay    time.Duration
	statuses []*pb.RateLimitResponse_DescriptorStatus
}

var _ limiter.RateLimitCache = (*delayedRateLimitCache)(nil)

func (c *delayedRateLimitCache) DoLimit(context.Context, *pb.RateLimitRequest, []*config.RateLimit) []*pb.RateLimitResponse_DescriptorStatus {
	if c.delay > 0 {
		time.Sleep(c.delay)
	}
	return c.statuses
}

func (c *delayedRateLimitCache) Flush() {}

type panicRateLimitCache struct{}

var _ limiter.RateLimitCache = (*panicRateLimitCache)(nil)

func (c *panicRateLimitCache) DoLimit(context.Context, *pb.RateLimitRequest, []*config.RateLimit) []*pb.RateLimitResponse_DescriptorStatus {
	panic("EOF")
}

func (c *panicRateLimitCache) Flush() {}

type delayedPanicRateLimitCache struct {
	delay    time.Duration
	panicked chan struct{}
}

var _ limiter.RateLimitCache = (*delayedPanicRateLimitCache)(nil)

func (c *delayedPanicRateLimitCache) DoLimit(context.Context, *pb.RateLimitRequest, []*config.RateLimit) []*pb.RateLimitResponse_DescriptorStatus {
	defer close(c.panicked)
	time.Sleep(c.delay)
	panic("EOF")
}

func (c *delayedPanicRateLimitCache) Flush() {}

func histogramSampleCount(t *testing.T, hv *prometheus.HistogramVec, labels ...string) uint64 {
	t.Helper()
	obs, err := hv.GetMetricWithLabelValues(labels...)
	require.NoError(t, err)
	m, ok := obs.(prometheus.Metric)
	require.True(t, ok)
	pbMetric := &dto.Metric{}
	require.NoError(t, m.Write(pbMetric))
	return pbMetric.GetHistogram().GetSampleCount()
}

func newTestBudget(t *testing.T) *RateLimiterBudget {
	t.Helper()
	logger := zerolog.Nop()
	cfg := &common.RateLimiterConfig{
		Store: &common.RateLimitStoreConfig{Driver: "memory"},
		Budgets: []*common.RateLimitBudgetConfig{
			{
				Id: "test-budget",
				Rules: []*common.RateLimitRuleConfig{
					{
						Method:   "eth_test",
						MaxCount: 100,
						Period:   common.RateLimitPeriodSecond,
					},
				},
			},
		},
	}
	registry, err := NewRateLimitersRegistry(context.Background(), cfg, &logger)
	require.NoError(t, err)
	budget, err := registry.GetBudget("test-budget")
	require.NoError(t, err)
	require.NotNil(t, budget)
	return budget
}

func newTestRequestWithAgent() *common.NormalizedRequest {
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_test"}`))
	req.EnrichFromHttp(http.Header{"User-Agent": []string{"curl/8.0.1"}}, nil, common.UserAgentTrackingModeSimplified)
	return req
}

func TestRateLimiterBudget_PermitTimingMetrics_Ok(t *testing.T) {
	budget := newTestBudget(t)

	beforeEval := histogramSampleCount(
		t,
		telemetry.MetricRateLimiterPermitEvaluationDuration,
		"test-budget",
		"eth_test",
		"",
		"ok",
	)
	beforeWait := histogramSampleCount(
		t,
		telemetry.MetricRateLimiterPermitWaitDuration,
		"test-budget",
		"eth_test",
		"",
		"ok",
	)

	ok, err := budget.TryAcquirePermit(context.Background(), "", nil, "eth_test", "", "", "", "")
	require.NoError(t, err)
	assert.True(t, ok)

	afterEval := histogramSampleCount(
		t,
		telemetry.MetricRateLimiterPermitEvaluationDuration,
		"test-budget",
		"eth_test",
		"",
		"ok",
	)
	afterWait := histogramSampleCount(
		t,
		telemetry.MetricRateLimiterPermitWaitDuration,
		"test-budget",
		"eth_test",
		"",
		"ok",
	)

	assert.GreaterOrEqual(t, afterEval-beforeEval, uint64(1))
	assert.GreaterOrEqual(t, afterWait-beforeWait, uint64(1))
}

func TestRateLimiterBudget_PermitTimingMetrics_TimeoutFailOpen(t *testing.T) {
	budget := newTestBudget(t)
	projectID := "project-a"
	req := newTestRequestWithAgent()
	budget.maxTimeout = 10 * time.Millisecond
	budget.registry.cacheMu.Lock()
	budget.registry.envoyCache = &delayedRateLimitCache{
		delay:    40 * time.Millisecond,
		statuses: []*pb.RateLimitResponse_DescriptorStatus{},
	}
	budget.registry.cacheMu.Unlock()

	beforeEval := histogramSampleCount(
		t,
		telemetry.MetricRateLimiterPermitEvaluationDuration,
		"test-budget",
		"eth_test",
		"",
		"timeout_fail_open",
	)
	beforeWait := histogramSampleCount(
		t,
		telemetry.MetricRateLimiterPermitWaitDuration,
		"test-budget",
		"eth_test",
		"",
		"timeout_fail_open",
	)
	beforeFailOpen := promUtil.ToFloat64(
		telemetry.MetricRateLimiterFailopenTotal.WithLabelValues(
			projectID,
			"n/a",
			"n/a",
			"curl",
			"test-budget",
			"eth_test",
			"limit_timeout",
		),
	)
	ok, err := budget.TryAcquirePermit(context.Background(), projectID, req, "eth_test", "", "", "", "")
	require.NoError(t, err)
	assert.True(t, ok)

	afterEval := histogramSampleCount(
		t,
		telemetry.MetricRateLimiterPermitEvaluationDuration,
		"test-budget",
		"eth_test",
		"",
		"timeout_fail_open",
	)
	afterWait := histogramSampleCount(
		t,
		telemetry.MetricRateLimiterPermitWaitDuration,
		"test-budget",
		"eth_test",
		"",
		"timeout_fail_open",
	)
	afterFailOpen := promUtil.ToFloat64(
		telemetry.MetricRateLimiterFailopenTotal.WithLabelValues(
			projectID,
			"n/a",
			"n/a",
			"curl",
			"test-budget",
			"eth_test",
			"limit_timeout",
		),
	)
	assert.GreaterOrEqual(t, afterEval-beforeEval, uint64(1))
	assert.GreaterOrEqual(t, afterWait-beforeWait, uint64(1))
	assert.Equal(t, beforeFailOpen+1, afterFailOpen)
}

func TestRateLimiterBudget_PermitTimingMetrics_PanicFailOpen(t *testing.T) {
	budget := newTestBudget(t)
	projectID := "project-a"
	req := newTestRequestWithAgent()
	budget.registry.cfg.Store = &common.RateLimitStoreConfig{
		Driver: "redis",
		Redis:  &common.RedisConnectorConfig{URI: "redis://test"},
	}
	budget.registry.initializer = util.NewInitializer(context.Background(), budget.logger, &util.InitializerConfig{
		TaskTimeout:   time.Second,
		AutoRetry:     false,
		RetryFactor:   1,
		RetryMinDelay: time.Second,
		RetryMaxDelay: time.Second,
	})
	require.NoError(t, budget.registry.initializer.ExecuteTasks(
		context.Background(),
		util.NewBootstrapTask(redisRateLimiterConnectTaskName, func(context.Context) error { return nil }),
	))
	budget.registry.cacheMu.Lock()
	budget.registry.envoyCache = &panicRateLimitCache{}
	budget.registry.cacheMu.Unlock()

	beforeEval := histogramSampleCount(
		t,
		telemetry.MetricRateLimiterPermitEvaluationDuration,
		"test-budget",
		"eth_test",
		"",
		"panic_fail_open",
	)
	beforeWait := histogramSampleCount(
		t,
		telemetry.MetricRateLimiterPermitWaitDuration,
		"test-budget",
		"eth_test",
		"",
		"panic_fail_open",
	)
	beforeFailOpen := promUtil.ToFloat64(
		telemetry.MetricRateLimiterFailopenTotal.WithLabelValues(
			projectID,
			"n/a",
			"n/a",
			"curl",
			"test-budget",
			"eth_test",
			"limit_panic",
		),
	)
	beforeUnexpectedPanic := promUtil.ToFloat64(
		telemetry.MetricUnexpectedPanicTotal.WithLabelValues(
			"ratelimiter-do-limit",
			"budget:test-budget",
			common.ErrorFingerprint("EOF"),
		),
	)
	ok, err := budget.TryAcquirePermit(context.Background(), projectID, req, "eth_test", "", "", "", "")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Nil(t, budget.registry.GetCache())

	status := budget.registry.initializer.Status()
	require.Len(t, status.Tasks, 1)
	assert.Equal(t, util.TaskFailed, status.Tasks[0].State)
	assert.Error(t, status.Tasks[0].Err)

	afterEval := histogramSampleCount(
		t,
		telemetry.MetricRateLimiterPermitEvaluationDuration,
		"test-budget",
		"eth_test",
		"",
		"panic_fail_open",
	)
	afterWait := histogramSampleCount(
		t,
		telemetry.MetricRateLimiterPermitWaitDuration,
		"test-budget",
		"eth_test",
		"",
		"panic_fail_open",
	)
	afterFailOpen := promUtil.ToFloat64(
		telemetry.MetricRateLimiterFailopenTotal.WithLabelValues(
			projectID,
			"n/a",
			"n/a",
			"curl",
			"test-budget",
			"eth_test",
			"limit_panic",
		),
	)
	afterUnexpectedPanic := promUtil.ToFloat64(
		telemetry.MetricUnexpectedPanicTotal.WithLabelValues(
			"ratelimiter-do-limit",
			"budget:test-budget",
			common.ErrorFingerprint("EOF"),
		),
	)
	assert.GreaterOrEqual(t, afterEval-beforeEval, uint64(1))
	assert.GreaterOrEqual(t, afterWait-beforeWait, uint64(1))
	assert.Equal(t, beforeFailOpen+1, afterFailOpen)
	assert.Equal(t, beforeUnexpectedPanic+1, afterUnexpectedPanic)
}

func TestRateLimiterBudget_PermitTimingMetrics_PanicBeforeTimeoutFailOpen(t *testing.T) {
	budget := newTestBudget(t)
	projectID := "project-a"
	req := newTestRequestWithAgent()
	budget.registry.cfg.Store = &common.RateLimitStoreConfig{
		Driver: "redis",
		Redis:  &common.RedisConnectorConfig{URI: "redis://test"},
	}
	budget.registry.initializer = util.NewInitializer(context.Background(), budget.logger, &util.InitializerConfig{
		TaskTimeout:   time.Second,
		AutoRetry:     false,
		RetryFactor:   1,
		RetryMinDelay: time.Second,
		RetryMaxDelay: time.Second,
	})
	require.NoError(t, budget.registry.initializer.ExecuteTasks(
		context.Background(),
		util.NewBootstrapTask(redisRateLimiterConnectTaskName, func(context.Context) error { return nil }),
	))
	budget.maxTimeout = 100 * time.Millisecond
	budget.registry.cacheMu.Lock()
	budget.registry.envoyCache = &panicRateLimitCache{}
	budget.registry.cacheMu.Unlock()

	beforeFailOpen := promUtil.ToFloat64(
		telemetry.MetricRateLimiterFailopenTotal.WithLabelValues(
			projectID,
			"n/a",
			"n/a",
			"curl",
			"test-budget",
			"eth_test",
			"limit_panic",
		),
	)

	ok, err := budget.TryAcquirePermit(context.Background(), projectID, req, "eth_test", "", "", "", "")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Nil(t, budget.registry.GetCache())

	afterFailOpen := promUtil.ToFloat64(
		telemetry.MetricRateLimiterFailopenTotal.WithLabelValues(
			projectID,
			"n/a",
			"n/a",
			"curl",
			"test-budget",
			"eth_test",
			"limit_panic",
		),
	)
	assert.Equal(t, beforeFailOpen+1, afterFailOpen)
}

func TestRateLimiterBudget_LatePanicFromStaleCacheDoesNotClearHealthyReconnect(t *testing.T) {
	budget := newTestBudget(t)
	budget.registry.cfg.Store = &common.RateLimitStoreConfig{
		Driver: "redis",
		Redis:  &common.RedisConnectorConfig{URI: "redis://test"},
	}
	budget.registry.initializer = util.NewInitializer(context.Background(), budget.logger, &util.InitializerConfig{
		TaskTimeout:   time.Second,
		AutoRetry:     false,
		RetryFactor:   1,
		RetryMinDelay: time.Second,
		RetryMaxDelay: time.Second,
	})
	require.NoError(t, budget.registry.initializer.ExecuteTasks(
		context.Background(),
		util.NewBootstrapTask(redisRateLimiterConnectTaskName, func(context.Context) error { return nil }),
	))

	staleCache := &delayedPanicRateLimitCache{
		delay:    40 * time.Millisecond,
		panicked: make(chan struct{}),
	}
	healthyCache := &delayedRateLimitCache{statuses: []*pb.RateLimitResponse_DescriptorStatus{}}
	budget.maxTimeout = 10 * time.Millisecond

	budget.registry.cacheMu.Lock()
	budget.registry.envoyCache = staleCache
	budget.registry.cacheMu.Unlock()

	ok, err := budget.TryAcquirePermit(context.Background(), "project-a", nil, "eth_test", "", "", "", "")
	require.NoError(t, err)
	assert.True(t, ok)

	budget.registry.cacheMu.Lock()
	budget.registry.envoyCache = healthyCache
	budget.registry.cacheMu.Unlock()

	select {
	case <-staleCache.panicked:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for stale cache panic")
	}

	assert.Same(t, healthyCache, budget.registry.GetCache())

	status := budget.registry.initializer.Status()
	require.Len(t, status.Tasks, 1)
	assert.Equal(t, util.TaskSucceeded, status.Tasks[0].State)
}

func TestNormalizeRateLimitMethodLabel(t *testing.T) {
	assert.Equal(t, "*", normalizeRateLimitMethodLabel(""))
	assert.Equal(t, "eth_getLogs", normalizeRateLimitMethodLabel("eth_getLogs"))
}
