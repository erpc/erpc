package failsafe

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
)

// State is the circuit breaker state machine.
type State int8

const (
	StateClosed State = iota
	StateOpen
	StateHalfOpen
)

func (s State) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half_open"
	default:
		return "unknown"
	}
}

// Outcome is the classifier return type. The integration site (per-scope
// executor) decides which outcome to record after each attempt.
type Outcome int8

const (
	// OutcomeIgnore — the attempt does not move the breaker's counters.
	// Used for cancellations, capacity issues, internal probes, hedges,
	// or any other "not a real signal" event.
	OutcomeIgnore Outcome = iota
	// OutcomeSuccess — record a success in the ring buffer.
	OutcomeSuccess
	// OutcomeFailure — record a failure in the ring buffer.
	OutcomeFailure
)

// ErrCircuitOpen is the sentinel returned by callers when the breaker
// refuses a permit. It is intentionally NOT a *common.BaseError so that
// the call-site retains the choice of how to wrap it for the public
// error type.
type errCircuitOpen struct{}

func (errCircuitOpen) Error() string { return "circuit breaker is open" }

// ErrCircuitOpen is the value-typed sentinel for callers to compare
// against (`errors.Is`).
var ErrCircuitOpen error = errCircuitOpen{}

// Breaker is a policy-free circuit breaker state machine. It does NOT
// know which errors count as failures — the caller decides via the
// Outcome argument to Record.
type Breaker struct {
	cfg    *common.CircuitBreakerPolicyConfig
	logger *zerolog.Logger
	// OnTransition fires (without holding the breaker mutex) every
	// time the state machine moves between states. Callers wire this
	// to metric/trace emission outside the failsafe/ package.
	OnTransition func(from, to State, reason string)

	mu sync.Mutex

	state atomic.Int32 // State

	// Ring buffer of recent outcomes in the active window. true = failure,
	// false = success. We track length explicitly because the buffer is
	// pre-allocated.
	results []bool
	head    int
	count   int

	// failures / successes count the booleans in `results`. They are kept
	// in sync with the buffer for O(1) ratio checks.
	failures  int
	successes int

	// HalfOpen trial budget — how many permits we've granted in HalfOpen.
	halfOpenInflight int
	halfOpenSuccess  int
	halfOpenFailure  int

	// When the breaker opened, used to gate transition Open → HalfOpen.
	openedAt time.Time

	// Lifetime counters for the Metrics() inspector.
	totalExecutions atomic.Uint64
	totalSuccesses  atomic.Uint64
	totalFailures   atomic.Uint64
}

// NewBreaker constructs a Breaker from config. The logger is captured
// for direct state-change emission — there is no listener registry.
//
// When cfg is nil the returned *Breaker is also nil — callers should
// treat a nil breaker as "no breaker policy", i.e. always permit.
func NewBreaker(cfg *common.CircuitBreakerPolicyConfig, logger *zerolog.Logger) *Breaker {
	if cfg == nil {
		return nil
	}
	cap := int(cfg.FailureThresholdCapacity)
	if cap <= 0 {
		cap = int(cfg.FailureThresholdCount)
	}
	if cap <= 0 {
		cap = 1
	}
	b := &Breaker{
		cfg:     cfg,
		logger:  logger,
		results: make([]bool, cap),
	}
	b.state.Store(int32(StateClosed))
	return b
}

// TryAcquirePermit returns true if the caller may execute. Closed always
// permits; HalfOpen permits up to the success-threshold capacity in
// flight; Open permits if the half-open delay has elapsed (and atomically
// transitions to HalfOpen).
func (b *Breaker) TryAcquirePermit() bool {
	if b == nil {
		return true
	}
	switch State(b.state.Load()) {
	case StateClosed:
		return true
	case StateHalfOpen:
		b.mu.Lock()
		defer b.mu.Unlock()
		if State(b.state.Load()) != StateHalfOpen {
			return true
		}
		// Limit concurrent trial permits to the success-threshold capacity
		// (or count if capacity not set).
		cap := int(b.cfg.SuccessThresholdCapacity)
		if cap <= 0 {
			cap = int(b.cfg.SuccessThresholdCount)
		}
		if cap <= 0 {
			cap = 1
		}
		if b.halfOpenInflight >= cap {
			return false
		}
		b.halfOpenInflight++
		return true
	case StateOpen:
		b.mu.Lock()
		defer b.mu.Unlock()
		if State(b.state.Load()) != StateOpen {
			// Raced — re-evaluate. Recurse safely (max one extra hop).
			b.mu.Unlock()
			defer b.mu.Lock()
			return b.TryAcquirePermit()
		}
		delay := b.cfg.HalfOpenAfter.Duration()
		if delay <= 0 || time.Since(b.openedAt) >= delay {
			b.transitionLocked(StateHalfOpen, "half_open_delay_elapsed")
			b.halfOpenInflight = 1
			return true
		}
		return false
	}
	return true
}

