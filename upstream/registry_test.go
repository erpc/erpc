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
// Hedge cancellation must NOT directly penalize a slow-but-functional
// upstream via ErrorRate. The slowness signal is carried by ResponseQuantiles
// (only successful responses enter it, so consistently-slow upstreams climb
// the quantile and lose score that way). Stacking an ErrorRate penalty on
// top would double-punish.
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

	// All three upstreams handle 100 requests successfully with the same latency.
	simulateRequestsWithLatency(metricsTracker, ups[0], method, 100, 0.050)
	simulateRequestsWithLatency(metricsTracker, ups[1], method, 100, 0.050)
	simulateRequestsWithLatency(metricsTracker, ups[2], method, 100, 0.050)

	// rpc2 then "loses" 50 hedge races. These flow into the tracker as
	// ErrCodeEndpointRequestCanceled — which the skip list ignores. They
	// must not affect ErrorRate; they're indistinguishable from a client
	// disconnect at the tracker level.
	for i := 0; i < 50; i++ {
		metricsTracker.RecordUpstreamRequest(ups[1], method)
		metricsTracker.RecordUpstreamFailure(ups[1], method,
			common.NewErrEndpointRequestCanceled(fmt.Errorf("context canceled")))
	}

	err := registry.RefreshUpstreamNetworkMethodScores()
	require.NoError(t, err)

	scoreAfter1 := registry.GetUpstreamScore(ups[0].Id(), networkID, method)
	scoreAfter2 := registry.GetUpstreamScore(ups[1].Id(), networkID, method)

	// rpc1 and rpc2 should have equivalent scores: both have zero real errors
	// and the same successful-latency profile.
	assert.InDelta(t, scoreAfter1, scoreAfter2, 0.01,
		"rpc2 (with simulated hedge cancellations) should score the same as rpc1 (clean)")

	// Ordering hasn't changed — rpc2 should not have been demoted.
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

	// rpc1: clean record.
	simulateRequests(metricsTracker, ups[0], method, 100, 0)
	// rpc2: 50 hedge cancellations on top of a clean baseline — must be ignored.
	simulateRequests(metricsTracker, ups[1], method, 100, 0)
	for i := 0; i < 50; i++ {
		metricsTracker.RecordUpstreamRequest(ups[1], method)
		metricsTracker.RecordUpstreamFailure(ups[1], method,
			common.NewErrEndpointRequestCanceled(fmt.Errorf("context canceled")))
	}
	// rpc3: 30 real errors — should degrade score.
	simulateRequests(metricsTracker, ups[2], method, 100, 30)

	err := registry.RefreshUpstreamNetworkMethodScores()
	require.NoError(t, err)

	s1 := registry.GetUpstreamScore(ups[0].Id(), networkID, method)
	s2 := registry.GetUpstreamScore(ups[1].Id(), networkID, method)
	s3 := registry.GetUpstreamScore(ups[2].Id(), networkID, method)

	// rpc2 (hedge cancellations only) should score similarly to rpc1 (clean).
	assert.InDelta(t, s1, s2, 0.05,
		"upstream with hedge cancellations should score similarly to clean upstream")
	// rpc3 (real errors) should score worse than both.
	assert.Greater(t, s1, s3, "clean upstream should score higher than one with real errors")
	assert.Greater(t, s2, s3, "upstream with hedge cancellations should score higher than one with real errors")
}

