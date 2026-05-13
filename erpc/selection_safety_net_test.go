// Selection safety-net tests.
//
// These tests CAPTURE the user-observable behavior of the upstream selection
// mechanism (scoring + selection policy) BEFORE the unification rewrite
// described in `specs/selection-policy/`. They are the regression contract:
// the same scenarios run AFTER the rewrite (via the new engine, with legacy
// YAML routed through the `common/legacy/` translator) MUST produce identical
// outputs.
//
// Design notes:
//   - Each test owns its setup. No `t.Parallel` because gock isn't thread-safe.
//   - Metrics are driven directly via `health.Tracker` (no HTTP traffic); this
//     keeps tests fast and deterministic.
//   - Helpers wrap the two legacy entry points: `GetSortedUpstreams` (scoring)
//     and `PolicyEvaluator.AcquirePermit` (selection policy). After the refactor
//     the helpers will be the ONLY place that flips to the new engine API; the
//     test bodies remain unchanged.
//   - We assert ordered upstream-ID slices. Stable strings are easy to diff
//     against captured baselines.
//
// To capture baseline: `go test ./erpc/ -run TestSafetyNet -count=1 -v`
//
// IMPORTANT: These tests reference symbols (`PolicyEvaluator`, `ScoreMultipliers`,
// `ResampleExcluded`, etc.) that Phase 1 deletes. They are intentionally tied
// to today's API. After Phase 1 the file will not compile until Phase 10 adds
// a replacement that exercises the same scenarios through the new engine.
// Capture goldens FIRST, then proceed.

package erpc

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/thirdparty"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─── harness ────────────────────────────────────────────────────────────────

// safetyNetFixture is the test rig: one Network, a tracker for metric injection,
// the registry, and the three upstreams pointed to by ID.
type safetyNetFixture struct {
	ntw       *Network
	tracker   *health.Tracker
	registry  *upstream.UpstreamsRegistry
	upstreams map[string]*upstream.Upstream
	logger    *zerolog.Logger
}

// safetyNetSetup builds a Network with the given upstream configs and project
// settings. Pass nil for projectCfg to use defaults; pass nil for selectionCfg
// to use the auto-default-policy behavior (kicks in when a "fallback" group
// upstream is present).
func safetyNetSetup(
	t *testing.T,
	ctx context.Context,
	projectCfg *common.ProjectConfig,
	upstreamCfgs []*common.UpstreamConfig,
	selectionCfg *common.SelectionPolicyConfig,
) *safetyNetFixture {
	t.Helper()

	util.ResetGock()
	t.Cleanup(util.ResetGock)
	util.SetupMocksForEvmStatePoller()
	t.Cleanup(func() { util.AssertNoPendingMocks(t, 0) })

	if projectCfg == nil {
		projectCfg = &common.ProjectConfig{Id: "prjA"}
	}
	require.NoError(t, projectCfg.SetDefaults(nil))

	logger := log.With().Str("test", t.Name()).Logger()

	rlr, err := upstream.NewRateLimitersRegistry(ctx, &common.RateLimiterConfig{
		Budgets: []*common.RateLimitBudgetConfig{},
	}, &logger)
	require.NoError(t, err)

	vr := thirdparty.NewVendorsRegistry()
	pr, err := thirdparty.NewProvidersRegistry(&logger, vr, []*common.ProviderConfig{}, nil)
	require.NoError(t, err)

	mt := health.NewTracker(&logger, projectCfg.Id, projectCfg.ScoreMetricsWindowSize.Duration())

	for _, u := range upstreamCfgs {
		require.NoError(t, u.SetDefaults(nil))
	}

	ssr, err := data.NewSharedStateRegistry(ctx, &logger, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: "memory",
			Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"},
		},
	})
	require.NoError(t, err)

	refreshInterval := projectCfg.ScoreRefreshInterval.Duration()
	if refreshInterval == 0 {
		refreshInterval = 50 * time.Millisecond
	}

	// Mirror the production wiring from `projects_registry.go` so the same
	// project-level scoring knobs (RoutingStrategy, ScoreGranularity,
	// ScorePenaltyDecayRate, ScoreSwitchHysteresis, ScoreMinSwitchInterval)
	// take effect under test.
	scoringCfg := &upstream.ScoringConfig{
		RoutingStrategy:   projectCfg.RoutingStrategy,
		ScoreGranularity:  projectCfg.ScoreGranularity,
		PenaltyDecayRate:  projectCfg.ScorePenaltyDecayRate,
		SwitchHysteresis:  projectCfg.ScoreSwitchHysteresis,
		MinSwitchInterval: projectCfg.ScoreMinSwitchInterval.Duration(),
	}

	upr := upstream.NewUpstreamsRegistry(
		ctx, &logger, projectCfg.Id, upstreamCfgs, ssr, rlr, vr, pr, nil, mt,
		refreshInterval,
		scoringCfg, nil,
	)
	upr.Bootstrap(ctx)
	time.Sleep(100 * time.Millisecond)

	networkId := util.EvmNetworkId(123)
	require.NoError(t, upr.PrepareUpstreamsForNetwork(ctx, networkId))

	netCfg := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
	}
	if selectionCfg != nil {
		netCfg.SelectionPolicy = selectionCfg
	}
	require.NoError(t, netCfg.SetDefaults(upstreamCfgs, nil))

	ntw, err := NewNetwork(ctx, &logger, projectCfg.Id, netCfg, rlr, upr, mt)
	require.NoError(t, err)

	ups := make(map[string]*upstream.Upstream)
	for _, u := range upr.GetNetworkUpstreams(ctx, networkId) {
		ups[u.Id()] = u
	}

	// Force-create the (upstream, "*") tracking entries so subsequent metric
	// injections (notably `SetLatestBlockNumber`, which only updates
	// pre-existing TrackedMetrics) actually propagate to the policy-visible
	// `metrics.blockHeadLag`.
	for _, u := range ups {
		_ = mt.GetUpstreamMethodMetrics(u, "*")
	}

	return &safetyNetFixture{
		ntw:       ntw,
		tracker:   mt,
		registry:  upr,
		upstreams: ups,
		logger:    &logger,
	}
}

