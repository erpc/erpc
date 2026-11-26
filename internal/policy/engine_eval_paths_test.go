package policy

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/telemetry"
	"github.com/grafana/sobek"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// modePolicy is driven tick-by-tick from Go through the `testMode()` global
// the primer installs, so a single registered network can walk through
// every eval outcome without re-registering.
const modePolicy = `(ups, ctx) => {
  const m = testMode();
  const byId = {};
  ups.forEach(u => { byId[u.id] = u; });
  switch (m) {
    case 'scored':
      return ups.map((u, i) => { u.score = ups.length - i; return u; });
    case 'exclude-b':
      globalThis.__policyLeafReasons['b'] = ['@step:excludeIf', 'error_rate_above'];
      globalThis.__policyProbeVerdicts['b'] = { e: true, b: true };
      globalThis.__policyShadowReasons['a'] = ['latency_p95_above'];
      return [byId.a, byId.c];
    case 'swap':
      globalThis.__policyStickyHeld = true;
      return [byId.b, byId.a, byId.c];
    case 'probe':
      globalThis.__probeConfig = { sampleRate: 1, minSamples: 0, maxConcurrent: 2 };
      return [byId.a];
    case 'probe-half':
      globalThis.__probeConfig = { sampleRate: 0.5, minSamples: 0 };
      return [byId.a];
    case 'steps':
      if (globalThis.__policyStepLogEnabled) {
        globalThis.__policyStepLog.push({ step: 'custom', args: { k: 1 }, inIds: ['a', 'b'], outIds: ['b', 'a'], dropped: ['c'], added: ['x'], reordered: true });
      }
      return [byId.b, byId.a];
    case 'throw':
      throw new Error('boom');
    case 'null':
      return null;
    default:
      return ups;
  }
}`

type modeEngine struct {
	*Engine
	mode *atomic.Value
}

func (m *modeEngine) set(mode string) { m.mode.Store(mode) }

func newModeEngine(t *testing.T, mutate func(*common.SelectionPolicyConfig)) (*modeEngine, *common.SelectionPolicyConfig) {
	t.Helper()
	mode := &atomic.Value{}
	mode.Store("plain")
	primer := func(rt *common.Runtime) error {
		return rt.VM().Set("testMode", func() string { return mode.Load().(string) })
	}
	cfg := &common.SelectionPolicyConfig{EvalFunc: modePolicy, DisableTickerForTest: true}
	require.NoError(t, cfg.SetDefaults())
	if mutate != nil {
		mutate(cfg)
	}
	logger := zerolog.Nop()
	tracker := health.NewTracker(&logger, "p1", time.Minute)
	e := NewEngine(context.Background(), &logger, "p1", tracker, primer, nil)
	t.Cleanup(e.Stop)
	return &modeEngine{Engine: e, mode: mode}, cfg
}

func stubUps(ids ...string) ([]common.Upstream, map[string]*stubProbeUpstream) {
	ups := make([]common.Upstream, 0, len(ids))
	byID := make(map[string]*stubProbeUpstream, len(ids))
	for _, id := range ids {
		s := &stubProbeUpstream{id: id}
		ups = append(ups, s)
		byID[id] = s
	}
	return ups, byID
}

func latestDecision(t *testing.T, e *Engine, network, method string) *Decision {
	t.Helper()
	ds := e.RecentDecisions(network, method, "*", 1)
	require.Len(t, ds, 1)
	return ds[0]
}