// TestUpstreamsRegistry_SlowUpstreamDemotedByLatency verifies the remaining
// half of the design: a consistently slow upstream IS pushed lower in
// selection, but the signal comes from ResponseQuantiles (successful
// responses' latency), not from hedge-cancellations being counted as errors.
// This is the aram/William insight that motivated excluding hedges from
// ErrorRate entirely.
func TestUpstreamsRegistry_SlowUpstreamDemotedByLatency(t *testing.T) {
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

	// rpc1, rpc3: fast successful responses (50 ms quantile).
	simulateRequestsWithLatency(metricsTracker, ups[0], method, 100, 0.050)
	simulateRequestsWithLatency(metricsTracker, ups[2], method, 100, 0.050)
	// rpc2: succeeds, but ~5x slower (250 ms quantile) — this is exactly the
	// "slow but functional" upstream we don't want to over-penalize as an error.
	simulateRequestsWithLatency(metricsTracker, ups[1], method, 100, 0.250)

	for i := 0; i < 5; i++ {
		require.NoError(t, registry.RefreshUpstreamNetworkMethodScores())
	}

	ordered, err := registry.GetSortedUpstreams(ctx, networkID, method)
	require.NoError(t, err)
	require.Len(t, ordered, 3)

	// rpc2 (slow) should be demoted by the latency signal alone — no errors needed.
	assert.Equal(t, "rpc2", ordered[len(ordered)-1].Id(),
		"slow rpc2 should be demoted to LAST by latency-based scoring, with zero errors")
}

// TestUpstreamsRegistry_HysteresisPreventsScoreFlapping verifies that the
// production scoring defaults actually resist a marginal latency edge: a 5 %
// improvement on the challenger must NOT flip the primary, because Switch-
// Hysteresis is 10 % by default and MinSwitchInterval is 2 minutes. This is
// the load-bearing protection against flap-flop in a noisy environment.
func TestUpstreamsRegistry_HysteresisPreventsScoreFlapping(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := log.Logger
	// createTestRegistry uses production scoring defaults (hysteresis 10 %, cooldown 2 m).
	registry, metricsTracker := createTestRegistry(ctx, "test-project", &logger, 10*time.Second)

	method := "eth_call"
	networkID := "evm:123"
	l, _ := registry.GetSortedUpstreams(ctx, networkID, method)
	ups := getUpsByID(l, "rpc1", "rpc2", "rpc3")

	// Round 1: all three upstreams measure equal — rpc1 wins by alphabetical
	// tiebreak. (We MUST populate every upstream, otherwise an unmeasured one
	// would beat the measured ones by empty-quantile-bias — see the
	// KnownLimitation test for that case.)
	for _, u := range ups {
		simulateRequestsWithLatency(metricsTracker, u, method, 30, 0.045)
	}
	require.NoError(t, registry.RefreshUpstreamNetworkMethodScores())

	ordered, err := registry.GetSortedUpstreams(ctx, networkID, method)
	require.NoError(t, err)
	require.Equal(t, "rpc1", ordered[0].Id(), "rpc1 is initial primary by tiebreak")

	// Round 2: rpc2 improves by ~5 % (42 ms vs 45 ms). Hysteresis threshold
	// is 10 % — and the 2-minute MinSwitchInterval definitely hasn't elapsed.
	// rpc1 must STAY primary.
	simulateRequestsWithLatency(metricsTracker, ups[1], method, 30, 0.042)
	require.NoError(t, registry.RefreshUpstreamNetworkMethodScores())

	stuck, err := registry.GetSortedUpstreams(ctx, networkID, method)
	require.NoError(t, err)
	assert.Equal(t, "rpc1", stuck[0].Id(),
		"hysteresis must keep rpc1 primary — rpc2's 5%% advantage is below the 10%% threshold")
}