// upstreamCfg creates a minimal evm upstream config for chainId 123.
func upstreamCfg(id, group string, multipliers []*common.ScoreMultiplierConfig) *common.UpstreamConfig {
	cfg := &common.UpstreamConfig{
		Type:     common.UpstreamTypeEvm,
		Id:       id,
		Group:    group,
		Endpoint: fmt.Sprintf("http://%s.localhost", id),
		Evm:      &common.EvmUpstreamConfig{ChainId: 123},
		JsonRpc:  &common.JsonRpcUpstreamConfig{SupportsBatch: &common.FALSE},
	}
	if len(multipliers) > 0 {
		cfg.Routing = &common.RoutingConfig{ScoreMultipliers: multipliers}
	}
	return cfg
}

// scoreMul is a builder for ScoreMultiplierConfig.
//
// The legacy scoring config treats a NIL pointer as "use the default" but a
// 0-valued pointer as "this dimension is disabled". We default the `overall`
// multiplier to 1.0 so that any per-dimension weight set via the builder
// actually contributes to the final score. Tests opt in to disabling
// dimensions by passing `0` explicitly.
type scoreMul struct {
	network         string
	method          string
	overall         *float64 // optional; defaults to 1.0
	errorRate       float64
	respLatency     float64
	totalRequests   float64
	blockHeadLag    float64
	finalizationLag float64
	throttledRate   float64
	misbehaviors    float64
}

func (s scoreMul) toConfig() *common.ScoreMultiplierConfig {
	overall := 1.0
	if s.overall != nil {
		overall = *s.overall
	}
	return &common.ScoreMultiplierConfig{
		Network:         orStar(s.network),
		Method:          orStar(s.method),
		Overall:         util.Float64Ptr(overall),
		ErrorRate:       util.Float64Ptr(s.errorRate),
		RespLatency:     util.Float64Ptr(s.respLatency),
		TotalRequests:   util.Float64Ptr(s.totalRequests),
		BlockHeadLag:    util.Float64Ptr(s.blockHeadLag),
		FinalizationLag: util.Float64Ptr(s.finalizationLag),
		ThrottledRate:   util.Float64Ptr(s.throttledRate),
		Misbehaviors:    util.Float64Ptr(s.misbehaviors),
	}
}

func orStar(s string) string {
	if s == "" {
		return "*"
	}
	return s
}

// driveErrorBurst injects N (request, failure) pairs for the given upstream+method.
// This pushes errorRate up; combined with at least one success it produces a
// realistic distribution.
func driveErrorBurst(t *testing.T, mt *health.Tracker, ups *upstream.Upstream, method string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		mt.RecordUpstreamRequest(ups, method)
		mt.RecordUpstreamFailure(ups, method, fmt.Errorf("synthetic failure"))
	}
}

// driveSuccessBurst injects N (request, duration-success) pairs.
func driveSuccessBurst(t *testing.T, mt *health.Tracker, ups *upstream.Upstream, method string, n int, d time.Duration) {
	t.Helper()
	for i := 0; i < n; i++ {
		mt.RecordUpstreamRequest(ups, method)
		mt.RecordUpstreamDuration(ups, method, d, true, "none", common.DataFinalityStateUnknown, "n/a")
	}
}

// scoredOrder returns the upstream IDs sorted by the legacy scoring mechanism
// for (network, method).
//
// The legacy registry's score refresh ONLY processes (network, method) pairs
// that have already been registered via a prior `GetSortedUpstreams` call.
// We therefore pre-warm the entry, refresh, and read out the sorted result.
//
// This helper is the ONLY swap point at refactor time:
//   - today:    registry.GetSortedUpstreams (pre-warm) + RefreshUpstreamNetworkMethodScores + GetSortedUpstreams
//   - tomorrow: engine.GetOrdered(ctx, networkId, method)
//
// Test bodies call scoredOrder; they don't care which implementation runs.
func scoredOrder(t *testing.T, fx *safetyNetFixture, networkId, method string) []string {
	t.Helper()
	ctx := context.Background()
	_, _ = fx.registry.GetSortedUpstreams(ctx, networkId, method) // pre-warm
	require.NoError(t, fx.registry.RefreshUpstreamNetworkMethodScores())
	sorted, err := fx.registry.GetSortedUpstreams(ctx, networkId, method)
	require.NoError(t, err)
	ids := make([]string, 0, len(sorted))
	for _, u := range sorted {
		ids = append(ids, u.Id())
	}
	return ids
}