func TestEngine_TickOutcomesFeedDecisionsAndMetrics(t *testing.T) {
	const label = "cov-tick"
	e, cfg := newModeEngine(t, nil)
	ups, _ := stubUps("a", "b", "c")
	require.NoError(t, e.RegisterNetwork("evm:1", label, func() []common.Upstream { return ups }, cfg))

	rejections := func(up, step string) float64 {
		return testutil.ToFloat64(telemetry.MetricSelectionRejectionTotal.WithLabelValues("p1", label, "*", up, step))
	}
	exclusions := func(up, reason string) float64 {
		return testutil.ToFloat64(telemetry.MetricSelectionExclusionTotal.WithLabelValues("p1", label, "*", up, reason))
	}
	shadows := func(up, reason string) float64 {
		return testutil.ToFloat64(telemetry.MetricSelectionShadowExclusionTotal.WithLabelValues("p1", label, "*", up, reason))
	}
	readmits := func(up string) float64 {
		return testutil.ToFloat64(telemetry.MetricSelectionReadmitTotal.WithLabelValues("p1", label, "*", up))
	}
	switches := func(from, to string) float64 {
		return testutil.ToFloat64(telemetry.MetricSelectionPrimarySwitchTotal.WithLabelValues("p1", label, "*", from, to))
	}
	holds := func(up string) float64 {
		return testutil.ToFloat64(telemetry.MetricSelectionStickyHoldTotal.WithLabelValues("p1", label, "*", up))
	}
	evalErrors := func(kind string) float64 {
		return testutil.ToFloat64(telemetry.MetricSelectionEvalErrorsTotal.WithLabelValues("p1", label, "*", kind))
	}

	// Initial synchronous tick: identity order, first primary counts as a switch.
	assert.Equal(t, []string{"a", "b", "c"}, ids(e.GetOrdered("evm:1", "*", "*")))
	assert.False(t, e.LastEvalAt("evm:1", "*", "*").IsZero())
	firstSwitch := e.LastSwitchAt("evm:1", "*", "*")
	require.False(t, firstSwitch.IsZero())
	assert.Nil(t, e.GetScores("evm:1", "*", "*"), "policy without scoring publishes no scores")

	// Scored tick keeps the primary, so the switch timestamp is untouched.
	e.set("scored")
	TickForTest(e.Engine, "evm:1", "*")
	assert.Equal(t, map[string]float64{"a": 3, "b": 2, "c": 1}, e.GetScores("evm:1", "*", "*"))
	assert.Equal(t, firstSwitch, e.LastSwitchAt("evm:1", "*", "*"))

	// Exclusion with step + leaf attribution and a probe-blocking verdict.
	rejBefore, exBefore, shBefore := rejections("b", "excludeIf"), exclusions("b", "error_rate_above"), shadows("a", "latency_p95_above")
	e.set("exclude-b")
	TickForTest(e.Engine, "evm:1", "*")
	assert.Equal(t, []string{"a", "c"}, ids(e.GetOrdered("evm:1", "*", "*")))
	assert.Empty(t, e.GetExcluded("evm:1", "*", "*"), "probe-blocking exclusion must not be offered to the prober")
	d := latestDecision(t, e.Engine, "evm:1", "*")
	require.Len(t, d.Output.Excluded, 1)
	assert.Equal(t, ExcludedUpstream{ID: "b", Reason: "error_rate_above", Step: "excludeIf", ProbeEligible: false, LeafReasons: []string{"error_rate_above"}}, d.Output.Excluded[0])
	assert.Equal(t, map[string][]string{"a": {"latency_p95_above"}}, d.Output.ShadowReasons)
	assert.Equal(t, []string{"b"}, d.Diff.Removed)
	assert.Equal(t, rejBefore+1, rejections("b", "excludeIf"))
	assert.Equal(t, exBefore+1, exclusions("b", "error_rate_above"))
	assert.Equal(t, shBefore+1, shadows("a", "latency_p95_above"))
	order, excluded := LatestDecisionOutputForTest(e.Engine, "evm:1", "*")
	assert.Equal(t, []string{"a", "c"}, order)
	assert.Equal(t, []string{"b"}, excluded)

	// Staying excluded for a second tick does not re-admit it.
	readmitBefore := readmits("b")
	TickForTest(e.Engine, "evm:1", "*")
	assert.Equal(t, readmitBefore, readmits("b"))

	// Back in rotation → readmit counted once.
	e.set("plain")
	TickForTest(e.Engine, "evm:1", "*")
	assert.Equal(t, readmitBefore+1, readmits("b"))
	assert.Equal(t, []string{"b"}, latestDecision(t, e.Engine, "evm:1", "*").Diff.Added)

	// Primary swap with an active sticky hold.
	swBefore, holdBefore := switches("a", "b"), holds("b")
	e.set("swap")
	TickForTest(e.Engine, "evm:1", "*")
	assert.Equal(t, swBefore+1, switches("a", "b"))
	assert.Equal(t, holdBefore+1, holds("b"))
	d = latestDecision(t, e.Engine, "evm:1", "*")
	assert.True(t, d.Diff.PrimaryChanged)
	assert.True(t, d.Diff.StickyHeld)
	assert.True(t, e.LastSwitchAt("evm:1", "*", "*").After(firstSwitch))

	// Eval failures keep the previous cache and are classified by kind.
	throwBefore, invalidBefore := evalErrors("throw"), evalErrors("invalid_return")
	e.set("throw")
	TickForTest(e.Engine, "evm:1", "*")
	assert.Contains(t, latestDecision(t, e.Engine, "evm:1", "*").Error, "boom")
	assert.Equal(t, throwBefore+1, evalErrors("throw"))
	e.set("null")
	TickForTest(e.Engine, "evm:1", "*")
	assert.Contains(t, latestDecision(t, e.Engine, "evm:1", "*").Error, ErrInvalidReturn.Error())
	assert.Equal(t, invalidBefore+1, evalErrors("invalid_return"))
	assert.Equal(t, []string{"b", "a", "c"}, ids(e.GetOrdered("evm:1", "*", "*")), "failed evals retain the last good order")
}

