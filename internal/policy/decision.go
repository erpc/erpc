package policy

import "time"

// Decision is the per-tick value the slot threads through metrics
// emission. It is NOT persisted: no ring buffer, no admin endpoint,
// no operator-visible retention knob. Investigations go through:
//
//   - traces (per-request span attributes — `policy.selected_upstream`,
//     `policy.tick_id`)
//   - metrics (`policy_selection_*` Prometheus families in telemetry/metrics.go)
//   - structured logs (slot.go logs eval failures with full context)
//
// If a future need genuinely requires per-tick records again, derive
// them from traces — don't add another in-memory mechanism.
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
type UpstreamMetrics struct {
	ErrorRate          float64 `json:"errorRate"`
	ErrorsTotal        int64   `json:"errorsTotal"`
	RequestsTotal      int64   `json:"requestsTotal"`
	ThrottledRate      float64 `json:"throttledRate"`
	MisbehaviorRate    float64 `json:"misbehaviorRate"`
	P50ResponseSeconds float64 `json:"p50ResponseSeconds"`
	P70ResponseSeconds float64 `json:"p70ResponseSeconds"`
	P90ResponseSeconds float64 `json:"p90ResponseSeconds"`
	P95ResponseSeconds float64 `json:"p95ResponseSeconds"`
	P99ResponseSeconds float64 `json:"p99ResponseSeconds"`
	BlockHeadLag       int64   `json:"blockHeadLag"`
	FinalizationLag    int64   `json:"finalizationLag"`
	CordonedReason     string  `json:"cordonedReason,omitempty"`
}
