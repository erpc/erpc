package policy

import (
	"fmt"
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
type healthTracker interface {
	GetUpstreamMethodMetrics(up common.Upstream, method string) *health.TrackedMetrics
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
	if m.Cordoned.Load() {
		if r, ok := m.LastCordonedReason.Load().(string); ok {
			out.CordonedReason = r
		}
	}
	return out
}

// jsUpstream is the JS-visible representation of one upstream. We use a
// plain struct (no pointer to the original Go Upstream) so the eval cannot
// mutate engine-owned state. The std-lib chainable methods build arrays of
// these.
//
// Annotations is pre-allocated (non-nil empty slice) so std-lib steps that
// `u.annotations.push(...)` from JS don't trip on undefined.
type jsUpstream struct {
	ID          string          `json:"id"`
	Vendor      string          `json:"vendor"`
	Type        string          `json:"type"`
	Group       string          `json:"group,omitempty"`
	Cohort      string          `json:"cohort,omitempty"`
	Metrics     UpstreamMetrics `json:"metrics"`
	Annotations []string        `json:"annotations"`

	Score *float64 `json:"score,omitempty"`
}

func buildJSUpstreams(ups []common.Upstream, metrics map[string]UpstreamMetrics) []jsUpstream {
	out := make([]jsUpstream, len(ups))
	for i, u := range ups {
		out[i] = jsUpstream{
			ID:          u.Id(),
			Vendor:      u.VendorName(),
			Metrics:     metrics[u.Id()],
			Annotations: []string{},
		}
		if cfg := u.Config(); cfg != nil {
			out[i].Type = string(cfg.Type)
			out[i].Group = cfg.Group
			out[i].Cohort = cfg.Cohort
		}
	}
	return out
}

// runEval executes the compiled eval program against the snapshot. Returns
// the ordered upstream IDs the eval produced. Std-lib bindings (Phase 5)
// are intentionally NOT installed here yet — the default policy used today
// is `(upstreams, ctx) => upstreams`, which works against bare JS objects.
func runEval(
	pool *runtimePool,
	cfg *common.SelectionPolicyConfig,
	ups []common.Upstream,
	metrics map[string]UpstreamMetrics,
	evalCtx EvalContext,
) ([]string, error) {
	if cfg.CompiledProgram == nil {
		return nil, fmt.Errorf("selectionPolicy.eval has no compiled program")
	}

	rt, err := pool.acquire()
	if err != nil {
		return nil, fmt.Errorf("acquire sobek runtime: %w", err)
	}
	defer pool.release(rt)

	vm := rt.VM()

	// Run the compiled program → the result is the eval function itself.
	fnValue, err := vm.RunProgram(cfg.CompiledProgram)
	if err != nil {
		return nil, fmt.Errorf("evaluate program: %w", err)
	}
	fn, ok := sobek.AssertFunction(fnValue)
	if !ok {
		return nil, fmt.Errorf("%w: expression did not evaluate to a function", ErrInvalidReturn)
	}

	upsArr := buildJSUpstreams(ups, metrics)
	upsValue := vm.ToValue(upsArr)
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
	defer func() {
		_ = vm.GlobalObject().Set("__policyCtx", sobek.Undefined())
		_ = vm.GlobalObject().Set("__policyAllUpstreams", sobek.Undefined())
	}()

	result, err := fn(sobek.Undefined(), upsValue, ctxValue)
	if err != nil {
		return nil, fmt.Errorf("call eval: %w", err)
	}

	return extractOrderedIDs(vm, result, ups)
}

// extractOrderedIDs validates that `result` is an array of upstreams whose
// ids match the input set. Returns the IDs in returned order.
func extractOrderedIDs(vm *sobek.Runtime, result sobek.Value, ups []common.Upstream) ([]string, error) {
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
	for i := 0; i < length; i++ {
		entry := obj.Get(fmt.Sprintf("%d", i))
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
	}
	return ids, nil
}