// eligibleByPolicy returns IDs of upstreams ALLOWED to serve (method) per the
// supplied PolicyEvaluator (which has been Started). Order is registry sort
// order; only the subset that passes `AcquirePermit` is returned.
//
// Like `scoredOrder`, this is the second swap point at refactor time:
//   - today:    evaluator.AcquirePermit per upstream
//   - tomorrow: engine.GetOrdered (already-filtered list)
func eligibleByPolicy(t *testing.T, fx *safetyNetFixture, ev *PolicyEvaluator, networkId, method string) []string {
	t.Helper()
	sorted, err := fx.registry.GetSortedUpstreams(context.Background(), networkId, method)
	require.NoError(t, err)
	allowed := make([]string, 0, len(sorted))
	for _, u := range sorted {
		if err := ev.AcquirePermit(fx.logger, u.(*upstream.Upstream), method); err == nil {
			allowed = append(allowed, u.Id())
		}
	}
	sort.Strings(allowed) // policy doesn't order; we compare sets
	return allowed
}

// startDefaultPolicyEvaluator creates an evaluator with the default policy +
// fast eval interval, starts it, and returns it. Caller drives metrics and
// then waits a couple of intervals before asserting.
func startDefaultPolicyEvaluator(t *testing.T, ctx context.Context, fx *safetyNetFixture) *PolicyEvaluator {
	t.Helper()
	evalFn, err := common.CompileFunction(common.DefaultPolicyFunction)
	require.NoError(t, err)
	cfg := &common.SelectionPolicyConfig{
		EvalInterval:  common.Duration(20 * time.Millisecond),
		EvalPerMethod: false,
		EvalFunction:  evalFn,
	}
	ev, err := NewPolicyEvaluator(util.EvmNetworkId(123), fx.logger, cfg, fx.registry, fx.tracker)
	require.NoError(t, err)
	require.NoError(t, ev.Start(ctx))
	return ev
}

// waitForEvalSettled blocks for enough ticks of the evaluator to consume
// freshly-driven metrics. Tests pin EvalInterval to 20ms; waiting 4 intervals
// is enough headroom even on a loaded CI runner.
func waitForEvalSettled() { time.Sleep(120 * time.Millisecond) }

// setBlockHeadLag directly stores `lag` on the (upstream, "*") TrackedMetrics
// entry. We use direct injection rather than `tracker.SetLatestBlockNumber`
// because the latter only propagates to entries that pre-exist in the
// per-network index AND whose own block number has been seeded via a prior
// `SetLatestBlockNumber` call — a chicken-and-egg dance that makes "set the
// lag and observe the policy" harder than it should be. For safety-net tests
// we only care that the policy SEES the right metric value.
func setBlockHeadLag(t *testing.T, mt *health.Tracker, ups *upstream.Upstream, lag int64) {
	t.Helper()
	m := mt.GetUpstreamMethodMetrics(ups, "*")
	m.BlockHeadLag.Store(lag)
}

// ─── tests: default policy ──────────────────────────────────────────────────

// TestSafetyNet_DefaultPolicy_NotAttachedWithoutFallbackGroup pins:
// the default policy is auto-attached to a NetworkConfig ONLY when at least
// one upstream has `group: fallback`. With no fallback group, no policy is
// active (Network.SelectionPolicy is nil) and ALL upstreams remain eligible.
func TestSafetyNet_DefaultPolicy_NotAttachedWithoutFallbackGroup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fx := safetyNetSetup(t, ctx, nil, []*common.UpstreamConfig{
		upstreamCfg("rpc1", "main", nil),
		upstreamCfg("rpc2", "main", nil),
	}, nil)

	// The network config built by NewDefaultNetworkConfig (called via
	// project.SetDefaults) would normally attach the policy; here we drove
	// the path through safetyNetSetup which calls NetworkConfig.SetDefaults
	// directly. Without a fallback group, SelectionPolicy stays nil.
	assert.Nil(t, fx.ntw.cfg.SelectionPolicy,
		"no fallback group → no default selection policy attached")
}

