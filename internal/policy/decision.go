package policy

import (
	"sync"
	"time"
)

// Decision is the structured record of a single tick of a slot's eval.
// Retained in the slot's ring buffer for `selectionPolicy.decisionHistory`
// and exposed via admin endpoints. See spec §6.
type Decision struct {
	// Stable identifier — network/method/tickUnixMillis. Joined to request
	// logs via `decision_id` so operators can answer "why was upstream X
	// used at time T?".
	ID string `json:"id"`

	NetworkID string    `json:"network"`
	Method    string    `json:"method"`
	TickAt    time.Time `json:"tickAt"`

	EvalDuration time.Duration `json:"evalDurationMs"`

	// Input snapshot: every upstream considered, with the metric snapshot
	// the eval was run against.
	Input DecisionInput `json:"input"`

	// Output: the ordered upstream IDs the eval returned, plus rejection
	// reasons for any input not in the output.
	Output DecisionOutput `json:"output"`

	// Cross-tick state at the START of this tick.
	State DecisionState `json:"state"`

	// Diff vs the previous tick's decision.
	Diff DecisionDiff `json:"diff"`

	// Any error from the eval (timeout, throw, invalid_return, ...).
	Error string `json:"error,omitempty"`
}

// DecisionInput captures the per-upstream metric snapshot the eval saw.
type DecisionInput struct {
	UpstreamIDs []string                    `json:"upstreamIds"`
	Metrics     map[string]UpstreamMetrics  `json:"metrics"`
	Annotations map[string][]string         `json:"annotations,omitempty"`
}

// DecisionOutput is what the eval produced.
type DecisionOutput struct {
	Order    []string             `json:"order"`
	Excluded []ExcludedUpstream   `json:"excluded,omitempty"`
}

// ExcludedUpstream explains why an upstream was dropped.
type ExcludedUpstream struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
	Step   string `json:"step,omitempty"`
}

// DecisionState mirrors the cross-tick fields of EvalContext as seen
// at the start of the tick (BEFORE any update from this tick's result).
type DecisionState struct {
	PreviousOrder    []string         `json:"previousOrder,omitempty"`
	PreviousExcluded []string         `json:"previousExcluded,omitempty"`
	LastSwitchAt     *time.Time       `json:"lastSwitchAt,omitempty"`
	ExcludedSince    map[string]int64 `json:"excludedSince,omitempty"`
	TickCount        uint64           `json:"tickCount"`
}

// DecisionDiff is a delta against the previous tick's output.
type DecisionDiff struct {
	OrderChanged    bool     `json:"orderChanged"`
	PrimaryChanged  bool     `json:"primaryChanged"`
	Added           []string `json:"added,omitempty"`
	Removed         []string `json:"removed,omitempty"`
}

// UpstreamMetrics is the metric snapshot the eval saw for one upstream.
// Mirrors §3.1 of the spec.
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

// ringBuffer is a fixed-capacity, FIFO buffer of decision pointers.
// Older entries are overwritten when full. Per the spec, the slot sizes
// it from `decisionHistory / evalInterval`.
type ringBuffer struct {
	mu   sync.RWMutex
	data []*Decision
	next int  // index of the next slot to write
	full bool // true once we've wrapped at least once
}

func newRingBuffer(capacity int) *ringBuffer {
	if capacity < 1 {
		capacity = 1
	}
	return &ringBuffer{data: make([]*Decision, capacity)}
}

func (r *ringBuffer) append(d *Decision) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.data[r.next] = d
	r.next = (r.next + 1) % len(r.data)
	if r.next == 0 {
		r.full = true
	}
}

// snapshot returns a chronologically-ordered copy (oldest → newest).
func (r *ringBuffer) snapshot() []*Decision {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := r.next
	if r.full {
		n = len(r.data)
	}
	out := make([]*Decision, 0, n)
	if !r.full {
		out = append(out, r.data[:r.next]...)
		return out
	}
	out = append(out, r.data[r.next:]...)
	out = append(out, r.data[:r.next]...)
	return out
}

// latest returns the most recently appended decision, or nil.
func (r *ringBuffer) latest() *Decision {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.full && r.next == 0 {
		return nil
	}
	idx := (r.next - 1 + len(r.data)) % len(r.data)
	return r.data[idx]
}