func ids(ups []common.Upstream) []string {
	if ups == nil {
		return nil
	}
	out := make([]string, len(ups))
	for i, u := range ups {
		out[i] = u.Id()
	}
	return out
}

func TestEngine_EvalExceedingTimeoutIsCounted(t *testing.T) {
	const label = "cov-timeout"
	e, cfg := newModeEngine(t, func(c *common.SelectionPolicyConfig) { c.EvalTimeout = common.Duration(time.Nanosecond) })
	ups, _ := stubUps("a")
	require.NoError(t, e.RegisterNetwork("evm:1", label, func() []common.Upstream { return ups }, cfg))
	timeouts := func() float64 {
		return testutil.ToFloat64(telemetry.MetricSelectionEvalErrorsTotal.WithLabelValues("p1", label, "*", "timeout"))
	}
	before := timeouts()
	TickForTest(e.Engine, "evm:1", "*")
	assert.Equal(t, before+1, timeouts())
	assert.Equal(t, []string{"a"}, ids(e.GetOrdered("evm:1", "*", "*")), "a slow eval's result still replaces the cache")
}

func TestEngine_ProberLifecycleFollowsProbeConfig(t *testing.T) {
	e, cfg := newModeEngine(t, nil)
	ups, byID := stubUps("a", "b")
	e.set("probe")
	require.NoError(t, e.RegisterNetwork("evm:1", "cov-probe", func() []common.Upstream { return ups }, cfg))

	prober := func() *Prober {
		e.proberMu.RLock()
		defer e.proberMu.RUnlock()
		return e.probers["evm:1"]
	}
	p := prober()
	require.NotNil(t, p, "probeExcluded config must create a prober")
	assert.Equal(t, 1.0, p.Config().SampleRate)
	assert.Equal(t, 2, p.Config().MaxConcurrent)
	assert.Equal(t, []string{"b"}, ids(e.GetExcluded("evm:1", "*", "*")))

	e.PublishRequest("evm:1", nil)
	e.PublishRequest("evm:1", makeProbeReq(t, "eth_blockNumber"))
	require.Eventually(t, func() bool { return byID["b"].calls.Load() == 1 }, 5*time.Second, 5*time.Millisecond)
	assert.Equal(t, int64(0), byID["a"].calls.Load(), "in-rotation upstreams are never shadow-probed")

	e.set("probe-half")
	TickForTest(e.Engine, "evm:1", "*")
	assert.Same(t, p, prober(), "a changed config hot-swaps the existing prober")
	assert.Equal(t, 0.5, prober().Config().SampleRate)

	e.set("plain")
	TickForTest(e.Engine, "evm:1", "*")
	assert.Nil(t, prober(), "dropping probeExcluded tears the prober down")
	e.PublishRequest("evm:1", makeProbeReq(t, "eth_blockNumber"))
	assert.Equal(t, int64(1), byID["b"].calls.Load())
}

