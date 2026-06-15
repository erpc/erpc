package policy

import "time"

// Decision is the per-tick value the slot threads through metrics
// emission. In production, investigations go through:
//
//   - traces (per-request span attributes — `policy.selected_upstream`,
//     `policy.tick_id`)
//   - metrics (`policy_selection_*` Prometheus families in telemetry/metrics.go)
//   - structured logs (slot.go logs eval failures with full context)
//
// Diagnostic tooling (`erpc-simulator`, admin readouts) ALSO reads the
// slot's bounded ring buffer of recent decisions via
// `Engine.RecentDecisions(network, method, limit)`. The buffer size
// is small (a few dozen entries) — production callers should still
// rely on traces / metrics for anything beyond "last N ticks".
type Decision struct {
	ID           string
	NetworkID    string
	Method       string
	TickAt       time.Time
	EvalDuration time.Duration

	Input  DecisionInput
	Output DecisionOutput
	State  DecisionState
	Diff   DecisionDiff

	// Eval error (timeout, throw, invalid_return, ...).
	Error string
}

// DecisionInput captures the per-upstream metric snapshot the eval saw.
// Built fresh each tick; never persisted.
type DecisionInput struct {
	UpstreamIDs []string
	Metrics     map[string]UpstreamMetrics
}

// DecisionOutput is what the eval produced for this tick.
type DecisionOutput struct {
	Order    []string
	Excluded []ExcludedUpstream

	// Scores[upstreamID] is the per-upstream score the JS attached
	// during `sortByScore(...)`. Higher = better. Missing for upstreams
	// added after the scoring step (probeExcluded / forceInclude) and
	// for policies without a sortByScore step. Carried per-decision so
	// historical ticks in the diagnostic ring still answer "what did
	// the policy rank this upstream at on tick T?"
	Scores map[string]float64

	// StepLog is the per-step trail the JS stdlib recorded during the
	// chain's evaluation. One entry per chainable stdlib method
	// invoked, in chain order. Nil when the engine's step-log toggle
	// is off (production default; flipped on by the simulator and by
	// DEBUG-level eRPC callers). Read by diagnostic surfaces — never
	// the request path.
	StepLog []StepEntry

	// ShadowReasons[upstreamID] is the leaf slug(s) the upstream WOULD
	// have been dropped for, had the predicate been written as `excludeIf`
	// instead of `shadowExcludeIf`. The upstream stays in rotation; the
	// slugs drive `erpc_selection_shadow_exclusion_total` so operators can
	// audition a new (or removed) rule in production before flipping it
	// for real. Always captured (production-default), not gated by the
	// step-log toggle.
	ShadowReasons map[string][]string
}

// ExcludedUpstream explains why an upstream was dropped this tick.
// Surfaces as a Prometheus counter label (`policy_selection_rejection_total`).
type ExcludedUpstream struct {
	ID     string
	Reason string
	Step   string
	// ProbeEligible is the resolved verdict-matrix outcome for this
	// upstream: true when shadow probing can help it re-admit (some
	// probe-eligible step excluded it and no probe-blocking step did,
	// or it was excluded by untracked means). False means the prober
	// skips it (e.g. static tag exclusion, cordon).
	ProbeEligible bool
	// LeafReasons is the stable metric-label slug(s) attributing this
	// exclusion. Populated by `excludeIf`'s leaf-walk against compound
	// predicates: `any(A,B)` excluding because A trips gives `[A.slug]`;
	// `all(A,B)` gives `[A.slug, B.slug]`; `not(A)` gives `[not_<A.slug>]`.
	// Drives `erpc_selection_exclusion_total{reason=<slug>}` with one
	// increment per slug — operators see WHICH signal actually caused the
	// drop, not the combinator boilerplate. Empty for upstreams excluded
	// outside the `excludeIf` path (e.g. dropped by `removeCordoned`,
	// `removeByErrorRate`, ...) — those still surface via `Reason`.
	LeafReasons []string
}

// DecisionState mirrors the cross-tick fields of EvalContext as seen
// at the start of the tick (BEFORE any update from this tick's result).
// Lives in the slot for one tick at a time, then is copied into the
// EvalContext passed into JS.
type DecisionState struct {
	PreviousOrder    []string
	PreviousExcluded []string
	LastSwitchAt     *time.Time
	ExcludedSince    map[string]int64
	TickCount        uint64
}

// DecisionDiff is a delta against the previous tick's output. Used to
// drive `policy_selection_primary_switch_total` and similar metrics.
type DecisionDiff struct {
	OrderChanged   bool
	PrimaryChanged bool
	Added          []string
	Removed        []string
	// StickyHeld is true when `stickyPrimary` ACTIVELY held the previous
	// primary this tick (challenger would have won without sticky, but
	// cooldown or hysteresis kept the incumbent). Drives
	// `erpc_selection_sticky_hold_total{upstream=<incumbent>}`. The ratio
	// against `selection_primary_switch_total` shows how much smoothing
	// the chain is doing.
	StickyHeld bool
}

// UpstreamMetrics is the metric snapshot the eval sees for one upstream.
// Mirrors §3.1 of the spec. Built per-tick from health.Tracker.
//
// Lag is exposed in TWO units:
//   * blockHeadLag / finalizationLag — count of blocks behind the network's
//     latest / finalized tip. Chain-agnostic but unitless for time-based
//     decisions (a 16-block lag means 4 min on Eth mainnet but ~32 s on a
//     2 s chain).
//   * blockHeadLagSeconds / finalizationLagSeconds — same lag multiplied
//     by the tracker's EMA-estimated block time for the network. Zero
//     until the tracker has enough samples to estimate block time
//     (typically a few seconds after first traffic). Use these when the
//     trip threshold should be wall-clock (e.g. "trip if more than 60 s
//     behind tip") rather than chain-relative.
type UpstreamMetrics struct {
	ErrorRate              float64 `json:"errorRate"`
	ErrorsTotal            int64   `json:"errorsTotal"`
	RequestsTotal          int64   `json:"requestsTotal"`
	ThrottledRate          float64 `json:"throttledRate"`
	MisbehaviorRate        float64 `json:"misbehaviorRate"`
	P50ResponseSeconds     float64 `json:"p50ResponseSeconds"`
	P70ResponseSeconds     float64 `json:"p70ResponseSeconds"`
	P90ResponseSeconds     float64 `json:"p90ResponseSeconds"`
	P95ResponseSeconds     float64 `json:"p95ResponseSeconds"`
	P99ResponseSeconds     float64 `json:"p99ResponseSeconds"`
	BlockHeadLag           int64   `json:"blockHeadLag"`
	FinalizationLag        int64   `json:"finalizationLag"`
	BlockHeadLagSeconds    float64 `json:"blockHeadLagSeconds"`
	FinalizationLagSeconds float64 `json:"finalizationLagSeconds"`
	CordonedReason         string  `json:"cordonedReason,omitempty"`
}
