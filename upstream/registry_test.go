package upstream

import (
	"context"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/thirdparty"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	telemetry.SetHistogramBuckets("0.05,0.5,5,30")
}

func getUpsByID(upsList []common.Upstream, ids ...string) []common.Upstream {
	var ups []common.Upstream
	for _, id := range ids {
		for _, u := range upsList {
			if u.Id() == id {
				ups = append(ups, u)
				break
			}
		}
	}
	return ups
}

func hasUpstreamID(upsList []common.Upstream, id string) bool {
	for _, ups := range upsList {
		if ups.Id() == id {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Penalty Dimension Tests: each test isolates one penalty component
// ---------------------------------------------------------------------------

func TestUpstreamsRegistry_ErrorRateOrdering(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := log.Logger
	registry, metricsTracker := createTestRegistry(ctx, "test-project", &logger, 10*time.Second)

	method := "eth_call"
	networkID := "evm:123"
	l, _ := registry.GetSortedUpstreams(ctx, networkID, method)
	ups := getUpsByID(l, "rpc1", "rpc2", "rpc3")

	simulateRequests(metricsTracker, ups[0], method, 100, 5)
	simulateRequests(metricsTracker, ups[1], method, 100, 50)
	simulateRequests(metricsTracker, ups[2], method, 100, 20)

	err := registry.RefreshUpstreamNetworkMethodScores()
	require.NoError(t, err)

	ordered, err := registry.GetSortedUpstreams(ctx, networkID, method)
	require.NoError(t, err)
	assert.Equal(t, "rpc1", ordered[0].Id(), "lowest error rate should be first")
	assert.Equal(t, "rpc3", ordered[1].Id(), "medium error rate should be second")
	assert.Equal(t, "rpc2", ordered[2].Id(), "highest error rate should be last")
}

func TestUpstreamsRegistry_LatencyOrdering(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := log.Logger
	registry, metricsTracker := createTestRegistry(ctx, "test-project", &logger, 10*time.Second)

	method := "eth_call"
	networkID := "evm:123"
	l, _ := registry.GetSortedUpstreams(ctx, networkID, method)
	ups := getUpsByID(l, "rpc1", "rpc2", "rpc3")

	simulateRequestsWithLatency(metricsTracker, ups[0], method, 20, 0.020)
	simulateRequestsWithLatency(metricsTracker, ups[1], method, 20, 0.700)
	simulateRequestsWithLatency(metricsTracker, ups[2], method, 20, 0.200)

	err := registry.RefreshUpstreamNetworkMethodScores()
	require.NoError(t, err)

	ordered, err := registry.GetSortedUpstreams(ctx, networkID, method)
	require.NoError(t, err)
	assert.Equal(t, "rpc1", ordered[0].Id(), "lowest latency should be first")
	assert.Equal(t, "rpc3", ordered[1].Id(), "medium latency should be second")
	assert.Equal(t, "rpc2", ordered[2].Id(), "highest latency should be last")
}

func TestUpstreamsRegistry_BlockHeadLagOrdering(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := log.Logger
	registry, metricsTracker := createTestRegistry(ctx, "test-project", &logger, 10*time.Hour)

	method := "eth_call"
	networkID := "evm:123"
	l, _ := registry.GetSortedUpstreams(ctx, networkID, method)
	ups := getUpsByID(l, "rpc1", "rpc2", "rpc3")

	simulateRequests(metricsTracker, ups[0], method, 100, 0)
	metricsTracker.SetLatestBlockNumber(ups[0], 4000090, 0)
	simulateRequests(metricsTracker, ups[1], method, 100, 0)
	metricsTracker.SetLatestBlockNumber(ups[1], 4000100, 0)
	simulateRequests(metricsTracker, ups[2], method, 100, 0)
	metricsTracker.SetLatestBlockNumber(ups[2], 3005020, 0)

	err := registry.RefreshUpstreamNetworkMethodScores()
	require.NoError(t, err)

	ordered, err := registry.GetSortedUpstreams(ctx, networkID, method)
	require.NoError(t, err)
	assert.Equal(t, "rpc2", ordered[0].Id(), "zero block lag should be first")
	assert.Equal(t, "rpc1", ordered[1].Id(), "small block lag should be second")
	assert.Equal(t, "rpc3", ordered[2].Id(), "large block lag should be last")
}

func TestUpstreamsRegistry_FinalizationLagOrdering(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := log.Logger
	registry, metricsTracker := createTestRegistry(ctx, "test-project", &logger, 10*time.Hour)

	method := "eth_call"
	networkID := "evm:123"
	l, _ := registry.GetSortedUpstreams(ctx, networkID, method)
	ups := getUpsByID(l, "rpc1", "rpc2", "rpc3")

	simulateRequests(metricsTracker, ups[0], method, 100, 0)
	metricsTracker.SetFinalizedBlockNumber(ups[0], 4000090)
	simulateRequests(metricsTracker, ups[1], method, 100, 0)
	metricsTracker.SetFinalizedBlockNumber(ups[1], 3005020)
	simulateRequests(metricsTracker, ups[2], method, 100, 0)
	metricsTracker.SetFinalizedBlockNumber(ups[2], 4000100)

	err := registry.RefreshUpstreamNetworkMethodScores()
	require.NoError(t, err)

	ordered, err := registry.GetSortedUpstreams(ctx, networkID, method)
	require.NoError(t, err)
	assert.Equal(t, "rpc3", ordered[0].Id(), "zero finalization lag should be first")
	assert.Equal(t, "rpc1", ordered[1].Id(), "small finalization lag should be second")
	assert.Equal(t, "rpc2", ordered[2].Id(), "large finalization lag should be last")
}

func TestUpstreamsRegistry_ThrottlingOrdering(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := log.Logger
	registry, metricsTracker := createTestRegistry(ctx, "test-project", &logger, 10*time.Second)

	method := "eth_getLogs"
	networkID := "evm:123"
	l, _ := registry.GetSortedUpstreams(ctx, networkID, method)
	ups := getUpsByID(l, "rpc1", "rpc2", "rpc3")

	simulateRequestsWithLatency(metricsTracker, ups[0], method, 10, 0.060)
	simulateRequestsWithLatency(metricsTracker, ups[1], method, 10, 0.060)
	simulateRequestsWithLatency(metricsTracker, ups[2], method, 10, 0.060)
	simulateRequestsWithRateLimiting(metricsTracker, ups[0], method, 20, 5)
	simulateRequestsWithRateLimiting(metricsTracker, ups[1], method, 20, 1)
	simulateRequestsWithRateLimiting(metricsTracker, ups[2], method, 20, 0)

	err := registry.RefreshUpstreamNetworkMethodScores()
	require.NoError(t, err)

	ordered, err := registry.GetSortedUpstreams(ctx, networkID, method)
	require.NoError(t, err)
	var i1, i2, i3 int
	for i, u := range ordered {
		switch u.Id() {
		case "rpc1":
			i1 = i
		case "rpc2":
			i2 = i
		case "rpc3":
			i3 = i
		}
	}
	assert.Less(t, i3, i2, "less throttled should rank before more throttled")
	assert.Less(t, i2, i1, "medium throttled should rank before heavily throttled")
}

func TestUpstreamsRegistry_BrownoutStateMachine(t *testing.T) {
	t.Setenv("ERPC_BROWNOUT_ENABLED", "true")
	t.Setenv("ERPC_BROWNOUT_MIN_REQUESTS", "10")
	t.Setenv("ERPC_BROWNOUT_MIN_REMOTE_RATE_LIMITED", "5")
	t.Setenv("ERPC_BROWNOUT_THROTTLED_RATE_THRESHOLD", "0.5")
	t.Setenv("ERPC_BROWNOUT_COOLDOWN", "80ms")
	t.Setenv("ERPC_BROWNOUT_HALF_OPEN_DURATION", "80ms")

	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := log.Logger
	registry, metricsTracker := createTestRegistry(ctx, "test-project", &logger, 10*time.Second)

	method := "eth_call"
	networkID := "evm:123"
	initial, _ := registry.GetSortedUpstreams(ctx, networkID, method)
	ups := getUpsByID(initial, "rpc1", "rpc2", "rpc3")
	require.Len(t, ups, 3)

	// Trigger brownout for rpc1.
	simulateRequestsWithRateLimiting(metricsTracker, ups[0], "*", 20, 16)
	require.NoError(t, registry.RefreshUpstreamNetworkMethodScores())

	closed, err := registry.GetSortedUpstreams(ctx, networkID, method)
	require.NoError(t, err)
	assert.False(t, hasUpstreamID(closed, "rpc1"), "brownout-closed upstream should be excluded from routing")

	// Cooldown elapsed -> half-open should include rpc1 again.
	require.Eventually(t, func() bool {
		if err := registry.RefreshUpstreamNetworkMethodScores(); err != nil {
			return false
		}
		halfOpen, err := registry.GetSortedUpstreams(ctx, networkID, method)
		if err != nil {
			return false
		}
		return hasUpstreamID(halfOpen, "rpc1")
	}, 700*time.Millisecond, 25*time.Millisecond, "half-open upstream should rejoin routing for recovery probe")

	// During half-open, sustained rate limiting should re-close rpc1.
	simulateRequestsWithRateLimiting(metricsTracker, ups[0], "*", 12, 9)
	require.Eventually(t, func() bool {
		if err := registry.RefreshUpstreamNetworkMethodScores(); err != nil {
			return false
		}
		reclosed, err := registry.GetSortedUpstreams(ctx, networkID, method)
		if err != nil {
			return false
		}
		return !hasUpstreamID(reclosed, "rpc1")
	}, 300*time.Millisecond, 20*time.Millisecond, "failed half-open should move upstream back to brownout-closed")
}

func TestUpstreamsRegistry_BrownoutHalfOpenSuccess_DoesNotImmediatelyReclose(t *testing.T) {
	t.Setenv("ERPC_BROWNOUT_ENABLED", "true")
	t.Setenv("ERPC_BROWNOUT_MIN_REQUESTS", "10")
	t.Setenv("ERPC_BROWNOUT_MIN_REMOTE_RATE_LIMITED", "5")
	t.Setenv("ERPC_BROWNOUT_THROTTLED_RATE_THRESHOLD", "0.5")
	t.Setenv("ERPC_BROWNOUT_COOLDOWN", "80ms")
	t.Setenv("ERPC_BROWNOUT_HALF_OPEN_DURATION", "80ms")

	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := log.Logger
	registry, metricsTracker := createTestRegistry(ctx, "test-project", &logger, 10*time.Second)

	method := "eth_call"
	networkID := "evm:123"
	initial, _ := registry.GetSortedUpstreams(ctx, networkID, method)
	ups := getUpsByID(initial, "rpc1", "rpc2", "rpc3")
	require.Len(t, ups, 3)

	// Trigger initial brownout close.
	simulateRequestsWithRateLimiting(metricsTracker, ups[0], "*", 20, 16)
	require.NoError(t, registry.RefreshUpstreamNetworkMethodScores())

	require.Eventually(t, func() bool {
		closed, err := registry.GetSortedUpstreams(ctx, networkID, method)
		if err != nil {
			return false
		}
		return !hasUpstreamID(closed, "rpc1")
	}, 300*time.Millisecond, 20*time.Millisecond, "rpc1 should enter brownout-closed first")

	// Wait through half-open and successful recovery without adding new throttling.
	require.Eventually(t, func() bool {
		if err := registry.RefreshUpstreamNetworkMethodScores(); err != nil {
			return false
		}
		registry.brownoutMu.RLock()
		st := registry.brownoutStates["rpc1"]
		registry.brownoutMu.RUnlock()
		return st == nil || st.Phase == brownoutPhaseOpen
	}, 1200*time.Millisecond, 25*time.Millisecond, "rpc1 should recover to open after half-open")

	// Keep refreshing with identical cumulative counters; rpc1 should stay routable.
	for i := 0; i < 4; i++ {
		require.NoError(t, registry.RefreshUpstreamNetworkMethodScores())
		ordered, err := registry.GetSortedUpstreams(ctx, networkID, method)
		require.NoError(t, err)
		assert.True(t, hasUpstreamID(ordered, "rpc1"), "rpc1 should remain open without new rate-limited traffic")
		time.Sleep(20 * time.Millisecond)
	}
}

func TestUpstreamsRegistry_BrownoutFailOpenWhenAllWouldBeExcluded(t *testing.T) {
	t.Setenv("ERPC_BROWNOUT_ENABLED", "true")
	t.Setenv("ERPC_BROWNOUT_MIN_REQUESTS", "10")
	t.Setenv("ERPC_BROWNOUT_MIN_REMOTE_RATE_LIMITED", "5")
	t.Setenv("ERPC_BROWNOUT_THROTTLED_RATE_THRESHOLD", "0.5")
	t.Setenv("ERPC_BROWNOUT_COOLDOWN", "100ms")
	t.Setenv("ERPC_BROWNOUT_HALF_OPEN_DURATION", "100ms")

	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := log.Logger
	registry, metricsTracker := createTestRegistry(ctx, "test-project", &logger, 10*time.Second)

	method := "eth_call"
	networkID := "evm:123"
	initial, _ := registry.GetSortedUpstreams(ctx, networkID, method)
	ups := getUpsByID(initial, "rpc1", "rpc2", "rpc3")
	require.Len(t, ups, 3)

	for _, up := range ups {
		simulateRequestsWithRateLimiting(metricsTracker, up, "*", 20, 16)
	}
	require.NoError(t, registry.RefreshUpstreamNetworkMethodScores())

	ordered, err := registry.GetSortedUpstreams(ctx, networkID, method)
	require.NoError(t, err)
	require.NotEmpty(t, ordered, "brownout fail-open should keep at least one upstream routable")
}

func TestUpstreamsRegistry_MixedLatencyAndErrors(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := log.Logger
	registry, metricsTracker := createTestRegistry(ctx, "test-project", &logger, 10*time.Second)

	method := "eth_call"
	networkID := "evm:123"
	l, _ := registry.GetSortedUpstreams(ctx, networkID, method)
	ups := getUpsByID(l, "rpc1", "rpc2", "rpc3")

	// rpc1: fast but extremely high error rate
	simulateRequestsWithLatency(metricsTracker, ups[0], method, 10, 0.050)
	simulateFailedRequests(metricsTracker, ups[0], method, 80)

	// rpc2: marginally slower but perfect reliability
	simulateRequestsWithLatency(metricsTracker, ups[1], method, 100, 0.065)

	// rpc3: moderate speed, moderate errors
	simulateRequestsWithLatency(metricsTracker, ups[2], method, 50, 0.060)
	simulateFailedRequests(metricsTracker, ups[2], method, 20)

	err := registry.RefreshUpstreamNetworkMethodScores()
	require.NoError(t, err)

	ordered, err := registry.GetSortedUpstreams(ctx, networkID, method)
	require.NoError(t, err)

	// rpc1: 89% errors (huge penalty), rpc2: 0% errors + mild latency above best,
	// rpc3: 29% errors + near-best latency. rpc2 should beat rpc1 despite being slightly slower.
	var i1, i2 int
	for i, u := range ordered {
		switch u.Id() {
		case "rpc1":
			i1 = i
		case "rpc2":
			i2 = i
		}
	}
	assert.Less(t, i2, i1, "reliable upstream with mild latency should rank before fast but error-prone upstream")
}

// ---------------------------------------------------------------------------
// Per-Method Isolation
// ---------------------------------------------------------------------------

func TestUpstreamsRegistry_PerMethodIsolation(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := log.Logger
	registry, metricsTracker := createTestRegistry(ctx, "test-project", &logger, 5*time.Second)

	methodA := "eth_call"
	methodB := "eth_getBalance"
	networkID := "evm:123"
	lA, _ := registry.GetSortedUpstreams(ctx, networkID, methodA)
	lB, _ := registry.GetSortedUpstreams(ctx, networkID, methodB)
	upsA := getUpsByID(lA, "rpc1", "rpc2", "rpc3")
	upsB := getUpsByID(lB, "rpc1", "rpc2", "rpc3")

	// Method A: rpc1 is much faster
	simulateRequestsWithLatency(metricsTracker, upsA[0], methodA, 20, 0.020)
	simulateRequestsWithLatency(metricsTracker, upsA[1], methodA, 20, 0.500)
	simulateRequestsWithLatency(metricsTracker, upsA[2], methodA, 20, 0.300)

	// Method B: rpc2 is much faster
	simulateRequestsWithLatency(metricsTracker, upsB[0], methodB, 20, 0.500)
	simulateRequestsWithLatency(metricsTracker, upsB[1], methodB, 20, 0.020)
	simulateRequestsWithLatency(metricsTracker, upsB[2], methodB, 20, 0.300)

	err := registry.RefreshUpstreamNetworkMethodScores()
	require.NoError(t, err)

	orderedA, _ := registry.GetSortedUpstreams(ctx, networkID, methodA)
	orderedB, _ := registry.GetSortedUpstreams(ctx, networkID, methodB)
	assert.Equal(t, "rpc1", orderedA[0].Id(), "Method A should prefer rpc1 (lowest latency)")
	assert.Equal(t, "rpc2", orderedB[0].Id(), "Method B should prefer rpc2 (lowest latency)")
}

func TestUpstreamsRegistry_MultipleMethodsDifferentErrors(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := log.Logger
	registry, metricsTracker := createTestRegistry(ctx, "test-project", &logger, 10*time.Second)

	methodGetLogs := "eth_getLogs"
	methodTrace := "eth_traceTransaction"
	networkID := "evm:123"
	l1, _ := registry.GetSortedUpstreams(ctx, networkID, methodGetLogs)
	ups := getUpsByID(l1, "rpc1", "rpc2", "rpc3")
	_, _ = registry.GetSortedUpstreams(ctx, networkID, methodTrace)

	simulateRequests(metricsTracker, ups[0], methodGetLogs, 100, 10)
	simulateRequests(metricsTracker, ups[1], methodGetLogs, 100, 30)
	simulateRequests(metricsTracker, ups[2], methodGetLogs, 100, 20)

	simulateRequests(metricsTracker, ups[0], methodTrace, 100, 20)
	simulateRequests(metricsTracker, ups[1], methodTrace, 100, 10)
	simulateRequests(metricsTracker, ups[2], methodTrace, 100, 30)

	err := registry.RefreshUpstreamNetworkMethodScores()
	require.NoError(t, err)

	orderedGL, _ := registry.GetSortedUpstreams(ctx, networkID, methodGetLogs)
	orderedTr, _ := registry.GetSortedUpstreams(ctx, networkID, methodTrace)
	assert.Equal(t, "rpc1", orderedGL[0].Id(), "eth_getLogs should prefer rpc1 (fewest errors)")
	assert.Equal(t, "rpc2", orderedTr[0].Id(), "eth_traceTransaction should prefer rpc2 (fewest errors)")
}

// ---------------------------------------------------------------------------
// Stickiness
// ---------------------------------------------------------------------------

func TestUpstreamsRegistry_StickyPrimaryPreventsFlip(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := log.Logger

	stickyCfg := &ScoringConfig{
		ScoreGranularity:  "method",
		SwitchHysteresis:  0.10,
		MinSwitchInterval: 2 * time.Minute,
	}
	registry, metricsTracker := createTestRegistry(ctx, "test-project", &logger, 5*time.Second, stickyCfg)

	method := "eth_getBalance"
	networkID := "evm:123"
	l, _ := registry.GetSortedUpstreams(ctx, networkID, method)
	ups := getUpsByID(l, "rpc1", "rpc2")
	u1, u2 := ups[0], ups[1]

	// Phase 1: give rpc1 heavy errors so rpc2 takes over
	simulateFailedRequests(metricsTracker, u1, method, 50)
	simulateRequestsWithLatency(metricsTracker, u1, method, 10, 0.200)
	simulateRequestsWithLatency(metricsTracker, u2, method, 10, 0.050)

	err := registry.RefreshUpstreamNetworkMethodScores()
	require.NoError(t, err)
	ordered, _ := registry.GetSortedUpstreams(ctx, networkID, method)
	firstPrimary := ordered[0].Id()

	// Phase 2: rpc1 becomes slightly faster — small advantage should NOT flip
	simulateRequestsWithLatency(metricsTracker, u1, method, 10, 0.048)
	simulateRequestsWithLatency(metricsTracker, u2, method, 10, 0.050)

	err = registry.RefreshUpstreamNetworkMethodScores()
	require.NoError(t, err)
	ordered, _ = registry.GetSortedUpstreams(ctx, networkID, method)
	assert.Equal(t, firstPrimary, ordered[0].Id(), "Sticky primary should keep leading despite small latency advantage")
}

// ---------------------------------------------------------------------------
// Round-Robin
// ---------------------------------------------------------------------------

func TestUpstreamsRegistry_RoundRobinStrategy(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := log.Logger

	metricsTracker := health.NewTracker(&logger, "test-project", 5*time.Second)
	metricsTracker.Bootstrap(ctx)
	upstreamConfigs := []*common.UpstreamConfig{
		{Id: "rpc1", Endpoint: "http://rpc1.localhost", Type: common.UpstreamTypeEvm, Evm: &common.EvmUpstreamConfig{ChainId: 123}},
		{Id: "rpc2", Endpoint: "http://rpc2.localhost", Type: common.UpstreamTypeEvm, Evm: &common.EvmUpstreamConfig{ChainId: 123}},
		{Id: "rpc3", Endpoint: "http://rpc3.localhost", Type: common.UpstreamTypeEvm, Evm: &common.EvmUpstreamConfig{ChainId: 123}},
	}
	vr := thirdparty.NewVendorsRegistry()
	pr, _ := thirdparty.NewProvidersRegistry(&logger, vr, nil, nil)
	ssr, _ := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{Driver: "memory", Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"}},
	})
	registry := NewUpstreamsRegistry(ctx, &logger, "test-project", upstreamConfigs, ssr, nil, vr, pr, nil, metricsTracker, 1*time.Second,
		&ScoringConfig{RoutingStrategy: "round-robin"},
		nil,
	)
	registry.Bootstrap(ctx)
	time.Sleep(100 * time.Millisecond)
	_ = registry.PrepareUpstreamsForNetwork(ctx, "evm:123")

	method := "eth_call"
	networkID := "evm:123"
	_, _ = registry.GetSortedUpstreams(ctx, networkID, method)

	seen := map[string]bool{}
	for i := 0; i < 6; i++ {
		err := registry.RefreshUpstreamNetworkMethodScores()
		require.NoError(t, err)
		ordered, err := registry.GetSortedUpstreams(ctx, networkID, method)
		require.NoError(t, err)
		assert.Len(t, ordered, 3)
		seen[ordered[0].Id()] = true
	}
	assert.Len(t, seen, 3, "Round-robin should rotate through all upstreams")
}

func TestUpstreamsRegistry_LatencyAwareStrategy(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := log.Logger

	registry, metricsTracker := createTestRegistry(ctx, "test-project", &logger, 5*time.Second, &ScoringConfig{
		RoutingStrategy:   "latency-aware",
		ScoreGranularity:  "method",
		SwitchHysteresis:  -1,
		MinSwitchInterval: -1,
	})

	method := "eth_call"
	networkID := "evm:123"
	l, _ := registry.GetSortedUpstreams(ctx, networkID, method)
	ups := getUpsByID(l, "rpc1", "rpc2", "rpc3")

	simulateRequestsWithLatency(metricsTracker, ups[0], method, 20, 0.020)
	simulateRequestsWithLatency(metricsTracker, ups[1], method, 20, 0.350)
	simulateRequestsWithLatency(metricsTracker, ups[2], method, 20, 0.120)

	require.NoError(t, registry.RefreshUpstreamNetworkMethodScores())

	ordered, err := registry.GetSortedUpstreams(ctx, networkID, method)
	require.NoError(t, err)
	require.Len(t, ordered, 3)
	assert.Equal(t, "rpc1", ordered[0].Id())
	assert.Equal(t, "rpc3", ordered[1].Id())
	assert.Equal(t, "rpc2", ordered[2].Id())
}

func TestUpstreamsRegistry_CostAwareStrategy(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := log.Logger

	registry, metricsTracker := createTestRegistry(ctx, "test-project", &logger, 5*time.Second, &ScoringConfig{
		RoutingStrategy:   "cost-aware",
		ScoreGranularity:  "method",
		SwitchHysteresis:  -1,
		MinSwitchInterval: -1,
	})

	method := "eth_getLogs"
	networkID := "evm:123"
	l, _ := registry.GetSortedUpstreams(ctx, networkID, method)
	ups := getUpsByID(l, "rpc1", "rpc2", "rpc3")

	// Similar latency profile; throttled rate should drive ordering.
	simulateRequestsWithLatency(metricsTracker, ups[0], method, 20, 0.060)
	simulateRequestsWithLatency(metricsTracker, ups[1], method, 20, 0.060)
	simulateRequestsWithLatency(metricsTracker, ups[2], method, 20, 0.060)
	simulateRequestsWithRateLimiting(metricsTracker, ups[0], method, 20, 1)
	simulateRequestsWithRateLimiting(metricsTracker, ups[1], method, 20, 8)
	simulateRequestsWithRateLimiting(metricsTracker, ups[2], method, 20, 0)

	require.NoError(t, registry.RefreshUpstreamNetworkMethodScores())

	ordered, err := registry.GetSortedUpstreams(ctx, networkID, method)
	require.NoError(t, err)
	require.Len(t, ordered, 3)
	assert.Equal(t, "rpc3", ordered[0].Id(), "lowest throttled pressure should be preferred")
	assert.Equal(t, "rpc1", ordered[1].Id(), "medium throttled pressure should be second")
	assert.Equal(t, "rpc2", ordered[2].Id(), "highest throttled pressure should be last")
}

func TestUpstreamsRegistry_RendezvousStrategyDeterministicDistribution(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := log.Logger

	registry, _ := createTestRegistry(ctx, "test-project", &logger, 5*time.Second, &ScoringConfig{
		RoutingStrategy:  "rendezvous",
		ScoreGranularity: "method",
	})

	networkID := "evm:123"
	methods := []string{
		"eth_call",
		"eth_getBalance",
		"eth_getLogs",
		"eth_getBlockByNumber",
		"eth_getTransactionByHash",
		"eth_blockNumber",
		"net_version",
		"web3_clientVersion",
		"eth_feeHistory",
		"eth_chainId",
		"eth_gasPrice",
		"trace_block",
	}

	for _, method := range methods {
		_, _ = registry.GetSortedUpstreams(ctx, networkID, method)
	}
	require.NoError(t, registry.RefreshUpstreamNetworkMethodScores())

	firstByMethod := make(map[string]string, len(methods))
	uniquePrimaries := map[string]struct{}{}
	for _, method := range methods {
		ordered, err := registry.GetSortedUpstreams(ctx, networkID, method)
		require.NoError(t, err)
		require.NotEmpty(t, ordered)
		firstByMethod[method] = ordered[0].Id()
		uniquePrimaries[ordered[0].Id()] = struct{}{}
	}
	assert.GreaterOrEqual(t, len(uniquePrimaries), 2, "rendezvous should distribute primaries across methods")

	require.NoError(t, registry.RefreshUpstreamNetworkMethodScores())
	for _, method := range methods {
		ordered, err := registry.GetSortedUpstreams(ctx, networkID, method)
		require.NoError(t, err)
		require.NotEmpty(t, ordered)
		assert.Equal(t, firstByMethod[method], ordered[0].Id(), "rendezvous ordering should be stable for method")
	}
}

// ---------------------------------------------------------------------------
// Edge Cases
// ---------------------------------------------------------------------------

func TestUpstreamsRegistry_AllPeersNoSamplesNeutral(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := log.Logger
	registry, _ := createTestRegistry(ctx, "test-project", &logger, 5*time.Second)

	method := "eth_maxPriorityFeePerGas"
	networkID := "evm:123"

	err := registry.RefreshUpstreamNetworkMethodScores()
	require.NoError(t, err)

	ordered, _ := registry.GetSortedUpstreams(ctx, networkID, method)
	assert.Len(t, ordered, 3)
	ids := []string{ordered[0].Id(), ordered[1].Id(), ordered[2].Id()}
	assert.ElementsMatch(t, []string{"rpc1", "rpc2", "rpc3"}, ids)
}

func TestUpstreamsRegistry_PenaltyDecayOverTime(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := log.Logger
	registry, metricsTracker := createTestRegistry(ctx, "test-project", &logger, 5*time.Second)

	method := "eth_chainId"
	networkID := "evm:123"
	l, _ := registry.GetSortedUpstreams(ctx, networkID, method)
	ups := getUpsByID(l, "rpc1", "rpc2")

	simulateRequestsWithLatency(metricsTracker, ups[0], method, 10, 0.040)
	simulateRequestsWithLatency(metricsTracker, ups[1], method, 10, 0.100)

	err := registry.RefreshUpstreamNetworkMethodScores()
	require.NoError(t, err)
	registry.upstreamsMu.RLock()
	s1 := registry.upstreamScores["rpc1"][networkID][method]
	registry.upstreamsMu.RUnlock()
	assert.Greater(t, s1, 0.0, "first score should be > 0 for faster upstream")

	err = registry.RefreshUpstreamNetworkMethodScores()
	require.NoError(t, err)
	registry.upstreamsMu.RLock()
	s2 := registry.upstreamScores["rpc1"][networkID][method]
	registry.upstreamsMu.RUnlock()
	assert.Greater(t, s2, 0.0, "score should remain positive after decay")
}

func TestUpstreamsRegistry_NaNGuardsPreventPropagation(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := log.Logger
	registry, metricsTracker := createTestRegistry(ctx, "test-project", &logger, 5*time.Second)

	method := "eth_getBalance"
	networkID := "evm:123"
	l, _ := registry.GetSortedUpstreams(ctx, networkID, method)
	ups := getUpsByID(l, "rpc1", "rpc2", "rpc3")

	simulateRequestsWithLatency(metricsTracker, ups[0], method, 10, 0.050)
	simulateRequestsWithLatency(metricsTracker, ups[1], method, 10, 0.060)
	simulateRequestsWithLatency(metricsTracker, ups[2], method, 10, 0.070)

	for i := 0; i < 10; i++ {
		err := registry.RefreshUpstreamNetworkMethodScores()
		require.NoError(t, err)

		for upsID, networkScores := range registry.upstreamScores {
			for netID, methodScores := range networkScores {
				for meth, score := range methodScores {
					assert.False(t, math.IsNaN(score),
						"Score for %s/%s/%s should not be NaN (iteration %d)", upsID, netID, meth, i)
					assert.False(t, math.IsInf(score, 0),
						"Score for %s/%s/%s should not be Inf (iteration %d)", upsID, netID, meth, i)
				}
			}
		}

		simulateRequestsWithLatency(metricsTracker, ups[0], method, 5, 0.040+float64(i)*0.001)
		simulateRequestsWithLatency(metricsTracker, ups[1], method, 5, 0.055+float64(i)*0.002)
	}
}

func TestUpstreamsRegistry_PenaltyNaNInjection(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := log.Logger
	registry, metricsTracker := createTestRegistry(ctx, "test-project", &logger, 5*time.Second)

	method := "eth_getBalance"
	networkID := "evm:123"
	l, _ := registry.GetSortedUpstreams(ctx, networkID, method)
	ups := getUpsByID(l, "rpc1", "rpc2", "rpc3")

	simulateRequestsWithLatency(metricsTracker, ups[0], method, 10, 0.050)
	simulateRequestsWithLatency(metricsTracker, ups[1], method, 10, 0.060)
	simulateRequestsWithLatency(metricsTracker, ups[2], method, 10, 0.070)

	err := registry.RefreshUpstreamNetworkMethodScores()
	require.NoError(t, err)

	for upsID := range registry.penaltyState {
		if registry.penaltyState[upsID][networkID] == nil {
			registry.penaltyState[upsID][networkID] = make(map[string]float64)
		}
		registry.penaltyState[upsID][networkID][method] = math.NaN()
	}

	simulateRequestsWithLatency(metricsTracker, ups[0], method, 5, 0.040)
	simulateRequestsWithLatency(metricsTracker, ups[1], method, 5, 0.050)
	simulateRequestsWithLatency(metricsTracker, ups[2], method, 5, 0.060)

	err = registry.RefreshUpstreamNetworkMethodScores()
	require.NoError(t, err)

	for upsID, networkScores := range registry.upstreamScores {
		for netID, methodScores := range networkScores {
			for meth, score := range methodScores {
				assert.False(t, math.IsNaN(score),
					"Score for %s/%s/%s should not be NaN after injection", upsID, netID, meth)
				assert.False(t, math.IsInf(score, 0),
					"Score for %s/%s/%s should not be Inf after injection", upsID, netID, meth)
				assert.GreaterOrEqual(t, score, 0.0,
					"Score for %s/%s/%s should be non-negative", upsID, netID, meth)
			}
		}
	}

	ordered, err := registry.GetSortedUpstreams(ctx, networkID, method)
	require.NoError(t, err)
	assert.Len(t, ordered, 3)
}

func TestUpstreamsRegistry_ZeroLatencyHandling(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx := context.Background()
	logger := log.Logger
	registry, metricsTracker := createTestRegistry(ctx, "test-project", &logger, 10*time.Second)

	method := "eth_call"
	networkID := "evm:123"
	upstreams := registry.GetAllUpstreams()
	require.Len(t, upstreams, 3)

	var workingUpstream, failingUpstream *Upstream
	for _, ups := range upstreams {
		if ups.Id() == "rpc1" {
			workingUpstream = ups
		} else if ups.Id() == "rpc2" {
			failingUpstream = ups
		}
	}
	require.NotNil(t, workingUpstream)
	require.NotNil(t, failingUpstream)

	simulateRequestsWithLatency(metricsTracker, workingUpstream, method, 10, 0.1)
	simulateRequests(metricsTracker, workingUpstream, method, 10, 1)
	simulateRequests(metricsTracker, failingUpstream, method, 10, 10)

	_, err := registry.GetSortedUpstreams(ctx, networkID, method)
	require.NoError(t, err)

	err = registry.RefreshUpstreamNetworkMethodScores()
	require.NoError(t, err)

	sortedUpstreams, err := registry.GetSortedUpstreams(ctx, networkID, method)
	require.NoError(t, err)

	registry.upstreamsMu.RLock()
	workingScore := registry.upstreamScores["rpc1"][networkID][method]
	failingScore := registry.upstreamScores["rpc2"][networkID][method]
	registry.upstreamsMu.RUnlock()

	assert.Greater(t, workingScore, failingScore, "Working upstream should have higher score")

	workingRank := -1
	failingRank := -1
	for i, ups := range sortedUpstreams {
		if ups.Id() == "rpc1" {
			workingRank = i
		} else if ups.Id() == "rpc2" {
			failingRank = i
		}
	}
	assert.Less(t, workingRank, failingRank, "Working upstream should rank higher")
}

func TestUpstreamsRegistry_ScoreHigherIsBetter(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := log.Logger
	registry, metricsTracker := createTestRegistry(ctx, "test-project", &logger, 10*time.Second)

	method := "eth_call"
	networkID := "evm:123"
	l, _ := registry.GetSortedUpstreams(ctx, networkID, method)
	ups := getUpsByID(l, "rpc1", "rpc2", "rpc3")

	simulateRequestsWithLatency(metricsTracker, ups[0], method, 50, 0.020)
	simulateRequestsWithLatency(metricsTracker, ups[1], method, 50, 0.500)
	simulateFailedRequests(metricsTracker, ups[1], method, 30)
	simulateRequestsWithLatency(metricsTracker, ups[2], method, 50, 0.100)

	err := registry.RefreshUpstreamNetworkMethodScores()
	require.NoError(t, err)

	registry.upstreamsMu.RLock()
	s1 := registry.upstreamScores["rpc1"][networkID][method]
	s2 := registry.upstreamScores["rpc2"][networkID][method]
	s3 := registry.upstreamScores["rpc3"][networkID][method]
	registry.upstreamsMu.RUnlock()

	assert.Greater(t, s1, s2, "Best upstream should have highest score (1/(1+penalty))")
	assert.Greater(t, s3, s2, "Medium upstream should have higher score than worst")
	assert.LessOrEqual(t, s1, 1.0, "Score should be <= 1.0")
	assert.Greater(t, s2, 0.0, "Score should be > 0.0")
}

// ---------------------------------------------------------------------------
// Helper functions
// ---------------------------------------------------------------------------

func createTestRegistry(ctx context.Context, projectID string, logger *zerolog.Logger, windowSize time.Duration, scoringCfgs ...*ScoringConfig) (*UpstreamsRegistry, *health.Tracker) {
	metricsTracker := health.NewTracker(logger, projectID, windowSize)
	metricsTracker.Bootstrap(ctx)

	upstreamConfigs := []*common.UpstreamConfig{
		{Id: "rpc1", Endpoint: "http://rpc1.localhost", Type: common.UpstreamTypeEvm, Evm: &common.EvmUpstreamConfig{ChainId: 123}},
		{Id: "rpc2", Endpoint: "http://rpc2.localhost", Type: common.UpstreamTypeEvm, Evm: &common.EvmUpstreamConfig{ChainId: 123}},
		{Id: "rpc3", Endpoint: "http://rpc3.localhost", Type: common.UpstreamTypeEvm, Evm: &common.EvmUpstreamConfig{ChainId: 123}},
	}

	var scoringCfg *ScoringConfig
	if len(scoringCfgs) > 0 && scoringCfgs[0] != nil {
		scoringCfg = scoringCfgs[0]
	} else {
		scoringCfg = &ScoringConfig{
			ScoreGranularity:  "method",
			SwitchHysteresis:  -1,
			MinSwitchInterval: -1,
		}
	}

	vr := thirdparty.NewVendorsRegistry()
	pr, err := thirdparty.NewProvidersRegistry(logger, vr, nil, nil)
	if err != nil {
		panic(err)
	}
	ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: "memory",
			Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"},
		},
	})
	if err != nil {
		panic(err)
	}
	registry := NewUpstreamsRegistry(ctx, logger, projectID, upstreamConfigs, ssr, nil, vr, pr, nil, metricsTracker,
		1*time.Second,
		scoringCfg,
		nil,
	)

	registry.Bootstrap(ctx)
	time.Sleep(100 * time.Millisecond)

	err = registry.PrepareUpstreamsForNetwork(ctx, "evm:123")
	if err != nil {
		panic(err)
	}

	return registry, metricsTracker
}