func TestEngine_StepLogFollowsToggle(t *testing.T) {
	e, cfg := newModeEngine(t, nil)
	ups, _ := stubUps("a", "b", "c")
	e.set("steps")
	require.NoError(t, e.RegisterNetwork("evm:1", "cov-steps", func() []common.Upstream { return ups }, cfg))
	assert.False(t, e.IsStepLogEnabled())
	assert.Nil(t, latestDecision(t, e.Engine, "evm:1", "*").Output.StepLog)

	e.SetStepLogEnabled(true)
	assert.True(t, e.IsStepLogEnabled())
	TickForTest(e.Engine, "evm:1", "*")
	steps := latestDecision(t, e.Engine, "evm:1", "*").Output.StepLog
	require.Len(t, steps, 1)
	assert.Equal(t, "custom", steps[0].Step)
	assert.JSONEq(t, `{"k":1}`, string(steps[0].Args))
	assert.Equal(t, []string{"a", "b"}, steps[0].InIDs)
	assert.Equal(t, []string{"c"}, steps[0].Dropped)
	assert.Equal(t, []string{"x"}, steps[0].Added)
	assert.True(t, steps[0].Reordered)

	e.SetStepLogEnabled(false)
	TickForTest(e.Engine, "evm:1", "*")
	assert.Nil(t, latestDecision(t, e.Engine, "evm:1", "*").Output.StepLog)
}

func TestEngine_RecentDecisionsRingIsBounded(t *testing.T) {
	e, cfg := newModeEngine(t, nil)
	ups, _ := stubUps("a")
	require.NoError(t, e.RegisterNetwork("evm:1", "cov-ring", func() []common.Upstream { return ups }, cfg))
	extra := decisionsRingSize + 6
	for range extra {
		TickForTest(e.Engine, "evm:1", "*")
	}
	all := e.RecentDecisions("evm:1", "*", "*", 0)
	require.Len(t, all, decisionsRingSize)
	last := e.RecentDecisions("evm:1", "*", "*", 3)
	require.Len(t, last, 3)
	// Register tick has TickCount 0, so the final tick saw TickCount == extra.
	for i, d := range last {
		assert.Equal(t, uint64(extra-2+i), d.State.TickCount, "oldest-first ordering")
	}
	assert.Equal(t, uint64(extra-decisionsRingSize+1), all[0].State.TickCount, "oldest entries are evicted")
}

func TestEngine_PerMethodSlotsAndLookups(t *testing.T) {
	const perMethod = `(ups, ctx) => ctx.method === 'eth_call' ? ups.filter(u => u.id === 'a') : ups`
	cfg := &common.SelectionPolicyConfig{EvalFunc: perMethod, EvalScope: common.EvalScopeNetworkMethod, DisableTickerForTest: true}
	require.NoError(t, cfg.SetDefaults())
	logger := zerolog.Nop()
	e := NewEngine(context.Background(), &logger, "p1", health.NewTracker(&logger, "p1", time.Minute), nil, nil)
	defer e.Stop()
	ups, _ := stubUps("a", "b")
	require.NoError(t, e.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))

	// Narrow slot is lazily created but untick'd: wildcard answers.
	assert.Nil(t, e.GetExcluded("evm:1", "eth_call", "*"))
	assert.Equal(t, []string{"a", "b"}, ids(e.GetOrdered("evm:1", "eth_call", "*")))
	assert.Equal(t, 2, SlotCountForTest(e))
	assert.True(t, e.LastEvalAt("evm:1", "eth_call", "*").IsZero(), "narrow slot has not evaluated yet")

	TickForTest(e, "evm:1", "eth_call")
	assert.Equal(t, []string{"a"}, ids(e.GetOrdered("evm:1", "eth_call", "*")))
	assert.Equal(t, []string{"b"}, ids(e.GetExcluded("evm:1", "eth_call", "*")))
	assert.Equal(t, []string{"a", "b"}, ids(e.GetOrdered("evm:1", "eth_getLogs", "*")), "other methods fall back to wildcard until ticked")
	assert.Equal(t, "eth_call", latestDecision(t, e, "evm:1", "eth_call").Method)

	// Unknown networks answer with zero values.
	assert.Nil(t, e.GetOrdered("evm:404", "*", "*"))
	assert.Nil(t, e.GetExcluded("evm:404", "*", "*"))
	assert.True(t, e.LastEvalAt("evm:404", "*", "*").IsZero())
	assert.True(t, e.LastSwitchAt("evm:404", "*", "*").IsZero())
	assert.Nil(t, e.RecentDecisions("evm:404", "*", "*", 0))
	assert.Nil(t, e.GetScores("evm:404", "*", "*"))
	assert.False(t, e.PerBoundaryEnabled("evm:404"))
	assert.False(t, e.PerBoundaryEnabled("evm:1"))

	on := true
	laneCfg := &common.SelectionPolicyConfig{EvalFunc: "(ups) => ups", EvalPerBoundary: &on, DisableTickerForTest: true}
	require.NoError(t, laneCfg.SetDefaults())
	require.NoError(t, e.RegisterNetwork("evm:2", "", func() []common.Upstream { return ups }, laneCfg))
	assert.True(t, e.PerBoundaryEnabled("evm:2"))

	before := SlotCountForTest(e)
	e.UnregisterNetwork("evm:1")
	assert.Equal(t, before-3, SlotCountForTest(e), "every slot of the network is torn down")
	assert.Nil(t, e.GetOrdered("evm:1", "*", "*"))
	assert.Equal(t, []string{"a", "b"}, ids(e.GetOrdered("evm:2", "*", "*")))
}

