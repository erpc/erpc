package policy

import (
	"encoding/json"
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
func buildJSUpstreams(vm *sobek.Runtime, ups []common.Upstream, metrics map[string]UpstreamMetrics, evalCtx EvalContext) sobek.Value {
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

		// scoreMultipliers — per-upstream weight overrides resolved for
		// THIS tick's (network, method, finality). Attached only when an
		// entry matches and carries at least one weight; `sortByScore`
		// reads it (`u.scoreMultipliers`) and merges it over the base
		// weights. ApplyDefaults inherits `upstreamDefaults.routing` onto
		// upstreams that didn't set their own, so the resolver is
		// single-source: only `u.Routing` is consulted here.
		if cfg := u.Config(); cfg != nil && cfg.Routing != nil {
			if mul := resolveScoreMultipliers(cfg.Routing, evalCtx.Network, evalCtx.Method, evalCtx.Finality); len(mul) > 0 {
				smObj := vm.NewObject()
				for k, v := range mul {
					_ = smObj.Set(k, v)
				}
				_ = obj.Set("scoreMultipliers", smObj)
			}
		}

		_ = arr.Set(strconv.Itoa(i), obj)
	}
	_ = arr.Set("length", vm.ToValue(len(ups)))
	return arr
}

// resolveScoreMultipliers selects the first `routing.scoreMultipliers`
// entry whose matchers fit the current (network, method, finality) and
// returns its weights as a flat map keyed by the JS-side metric names
// (errorRate, respLatency, throttledRate, blockHeadLag, finalizationLag,
// misbehaviors) plus `overall`. Only fields the operator actually set are
// included, so `sortByScore` can distinguish "unset → inherit base" from
// "explicitly zero → drop this metric".
//
// Matcher semantics — all three must match; first matching entry wins:
//   - network / method: glob via common.WildcardMatch; empty or "*" == any.
//     A per-method entry only takes effect when the policy runs per-method
//     (evalPerMethod=true); otherwise ctx.method is the "*" wildcard slot
//     and only empty/"*" method entries apply. Likewise per-finality
//     entries need evalPerFinality=true.
//   - finality: membership in the entry's list; empty == any.
//
// Returns nil when routing is nil, has no entries, or nothing matches.
// `upstreamDefaults.routing` inheritance is handled at config-load time by
// ApplyDefaults (all-or-nothing), so this function is single-source.
func resolveScoreMultipliers(routing *common.UpstreamRoutingConfig, network, method, finality string) map[string]float64 {
	if routing == nil || len(routing.ScoreMultipliers) == 0 {
		return nil
	}
	for _, sm := range routing.ScoreMultipliers {
		if sm == nil {
			continue
		}
		if !scoreMatcherMatches(sm.Network, network) ||
			!scoreMatcherMatches(sm.Method, method) ||
			!finalityListMatches(sm.Finality, finality) {
			continue
		}
		out := make(map[string]float64, 7)
		setIf := func(key string, p *float64) {
			if p != nil {
				out[key] = *p
			}
		}
		setIf("overall", sm.Overall)
		setIf("errorRate", sm.ErrorRate)
		setIf("respLatency", sm.RespLatency)
		setIf("throttledRate", sm.ThrottledRate)
		setIf("blockHeadLag", sm.BlockHeadLag)
		setIf("finalizationLag", sm.FinalizationLag)
		setIf("misbehaviors", sm.Misbehaviors)
		// First matching entry wins even if it carries no weights — that's
		// a deliberate no-op override that shadows broader entries below it.
		if len(out) == 0 {
			return nil
		}
		return out
	}
	return nil
}

// scoreMatcherMatches treats empty / "*" as match-any and otherwise defers
// to common.WildcardMatch (the same glob engine used for method/network
// matching elsewhere). A malformed pattern fails closed (no match).
func scoreMatcherMatches(pattern, value string) bool {
	if pattern == "" || pattern == "*" {
		return true
	}
	m, err := common.WildcardMatch(pattern, value)
	return err == nil && m
}

// finalityListMatches returns true if `finality` is in the entry's list,
// or the list is empty (match-any).
func finalityListMatches(list []common.DataFinalityState, finality string) bool {
	if len(list) == 0 {
		return true
	}
	for _, f := range list {
		if f.String() == finality {
			return true
		}
	}
	return false
}