// Record applies the given outcome to the breaker's state machine.
// OutcomeIgnore is a no-op.
func (b *Breaker) Record(o Outcome) {
	if b == nil || o == OutcomeIgnore {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	b.totalExecutions.Add(1)
	state := State(b.state.Load())

	switch state {
	case StateClosed:
		b.pushLocked(o == OutcomeFailure)
		if o == OutcomeSuccess {
			b.totalSuccesses.Add(1)
		} else {
			b.totalFailures.Add(1)
		}
		b.checkOpenLocked()
	case StateHalfOpen:
		if b.halfOpenInflight > 0 {
			b.halfOpenInflight--
		}
		if o == OutcomeSuccess {
			b.halfOpenSuccess++
			b.totalSuccesses.Add(1)
		} else {
			b.halfOpenFailure++
			b.totalFailures.Add(1)
		}
		successCap := int(b.cfg.SuccessThresholdCapacity)
		if successCap <= 0 {
			successCap = int(b.cfg.SuccessThresholdCount)
		}
		if successCap <= 0 {
			successCap = 1
		}
		successCount := int(b.cfg.SuccessThresholdCount)
		if successCount <= 0 {
			successCount = 1
		}
		// Threshold check: if we've accumulated enough trials and successes hit count, close.
		if b.halfOpenSuccess+b.halfOpenFailure >= successCap {
			if b.halfOpenSuccess >= successCount {
				b.resetWindowLocked()
				b.transitionLocked(StateClosed, "half_open_success_threshold")
			} else {
				b.openedAt = time.Now()
				b.transitionLocked(StateOpen, "half_open_failure")
			}
			b.halfOpenSuccess = 0
			b.halfOpenFailure = 0
		} else if o == OutcomeFailure && b.halfOpenFailure > 0 {
			// Single failure in HalfOpen immediately re-opens.
			b.openedAt = time.Now()
			b.transitionLocked(StateOpen, "half_open_failure")
			b.halfOpenSuccess = 0
			b.halfOpenFailure = 0
		}
	case StateOpen:
		// Should not normally happen — caller bypassed TryAcquirePermit.
		// Still record lifetime counters.
		if o == OutcomeSuccess {
			b.totalSuccesses.Add(1)
		} else {
			b.totalFailures.Add(1)
		}
	}
}

// pushLocked appends a result to the ring buffer, evicting the oldest
// if full. Updates the failure/success counts to stay consistent.
func (b *Breaker) pushLocked(isFailure bool) {
	if len(b.results) == 0 {
		return
	}
	if b.count == len(b.results) {
		// Evict head.
		old := b.results[b.head]
		if old {
			b.failures--
		} else {
			b.successes--
		}
		b.results[b.head] = isFailure
		b.head = (b.head + 1) % len(b.results)
	} else {
		idx := (b.head + b.count) % len(b.results)
		b.results[idx] = isFailure
		b.count++
	}
	if isFailure {
		b.failures++
	} else {
		b.successes++
	}
}

// checkOpenLocked transitions Closed → Open if the failure threshold has
// been reached. Capacity not yet met → keep collecting.
func (b *Breaker) checkOpenLocked() {
	failCap := int(b.cfg.FailureThresholdCapacity)
	if failCap <= 0 {
		failCap = int(b.cfg.FailureThresholdCount)
	}
	if failCap <= 0 {
		return
	}
	failCount := int(b.cfg.FailureThresholdCount)
	if failCount <= 0 {
		return
	}
	if b.count < failCap {
		return
	}
	if b.failures >= failCount {
		b.openedAt = time.Now()
		b.resetWindowLocked()
		b.transitionLocked(StateOpen, "failure_threshold")
	}
}

func (b *Breaker) resetWindowLocked() {
	for i := range b.results {
		b.results[i] = false
	}
	b.head = 0
	b.count = 0
	b.failures = 0
	b.successes = 0
}

func (b *Breaker) transitionLocked(to State, reason string) {
	from := State(b.state.Load())
	if from == to {
		return
	}
	b.state.Store(int32(to))
	if b.logger != nil {
		b.logger.Warn().
			Str("from", from.String()).
			Str("to", to.String()).
			Str("reason", reason).
			Uint64("executions", b.totalExecutions.Load()).
			Uint64("successes", b.totalSuccesses.Load()).
			Uint64("failures", b.totalFailures.Load()).
			Msg("circuit breaker state changed")
	}
	if b.OnTransition != nil {
		// Fire without holding b.mu — caller side-effects must not
		// recurse into the breaker.
		hook := b.OnTransition
		go hook(from, to, reason)
	}
}

// State returns the current state. Read-only.
func (b *Breaker) State() State {
	if b == nil {
		return StateClosed
	}
	return State(b.state.Load())
}

// Metrics returns lifetime counts. Read-only.
func (b *Breaker) Metrics() (failures, successes, executions uint64) {
	if b == nil {
		return 0, 0, 0
	}
	return b.totalFailures.Load(), b.totalSuccesses.Load(), b.totalExecutions.Load()
}
