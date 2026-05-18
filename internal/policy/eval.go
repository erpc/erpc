package policy

import (
	"fmt"
	"strconv"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/grafana/sobek"
)

// EvalContext mirrors the JS-side `ctx` argument (spec §3.2). Constructed
// per-tick and passed by value into the sobek runtime.
type EvalContext struct {
	Network          string           `json:"network"`
	Method           string           `json:"method"`
	Finality         string           `json:"finality"`
	Now              int64            `json:"now"`
	PreviousOrder    []string         `json:"previousOrder,omitempty"`
	PreviousExcluded []string         `json:"previousExcluded,omitempty"`
	LastSwitchAt     *int64           `json:"lastSwitchAt,omitempty"`
	ExcludedSince    map[string]int64 `json:"excludedSince,omitempty"`
	TickCount        uint64           `json:"tickCount"`
}

func buildEvalContext(networkID, method string, state DecisionState) EvalContext {
	ctx := EvalContext{
		Network:          networkID,
		Method:           method,
		Finality:         "unknown",
		Now:              time.Now().UnixMilli(),
		PreviousOrder:    state.PreviousOrder,
		PreviousExcluded: state.PreviousExcluded,
		ExcludedSince:    state.ExcludedSince,
		TickCount:        state.TickCount,
	}
	if state.LastSwitchAt != nil {
		ms := state.LastSwitchAt.UnixMilli()
		ctx.LastSwitchAt = &ms
	}
	return ctx
}

// healthTracker is the narrow read-side interface the engine needs.
// Implemented by *health.Tracker; declared here so tests can inject a fake.
//
// `GetNetworkBlockTime` is consulted to convert block-count lag into a
// wall-clock seconds lag (`BlockHeadLagSeconds` /
// `FinalizationLagSeconds`). The tracker returns 0 until it has enough
// block-time samples; in that case the *Seconds fields stay at 0 too,
// and policies relying on time-based trips just won't fire — which is
// the correct conservative behavior on a freshly-booted engine.
type healthTracker interface {
	GetUpstreamMethodMetrics(up common.Upstream, method string) *health.TrackedMetrics
	GetNetworkBlockTime(networkId string) time.Duration
}

// readUpstreamMetrics builds the JS-visible metrics object for one upstream.
// Mirrors §3.1 of the spec.
func readUpstreamMetrics(tr healthTracker, u common.Upstream, method string) UpstreamMetrics {
	m := tr.GetUpstreamMethodMetrics(u, method)
	if m == nil {
		return UpstreamMetrics{}
	}
	out := UpstreamMetrics{
		ErrorRate:       m.ErrorRate(),
		ErrorsTotal:     m.ErrorsTotal.Load(),
		RequestsTotal:   m.RequestsTotal.Load(),
		ThrottledRate:   m.ThrottledRate(),
		MisbehaviorRate: m.MisbehaviorRate(),
		BlockHeadLag:    m.BlockHeadLag.Load(),
		FinalizationLag: m.FinalizationLag.Load(),
	}
	if m.ResponseQuantiles != nil {
		out.P50ResponseSeconds = m.ResponseQuantiles.GetQuantile(0.50).Seconds()
		out.P70ResponseSeconds = m.ResponseQuantiles.GetQuantile(0.70).Seconds()
		out.P90ResponseSeconds = m.ResponseQuantiles.GetQuantile(0.90).Seconds()
		out.P95ResponseSeconds = m.ResponseQuantiles.GetQuantile(0.95).Seconds()
		out.P99ResponseSeconds = m.ResponseQuantiles.GetQuantile(0.99).Seconds()
	}
	// Convert block-count lag to wall-clock seconds using the network's
	// EMA-estimated block time. Zero until the tracker has enough
	// samples — policies that trip on `*LagSeconds` thresholds will be
	// no-ops on a freshly-booted engine, by design.
	if bt := tr.GetNetworkBlockTime(u.NetworkId()); bt > 0 {
		btSec := bt.Seconds()
		out.BlockHeadLagSeconds = float64(out.BlockHeadLag) * btSec
		out.FinalizationLagSeconds = float64(out.FinalizationLag) * btSec
	}
	if m.Cordoned.Load() {
		if r, ok := m.LastCordonedReason.Load().(string); ok {
			out.CordonedReason = r
		}
	}
	return out
}