func TestEngine_PauseGatesTicker(t *testing.T) {
	cfg := &common.SelectionPolicyConfig{EvalFunc: "(ups) => ups", EvalInterval: common.Duration(5 * time.Millisecond)}
	require.NoError(t, cfg.SetDefaults())
	logger := zerolog.Nop()
	e := NewEngine(context.Background(), &logger, "p1", health.NewTracker(&logger, "p1", time.Minute), nil, nil)
	defer e.Stop()
	var calls atomic.Int64
	ups, _ := stubUps("a")
	require.NoError(t, e.RegisterNetwork("evm:1", "", func() []common.Upstream { calls.Add(1); return ups }, cfg))
	require.Eventually(t, func() bool { return calls.Load() >= 3 }, 5*time.Second, time.Millisecond)

	e.SetPaused(true)
	assert.True(t, e.IsPaused())
	paused := calls.Load()
	// At most one tick that already passed the gate may still land.
	require.Never(t, func() bool { return calls.Load() > paused+1 }, 100*time.Millisecond, 5*time.Millisecond)

	e.SetPaused(false)
	assert.False(t, e.IsPaused())
	resumed := calls.Load()
	require.Eventually(t, func() bool { return calls.Load() >= resumed+2 }, 5*time.Second, time.Millisecond)
}

func TestEngine_TSFunctionSentinels(t *testing.T) {
	userScript, err := sobek.Compile("user.js", `
globalThis.__erpcFns = globalThis.__erpcFns || {};
globalThis.__erpcFns['reverse'] = function (ups, ctx) { return ups.slice().reverse(); };
globalThis.__erpcFns['notfn'] = 42;
`, false)
	require.NoError(t, err)
	logger := zerolog.Nop()
	newCfg := func(id string) *common.SelectionPolicyConfig {
		c := &common.SelectionPolicyConfig{EvalFunc: "__ts_fn__:" + id, DisableTickerForTest: true}
		require.NoError(t, c.SetDefaults())
		require.Nil(t, c.CompiledProgram)
		return c
	}
	ups, _ := stubUps("a", "b")
	upsFn := func() []common.Upstream { return ups }

	e := NewEngine(context.Background(), &logger, "p1", health.NewTracker(&logger, "p1", time.Minute), nil, userScript)
	defer e.Stop()
	require.NoError(t, e.RegisterNetwork("evm:1", "", upsFn, newCfg("reverse")))
	assert.Equal(t, []string{"b", "a"}, ids(e.GetOrdered("evm:1", "*", "*")))

	require.NoError(t, e.RegisterNetwork("evm:2", "", upsFn, newCfg("missing")))
	assert.Contains(t, latestDecision(t, e, "evm:2", "*").Error, `id "missing" not found in __erpcFns`)
	assert.Nil(t, e.GetOrdered("evm:2", "*", "*"))

	require.NoError(t, e.RegisterNetwork("evm:3", "", upsFn, newCfg("notfn")))
	assert.Contains(t, latestDecision(t, e, "evm:3", "*").Error, ErrInvalidReturn.Error())

	bare := NewEngine(context.Background(), &logger, "p1", health.NewTracker(&logger, "p1", time.Minute), nil, nil)
	defer bare.Stop()
	require.NoError(t, bare.RegisterNetwork("evm:1", "", upsFn, newCfg("reverse")))
	assert.Contains(t, latestDecision(t, bare, "evm:1", "*").Error, "__erpcFns registry not populated")
}

