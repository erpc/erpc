package common

import (
	"context"
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

// Context markers for selection reasons only the spawning executor knows.
// The attempt recorder (upstream tryForward's deferred record) runs at the
// bottom of the failsafe chain and cannot see WHICH executor caused this
// attempt to exist, so fan-out executors tag the context they hand each
// attempt: the consensus executor marks every participant slot
// (consensus_slot) and the network sweep marks its non-first picks (sweep).
// Keys follow the repo's ContextKey convention (see RequestContextKey).

// ConsensusSlotContextKey marks a context executing inside one consensus
// participant slot.
const ConsensusSlotContextKey ContextKey = "consensusSlot"

// SweepIterationContextKey marks a context executing a non-first pick of
// the try-all-upstreams sweep within one execution.
const SweepIterationContextKey ContextKey = "sweepIteration"

// WithConsensusSlot returns ctx marked as a consensus participant slot.
func WithConsensusSlot(ctx context.Context) context.Context {
	return context.WithValue(ctx, ConsensusSlotContextKey, true)
}

// IsConsensusSlot reports whether ctx executes inside a consensus
// participant slot.
func IsConsensusSlot(ctx context.Context) bool {
	v, _ := ctx.Value(ConsensusSlotContextKey).(bool)
	return v
}

// WithSweepIteration returns ctx marked as a non-first sweep pick.
func WithSweepIteration(ctx context.Context) context.Context {
	return context.WithValue(ctx, SweepIterationContextKey, true)
}

// IsSweepIteration reports whether ctx executes a non-first pick of the
// try-all-upstreams sweep.
func IsSweepIteration(ctx context.Context) bool {
	v, _ := ctx.Value(SweepIterationContextKey).(bool)
	return v
}

// UpstreamAttempt is one (upstream, attempt) record. The executors
// append these as participants come and go so operators can answer
// "which upstreams were involved in this request, why were they
// chosen, and what happened to them?" without parsing trace data.
//
// Won is flipped to true by the executor when this attempt's response
// contributed to the final response returned to the client. For a
// non-consensus request that's exactly one attempt (the winning one);
// for consensus it's every participant whose vote landed in the
// winning agreement group.
type UpstreamAttempt struct {
	UpstreamId  string
	VendorName  string
	StartedAt   time.Time
	Duration    time.Duration
	Outcome     UpstreamAttemptOutcome
	Reason      UpstreamSelectionReason
	IsHedge     bool
	IsRetry     bool
	Won         bool   // true when this attempt contributed to the response
	AttemptIdx  int    // 0-based attempt index within the parent loop
	ErrorCode   string // ErrorCode string when Outcome is an error variant
	ErrorDetail string // free-form short description (truncated)
	// CreditUnits is the vendor credit-unit cost this attempt accrued
	// (the upstream's resolved table — vendor defaults merged with config
	// overrides; vendors with no table default to a flat 1 credit per
	// request). 0 when the attempt provably never dialed the vendor
	// (skipped / breaker-open) or the vendor was opted out ("*": 0).
	CreditUnits int64
}

// ExecState centralizes the per-request execution counters and the
// per-upstream attempt log. Created lazily on first access via
// (*NormalizedRequest).ExecState().
//
// All counters are atomic; the struct itself is safe for concurrent use.
//
// Counter model — every executor increments its OWN scope only.
// Snapshot derives the totals so "forgot to increment the total" is
// impossible by construction. The derivation is NOT a flat sum because
// the scopes are nested: each network rotation triggers exactly one
// upstream invocation chain, so summing both would double-count
// physical attempts.
//
//	total Attempts = UpstreamAttempts + CacheAttempts
//	    (every physical call is counted at the deepest scope that
//	    actually performed it — upstreams for HTTP, cache for connector
//	    reads. NetworkAttempts is a separate rotation-count signal,
//	    exposed as its own counter but NOT summed into the total.)
//
//	total Retries = sum of UpstreamRetries + NetworkRetries + CacheRetries
//	total Hedges  = sum of UpstreamHedges  + NetworkHedges  + CacheHedges
//	    (retries and hedges ARE different events at each scope — an
//	    upstream-scope retry retries the SAME upstream, a network-scope
//	    retry rotates to a NEW upstream. Summing is correct.)
//
// Scope semantics:
//   - UpstreamAttempts: physical Forward calls to a single upstream's
//     transport (primary + retries + hedges within one upstream).
//   - NetworkAttempts: rotations across upstreams driven by the
//     network executor's retry / hedge / consensus loop. Each rotation
//     triggers one upstream invocation chain. Not summed into total.
//   - CacheAttempts: cache-connector reads/writes including
//     within-connector retries and hedges.
type ExecState struct {
	// Per-scope counters. Each executor owns its OWN counter set and
	// MUST NOT touch another scope's counters.
	UpstreamAttempts atomic.Int32
	UpstreamRetries  atomic.Int32
	UpstreamHedges   atomic.Int32

	NetworkAttempts atomic.Int32
	NetworkRetries  atomic.Int32
	NetworkHedges   atomic.Int32

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

// UpstreamAttemptLog returns a copy of the recorded participation log
// (one entry per physical Forward attempt). Safe to call concurrently
// with RecordUpstreamAttempt.
func (s *ExecState) UpstreamAttemptLog() []UpstreamAttempt {
	if s == nil {
		return nil
	}
	s.upstreamMu.Lock()
	defer s.upstreamMu.Unlock()
	out := make([]UpstreamAttempt, len(s.upstreamAttempts))
	copy(out, s.upstreamAttempts)
	return out
}

// MarkUpstreamAttemptWon flags the most-recent attempt on the given
// upstream as Won (its response contributed to the final response).
// Called by executors at the moment a winning response is selected:
//   - Network executor: once per request, with the kept response's upstream id
//   - Consensus executor: once per participant in the winning agreement group
//
// Walks the log from the end since the most-recent attempt is always
// the one that produced the kept response. A no-op when no matching
// attempt exists.
func (s *ExecState) MarkUpstreamAttemptWon(upstreamId string) {
	if s == nil || upstreamId == "" {
		return
	}
	s.upstreamMu.Lock()
	defer s.upstreamMu.Unlock()
	for i := len(s.upstreamAttempts) - 1; i >= 0; i-- {
		if s.upstreamAttempts[i].UpstreamId == upstreamId {
			s.upstreamAttempts[i].Won = true
			return
		}
	}
}

// ExecStateSnapshot is a plain-int view of ExecState for log/span
// labeling — captured at a point in time. Total Attempts/Retries/Hedges
// are derived as the sum of per-scope counters at snapshot time.
type ExecStateSnapshot struct {
	// Totals (derived: Upstream + Network + Cache).
	Attempts int
	Retries  int
	Hedges   int

	// Per-scope counters (each executor's own bookkeeping).
	UpstreamAttempts         int
	UpstreamRetries          int
	UpstreamHedges           int
	NetworkAttempts          int
	NetworkRetries           int
	NetworkHedges            int
	CacheAttempts            int
	CacheRetries             int
	CacheHedges              int
	ConsensusSlots           int
	ConsensusDisputes        int
	ConsensusLowParticipants int

	StartedAt time.Time
}

// Snapshot returns a plain-int view of the current counters. Each Load
// is independent — under heavy concurrency the totals may briefly drift
// from the sum of components mid-snapshot. The intended use is
// observability emission (headers / spans / metrics) where eventual
// consistency is acceptable.
//
// Totals are derived; see the ExecState doc comment for the model.
func (s *ExecState) Snapshot() ExecStateSnapshot {
	if s == nil {
		return ExecStateSnapshot{}
	}
	upAttempts := int(s.UpstreamAttempts.Load())
	upRetries := int(s.UpstreamRetries.Load())
	upHedges := int(s.UpstreamHedges.Load())
	nwAttempts := int(s.NetworkAttempts.Load())
	nwRetries := int(s.NetworkRetries.Load())
	nwHedges := int(s.NetworkHedges.Load())
	chAttempts := int(s.CacheAttempts.Load())
	chRetries := int(s.CacheRetries.Load())
	chHedges := int(s.CacheHedges.Load())
	return ExecStateSnapshot{
		// Total Attempts = physical operations. NetworkAttempts is a
		// rotation count and is NOT summed (each rotation triggers one
		// upstream invocation chain, already counted in UpstreamAttempts).
		Attempts: upAttempts + chAttempts,
		// Retries and Hedges ARE distinct events per scope; safe to sum.
		Retries: upRetries + nwRetries + chRetries,
		Hedges:  upHedges + nwHedges + chHedges,
		UpstreamAttempts:         upAttempts,
		UpstreamRetries:          upRetries,
		UpstreamHedges:           upHedges,
		NetworkAttempts:          nwAttempts,
		NetworkRetries:           nwRetries,
		NetworkHedges:            nwHedges,
		CacheAttempts:            chAttempts,
		CacheRetries:             chRetries,
		CacheHedges:              chHedges,
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
// In addition to the counter triplet, Apply emits the per-attempt
// upstream participation log as parallel slices indexed identically:
//
//   - upstreams.attempts: int count of recorded attempts
//   - upstreams.tried: upstream IDs in order
//   - upstreams.outcomes: outcome per attempt
//   - upstreams.reasons: selection reason per attempt
//   - upstreams.durations_ms: duration per attempt
//   - upstreams.won: bool per attempt (true = contributed to response)
//
// Operators reading a trace can answer "which upstreams were involved,
// why were they chosen, what happened, and which one(s) actually
// contributed to the response" without enumerating child spans.
func (s *ExecState) Apply(span trace.Span) {
	if s == nil || span == nil {
		return
	}
	snap := s.Snapshot()
	span.SetAttributes(
		attribute.Int("execution.attempts", snap.Attempts),
		attribute.Int("execution.retries", snap.Retries),
		attribute.Int("execution.hedges", snap.Hedges),
		attribute.Int("execution.upstream_attempts", snap.UpstreamAttempts),
		attribute.Int("execution.upstream_retries", snap.UpstreamRetries),
		attribute.Int("execution.upstream_hedges", snap.UpstreamHedges),
		attribute.Int("execution.network_attempts", snap.NetworkAttempts),
		attribute.Int("execution.network_retries", snap.NetworkRetries),
		attribute.Int("execution.network_hedges", snap.NetworkHedges),
		attribute.Int("execution.cache_attempts", snap.CacheAttempts),
		attribute.Int("execution.cache_retries", snap.CacheRetries),
		attribute.Int("execution.cache_hedges", snap.CacheHedges),
	)
	attempts := s.UpstreamAttemptLog()
	if len(attempts) == 0 {
		return
	}
	tried := make([]string, len(attempts))
	outcomes := make([]string, len(attempts))
	reasons := make([]string, len(attempts))
	durations := make([]int64, len(attempts))
	won := make([]bool, len(attempts))
	for i, a := range attempts {
		tried[i] = a.UpstreamId
		outcomes[i] = string(a.Outcome)
		reasons[i] = string(a.Reason)
		durations[i] = a.Duration.Milliseconds()
		won[i] = a.Won
	}
	span.SetAttributes(
		attribute.Int("upstreams.attempts", len(attempts)),
		attribute.StringSlice("upstreams.tried", tried),
		attribute.StringSlice("upstreams.outcomes", outcomes),
		attribute.StringSlice("upstreams.reasons", reasons),
		attribute.Int64Slice("upstreams.durations_ms", durations),
		attribute.BoolSlice("upstreams.won", won),
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