// buildJSUpstreams constructs a JS array of upstream objects ready to be
// passed to the eval function. We build via `vm.NewObject` (rather than
// letting sobek auto-convert a Go struct slice) so we can attach JS-side
// methods to each upstream — `u.hasTag(...)`, `u.is(...)`, and
// `u.metrics.latencyP(q)`.
//
// These methods exist because they read MORE naturally in policy code
// than the equivalent field-access expressions:
//
//	u.is('tier:free')           vs  u.tags.includes('tier:free')
//	u.metrics.latencyP(99)      vs  u.metrics.p99ResponseSeconds * 1000
//
// `is` is just an alias for `hasTag` (matches the engine's vocabulary
// from two directions). `latencyP` accepts either 0..1 fractions or
// 0..100 percentile numbers and returns milliseconds — it snaps to the
// nearest pre-computed quantile bucket since the underlying
// QuantileTracker isn't (and shouldn't be) exposed across the JS bridge.
//
// Annotations is pre-allocated (non-nil empty array) so stdlib steps
// that `u.annotations.push(...)` don't trip on undefined.
func buildJSUpstreams(vm *sobek.Runtime, ups []common.Upstream, metrics map[string]UpstreamMetrics) sobek.Value {
	arr := vm.NewArray()
	for i, u := range ups {
		obj := vm.NewObject()
		_ = obj.Set("id", u.Id())
		_ = obj.Set("vendor", u.VendorName())

		// Tags: copy once and capture by reference. The hasTag/is
		// closures below close over this slice — sobek won't see them
		// mutate (we never mutate `tags` from JS).
		var tags []string
		if cfg := u.Config(); cfg != nil {
			_ = obj.Set("type", string(cfg.Type))
			if len(cfg.Tags) > 0 {
				tags = append(tags, cfg.Tags...)
			}
		}
		_ = obj.Set("tags", vm.ToValue(tags))

		// hasTag(tag) → bool; `is` is an alias. Shared closure so we
		// allocate one function per upstream, not two.
		hasTagFn := func(call sobek.FunctionCall) sobek.Value {
			if len(call.Arguments) == 0 {
				return vm.ToValue(false)
			}
			target := call.Argument(0).String()
			for _, t := range tags {
				if t == target {
					return vm.ToValue(true)
				}
			}
			return vm.ToValue(false)
		}
		_ = obj.Set("hasTag", hasTagFn)
		_ = obj.Set("is", hasTagFn)

		// Annotations: real JS array so `u.annotations.push(...)` works.
		_ = obj.Set("annotations", vm.NewArray())

		// Metrics — same shape the existing stdlib reads from, PLUS a
		// `latencyP(q)` method for ergonomic policy expressions.
		m := metrics[u.Id()]
		mObj := vm.NewObject()
		_ = mObj.Set("errorRate", m.ErrorRate)
		_ = mObj.Set("errorsTotal", m.ErrorsTotal)
		_ = mObj.Set("requestsTotal", m.RequestsTotal)
		_ = mObj.Set("throttledRate", m.ThrottledRate)
		_ = mObj.Set("misbehaviorRate", m.MisbehaviorRate)
		_ = mObj.Set("blockHeadLag", m.BlockHeadLag)
		_ = mObj.Set("finalizationLag", m.FinalizationLag)
		// Wall-clock lag — block-count × network block-time EMA. Zero
		// until the tracker has enough samples; policies relying on
		// `*LagSeconds` should AND with `samplesAbove(N)` or similar
		// if they want to avoid no-oping during boot.
		_ = mObj.Set("blockHeadLagSeconds", m.BlockHeadLagSeconds)
		_ = mObj.Set("finalizationLagSeconds", m.FinalizationLagSeconds)
		_ = mObj.Set("p50ResponseSeconds", m.P50ResponseSeconds)
		_ = mObj.Set("p70ResponseSeconds", m.P70ResponseSeconds)
		_ = mObj.Set("p90ResponseSeconds", m.P90ResponseSeconds)
		_ = mObj.Set("p95ResponseSeconds", m.P95ResponseSeconds)
		_ = mObj.Set("p99ResponseSeconds", m.P99ResponseSeconds)
		if m.CordonedReason != "" {
			_ = mObj.Set("cordonedReason", m.CordonedReason)
		}

		// latencyP(quantile) → ms. Accepts 95 or 0.95 (auto-normalized).
		// Snaps to nearest precomputed bucket — adequate for policy
		// predicates and avoids exposing the live QuantileTracker
		// across the JS bridge. Closure captures the precomputed values
		// for this tick, no escape of engine-owned state.
		p50, p70 := m.P50ResponseSeconds, m.P70ResponseSeconds
		p90, p95, p99 := m.P90ResponseSeconds, m.P95ResponseSeconds, m.P99ResponseSeconds
		_ = mObj.Set("latencyP", func(call sobek.FunctionCall) sobek.Value {
			if len(call.Arguments) == 0 {
				return vm.ToValue(0.0)
			}
			q := call.Argument(0).ToFloat()
			if q > 1 {
				q = q / 100.0
			}
			var sec float64
			switch {
			case q <= 0.50:
				sec = p50
			case q <= 0.70:
				sec = p70
			case q <= 0.90:
				sec = p90
			case q <= 0.95:
				sec = p95
			default:
				sec = p99
			}
			return vm.ToValue(sec * 1000.0)
		})

		_ = obj.Set("metrics", mObj)
		_ = arr.Set(strconv.Itoa(i), obj)
	}
	_ = arr.Set("length", vm.ToValue(len(ups)))
	return arr
}