func TestEngine_SharedStickyRegister(t *testing.T) {
	const src = `(ups, ctx) => {
  __setSharedSticky('network', '', 1);
  const s = __getSharedSticky('network');
  if (s) {
    return ups.filter(u => u.id === s.primary).concat(ups.filter(u => u.id !== s.primary));
  }
  __setSharedSticky('network', 'b', 777);
  return ups;
}`
	cfg := &common.SelectionPolicyConfig{EvalFunc: src, DisableTickerForTest: true}
	require.NoError(t, cfg.SetDefaults())
	logger := zerolog.Nop()
	e := NewEngine(context.Background(), &logger, "p1", health.NewTracker(&logger, "p1", time.Minute), nil, nil)
	defer e.Stop()
	ups, _ := stubUps("a", "b")
	require.NoError(t, e.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))
	assert.Equal(t, []string{"a", "b"}, ids(e.GetOrdered("evm:1", "*", "*")), "cold start: nothing shared yet")
	assert.Equal(t, map[stickyKey]stickyEntry{{network: "evm:1", method: "*", finality: "*"}: {Primary: "b", LastSwitchAt: 777}}, e.sticky.snapshot(),
		"empty primary writes are ignored; the network-scope key collapses method/finality")

	TickForTest(e, "evm:1", "*")
	assert.Equal(t, []string{"b", "a"}, ids(e.GetOrdered("evm:1", "*", "*")), "second tick reads the shared primary")

	e.SetIdleEvictionAfter(time.Millisecond)
	e.sweepIdleSticky()
	assert.Empty(t, e.sticky.snapshot(), "entries older than the idle window are evicted")
}

func TestResolveStickyKey(t *testing.T) {
	tests := []struct {
		scope common.EvalScope
		want  stickyKey
	}{
		{common.EvalScopeNetwork, stickyKey{"n", "*", "*"}},
		{common.EvalScopeNetworkMethod, stickyKey{"n", "m", "*"}},
		{common.EvalScopeNetworkFinality, stickyKey{"n", "*", "f"}},
		{common.EvalScopeNetworkMethodFinality, stickyKey{"n", "m", "f"}},
		{common.EvalScope("bogus"), stickyKey{"n", "m", "f"}},
	}
	for _, tc := range tests {
		t.Run(string(tc.scope), func(t *testing.T) {
			assert.Equal(t, tc.want, resolveStickyKey(tc.scope, "n", "m", "f"))
		})
	}
}

func TestStickyStore_SnapshotAndEviction(t *testing.T) {
	s := newStickyStore()
	old, fresh := stickyKey{"n", "*", "*"}, stickyKey{"n", "m", "*"}
	s.Set(old, "a", 100)
	s.Set(fresh, "b", 1000)
	snap := s.snapshot()
	delete(snap, old)
	_, _, ok := s.Get(old)
	assert.True(t, ok, "snapshot is a copy")

	s.evictIdle(500)
	_, _, ok = s.Get(old)
	assert.False(t, ok)
	primary, at, ok := s.Get(fresh)
	assert.True(t, ok)
	assert.Equal(t, "b", primary)
	assert.Equal(t, int64(1000), at)
}