func simulateRequests(tracker *health.Tracker, upstream common.Upstream, method string, total, errors int) {
	for i := 0; i < total; i++ {
		tracker.RecordUpstreamRequest(upstream, method)
		if i < errors {
			tracker.RecordUpstreamFailure(upstream, method, fmt.Errorf("test problem"))
		}
	}
}

func simulateRequestsWithRateLimiting(tracker *health.Tracker, upstream common.Upstream, method string, total, remoteLimited int) {
	for i := 0; i < total; i++ {
		tracker.RecordUpstreamRequest(upstream, method)
		if i < remoteLimited {
			tracker.RecordUpstreamRemoteRateLimited(context.Background(), upstream, method, nil)
		}
	}
}

func simulateRequestsWithLatency(tracker *health.Tracker, upstream common.Upstream, method string, total int, latency float64) {
	wg := sync.WaitGroup{}
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tracker.RecordUpstreamRequest(upstream, method)
			tracker.RecordUpstreamDuration(upstream, method, time.Duration(latency*float64(time.Second)), true, "none", common.DataFinalityStateUnknown, "n/a")
		}()
	}
	wg.Wait()
}

func simulateFailedRequests(tracker *health.Tracker, upstream common.Upstream, method string, count int) {
	for i := 0; i < count; i++ {
		tracker.RecordUpstreamRequest(upstream, method)
		tracker.RecordUpstreamFailure(upstream, method, fmt.Errorf("test problem"))
	}
}