// TestSafetyNet_DefaultPolicy_FiltersByErrorRateAboveThreshold pins:
// when ROUTING_POLICY_MAX_ERROR_RATE is at its default 0.7, an upstream whose
// errorRate exceeds 0.7 is excluded; the rest are eligible.
func TestSafetyNet_DefaultPolicy_FiltersByErrorRateAboveThreshold(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fx := safetyNetSetup(t, ctx, nil, []*common.UpstreamConfig{
		upstreamCfg("rpc1", "main", nil),
		upstreamCfg("rpc2", "main", nil),
		upstreamCfg("rpc3", "fallback", nil),
	}, nil)
	ev := startDefaultPolicyEvaluator(t, ctx, fx)

	// rpc1: 100 requests, 90 failures → errorRate = 0.9 > 0.7 → excluded.
	driveErrorBurst(t, fx.tracker, fx.upstreams["rpc1"], "*", 90)
	driveSuccessBurst(t, fx.tracker, fx.upstreams["rpc1"], "*", 10, 10*time.Millisecond)
	// rpc2: 100 successes → errorRate = 0 → eligible.
	driveSuccessBurst(t, fx.tracker, fx.upstreams["rpc2"], "*", 100, 10*time.Millisecond)

	waitForEvalSettled()
	allowed := eligibleByPolicy(t, fx, ev, util.EvmNetworkId(123), "*")
	assert.Equal(t, []string{"rpc2"}, allowed,
		"rpc1 excluded for high error rate; default policy returns only healthy defaults (rpc3 is fallback)")
}

// TestSafetyNet_DefaultPolicy_FiltersByBlockHeadLagAboveThreshold pins:
// ROUTING_POLICY_MAX_BLOCK_HEAD_LAG default = 10; upstream lagging > 10 blocks
// is excluded.
func TestSafetyNet_DefaultPolicy_FiltersByBlockHeadLagAboveThreshold(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fx := safetyNetSetup(t, ctx, nil, []*common.UpstreamConfig{
		upstreamCfg("rpc1", "main", nil),
		upstreamCfg("rpc2", "main", nil),
		upstreamCfg("rpc3", "fallback", nil),
	}, nil)
	ev := startDefaultPolicyEvaluator(t, ctx, fx)

	// Inject metrics directly: rpc1 in-sync, rpc2 lagging 20 blocks (> default 10).
	setBlockHeadLag(t, fx.tracker, fx.upstreams["rpc1"], 0)
	setBlockHeadLag(t, fx.tracker, fx.upstreams["rpc2"], 20)
	setBlockHeadLag(t, fx.tracker, fx.upstreams["rpc3"], 0)

	waitForEvalSettled()
	allowed := eligibleByPolicy(t, fx, ev, util.EvmNetworkId(123), "*")
	assert.Equal(t, []string{"rpc1"}, allowed,
		"rpc2 excluded for high block-head lag")
}

// TestSafetyNet_DefaultPolicy_PromotesFallbackWhenDefaultsUnhealthy pins:
// when fewer than minHealthyThreshold defaults are healthy AND fallback group
// has at least one healthy member, the fallback group becomes the eligible set.
func TestSafetyNet_DefaultPolicy_PromotesFallbackWhenDefaultsUnhealthy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fx := safetyNetSetup(t, ctx, nil, []*common.UpstreamConfig{
		upstreamCfg("rpc1", "main", nil),
		upstreamCfg("rpc2", "main", nil),
		upstreamCfg("rpc3", "fallback", nil),
	}, nil)
	ev := startDefaultPolicyEvaluator(t, ctx, fx)

	// Both defaults break.
	driveErrorBurst(t, fx.tracker, fx.upstreams["rpc1"], "*", 90)
	driveSuccessBurst(t, fx.tracker, fx.upstreams["rpc1"], "*", 10, 10*time.Millisecond)
	driveErrorBurst(t, fx.tracker, fx.upstreams["rpc2"], "*", 90)
	driveSuccessBurst(t, fx.tracker, fx.upstreams["rpc2"], "*", 10, 10*time.Millisecond)
	// Fallback is healthy.
	driveSuccessBurst(t, fx.tracker, fx.upstreams["rpc3"], "*", 100, 10*time.Millisecond)

	waitForEvalSettled()
	allowed := eligibleByPolicy(t, fx, ev, util.EvmNetworkId(123), "*")
	assert.Equal(t, []string{"rpc3"}, allowed,
		"only fallback upstream is eligible when both defaults are unhealthy")
}

// TestSafetyNet_DefaultPolicy_ReturnsAllWhenNoneHealthy pins the last-resort
// behavior: when nothing meets thresholds (defaults AND fallback), the policy
// returns ALL upstreams to keep the network reachable.
func TestSafetyNet_DefaultPolicy_ReturnsAllWhenNoneHealthy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fx := safetyNetSetup(t, ctx, nil, []*common.UpstreamConfig{
		upstreamCfg("rpc1", "main", nil),
		upstreamCfg("rpc2", "main", nil),
		upstreamCfg("rpc3", "fallback", nil),
	}, nil)
	ev := startDefaultPolicyEvaluator(t, ctx, fx)

	for id := range fx.upstreams {
		driveErrorBurst(t, fx.tracker, fx.upstreams[id], "*", 90)
		driveSuccessBurst(t, fx.tracker, fx.upstreams[id], "*", 10, 10*time.Millisecond)
	}

	waitForEvalSettled()
	allowed := eligibleByPolicy(t, fx, ev, util.EvmNetworkId(123), "*")
	assert.ElementsMatch(t, []string{"rpc1", "rpc2", "rpc3"}, allowed,
		"all upstreams returned as last resort when none meet thresholds")
}