// StepEntry is one row in the per-tick step-trail the stdlib pushes
// while the JS chain executes. Captured Go-side from `__policyStepLog`
// after `runEval`. Used by diagnostic surfaces (admin endpoints, the
// simulator's "policy history" detail view) and by DEBUG-level eRPC
// logging.
//
// Wire-format-friendly: every field JSON-marshals into a stable shape.
// `Args` is held as raw JSON because the JS-side captures arbitrary
// shallow objects (predicate reasons, tag patterns, score presets) —
// we don't want to flatten through a Go struct that'd lose fields.
type StepEntry struct {
	Step      string          `json:"step"`
	Args      json.RawMessage `json:"args,omitempty"`
	InIDs     []string        `json:"inIds"`
	OutIDs    []string        `json:"outIds"`
	Dropped   []string        `json:"dropped,omitempty"`
	Added     []string        `json:"added,omitempty"`
	Reordered bool            `json:"reordered,omitempty"`
}

// EvalResult bundles everything `runEval` produces from one tick — the
// chain's final order plus the diagnostic trail. Returning a struct
// instead of a 4-tuple keeps the slot caller readable and gives the
// step-log/annotation channels somewhere to grow.
type EvalResult struct {
	OrderedIDs []string
	Scores     map[string]float64
	// Annotations[upstreamID] = ordered list of `annotate(u, note)`
	// strings the chain attached to this upstream. Empty when step-log
	// recording is disabled (production-default), populated when the
	// caller flips the toggle (simulator, DEBUG logger).
	Annotations map[string][]string
	// StepLog is the chronological trail of stdlib steps invoked
	// during the eval. Order follows JS chain order. Empty when
	// step-log recording is disabled.
	StepLog []StepEntry
	// LeafReasons[upstreamID] = stable metric-label slugs ("error_rate_above",
	// "latency_p95_above", "block_head_lag_above", "not_<slug>", ...) for
	// each leaf predicate that contributed to dropping this upstream.
	// Always populated when `excludeIf` dropped the upstream (NOT gated by
	// stepLogEnabled — these drive a Prometheus counter, not a diagnostic
	// surface). One slug per leaf — `any(A,B)` excluding because A trips
	// gives `[A.slug]`; `all(A,B)` gives `[A.slug, B.slug]`; `not(A)`
	// gives `[not_<A.slug>]`. Cardinality bounded by the set of predicate
	// factories (~25). See option (c) in the metrics design doc.
	LeafReasons map[string][]string
	// ShadowReasons[upstreamID] = leaf slugs for predicates that would have
	// dropped the upstream had they been written as `excludeIf` instead of
	// `shadowExcludeIf`. Same slug shape as LeafReasons, same option-(c)
	// attribution semantics — drives `erpc_selection_shadow_exclusion_total`
	// so operators can audition a new (or removed) rule in production
	// before flipping it for real.
	ShadowReasons map[string][]string
	// StickyHeld is true when `stickyPrimary` ACTIVELY held the previous
	// primary this tick (i.e. challenger would have won under a no-sticky
	// ordering, but cooldown or hysteresis kept the incumbent). Drives
	// `erpc_selection_sticky_hold_total{upstream=<held primary>}`.
	StickyHeld bool
}