func TestRunEval_ErrorPaths(t *testing.T) {
	ups, _ := stubUps("a", "b")
	evalCtx := EvalContext{Network: "evm:1", Method: "*", Finality: "unknown"}
	compiled := func(src string) *common.SelectionPolicyConfig {
		p, err := common.CompileProgram(src)
		require.NoError(t, err)
		return &common.SelectionPolicyConfig{EvalFunc: src, CompiledProgram: p}
	}
	run := func(pool *runtimePool, cfg *common.SelectionPolicyConfig) (*EvalResult, error) {
		return runEval(pool, cfg, ups, nil, nil, nil, evalCtx, false, nil)
	}

	_, err := run(newRuntimePool(nil, nil), &common.SelectionPolicyConfig{EvalFunc: "(u) => u"})
	assert.ErrorContains(t, err, "no compiled program")

	_, err = run(newRuntimePool(func(*common.Runtime) error { return assert.AnError }, nil), compiled("(u) => u"))
	assert.ErrorContains(t, err, "acquire sobek runtime")

	badScript, err := sobek.Compile("bad.js", `throw new Error("user script broke")`, false)
	require.NoError(t, err)
	_, err = run(newRuntimePool(nil, badScript), compiled("(u) => u"))
	assert.ErrorContains(t, err, "evaluate user TS config in policy runtime")

	_, err = run(newRuntimePool(nil, nil), compiled("42"))
	assert.ErrorIs(t, err, ErrInvalidReturn)

	_, err = run(newRuntimePool(nil, nil), compiled("(u) => undefinedFn()"))
	assert.ErrorContains(t, err, "call eval")

	returns := []struct {
		name    string
		src     string
		wantErr string
	}{
		{"null", "() => null", "returned null/undefined"},
		{"primitive", "() => 5", "eval returned"},
		{"object without length", "() => ({})", "not an array"},
		{"non-object entry", "() => [5]", "entry 0 is not an object"},
		{"entry without id", "() => [{}]", "entry 0 has no id"},
		{"unknown id", "() => [{id: 'zzz'}]", `unknown id "zzz"`},
	}
	for _, tc := range returns {
		t.Run(tc.name, func(t *testing.T) {
			_, err := run(newRuntimePool(nil, nil), compiled(tc.src))
			require.ErrorIs(t, err, ErrInvalidReturn)
			assert.ErrorContains(t, err, tc.wantErr)
		})
	}

	t.Run("holes and null scores are skipped; nil sticky store is inert", func(t *testing.T) {
		res, err := run(newRuntimePool(nil, nil), compiled(`(ups) => {
  __setSharedSticky('network', 'a', 1);
  if (__getSharedSticky('network') !== undefined) throw new Error('sticky leaked');
  return [undefined, {id: 'b', score: null}, {id: 'a', score: 2.5}];
}`))
		require.NoError(t, err)
		assert.Equal(t, []string{"b", "a"}, res.OrderedIDs)
		assert.Equal(t, map[string]float64{"a": 2.5}, res.Scores)
		assert.Nil(t, res.ProbeConfig)
		assert.False(t, res.StickyHeld)
	})
}

func TestRuntimePool_ReusesReleasedRuntime(t *testing.T) {
	var primed atomic.Int64
	pool := newRuntimePool(func(*common.Runtime) error { primed.Add(1); return nil }, nil)
	rt, err := pool.acquire()
	require.NoError(t, err)
	pool.release(rt)
	pool.release(nil)
	again, err := pool.acquire()
	require.NoError(t, err)
	assert.Same(t, rt, again)
	assert.Equal(t, int64(1), primed.Load(), "a pooled runtime is primed once")
	fresh, err := pool.acquire()
	require.NoError(t, err)
	assert.NotSame(t, rt, fresh)
	assert.Equal(t, int64(2), primed.Load())
}

func vmWithGlobal(t *testing.T, name, js string) *sobek.Runtime {
	t.Helper()
	vm := sobek.New()
	if js != "" {
		_, err := vm.RunString("globalThis." + name + " = " + js + ";")
		require.NoError(t, err)
	}
	return vm
}