// ─── tests: ROUTING_POLICY_* env vars ───────────────────────────────────────

// TestSafetyNet_RoutingPolicyEnv_TightensMaxErrorRate pins: setting
// ROUTING_POLICY_MAX_ERROR_RATE=0.2 lowers the threshold so a 0.3-error-rate
// upstream is excluded that would otherwise be eligible at the default 0.7.
func TestSafetyNet_RoutingPolicyEnv_TightensMaxErrorRate(t *testing.T) {
	t.Setenv("ROUTING_POLICY_MAX_ERROR_RATE", "0.2")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fx := safetyNetSetup(t, ctx, nil, []*common.UpstreamConfig{
		upstreamCfg("rpc1", "main", nil),
		upstreamCfg("rpc2", "main", nil),
		upstreamCfg("rpc3", "fallback", nil),
	}, nil)
	ev := startDefaultPolicyEvaluator(t, ctx, fx)

	// rpc1: 30% error rate (would pass at 0.7, fails at 0.2)
	driveErrorBurst(t, fx.tracker, fx.upstreams["rpc1"], "*", 30)
	driveSuccessBurst(t, fx.tracker, fx.upstreams["rpc1"], "*", 70, 10*time.Millisecond)
	driveSuccessBurst(t, fx.tracker, fx.upstreams["rpc2"], "*", 100, 10*time.Millisecond)

	waitForEvalSettled()
	allowed := eligibleByPolicy(t, fx, ev, util.EvmNetworkId(123), "*")
	assert.Equal(t, []string{"rpc2"}, allowed,
		"rpc1 excluded because 0.3 > tightened 0.2 threshold")
}

// TestSafetyNet_RoutingPolicyEnv_TightensMaxBlockHeadLag pins: setting
// ROUTING_POLICY_MAX_BLOCK_HEAD_LAG=3 lowers the lag tolerance so a 5-block
// lag upstream is excluded.
func TestSafetyNet_RoutingPolicyEnv_TightensMaxBlockHeadLag(t *testing.T) {
	t.Setenv("ROUTING_POLICY_MAX_BLOCK_HEAD_LAG", "3")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fx := safetyNetSetup(t, ctx, nil, []*common.UpstreamConfig{
		upstreamCfg("rpc1", "main", nil),
		upstreamCfg("rpc2", "main", nil),
		upstreamCfg("rpc3", "fallback", nil),
	}, nil)
	ev := startDefaultPolicyEvaluator(t, ctx, fx)

	setBlockHeadLag(t, fx.tracker, fx.upstreams["rpc1"], 0)
	setBlockHeadLag(t, fx.tracker, fx.upstreams["rpc2"], 5) // > tightened 3
	setBlockHeadLag(t, fx.tracker, fx.upstreams["rpc3"], 0)

	waitForEvalSettled()
	allowed := eligibleByPolicy(t, fx, ev, util.EvmNetworkId(123), "*")
	assert.Equal(t, []string{"rpc1"}, allowed,
		"rpc2 excluded because 5 > tightened 3-block tolerance")
}

// TestSafetyNet_RoutingPolicyEnv_RaisesMinHealthyThreshold pins: with
// ROUTING_POLICY_MIN_HEALTHY_THRESHOLD=2, having only ONE healthy default
// triggers the fallback-group promotion path even though one default is
// healthy. (Default threshold is 1, so a single healthy default would
// normally suffice.)
func TestSafetyNet_RoutingPolicyEnv_RaisesMinHealthyThreshold(t *testing.T) {
	t.Setenv("ROUTING_POLICY_MIN_HEALTHY_THRESHOLD", "2")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fx := safetyNetSetup(t, ctx, nil, []*common.UpstreamConfig{
		upstreamCfg("rpc1", "main", nil),
		upstreamCfg("rpc2", "main", nil),
		upstreamCfg("rpc3", "fallback", nil),
	}, nil)
	ev := startDefaultPolicyEvaluator(t, ctx, fx)

	// rpc1 healthy, rpc2 broken → only 1 healthy default; need 2 → fallback fires.
	driveSuccessBurst(t, fx.tracker, fx.upstreams["rpc1"], "*", 100, 10*time.Millisecond)
	driveErrorBurst(t, fx.tracker, fx.upstreams["rpc2"], "*", 90)
	driveSuccessBurst(t, fx.tracker, fx.upstreams["rpc2"], "*", 10, 10*time.Millisecond)
	driveSuccessBurst(t, fx.tracker, fx.upstreams["rpc3"], "*", 100, 10*time.Millisecond)

	waitForEvalSettled()
	allowed := eligibleByPolicy(t, fx, ev, util.EvmNetworkId(123), "*")
	assert.Equal(t, []string{"rpc3"}, allowed,
		"fallback promoted because only 1 of 2 required defaults is healthy")
}

// ─── tests: scoring (single-dimension) ─────────────────────────────────────