// runEval executes the compiled eval program against the snapshot.
// Returns the ordered upstream IDs the eval produced AND the per-upstream
// `score` map the JS attached during scoring (via `sortByScore(...)`).
// The scores map is what makes the engine the single source of truth
// for policy ranking — diagnostics no longer need to re-implement the
// PREFER_FASTEST weight formula in Go (drift risk, e.g. p90 vs p70 quantile).
// Entries missing from the map mean "this upstream was added after the
// scoring step" (probeExcluded / forceInclude) and has no comparable
// score.
//
// `stepLogEnabled` toggles the per-step trail. When true, each stdlib
// step pushes an entry into a JS global that this function reads back
// and surfaces in `EvalResult.StepLog` along with each upstream's
// `.annotations`. When false (production default), the JS wrapper
// fast-paths the recording entirely — zero overhead beyond the
// function-call indirection.
func runEval(
	pool *runtimePool,
	cfg *common.SelectionPolicyConfig,
	ups []common.Upstream,
	metrics map[string]UpstreamMetrics,
	evalCtx EvalContext,
	stepLogEnabled bool,
) (*EvalResult, error) {
	tsSentinel := common.IsTSFunctionSentinel(cfg.EvalFunc)
	if !tsSentinel && cfg.CompiledProgram == nil {
		return nil, fmt.Errorf("selectionPolicy.eval has no compiled program")
	}

	rt, err := pool.acquire()
	if err != nil {
		return nil, fmt.Errorf("acquire sobek runtime: %w", err)
	}
	defer pool.release(rt)

	vm := rt.VM()

	var fn sobek.Callable
	if tsSentinel {
		// TS path: the user's whole module was already evaluated in this
		// runtime by the pool primer, and the function is registered on
		// `globalThis.__erpcFns[id]` with its native closure scope.
		id := common.TSFunctionSentinelID(cfg.EvalFunc)
		fnsRaw := vm.GlobalObject().Get("__erpcFns")
		if fnsRaw == nil || sobek.IsUndefined(fnsRaw) || sobek.IsNull(fnsRaw) {
			return nil, fmt.Errorf("ts selectionPolicy.evalFunc lookup: __erpcFns registry not populated (user script did not run?)")
		}
		fnsObj := fnsRaw.ToObject(vm)
		if fnsObj == nil {
			return nil, fmt.Errorf("ts selectionPolicy.evalFunc lookup: __erpcFns is not an object")
		}
		fnValue := fnsObj.Get(id)
		if fnValue == nil || sobek.IsUndefined(fnValue) || sobek.IsNull(fnValue) {
			return nil, fmt.Errorf("ts selectionPolicy.evalFunc lookup: id %q not found in __erpcFns", id)
		}
		var ok bool
		fn, ok = sobek.AssertFunction(fnValue)
		if !ok {
			return nil, fmt.Errorf("%w: __erpcFns[%q] is not a function", ErrInvalidReturn, id)
		}
	} else {
		// YAML path: compile-once Program; running it returns the
		// (parens-wrapped) arrow expression which evaluates to the
		// function value.
		fnValue, err := vm.RunProgram(cfg.CompiledProgram)
		if err != nil {
			return nil, fmt.Errorf("evaluate program: %w", err)
		}
		var ok bool
		fn, ok = sobek.AssertFunction(fnValue)
		if !ok {
			return nil, fmt.Errorf("%w: expression did not evaluate to a function", ErrInvalidReturn)
		}
	}

	upsValue := buildJSUpstreams(vm, ups, metrics, evalCtx)
	ctxValue := vm.ToValue(evalCtx)

	// Per-tick globals consumed by the stdlib (probeExcluded reads the full
	// input universe; sticky/cooldown read ctx). Cleared on the way out so
	// the pooled runtime cannot leak references across ticks.
	if err := vm.GlobalObject().Set("__policyCtx", ctxValue); err != nil {
		return nil, err
	}
	if err := vm.GlobalObject().Set("__policyAllUpstreams", upsValue); err != nil {
		return nil, err
	}
	// Step-log scaffolding — the stdlib wrapper around `define(name, fn)`
	// pushes one entry per chainable step when this flag is true.
	// Always reset the log to a fresh array each tick so a previous
	// tick's entries don't leak into this one if the runtime is reused.
	if err := vm.GlobalObject().Set("__policyStepLogEnabled", stepLogEnabled); err != nil {
		return nil, err
	}
	if err := vm.GlobalObject().Set("__policyStepLog", vm.NewArray()); err != nil {
		return nil, err
	}
	// Per-upstream leaf-reason slugs populated by `excludeIf`. Reset to
	// a fresh empty object each tick. Drives the
	// `erpc_selection_exclusion_total{reason}` metric with option (c)
	// leaf attribution — `any(A,B)` excluding via A attributes to A's slug.
	// Unlike StepLog, this is ALWAYS captured (it's a metric input, not a
	// diagnostic-only payload), so it ignores `stepLogEnabled`.
	if err := vm.GlobalObject().Set("__policyLeafReasons", vm.NewObject()); err != nil {
		return nil, err
	}
	// Per-upstream "would-have-been-excluded" leaf slugs from
	// `shadowExcludeIf`. Reset each tick; the metric emitter consumes it
	// to drive `erpc_selection_shadow_exclusion_total`. Always captured
	// (production-default), independent of `stepLogEnabled`.
	if err := vm.GlobalObject().Set("__policyShadowReasons", vm.NewObject()); err != nil {
		return nil, err
	}
	// Cleared each tick to detect active stickyPrimary holds.
	if err := vm.GlobalObject().Set("__policyStickyHeld", false); err != nil {
		return nil, err
	}
	defer func() {
		_ = vm.GlobalObject().Set("__policyCtx", sobek.Undefined())
		_ = vm.GlobalObject().Set("__policyAllUpstreams", sobek.Undefined())
		_ = vm.GlobalObject().Set("__policyStepLogEnabled", false)
		_ = vm.GlobalObject().Set("__policyStepLog", sobek.Undefined())
		_ = vm.GlobalObject().Set("__policyLeafReasons", sobek.Undefined())
		_ = vm.GlobalObject().Set("__policyShadowReasons", sobek.Undefined())
		_ = vm.GlobalObject().Set("__policyStickyHeld", false)
	}()

	result, err := fn(sobek.Undefined(), upsValue, ctxValue)
	if err != nil {
		return nil, fmt.Errorf("call eval: %w", err)
	}

	out, err := extractOrderedResult(vm, result, ups)
	if err != nil {
		return nil, err
	}

	if stepLogEnabled {
		// Step log first — order matters: the chain's own reads happen on
		// the SAME JS objects whose annotations we'll capture next, but
		// the step log is a separate global so order's just cosmetic here.
		if steps, err := readStepLog(vm); err == nil {
			out.StepLog = steps
		}
		// Per-upstream annotations: read directly from the INPUT array's
		// objects. Every upstream we built with `buildJSUpstreams` got an
		// `annotations` array attached; we read it back regardless of
		// whether the upstream survived the chain. This captures BOTH
		// what stayed AND why each dropped upstream got dropped.
		out.Annotations = readAnnotations(vm, upsValue, ups)
	}

	// LeafReasons + ShadowReasons + StickyHeld are read every tick (not
	// gated by stepLogEnabled). They feed Prometheus counters in the
	// slot's emitMetrics, not the diagnostic-only step trail.
	out.LeafReasons = readReasonsMap(vm, "__policyLeafReasons")
	out.ShadowReasons = readReasonsMap(vm, "__policyShadowReasons")
	if heldVal := vm.GlobalObject().Get("__policyStickyHeld"); heldVal != nil && !sobek.IsUndefined(heldVal) && !sobek.IsNull(heldVal) {
		out.StickyHeld = heldVal.ToBoolean()
	}

	return out, nil
}

