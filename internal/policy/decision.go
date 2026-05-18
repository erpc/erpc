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

	// StepLog is the per-step trail the JS stdlib recorded during the
	// chain's evaluation. One entry per chainable stdlib method
	// invoked, in chain order. Nil when the engine's step-log toggle
	// is off (production default; flipped on by the simulator and by
	// DEBUG-level eRPC callers). Read by diagnostic surfaces — never
	// the request path.
	StepLog []StepEntry

	// Annotations[upstreamID] is the ordered list of `annotate(u, note)`
	// strings each step attached to the upstream during this tick.
	// Populated under the same toggle as `StepLog`. Used to surface
	// per-upstream "why was this dropped / kept / re-ordered" rationale
	// in the simulator's policy-history drawer and in DEBUG logs.
	Annotations map[string][]string
}

// ExcludedUpstream explains why an upstream was dropped this tick.
// Surfaces as a Prometheus counter label (`policy_selection_rejection_total`).
type ExcludedUpstream struct {
	ID     string
	Reason string
	Step   string
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