// TestSafetyNet_ScoreBased_HighErrorRateMovesToBack pins: when upstreams have
// equal weights but different error rates, the higher-error-rate upstream is
// sorted toward the back.
func TestSafetyNet_ScoreBased_HighErrorRateMovesToBack(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Weight only error rate; zero out other dimensions to isolate.
	muls := []*common.ScoreMultiplierConfig{
		scoreMul{errorRate: 8}.toConfig(),
	}
	fx := safetyNetSetup(t, ctx, nil, []*common.UpstreamConfig{
		upstreamCfg("rpc1", "main", muls),
		upstreamCfg("rpc2", "main", muls),
	}, nil)

	driveErrorBurst(t, fx.tracker, fx.upstreams["rpc1"], "eth_call", 80)
	driveSuccessBurst(t, fx.tracker, fx.upstreams["rpc1"], "eth_call", 20, 10*time.Millisecond)
	driveSuccessBurst(t, fx.tracker, fx.upstreams["rpc2"], "eth_call", 100, 10*time.Millisecond)

	order := scoredOrder(t, fx, util.EvmNetworkId(123), "eth_call")
	assert.Equal(t, []string{"rpc2", "rpc1"}, order)
}

// TestSafetyNet_ScoreBased_HighLatencyMovesToBack pins: respLatency multiplier
// pushes high-latency upstream behind low-latency.
func TestSafetyNet_ScoreBased_HighLatencyMovesToBack(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	muls := []*common.ScoreMultiplierConfig{
		scoreMul{respLatency: 8}.toConfig(),
	}
	fx := safetyNetSetup(t, ctx, nil, []*common.UpstreamConfig{
		upstreamCfg("rpc1", "main", muls),
		upstreamCfg("rpc2", "main", muls),
	}, nil)

	driveSuccessBurst(t, fx.tracker, fx.upstreams["rpc1"], "eth_call", 100, 500*time.Millisecond)
	driveSuccessBurst(t, fx.tracker, fx.upstreams["rpc2"], "eth_call", 100, 10*time.Millisecond)

	order := scoredOrder(t, fx, util.EvmNetworkId(123), "eth_call")
	assert.Equal(t, []string{"rpc2", "rpc1"}, order)
}

// TestSafetyNet_ScoreBased_HighBlockHeadLagMovesToBack pins: blockHeadLag
// multiplier orders lagging upstream behind in-sync upstream.
func TestSafetyNet_ScoreBased_HighBlockHeadLagMovesToBack(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	muls := []*common.ScoreMultiplierConfig{
		scoreMul{blockHeadLag: 8}.toConfig(),
	}
	fx := safetyNetSetup(t, ctx, nil, []*common.UpstreamConfig{
		upstreamCfg("rpc1", "main", muls),
		upstreamCfg("rpc2", "main", muls),
	}, nil)

	// Give both upstreams equal traffic so neutral baseline holds; differ on head lag.
	driveSuccessBurst(t, fx.tracker, fx.upstreams["rpc1"], "eth_call", 100, 10*time.Millisecond)
	driveSuccessBurst(t, fx.tracker, fx.upstreams["rpc2"], "eth_call", 100, 10*time.Millisecond)
	// Inject lag on both per-method AND aggregate "*" entries since the
	// legacy scoring reads from method "*" under the default `upstream`
	// granularity.
	setBlockHeadLag(t, fx.tracker, fx.upstreams["rpc1"], 0)
	setBlockHeadLag(t, fx.tracker, fx.upstreams["rpc2"], 50)

	order := scoredOrder(t, fx, util.EvmNetworkId(123), "eth_call")
	assert.Equal(t, []string{"rpc1", "rpc2"}, order)
}

// ─── tests: score multipliers (per-method) ─────────────────────────────────

// TestSafetyNet_ScoreMultiplier_PerMethodReweights pins: a method-specific
// multiplier overrides the wildcard multiplier — error-rate weight applies
// only for the named method; another method keeps the wildcard weight.
//
// Requires `scoreGranularity: method` so per-method weights are evaluated;
// the default `upstream` granularity collapses all methods into a single
// score per upstream.
func TestSafetyNet_ScoreMultiplier_PerMethodReweights(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	projectCfg := &common.ProjectConfig{
		Id:               "prjA",
		RoutingStrategy:  "score-based",
		ScoreGranularity: "method",
	}

	// Wildcard: zero out everything (no preference).
	// Per-method (eth_getTransactionReceipt): heavy error-rate weight.
	muls := []*common.ScoreMultiplierConfig{
		scoreMul{method: "eth_getTransactionReceipt", errorRate: 8}.toConfig(),
		scoreMul{}.toConfig(), // neutral wildcard
	}

	fx := safetyNetSetup(t, ctx, projectCfg, []*common.UpstreamConfig{
		upstreamCfg("rpc1", "main", muls),
		upstreamCfg("rpc2", "main", muls),
	}, nil)

	// rpc1 has errors only for eth_getTransactionReceipt.
	driveErrorBurst(t, fx.tracker, fx.upstreams["rpc1"], "eth_getTransactionReceipt", 80)
	driveSuccessBurst(t, fx.tracker, fx.upstreams["rpc1"], "eth_getTransactionReceipt", 20, 10*time.Millisecond)
	driveSuccessBurst(t, fx.tracker, fx.upstreams["rpc2"], "eth_getTransactionReceipt", 100, 10*time.Millisecond)
	// Both equal for eth_call.
	driveSuccessBurst(t, fx.tracker, fx.upstreams["rpc1"], "eth_call", 100, 10*time.Millisecond)
	driveSuccessBurst(t, fx.tracker, fx.upstreams["rpc2"], "eth_call", 100, 10*time.Millisecond)

	receiptOrder := scoredOrder(t, fx, util.EvmNetworkId(123), "eth_getTransactionReceipt")
	assert.Equal(t, []string{"rpc2", "rpc1"}, receiptOrder,
		"per-method weight applies: rpc1 deprioritized for eth_getTransactionReceipt")

	callOrder := scoredOrder(t, fx, util.EvmNetworkId(123), "eth_call")
	// Neutral weight + equal metrics → either order acceptable but stable.
	assert.ElementsMatch(t, []string{"rpc1", "rpc2"}, callOrder,
		"per-method weight does not bleed into eth_call ordering")
}