// ---------------------------------------------------------------------------
// Hedge cancellation must NOT penalize upstream scoring
// ---------------------------------------------------------------------------

func TestUpstreamsRegistry_HedgeCancellationDoesNotDegradeScore(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := log.Logger
	registry, metricsTracker := createTestRegistry(ctx, "test-project", &logger, 10*time.Second)

	method := "trace_block"
	networkID := "evm:123"
	l, _ := registry.GetSortedUpstreams(ctx, networkID, method)
	ups := getUpsByID(l, "rpc1", "rpc2", "rpc3")

	// rpc1 and rpc2 each handle 100 requests successfully
	simulateRequestsWithLatency(metricsTracker, ups[0], method, 100, 0.050)
	simulateRequestsWithLatency(metricsTracker, ups[1], method, 100, 0.050)
	simulateRequestsWithLatency(metricsTracker, ups[2], method, 100, 0.050)

	// Simulate 50 hedge cancellations on rpc2 (rpc2 always lost the hedge race).
	// These should NOT affect rpc2's score because they aren't real failures.
	for i := 0; i < 50; i++ {
		metricsTracker.RecordUpstreamRequest(ups[1], method)
		metricsTracker.RecordUpstreamFailure(ups[1], method,
			common.NewErrEndpointRequestCanceled(fmt.Errorf("context canceled")))
	}

	err := registry.RefreshUpstreamNetworkMethodScores()
	require.NoError(t, err)

	// rpc1 and rpc2 should have equivalent scores because hedge cancellations
	// are ignored by RecordUpstreamFailure. Both have zero real errors and
	// the same latency. The EMA decay applies equally to both so comparing
	// them directly avoids decay-drift issues.
	scoreAfter1 := registry.GetUpstreamScore(ups[0].Id(), networkID, method)
	scoreAfter2 := registry.GetUpstreamScore(ups[1].Id(), networkID, method)

	assert.InDelta(t, scoreAfter1, scoreAfter2, 0.01,
		"rpc2 (with hedge cancellations) should score the same as rpc1 (clean)")

	// Verify ordering hasn't changed — rpc2 should not have been demoted
	ordered, err := registry.GetSortedUpstreams(ctx, networkID, method)
	require.NoError(t, err)

	rpc2Rank := -1
	for i, u := range ordered {
		if u.Id() == "rpc2" {
			rpc2Rank = i
			break
		}
	}
	assert.LessOrEqual(t, rpc2Rank, 2, "rpc2 should still be in the top positions")
}