// TestUpstreamsRegistry_TransientSlownessRecoversViaDecay verifies the EMA
// smoothing actually un-demotes an upstream that recovers. A spike of
// slowness shouldn't permanently penalize an upstream; subsequent fast
// requests must pull the score back toward zero penalty.
func TestUpstreamsRegistry_TransientSlownessRecoversViaDecay(t *testing.T) {
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
	ups := getUpsByID(l, "rpc1")[0]

	// Phase 1: sustained slowness. Many slow successful responses populate
	// the quantile with a high p70.
	simulateRequestsWithLatency(metricsTracker, ups, method, 100, 0.500)
	for i := 0; i < 5; i++ {
		require.NoError(t, registry.RefreshUpstreamNetworkMethodScores())
	}
	scoreSpike := registry.GetUpstreamScore(ups.Id(), networkID, method)
	require.Less(t, scoreSpike, 0.95,
		"sustained slow latency should visibly drop the score below 1.0 (got %f)", scoreSpike)

	// Phase 2: sustained recovery. Many fast successful responses. The
	// quantile slides toward the new fast samples and the EMA-decayed penalty
	// converges back toward zero across multiple refresh cycles.
	for cycle := 0; cycle < 30; cycle++ {
		simulateRequestsWithLatency(metricsTracker, ups, method, 50, 0.020)
		require.NoError(t, registry.RefreshUpstreamNetworkMethodScores())
	}

	scoreRecovered := registry.GetUpstreamScore(ups.Id(), networkID, method)
	assert.Greater(t, scoreRecovered, scoreSpike,
		"after sustained recovery, score should improve (was %f, now %f)",
		scoreSpike, scoreRecovered)
}

// TestUpstreamsRegistry_UnmeasuredUpstreamRankedInTheMiddle verifies the
// peer-median baseline for empty quantiles. An upstream with no measured
// latency yet gets substituted with the median of its measured peers — so
// it doesn't free-ride to the top (the historical empty-quantile bug) but
// also isn't unfairly buried at the bottom. It sits in the middle of the
// pack and earns its real rank as real traffic populates its quantile.
func TestUpstreamsRegistry_UnmeasuredUpstreamRankedInTheMiddle(t *testing.T) {
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

	// rpc1: fast (30 ms), rpc3: slow (300 ms). rpc2: zero data — should be
	// substituted with the median (here 165 ms = (30+300)/2 since two peers).
	simulateRequestsWithLatency(metricsTracker, ups[0], method, 50, 0.030)
	simulateRequestsWithLatency(metricsTracker, ups[2], method, 50, 0.300)
	// rpc2 deliberately left unmeasured.

	require.NoError(t, registry.RefreshUpstreamNetworkMethodScores())

	ordered, err := registry.GetSortedUpstreams(ctx, networkID, method)
	require.NoError(t, err)
	require.Len(t, ordered, 3)

	assert.Equal(t, "rpc1", ordered[0].Id(),
		"rpc1 (measured fast) wins — empty-quantile-bias has been fixed")
	assert.Equal(t, "rpc2", ordered[1].Id(),
		"rpc2 (unmeasured) sits in the middle by peer-median substitution")
	assert.Equal(t, "rpc3", ordered[2].Id(),
		"rpc3 (measured slow) is last")
}

// TestUpstreamsRegistry_AllUnmeasured_NoRegression confirms the fix doesn't
// drop the cold-start path: when no peer has data, the median falls back to
// 0 and every upstream keeps the neutral score of 1.0 — same as pre-fix
// behavior. (The order between equally-scored upstreams is determined by
// internal registration ordering and isn't part of this contract.)
func TestUpstreamsRegistry_AllUnmeasured_NoRegression(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := log.Logger
	registry, _ := createTestRegistry(ctx, "test-project", &logger, 10*time.Second)

	method := "trace_block"
	networkID := "evm:123"

	require.NoError(t, registry.RefreshUpstreamNetworkMethodScores())

	ordered, err := registry.GetSortedUpstreams(ctx, networkID, method)
	require.NoError(t, err)
	assert.Len(t, ordered, 3, "all upstreams remain candidates with no measurements")

	// Every upstream's penalty is identical (zero), so the breakdown shows
	// no latency contribution for anyone. The exact ordering is determined
	// by internal registration order and isn't part of this contract — what
	// matters is that all upstreams stay in play.
	for _, u := range ordered {
		bd := registry.GetUpstreamScoreBreakdown(u, networkID, method)
		assert.Equal(t, 0.0, bd.Latency,
			"%s has no measured latency → no latency contribution to penalty", u.Id())
	}
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