// ─── tests: sticky primary ─────────────────────────────────────────────────

// TestSafetyNet_StickyPrimary_HysteresisPreventsFlip pins: with
// `scoreSwitchHysteresis: 0.3`, the primary keeps its spot unless the
// challenger's penalty drops below `primary_penalty * (1 - 0.3)`.
//
// We give BOTH upstreams similar (non-zero) error rates so the challenger's
// penalty stays within the hysteresis band relative to the primary's.
func TestSafetyNet_StickyPrimary_HysteresisPreventsFlip(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	projectCfg := &common.ProjectConfig{
		Id:                     "prjA",
		RoutingStrategy:        "score-based",
		ScoreSwitchHysteresis:  0.3, // requires challenger < primary*0.7
		ScoreMinSwitchInterval: common.Duration(0),
	}

	muls := []*common.ScoreMultiplierConfig{
		scoreMul{errorRate: 8}.toConfig(),
	}
	fx := safetyNetSetup(t, ctx, projectCfg, []*common.UpstreamConfig{
		upstreamCfg("rpc1", "main", muls),
		upstreamCfg("rpc2", "main", muls),
	}, nil)

	// Phase 1: rpc1 worse than rpc2 → rpc1 is the lagging primary candidate;
	// the sticky tie-break sorts the WORSE upstream as primary only if it's
	// already locked in. To set up the test, give rpc1 a head start as
	// primary by running an initial refresh with rpc1 better.
	driveSuccessBurst(t, fx.tracker, fx.upstreams["rpc1"], "eth_call", 100, 10*time.Millisecond)
	driveErrorBurst(t, fx.tracker, fx.upstreams["rpc2"], "eth_call", 10)
	driveSuccessBurst(t, fx.tracker, fx.upstreams["rpc2"], "eth_call", 90, 10*time.Millisecond)
	first := scoredOrder(t, fx, util.EvmNetworkId(123), "eth_call")
	require.Equal(t, "rpc1", first[0], "rpc1 must be initial primary for test premise")

	// Phase 2: introduce a small gap with rpc1 SLIGHTLY worse than rpc2,
	// but well within the 30 % hysteresis band relative to existing penalties.
	// rpc1 errorRate ≈ 0.05 → penalty ≈ 0.4 (instant)
	// rpc2 errorRate ≈ 0.10 → penalty stays from Phase 1
	// After decay, the gap stays small enough that challenger >= primary*0.7.
	driveErrorBurst(t, fx.tracker, fx.upstreams["rpc1"], "eth_call", 5)
	driveSuccessBurst(t, fx.tracker, fx.upstreams["rpc1"], "eth_call", 95, 10*time.Millisecond)
	driveSuccessBurst(t, fx.tracker, fx.upstreams["rpc2"], "eth_call", 100, 10*time.Millisecond)

	order := scoredOrder(t, fx, util.EvmNetworkId(123), "eth_call")
	assert.Equal(t, "rpc1", order[0],
		"hysteresis suppresses flip when challenger's gap stays within band")
}