func TestReadProbeConfig(t *testing.T) {
	defaults := ProbeConfig{SampleRate: 0.1, MinSamples: 10, MinSamplesWindow: 60 * time.Second, MaxConcurrent: 4, Timeout: 10 * time.Second}
	tests := []struct {
		name string
		js   string
		want *ProbeConfig
	}{
		{name: "absent", js: "", want: nil},
		{name: "null", js: "null", want: nil},
		{name: "non-object", js: "5", want: nil},
		{name: "empty object takes defaults", js: "{}", want: &defaults},
		{
			name: "values below range clamp, string durations parse",
			js:   `{sampleRate: -1, minSamples: -3, maxConcurrent: 0, minSamplesWindow: "30s", timeout: "2s"}`,
			want: &ProbeConfig{SampleRate: 0, MinSamples: 0, MinSamplesWindow: 30 * time.Second, MaxConcurrent: 1, Timeout: 2 * time.Second},
		},
		{
			name: "values above range clamp, numeric durations are ms",
			js:   `{sampleRate: 5, minSamples: 3, maxConcurrent: 7, minSamplesWindow: 1500, timeout: 250}`,
			want: &ProbeConfig{SampleRate: 1, MinSamples: 3, MinSamplesWindow: 1500 * time.Millisecond, MaxConcurrent: 7, Timeout: 250 * time.Millisecond},
		},
		{
			name: "invalid durations keep defaults",
			js:   `{minSamplesWindow: "bogus", timeout: "0s"}`,
			want: &defaults,
		},
		{
			name: "non-positive numeric durations keep defaults",
			js:   `{minSamplesWindow: 0, timeout: -5}`,
			want: &defaults,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := readProbeConfig(vmWithGlobal(t, "__probeConfig", tc.js))
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestReadReasonsMap(t *testing.T) {
	tests := []struct {
		name string
		js   string
		want map[string][]string
	}{
		{"absent", "", nil},
		{"non-object", "7", nil},
		{"empty", "{}", nil},
		{
			"drops empty, null and malformed entries",
			`{a: ["x", "", null, "y"], b: [], c: 5, d: [null, ""], e: ["z"]}`,
			map[string][]string{"a": {"x", "y"}, "e": {"z"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, readReasonsMap(vmWithGlobal(t, "__r", tc.js), "__r"))
		})
	}
}

func TestReadProbeVerdicts(t *testing.T) {
	assert.Nil(t, readProbeVerdicts(vmWithGlobal(t, "__policyProbeVerdicts", "")))
	assert.Nil(t, readProbeVerdicts(vmWithGlobal(t, "__policyProbeVerdicts", "3")))
	assert.Nil(t, readProbeVerdicts(vmWithGlobal(t, "__policyProbeVerdicts", "{}")))

	got := readProbeVerdicts(vmWithGlobal(t, "__policyProbeVerdicts", `{a: {e: true}, b: {e: true, b: 1}, c: 5}`))
	assert.Equal(t, map[string]ProbeVerdict{"a": {Eligible: true}, "b": {Eligible: true, Blocking: true}}, got)
	assert.True(t, got["a"].ShouldProbe())
	assert.False(t, got["b"].ShouldProbe(), "a blocking step vetoes probing")
	assert.True(t, ProbeVerdict{}.ShouldProbe(), "untracked exclusion defaults to probing")
}

func TestReadStepLog(t *testing.T) {
	steps, err := readStepLog(vmWithGlobal(t, "__policyStepLog", ""))
	require.NoError(t, err)
	assert.Nil(t, steps)

	steps, err = readStepLog(vmWithGlobal(t, "__policyStepLog", `[{step: "take", inIds: ["a","b"], outIds: ["a"], dropped: ["b"]}]`))
	require.NoError(t, err)
	assert.Equal(t, []StepEntry{{Step: "take", InIDs: []string{"a", "b"}, OutIDs: []string{"a"}, Dropped: []string{"b"}}}, steps)

	_, err = readStepLog(vmWithGlobal(t, "__policyStepLog", `[{step: "x", inIds: 5}]`))
	var typeErr *json.UnmarshalTypeError
	assert.ErrorAs(t, err, &typeErr)
}

func TestTestHelpers_OverrideAllAndReset(t *testing.T) {
	OverrideAllForTest(nil) // nil engine is a documented no-op

	e, cfg := newModeEngine(t, nil)
	ups, _ := stubUps("c", "a", "b")
	upsFn := func() []common.Upstream { return ups }
	require.NoError(t, e.RegisterNetwork("evm:1", "", upsFn, cfg))
	require.NoError(t, e.RegisterNetwork("evm:2", "", upsFn, cfg))

	OverrideAllForTest(e.Engine)
	assert.Equal(t, []string{"a", "b", "c"}, ids(e.GetOrdered("evm:1", "*", "*")))
	assert.Equal(t, []string{"a", "b", "c"}, ids(e.GetOrdered("evm:2", "*", "*")))
	OverrideAllForTest(e.Engine, "b", "nope")
	assert.Equal(t, []string{"b"}, ids(e.GetOrdered("evm:2", "*", "*")), "unknown ids are dropped")

	order, _ := LatestDecisionOutputForTest(e.Engine, "evm:1", "*")
	require.NotEmpty(t, order)
	ResetSlotStateForTest(e.Engine, "evm:1", "*")
	ResetSlotStateForTest(e.Engine, "evm:404", "*")
	order, excluded := LatestDecisionOutputForTest(e.Engine, "evm:1", "*")
	assert.Nil(t, order)
	assert.Nil(t, excluded)
	assert.True(t, e.LastSwitchAt("evm:1", "*", "*").IsZero())
	o404, x404 := LatestDecisionOutputForTest(e.Engine, "evm:404", "*")
	assert.Nil(t, o404)
	assert.Nil(t, x404)
}