// runEval executes the compiled eval program against the snapshot.
// Returns the ordered upstream IDs the eval produced AND the per-upstream
// `score` map the JS attached during scoring (via `sortByScore(...)`).
// The scores map is what makes the engine the single source of truth
// for policy ranking — diagnostics no longer need to re-implement the
// BALANCED weight formula in Go (drift risk, e.g. p90 vs p70 quantile).
// Entries missing from the map mean "this upstream was added after the
// scoring step" (probeExcluded / forceInclude) and has no comparable
// score.
func runEval(
	pool *runtimePool,
	cfg *common.SelectionPolicyConfig,
	ups []common.Upstream,
	metrics map[string]UpstreamMetrics,
	evalCtx EvalContext,
) ([]string, map[string]float64, error) {
	if cfg.CompiledProgram == nil {
		return nil, nil, fmt.Errorf("selectionPolicy.eval has no compiled program")
	}

	rt, err := pool.acquire()
	if err != nil {
		return nil, nil, fmt.Errorf("acquire sobek runtime: %w", err)
	}
	defer pool.release(rt)

	vm := rt.VM()

	// Run the compiled program → the result is the eval function itself.
	fnValue, err := vm.RunProgram(cfg.CompiledProgram)
	if err != nil {
		return nil, nil, fmt.Errorf("evaluate program: %w", err)
	}
	fn, ok := sobek.AssertFunction(fnValue)
	if !ok {
		return nil, nil, fmt.Errorf("%w: expression did not evaluate to a function", ErrInvalidReturn)
	}

	upsValue := buildJSUpstreams(vm, ups, metrics)
	ctxValue := vm.ToValue(evalCtx)

	// Per-tick globals consumed by the stdlib (probeExcluded reads the full
	// input universe; sticky/cooldown read ctx). Cleared on the way out so
	// the pooled runtime cannot leak references across ticks.
	if err := vm.GlobalObject().Set("__policyCtx", ctxValue); err != nil {
		return nil, nil, err
	}
	if err := vm.GlobalObject().Set("__policyAllUpstreams", upsValue); err != nil {
		return nil, nil, err
	}
	defer func() {
		_ = vm.GlobalObject().Set("__policyCtx", sobek.Undefined())
		_ = vm.GlobalObject().Set("__policyAllUpstreams", sobek.Undefined())
	}()

	result, err := fn(sobek.Undefined(), upsValue, ctxValue)
	if err != nil {
		return nil, nil, fmt.Errorf("call eval: %w", err)
	}

	return extractOrderedResult(vm, result, ups)
}

// extractOrderedResult validates that `result` is an array of upstreams
// whose ids match the input set. Returns IDs in returned order AND a
// map of id → score (from each entry's `.score` field, set by the JS
// `sortByScore` step). Score map omits entries that don't have a
// `.score` field (typically: probed/forced-included upstreams).
func extractOrderedResult(vm *sobek.Runtime, result sobek.Value, ups []common.Upstream) ([]string, map[string]float64, error) {
	if result == nil || sobek.IsUndefined(result) || sobek.IsNull(result) {
		return nil, nil, fmt.Errorf("%w: eval returned null/undefined", ErrInvalidReturn)
	}
	obj, ok := result.(*sobek.Object)
	if !ok {
		return nil, nil, fmt.Errorf("%w: eval returned %T", ErrInvalidReturn, result)
	}

	known := make(map[string]bool, len(ups))
	for _, u := range ups {
		known[u.Id()] = true
	}

	lengthV := obj.Get("length")
	if lengthV == nil {
		return nil, nil, fmt.Errorf("%w: eval result is not an array (no length)", ErrInvalidReturn)
	}
	length := int(lengthV.ToInteger())

	ids := make([]string, 0, length)
	scores := make(map[string]float64, length)
	for i := 0; i < length; i++ {
		entry := obj.Get(fmt.Sprintf("%d", i))
		if entry == nil || sobek.IsUndefined(entry) {
			continue
		}
		entryObj, ok := entry.(*sobek.Object)
		if !ok {
			return nil, nil, fmt.Errorf("%w: result entry %d is not an object", ErrInvalidReturn, i)
		}
		idVal := entryObj.Get("id")
		if idVal == nil {
			return nil, nil, fmt.Errorf("%w: result entry %d has no id", ErrInvalidReturn, i)
		}
		id := idVal.String()
		if !known[id] {
			return nil, nil, fmt.Errorf("%w: result entry %d has unknown id %q", ErrInvalidReturn, i, id)
		}
		ids = append(ids, id)
		if sv := entryObj.Get("score"); sv != nil && !sobek.IsUndefined(sv) && !sobek.IsNull(sv) {
			scores[id] = sv.ToFloat()
		}
	}
	return ids, scores, nil
}