// TestSafetyNet_StickyPrimary_MinSwitchIntervalDelaysFlip pins: with
// `scoreMinSwitchInterval: 5s`, a clearly-superior runner-up does NOT take
// over until the interval has elapsed since the last switch.
func TestSafetyNet_StickyPrimary_MinSwitchIntervalDelaysFlip(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	projectCfg := &common.ProjectConfig{
		Id:                     "prjA",
		RoutingStrategy:        "score-based",
		ScoreSwitchHysteresis:  0.0,
		ScoreMinSwitchInterval: common.Duration(5 * time.Second),
	}

	muls := []*common.ScoreMultiplierConfig{
		scoreMul{errorRate: 8}.toConfig(),
	}
	fx := safetyNetSetup(t, ctx, projectCfg, []*common.UpstreamConfig{
		upstreamCfg("rpc1", "main", muls),
		upstreamCfg("rpc2", "main", muls),
	}, nil)

	// Establish rpc1 as primary by giving it a head start.
	driveSuccessBurst(t, fx.tracker, fx.upstreams["rpc1"], "eth_call", 100, 10*time.Millisecond)
	driveErrorBurst(t, fx.tracker, fx.upstreams["rpc2"], "eth_call", 50)
	driveSuccessBurst(t, fx.tracker, fx.upstreams["rpc2"], "eth_call", 50, 10*time.Millisecond)
	first := scoredOrder(t, fx, util.EvmNetworkId(123), "eth_call")
	require.Equal(t, "rpc1", first[0])

	// Now flip: rpc1 errors, rpc2 healthy. Without min-switch-interval, rpc2
	// would win; with 5s lockout, rpc1 retains primary.
	driveErrorBurst(t, fx.tracker, fx.upstreams["rpc1"], "eth_call", 90)
	driveSuccessBurst(t, fx.tracker, fx.upstreams["rpc2"], "eth_call", 100, 10*time.Millisecond)

	order := scoredOrder(t, fx, util.EvmNetworkId(123), "eth_call")
	assert.Equal(t, "rpc1", order[0],
		"min-switch-interval 5s suppresses flip even with clear score gap")
}

// ─── tests: routing strategy round-robin ────────────────────────────────────

// TestSafetyNet_RoutingStrategy_RoundRobin_Rotates pins: with
// `routingStrategy: round-robin`, six successive refreshes cycle every
// upstream into the primary position at least once.
func TestSafetyNet_RoutingStrategy_RoundRobin_Rotates(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	projectCfg := &common.ProjectConfig{
		Id:              "prjA",
		RoutingStrategy: "round-robin",
	}

	fx := safetyNetSetup(t, ctx, projectCfg, []*common.UpstreamConfig{
		upstreamCfg("rpc1", "main", nil),
		upstreamCfg("rpc2", "main", nil),
		upstreamCfg("rpc3", "main", nil),
	}, nil)

	method := "eth_call"
	networkId := util.EvmNetworkId(123)
	_, _ = fx.registry.GetSortedUpstreams(ctx, networkId, method) // pre-warm

	seenPrimary := map[string]bool{}
	for i := 0; i < 6; i++ {
		require.NoError(t, fx.registry.RefreshUpstreamNetworkMethodScores())
		ordered, err := fx.registry.GetSortedUpstreams(ctx, networkId, method)
		require.NoError(t, err)
		require.Len(t, ordered, 3)
		seenPrimary[ordered[0].Id()] = true
	}
	assert.Len(t, seenPrimary, 3,
		"round-robin should put each of the three upstreams in primary slot at least once")
}

// ─── tests: custom evalFunction ────────────────────────────────────────────

// TestSafetyNet_CustomEvalFunction_ReadsProcessEnv pins: a user-supplied eval
// function can read environment variables via process.env.X.
// (This is what the legacy default policy relies on.)
func TestSafetyNet_CustomEvalFunction_ReadsProcessEnv(t *testing.T) {
	t.Setenv("CUSTOM_MAX_ERROR_RATE", "0.1")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fx := safetyNetSetup(t, ctx, nil, []*common.UpstreamConfig{
		upstreamCfg("rpc1", "main", nil),
		upstreamCfg("rpc2", "main", nil),
		upstreamCfg("rpc3", "fallback", nil),
	}, nil)

	evalFn, err := common.CompileFunction(`
		(upstreams, method) => {
			const cap = parseFloat(process.env.CUSTOM_MAX_ERROR_RATE || '0.5');
			return upstreams.filter(u => u.metrics.errorRate < cap);
		}
	`)
	require.NoError(t, err)

	cfg := &common.SelectionPolicyConfig{
		EvalInterval:  common.Duration(20 * time.Millisecond),
		EvalPerMethod: false,
		EvalFunction:  evalFn,
	}
	ev, err := NewPolicyEvaluator(util.EvmNetworkId(123), fx.logger, cfg, fx.registry, fx.tracker)
	require.NoError(t, err)
	require.NoError(t, ev.Start(ctx))

	// rpc1: 20% error rate (would pass at 0.5, fails at 0.1)
	driveErrorBurst(t, fx.tracker, fx.upstreams["rpc1"], "*", 20)
	driveSuccessBurst(t, fx.tracker, fx.upstreams["rpc1"], "*", 80, 10*time.Millisecond)
	driveSuccessBurst(t, fx.tracker, fx.upstreams["rpc2"], "*", 100, 10*time.Millisecond)
	driveSuccessBurst(t, fx.tracker, fx.upstreams["rpc3"], "*", 100, 10*time.Millisecond)

	waitForEvalSettled()
	allowed := eligibleByPolicy(t, fx, ev, util.EvmNetworkId(123), "*")
	assert.ElementsMatch(t, []string{"rpc2", "rpc3"}, allowed,
		"process.env override of 0.1 excludes rpc1's 0.2 error rate")
}