// readReasonsMap deserializes a JS-side `{ [upstreamId]: string[] }`
// global (populated by `excludeIf` → __policyLeafReasons or
// `shadowExcludeIf` → __policyShadowReasons) into a Go map. Empty map on
// missing / malformed input — attribution is best-effort and shouldn't
// break the eval if the JS-side wiring drifts.
func readReasonsMap(vm *sobek.Runtime, globalName string) map[string][]string {
	v := vm.GlobalObject().Get(globalName)
	if v == nil || sobek.IsUndefined(v) || sobek.IsNull(v) {
		return nil
	}
	obj, ok := v.(*sobek.Object)
	if !ok {
		return nil
	}
	keys := obj.Keys()
	if len(keys) == 0 {
		return nil
	}
	out := make(map[string][]string, len(keys))
	for _, k := range keys {
		arrVal := obj.Get(k)
		if arrVal == nil {
			continue
		}
		arrObj, ok := arrVal.(*sobek.Object)
		if !ok {
			continue
		}
		lenVal := arrObj.Get("length")
		if lenVal == nil {
			continue
		}
		n := int(lenVal.ToInteger())
		if n <= 0 {
			continue
		}
		slugs := make([]string, 0, n)
		for i := 0; i < n; i++ {
			item := arrObj.Get(fmt.Sprintf("%d", i))
			if item == nil || sobek.IsUndefined(item) || sobek.IsNull(item) {
				continue
			}
			s := item.String()
			if s == "" {
				continue
			}
			slugs = append(slugs, s)
		}
		if len(slugs) > 0 {
			out[k] = slugs
		}
	}
	return out
}