func TestUpstreamsRegistry_RealErrorsDegradeButHedgeCancellationsDont(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := log.Logger
	registry, metricsTracker := createTestRegistry(ctx, "test-project", &logger, 10*time.Second)

	method := "eth_call"
	networkID := "evm:123"
	l, _ := registry.GetSortedUpstreams(ctx, networkID, method)
	ups := getUpsByID(l, "rpc1", "rpc2", "rpc3")

	// rpc1: clean record
	simulateRequests(metricsTracker, ups[0], method, 100, 0)
	// rpc2: 50 hedge cancellations (should be ignored) + 0 real errors
	simulateRequests(metricsTracker, ups[1], method, 100, 0)
	for i := 0; i < 50; i++ {
		metricsTracker.RecordUpstreamRequest(ups[1], method)
		metricsTracker.RecordUpstreamFailure(ups[1], method,
			common.NewErrEndpointRequestCanceled(fmt.Errorf("context canceled")))
	}
	// rpc3: 30 real errors (should degrade score)
	simulateRequests(metricsTracker, ups[2], method, 100, 30)

	err := registry.RefreshUpstreamNetworkMethodScores()
	require.NoError(t, err)

	s1 := registry.GetUpstreamScore(ups[0].Id(), networkID, method)
	s2 := registry.GetUpstreamScore(ups[1].Id(), networkID, method)
	s3 := registry.GetUpstreamScore(ups[2].Id(), networkID, method)

	// rpc2 (hedge cancellations only) should score similarly to rpc1 (clean)
	assert.InDelta(t, s1, s2, 0.05,
		"upstream with hedge cancellations should score similarly to clean upstream")
	// rpc3 (real errors) should score worse than both
	assert.Greater(t, s1, s3, "clean upstream should score higher than one with real errors")
	assert.Greater(t, s2, s3, "upstream with hedge cancellations should score higher than one with real errors")
}

