package common

import (
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// UpstreamAttemptOutcome enumerates the possible per-attempt outcomes
// recorded against an upstream. The set is closed: every attempt ends
// in exactly one of these.
type UpstreamAttemptOutcome string

const (
	UpstreamOutcomeSuccess          UpstreamAttemptOutcome = "success"
	UpstreamOutcomeEmpty            UpstreamAttemptOutcome = "empty"
	UpstreamOutcomeTransportError   UpstreamAttemptOutcome = "transport_error"
	UpstreamOutcomeServerError      UpstreamAttemptOutcome = "server_error"
	UpstreamOutcomeClientError      UpstreamAttemptOutcome = "client_error"
	UpstreamOutcomeRateLimited      UpstreamAttemptOutcome = "rate_limited"
	UpstreamOutcomeMissingData      UpstreamAttemptOutcome = "missing_data"
	UpstreamOutcomeExecRevert       UpstreamAttemptOutcome = "exec_revert"
	UpstreamOutcomeBlockUnavailable UpstreamAttemptOutcome = "block_unavailable"
	UpstreamOutcomeBreakerOpen      UpstreamAttemptOutcome = "breaker_open"
	UpstreamOutcomeCancelled        UpstreamAttemptOutcome = "cancelled"
	UpstreamOutcomeTimeout          UpstreamAttemptOutcome = "timeout"
	UpstreamOutcomeSkipped          UpstreamAttemptOutcome = "skipped"
)

// UpstreamSelectionReason describes WHY a particular upstream was
// selected for a given attempt. Operators use this to debug skew in
// upstream-pick distribution (e.g. why is one upstream getting all
// the hedge fan-out?).
type UpstreamSelectionReason string

const (
	SelectionReasonPrimary       UpstreamSelectionReason = "primary"        // initial pick
	SelectionReasonRetry         UpstreamSelectionReason = "retry"          // network-scope retry
	SelectionReasonHedge         UpstreamSelectionReason = "hedge"          // speculative hedge fan-out
	SelectionReasonConsensusSlot UpstreamSelectionReason = "consensus_slot" // one consensus participant
	SelectionReasonSweep         UpstreamSelectionReason = "sweep"          // try-all-upstreams iteration
)

// UpstreamAttempt is one (upstream, attempt) record. The executors
// append these as participants come and go so operators can answer
// "which upstreams were involved in this request, why were they
// chosen, and what happened to them?" without parsing trace data.
type UpstreamAttempt struct {
	UpstreamId  string
	VendorName  string
	StartedAt   time.Time
	Duration    time.Duration
	Outcome     UpstreamAttemptOutcome
	Reason      UpstreamSelectionReason
	IsHedge     bool
	IsRetry     bool
	AttemptIdx  int    // 0-based attempt index within the parent loop
	ErrorCode   string // ErrorCode string when Outcome is an error variant
	ErrorDetail string // free-form short description (truncated)
}

// ExecState centralizes the per-request execution counters and the
// per-upstream attempt log. Created lazily on first access via
// (*NormalizedRequest).ExecState().
//
// All counters are atomic; the struct itself is safe for concurrent use.
type ExecState struct {
	// Attempts counts every Forward attempt: the primary + any retry +
	// any hedge.
	Attempts atomic.Int32
	// Retries counts retry attempts only (Attempts - 1 - Hedges).
	Retries atomic.Int32
	// Hedges counts the number of hedge fires (1 = the primary plus one
	// hedge ran in parallel).
	Hedges atomic.Int32

	// NetworkAttempts / NetworkRetries / NetworkHedges track the same
	// metrics at the network scope. The upstream scope's counters are
	// the bare Attempts/Retries/Hedges above.
	NetworkAttempts atomic.Int32
	NetworkRetries  atomic.Int32
	NetworkHedges   atomic.Int32

	// CacheAttempts / CacheRetries / CacheHedges track cache-layer
	// retries and hedges.
	CacheAttempts atomic.Int32
	CacheRetries  atomic.Int32
	CacheHedges   atomic.Int32

	// ConsensusSlots counts how many consensus participants ran.
	ConsensusSlots atomic.Int32
	// ConsensusDisputes counts dispute events.
	ConsensusDisputes atomic.Int32
	// ConsensusLowParticipants counts low-participant events.
	ConsensusLowParticipants atomic.Int32

	StartedAt time.Time

	// upstreamAttempts records every (upstream, attempt) tuple in the
	// order they were started. Append-only; protected by upstreamMu so
	// the slice is safe across concurrent participant goroutines.
	upstreamMu       sync.Mutex
	upstreamAttempts []UpstreamAttempt
}

// RecordUpstreamAttempt appends a participant record. Called by the
// executors at the boundary of every upstream call.
func (s *ExecState) RecordUpstreamAttempt(a UpstreamAttempt) {
	if s == nil {
		return
	}
	s.upstreamMu.Lock()
	defer s.upstreamMu.Unlock()
	s.upstreamAttempts = append(s.upstreamAttempts, a)
}

// UpstreamAttempts returns a copy of the recorded attempts. Safe to
// call concurrently with RecordUpstreamAttempt.
func (s *ExecState) UpstreamAttempts() []UpstreamAttempt {
	if s == nil {
		return nil
	}
	s.upstreamMu.Lock()
	defer s.upstreamMu.Unlock()
	out := make([]UpstreamAttempt, len(s.upstreamAttempts))
	copy(out, s.upstreamAttempts)
	return out
}

// ExecStateSnapshot is a plain-int view of ExecState for log/span
// labeling — captured at a point in time.
type ExecStateSnapshot struct {
	Attempts                 int
	Retries                  int
	Hedges                   int
	NetworkAttempts          int
	NetworkRetries           int
	NetworkHedges            int
	CacheAttempts            int
	CacheRetries             int
	CacheHedges              int
	ConsensusSlots           int
	ConsensusDisputes        int
	ConsensusLowParticipants int
	StartedAt                time.Time
}

// Snapshot returns a plain-int view of the current counters.
func (s *ExecState) Snapshot() ExecStateSnapshot {
	if s == nil {
		return ExecStateSnapshot{}
	}
	return ExecStateSnapshot{
		Attempts:                 int(s.Attempts.Load()),
		Retries:                  int(s.Retries.Load()),
		Hedges:                   int(s.Hedges.Load()),
		NetworkAttempts:          int(s.NetworkAttempts.Load()),
		NetworkRetries:           int(s.NetworkRetries.Load()),
		NetworkHedges:            int(s.NetworkHedges.Load()),
		CacheAttempts:            int(s.CacheAttempts.Load()),
		CacheRetries:             int(s.CacheRetries.Load()),
		CacheHedges:              int(s.CacheHedges.Load()),
		ConsensusSlots:           int(s.ConsensusSlots.Load()),
		ConsensusDisputes:        int(s.ConsensusDisputes.Load()),
		ConsensusLowParticipants: int(s.ConsensusLowParticipants.Load()),
		StartedAt:                s.StartedAt,
	}
}

// Apply sets the standard execution.* attributes on a span. Callers
// should invoke this once at the boundary of network.Forward (success
// or error) instead of setting attributes manually.
//
// In addition to the counter triplet, Apply emits a compact per-attempt
// trace of upstream participation:
//   - upstreams.attempts: int count of recorded attempts
//   - upstreams.tried: comma-separated upstream IDs in order
//   - upstreams.outcomes: comma-separated outcome strings in order
//   - upstreams.reasons: comma-separated selection-reason strings
//   - upstreams.durations_ms: comma-separated duration-ms ints
//
// Operators reading a trace can answer "which upstreams were involved,
// why were they chosen, and what happened" without enumerating child
// spans.
func (s *ExecState) Apply(span trace.Span) {
	if s == nil || span == nil {
		return
	}
	snap := s.Snapshot()
	span.SetAttributes(
		attribute.Int("execution.attempts", snap.Attempts),
		attribute.Int("execution.retries", snap.Retries),
		attribute.Int("execution.hedges", snap.Hedges),
		attribute.Int("execution.network_attempts", snap.NetworkAttempts),
		attribute.Int("execution.network_retries", snap.NetworkRetries),
		attribute.Int("execution.network_hedges", snap.NetworkHedges),
	)
	attempts := s.UpstreamAttempts()
	if len(attempts) == 0 {
		return
	}
	tried := make([]string, len(attempts))
	outcomes := make([]string, len(attempts))
	reasons := make([]string, len(attempts))
	durations := make([]int64, len(attempts))
	for i, a := range attempts {
		tried[i] = a.UpstreamId
		outcomes[i] = string(a.Outcome)
		reasons[i] = string(a.Reason)
		durations[i] = a.Duration.Milliseconds()
	}
	span.SetAttributes(
		attribute.Int("upstreams.attempts", len(attempts)),
		attribute.StringSlice("upstreams.tried", tried),
		attribute.StringSlice("upstreams.outcomes", outcomes),
		attribute.StringSlice("upstreams.reasons", reasons),
		attribute.Int64Slice("upstreams.durations_ms", durations),
	)
}

// execStateOnce is embedded on NormalizedRequest to lazy-init the
// ExecState struct without making every request pay the allocation when
// the field is never accessed.
type execStateHolder struct {
	once sync.Once
	st   *ExecState
}

func (h *execStateHolder) get() *ExecState {
	h.once.Do(func() {
		h.st = &ExecState{StartedAt: time.Now()}
	})
	return h.st
}