// readStepLog deserializes the JS-side `__policyStepLog` global into Go
// `[]StepEntry`. Tolerates a missing / non-array value (returns nil) —
// step-log is purely diagnostic and shouldn't break the request path.
func readStepLog(vm *sobek.Runtime) ([]StepEntry, error) {
	v := vm.GlobalObject().Get("__policyStepLog")
	if v == nil || sobek.IsUndefined(v) || sobek.IsNull(v) {
		return nil, nil
	}
	// Marshal the JS value to JSON then unmarshal into our struct. The
	// double-hop is cheap (the array is small — one entry per stdlib step
	// invocation, typically <30 per tick) and dodges the trickier sobek
	// "Export" path which loses untyped object fields.
	exported := v.Export()
	raw, err := json.Marshal(exported)
	if err != nil {
		return nil, err
	}
	var entries []StepEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// readAnnotations walks the JS-side input upstream array and pulls each
// element's `annotations` array into a Go map keyed by upstream ID. We
// read from the INPUT array, not the result array, so dropped upstreams
// are included — their annotation trail is exactly what explains why
// they were dropped.
func readAnnotations(vm *sobek.Runtime, upsValue sobek.Value, ups []common.Upstream) map[string][]string {
	if upsValue == nil {
		return nil
	}
	obj, ok := upsValue.(*sobek.Object)
	if !ok {
		return nil
	}
	out := make(map[string][]string, len(ups))
	for i := range ups {
		entry := obj.Get(strconv.Itoa(i))
		if entry == nil || sobek.IsUndefined(entry) {
			continue
		}
		entryObj, ok := entry.(*sobek.Object)
		if !ok {
			continue
		}
		idVal := entryObj.Get("id")
		annV := entryObj.Get("annotations")
		if idVal == nil || annV == nil {
			continue
		}
		id := idVal.String()
		exported := annV.Export()
		// `annotations` is a plain JS array of strings. sobek exports
		// arrays as `[]interface{}` — coerce to []string.
		raw, ok := exported.([]interface{})
		if !ok || len(raw) == 0 {
			continue
		}
		notes := make([]string, 0, len(raw))
		for _, x := range raw {
			if s, ok := x.(string); ok {
				notes = append(notes, s)
			}
		}
		if len(notes) > 0 {
			out[id] = notes
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// extractOrderedResult validates that `result` is an array of upstreams
// whose ids match the input set. Returns the ordered IDs AND a
// map of id → score (from each entry's `.score` field, set by the JS
// `sortByScore` step). Score map omits entries that don't have a
// `.score` field (typically: probed/forced-included upstreams).
func extractOrderedResult(vm *sobek.Runtime, result sobek.Value, ups []common.Upstream) (*EvalResult, error) {
	if result == nil || sobek.IsUndefined(result) || sobek.IsNull(result) {
		return nil, fmt.Errorf("%w: eval returned null/undefined", ErrInvalidReturn)
	}
	obj, ok := result.(*sobek.Object)
	if !ok {
		return nil, fmt.Errorf("%w: eval returned %T", ErrInvalidReturn, result)
	}

	known := make(map[string]bool, len(ups))
	for _, u := range ups {
		known[u.Id()] = true
	}

	lengthV := obj.Get("length")
	if lengthV == nil {
		return nil, fmt.Errorf("%w: eval result is not an array (no length)", ErrInvalidReturn)
	}
	length := int(lengthV.ToInteger())

	ids := make([]string, 0, length)
	scores := make(map[string]float64, length)
	for i := 0; i < length; i++ {
		entry := obj.Get(strconv.Itoa(i))
		if entry == nil || sobek.IsUndefined(entry) {
			continue
		}
		entryObj, ok := entry.(*sobek.Object)
		if !ok {
			return nil, fmt.Errorf("%w: result entry %d is not an object", ErrInvalidReturn, i)
		}
		idVal := entryObj.Get("id")
		if idVal == nil {
			return nil, fmt.Errorf("%w: result entry %d has no id", ErrInvalidReturn, i)
		}
		id := idVal.String()
		if !known[id] {
			return nil, fmt.Errorf("%w: result entry %d has unknown id %q", ErrInvalidReturn, i, id)
		}
		ids = append(ids, id)
		if sv := entryObj.Get("score"); sv != nil && !sobek.IsUndefined(sv) && !sobek.IsNull(sv) {
			scores[id] = sv.ToFloat()
		}
	}
	return &EvalResult{OrderedIDs: ids, Scores: scores}, nil
}