// ---------------------------------------------------------------------------
// GetUpstreamScoreBreakdown
// ---------------------------------------------------------------------------

func TestUpstreamsRegistry_GetUpstreamScoreBreakdown(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := log.Logger
	registry, metricsTracker := createTestRegistry(ctx, "test-project", &logger, 10*time.Second)

	method := "eth_call"
	networkID := "evm:123"
	l, _ := registry.GetSortedUpstreams(ctx, networkID, method)
	ups := getUpsByID(l, "rpc1", "rpc2")

	// rpc1: some errors and latency
	simulateRequests(metricsTracker, ups[0], method, 100, 20)
	simulateRequestsWithLatency(metricsTracker, ups[0], method, 50, 0.100)
	// rpc2: clean
	simulateRequestsWithLatency(metricsTracker, ups[1], method, 100, 0.020)

	err := registry.RefreshUpstreamNetworkMethodScores()
	require.NoError(t, err)

	bd1 := registry.GetUpstreamScoreBreakdown(ups[0], networkID, method)
	bd2 := registry.GetUpstreamScoreBreakdown(ups[1], networkID, method)

	// Basic sanity: scores should be in (0, 1]
	assert.Greater(t, bd1.Score, 0.0)
	assert.LessOrEqual(t, bd1.Score, 1.0)
	assert.Greater(t, bd2.Score, 0.0)
	assert.LessOrEqual(t, bd2.Score, 1.0)

	// rpc1 has errors so its error rate should be > 0
	assert.Greater(t, bd1.ErrorRate, 0.0, "rpc1 should have nonzero error rate")
	assert.Equal(t, 0.0, bd2.ErrorRate, "rpc2 should have zero error rate")

	// rpc1 has higher latency
	assert.Greater(t, bd1.Latency, bd2.Latency, "rpc1 should have higher latency than rpc2")

	// Penalty should be higher for rpc1 (worse metrics)
	assert.Greater(t, bd1.Penalty, bd2.Penalty, "rpc1 should have higher penalty")

	// Score should be lower for rpc1
	assert.Less(t, bd1.Score, bd2.Score, "rpc1 should have lower score")

	// Neither should be cordoned
	assert.False(t, bd1.Cordoned)
	assert.False(t, bd2.Cordoned)
}
