package data

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

type SharedVariable interface {
	IsStale(staleness time.Duration) bool
}

type CounterInt64SharedVariable interface {
	SharedVariable
	GetValue() int64
	TryUpdateIfStale(ctx context.Context, staleness time.Duration, getNewValue func(ctx context.Context) (int64, error)) (int64, error)
	// TryUpdate reports newValue, its evidence taken at at (ReportAt).
	TryUpdate(ctx context.Context, newValue int64, at time.Time) int64
	OnValue(callback func(int64))
	OnLargeRollback(callback func(currentVal, newVal int64))
	// Rewind lowers a head counter to newValue, a hash-proven height (spec
	// §14 rollback write): any drop, no OnLargeRollback, published with a
	// rewind mark. It reports whether the value moved.
	Rewind(newValue int64) bool
	// OnRewind fires on a rewind, local or remote, before OnValue.
	OnRewind(callback func(newVal int64))
	// ReportAt is when the evidence for the counter's newest report was
	// taken, by this instance's clock (spec §6.4, §14): a fetch's or a
	// refresh's send time, since its answer can show no later value than
	// the source had then; a push's or a remote value's receive time. A
	// later report never dates it back. Zero before any.
	ReportAt() time.Time
}

type baseSharedVariable struct {
	// updatedAtUnixMs is the reconcile order key: the newest state's
	// UpdatedAt, by its writer's clock. It never decides freshness (spec
	// §14), since a writer's clock may run ahead of this instance's.
	//
	// Semantics: unix milliseconds, monotonic increasing within a process.
	updatedAtUnixMs atomic.Int64
}

func (v *baseSharedVariable) updatedAtMs() int64 {
	return v.updatedAtUnixMs.Load()
}

// allocateUpdatedAtMs allocates a strictly-increasing UpdatedAt timestamp (unix ms) for a local update.
// It also stores the allocated value into updatedAtUnixMs, ensuring uniqueness even under concurrency.
func (v *baseSharedVariable) allocateUpdatedAtMs() int64 {
	for {
		current := v.updatedAtUnixMs.Load()
		next := time.Now().UnixMilli()
		if next <= current {
			next = current + 1
		}
		if v.updatedAtUnixMs.CompareAndSwap(current, next) {
			return next
		}
	}
}

// updateUpdatedAtMs atomically advances updatedAtUnixMs if newUpdatedAt is newer.
// Returns true if updatedAtUnixMs was advanced.
func (v *baseSharedVariable) updateUpdatedAtMs(newUpdatedAt int64) bool {
	for {
		current := v.updatedAtUnixMs.Load()
		if newUpdatedAt <= current {
			return false
		}
		if v.updatedAtUnixMs.CompareAndSwap(current, newUpdatedAt) {
			return true
		}
	}
}

// advanceTimestampPast advances updatedAtUnixMs to be strictly greater than remoteTs.
// Uses current wall clock if it's newer, otherwise uses remoteTs + 1.
// This is used when rejecting a remote value (e.g., small rollback) but wanting to
// ensure our local state will "win" the next reconciliation and get pushed to remote.
func (v *baseSharedVariable) advanceTimestampPast(remoteTs int64) {
	for {
		current := v.updatedAtUnixMs.Load()
		// Explicit check: if already past remoteTs, no action needed
		if current > remoteTs {
			return
		}
		// current <= remoteTs, need to advance past remoteTs
		next := time.Now().UnixMilli()
		if next <= remoteTs {
			next = remoteTs + 1
		}
		// At this point: next > remoteTs (guaranteed)
		if v.updatedAtUnixMs.CompareAndSwap(current, next) {
			return
		}
		// CAS failed, another goroutine modified timestamp - retry
	}
}

// dropKind is how a head counter's value last moved down; any rise clears it.
type dropKind int32

const (
	dropNone dropKind = iota
	// dropRollback: a rollback larger than ignoreRollbackOf was applied.
	dropRollback
	// dropRewind: a rollback write (Rewind), local or remote; the state
	// carries the rewind mark.
	dropRewind
)

type counterInt64 struct {
	baseSharedVariable
	// reportAtUnixMs is ReportAt.
	reportAtUnixMs atomic.Int64
	// attemptAtUnixMs is this instance's clock at its last refresh attempt
	// that brought no report (an error or a timeout): it debounces the next
	// refresh as a report does.
	attemptAtUnixMs atomic.Int64
	// drop is a dropKind. While set, a higher state stamped no later than
	// the local one predates the drop: it is the branch the drop left, so
	// it wins neither over pubsub nor in reconcile.
	drop                   atomic.Int32
	registry               *sharedStateRegistry
	key                    string
	value                  atomic.Int64
	updateMu               sync.Mutex
	valueCallbacks         []func(int64)
	ignoreRollbackOf       int64
	largeRollbackCallbacks []func(localVal, newVal int64)
	rewindCallbacks        []func(newVal int64)
	callbackMu             sync.RWMutex
	// bgPushInProgress dedupes best-effort remote reconciliation/push operations.
	// It MUST NOT be held while waiting on updateMu; remote sync should never block request flow.
	bgPushInProgress sync.Mutex
	// bgPushRequested indicates a newer value should be pushed after the current push attempt.
	// This avoids dropping updates when scheduleBackgroundPushCurrent() is called while a push is in progress.
	bgPushRequested atomic.Bool
}

func (c *counterInt64) GetValue() int64 {
	return c.value.Load()
}

// IsStale: no report and no refresh attempt within staleness, by this
// instance's clock (spec §14).
func (c *counterInt64) IsStale(staleness time.Duration) bool {
	at := max(c.reportAtUnixMs.Load(), c.attemptAtUnixMs.Load())
	if at <= 0 {
		return true
	}
	return time.Since(time.UnixMilli(at)) > staleness
}

func (c *counterInt64) ReportAt() time.Time {
	if ms := c.reportAtUnixMs.Load(); ms > 0 {
		return time.UnixMilli(ms)
	}
	return time.Time{}
}

// markReported dates the counter's report at at, unless it already has a
// newer one: heads only move on, so newer evidence stands.
func (c *counterInt64) markReported(at time.Time) {
	ms := at.UnixMilli()
	for {
		cur := c.reportAtUnixMs.Load()
		if ms <= cur || c.reportAtUnixMs.CompareAndSwap(cur, ms) {
			return
		}
	}
}

func (c *counterInt64) dropped() bool { return dropKind(c.drop.Load()) != dropNone }

// setDrop records how the value moves: a drop before its CAS, so no
// snapshot pairs the lower value with dropNone; dropNone after a rise's.
func (c *counterInt64) setDrop(k dropKind) { c.drop.Store(int32(k)) }

// processNewState applies a remote state (pubsub, initial fetch, reconcile).
// Head counters (ignoreRollbackOf > 0: latest, finalized) are max-only on
// every path: a higher value wins whatever its timestamp, a drop of up to
// ignoreRollbackOf is noise, and timestamps decide only larger drops (spec
// §14, #1243, #222). Other counters (earliest, cordon, floors) take any
// change, in timestamp order.
//
// A rejected drop with a newer timestamp moves the order key past it, so
// the higher local value wins the next reconcile and goes back to remote.
func (c *counterInt64) processNewState(source string, st CounterInt64State) bool {
	isRemoteUpdate := st.UpdatedAt > 0
	newer := !isRemoteUpdate || st.UpdatedAt > c.updatedAtMs()
	if !isRemoteUpdate {
		st.UpdatedAt = c.allocateUpdatedAtMs()
	}

	currentValue := c.value.Load()
	newVal := st.Value

	if c.ignoreRollbackOf == 0 && !newer && newVal != currentValue {
		countRejected(rejectStaleTimestamp)
		return false
	}

	if newVal == currentValue {
		// Values appear equal - no value change needed.
		// For remote updates, we want to update timestamp since remote is fresher.
		// However, we must verify the value hasn't changed concurrently to avoid a TOCTOU race:
		// a concurrent TryUpdate could change the value between our Load() and this check,
		// causing us to incorrectly set the remote's stale timestamp for a different local value.
		if isRemoteUpdate {
			if c.updateUpdatedAtMs(st.UpdatedAt) {
				c.markReported(time.Now())
				// Timestamp was updated. Verify value is still what we expect.
				// If a concurrent update changed the value after our initial Load(),
				// we need to ensure our timestamp reflects that change, not the remote's stale state.
				if c.value.Load() != newVal {
					// Value changed concurrently - allocate fresh timestamp to ensure
					// local state wins in reconciliation. This maintains the invariant that
					// timestamps accurately reflect when values were last updated locally.
					c.allocateUpdatedAtMs()
				}
			}
		}
		return false
	}

	if newVal > currentValue {
		if !newer && c.dropped() {
			countRejected(rejectStaleTimestamp)
			return false
		}
		for {
			if c.value.CompareAndSwap(currentValue, newVal) {
				c.setDrop(dropNone)
				if isRemoteUpdate {
					c.updateUpdatedAtMs(st.UpdatedAt)
				}
				c.markReported(time.Now())
				c.registry.logger.Trace().
					Str("source", source).
					Str("key", c.key).
					Int64("from", currentValue).
					Int64("to", newVal).
					Int64("updatedAt", st.UpdatedAt).
					Msg("counter value increased (remote)")
				c.triggerValueCallback(newVal)
				return true
			}
			currentValue = c.value.Load()
			if newVal <= currentValue {
				return false
			}
		}
	}

	// A head counter never legitimately moves to zero or below. Zero is the zero
	// VALUE of an unset CounterInt64State, not an observation of a chain, and it
	// arrives here from ordinary sources: an instance that pushes its counter
	// before its first poll, or a connector handing back an empty record with a
	// live timestamp. Left alone it reads as a rollback the size of the whole
	// chain, which is necessarily larger than ignoreRollbackOf, so the branch
	// below -- whose entire job is to admit only real reorgs -- waves it through,
	// and every instance that receives it drops its head to 0.
	//
	// The floor is scoped to counters that ignore small rollbacks. The
	// earliest-block counter is created with ignoreRollbackOf=0 and legitimately
	// sits at 0 on an archive node, so it keeps the old behaviour.
	if newVal <= 0 && c.ignoreRollbackOf > 0 {
		// Same bookkeeping as a rejected small rollback: our higher local value
		// must out-rank the remote state, or reconciliation hands the zero back
		// to us on the next tick and the counter flaps.
		if isRemoteUpdate {
			c.advanceTimestampPast(st.UpdatedAt)
			countRejected(rejectNonPositive)
		}
		c.registry.logger.Warn().
			Str("source", source).
			Str("key", c.key).
			Int64("currentValue", currentValue).
			Int64("newVal", newVal).
			Msg("rejected non-positive value for a head counter (remote)")
		return false
	}

	// A rewind-marked drop newer than the local state is another instance's
	// rollback write: taken whatever its size, never pushed back, counted
	// against no upstream.
	if st.Rewind && c.ignoreRollbackOf > 0 {
		if !newer {
			countRejected(rejectStaleTimestamp)
			return false
		}
		return c.applyRewind(source, currentValue, newVal, st.UpdatedAt)
	}

	// Rollback handling: only accept rollbacks where gap exceeds the threshold.
	// - ignoreRollbackOf=0 (earliest): gap > 0 is always true, so ALL rollbacks accepted
	// - ignoreRollbackOf=1024 (latest/finalized): only accept large rollbacks (real reorgs)
	gap := currentValue - newVal

	if gap <= c.ignoreRollbackOf {
		// Small rollback within threshold - ignore as noise.
		// For remote updates with newer timestamps: advance our local timestamp past theirs.
		// This ensures our higher local value will "win" reconciliation and get pushed to remote,
		// since local.UpdatedAt will now be > remote.UpdatedAt.
		if isRemoteUpdate {
			c.advanceTimestampPast(st.UpdatedAt)
			countRejected(rejectSmallRollback)
		}
		c.registry.logger.Trace().
			Str("source", source).
			Str("key", c.key).
			Int64("currentValue", currentValue).
			Int64("newVal", newVal).
			Int64("gap", gap).
			Int64("ignoreRollbackOf", c.ignoreRollbackOf).
			Msg("small rollback ignored (remote)")
		return false
	}

	// Timestamps decide large drops: an older one is a state this instance
	// already moved past.
	if !newer {
		countRejected(rejectStaleTimestamp)
		return false
	}

	// Large rollback exceeds threshold - apply it and trigger callback
	c.setDrop(dropRollback)
	for {
		if c.value.CompareAndSwap(currentValue, newVal) {
			// Update timestamp AFTER accepting the value (for remote updates)
			if isRemoteUpdate {
				c.updateUpdatedAtMs(st.UpdatedAt)
			}
			c.markReported(time.Now())
			c.registry.logger.Trace().
				Str("source", source).
				Str("key", c.key).
				Int64("from", currentValue).
				Int64("to", newVal).
				Int64("gap", gap).
				Int64("updatedAt", st.UpdatedAt).
				Msg("large rollback applied (remote)")
			c.triggerValueCallback(newVal)

			// Trigger callback for observability
			c.callbackMu.RLock()
			callbacks := make([]func(localVal, newVal int64), len(c.largeRollbackCallbacks))
			copy(callbacks, c.largeRollbackCallbacks)
			c.callbackMu.RUnlock()
			for _, cb := range callbacks {
				if cb != nil {
					cb(currentValue, newVal)
				}
			}
			return true
		}
		currentValue = c.value.Load()
		if newVal >= currentValue {
			return false
		}
	}
}

// processNewValue handles local updates (TryUpdate, applyRefreshResult) where we don't have
// an external timestamp; at is when the value's evidence was taken (ReportAt). The ignoreRollbackOf parameter controls rollback behavior:
// - ignoreRollbackOf = 0: Accept all changes (used for earliest block - can decrease due to pruning)
// - ignoreRollbackOf > 0: Ignore small rollbacks (noise from lagging RPCs), trigger callback for large ones
//
// This is different from processNewState which uses timestamp ordering for remote reconciliation.
func (c *counterInt64) processNewValue(source string, newVal int64, at time.Time) bool {
	currentValue := c.value.Load()
	if newVal == currentValue {
		// Value is the same, but mark as fresh to prevent repeated refresh attempts
		c.allocateUpdatedAtMs()
		c.markReported(at)
		return false
	}

	if newVal > currentValue {
		// Forward progress: always accept
		for {
			if c.value.CompareAndSwap(currentValue, newVal) {
				c.setDrop(dropNone)
				c.allocateUpdatedAtMs() // Mark as fresh
				c.markReported(at)
				c.registry.logger.Trace().
					Str("source", source).
					Str("key", c.key).
					Int64("from", currentValue).
					Int64("to", newVal).
					Msg("counter value increased (local)")
				c.triggerValueCallback(newVal)
				return true
			}
			currentValue = c.value.Load()
			if newVal <= currentValue {
				return false
			}
		}
	}

	// See processNewState: zero is an unset state, not a head. Same floor, same
	// scoping to counters that ignore small rollbacks.
	if newVal <= 0 && c.ignoreRollbackOf > 0 {
		c.registry.logger.Warn().
			Str("source", source).
			Str("key", c.key).
			Int64("currentValue", currentValue).
			Int64("newVal", newVal).
			Msg("rejected non-positive value for a head counter (local)")
		return false
	}

	// Rollback handling: only accept rollbacks where gap exceeds the threshold.
	// - ignoreRollbackOf=0 (earliest): gap > 0 is always true, so ALL rollbacks accepted
	// - ignoreRollbackOf=1024 (latest/finalized): only accept large rollbacks (real reorgs)
	gap := currentValue - newVal

	if gap <= c.ignoreRollbackOf {
		// Small rollback within threshold - ignore as noise
		c.registry.logger.Trace().
			Str("source", source).
			Str("key", c.key).
			Int64("currentValue", currentValue).
			Int64("newVal", newVal).
			Int64("gap", gap).
			Int64("ignoreRollbackOf", c.ignoreRollbackOf).
			Msg("small rollback ignored (local)")
		return false
	}

	// Large rollback exceeds threshold - apply it and trigger callback
	c.setDrop(dropRollback)
	for {
		if c.value.CompareAndSwap(currentValue, newVal) {
			c.allocateUpdatedAtMs() // Mark as fresh
			c.markReported(at)
			c.registry.logger.Trace().
				Str("source", source).
				Str("key", c.key).
				Int64("from", currentValue).
				Int64("to", newVal).
				Int64("gap", gap).
				Msg("large rollback applied (local)")
			c.triggerValueCallback(newVal)

			// Trigger callback for observability
			c.callbackMu.RLock()
			callbacks := make([]func(localVal, newVal int64), len(c.largeRollbackCallbacks))
			copy(callbacks, c.largeRollbackCallbacks)
			c.callbackMu.RUnlock()
			for _, cb := range callbacks {
				if cb != nil {
					cb(currentValue, newVal)
				}
			}
			return true
		}
		currentValue = c.value.Load()
		if newVal >= currentValue {
			return false
		}
	}
}

func (c *counterInt64) triggerValueCallback(val int64) {
	c.callbackMu.RLock()
	callbacks := make([]func(int64), len(c.valueCallbacks))
	copy(callbacks, c.valueCallbacks)
	c.callbackMu.RUnlock()

	for _, cb := range callbacks {
		if cb != nil {
			cb(val)
		}
	}
}

func (c *counterInt64) TryUpdate(ctx context.Context, newValue int64, at time.Time) int64 {
	ctx, span := common.StartSpan(ctx, "CounterInt64.TryUpdate",
		trace.WithAttributes(
			attribute.String("key", c.key),
		),
	)
	defer span.End()
	if common.IsTracingDetailed {
		span.SetAttributes(
			attribute.Int64("new_value", newValue),
		)
	}
	// IMPORTANT: TryUpdate must never wait on updateMu.
	// updateMu exists to coordinate expensive refresh execution in TryUpdateIfStale (thundering herd control),
	// but local counter advancement must remain fast even if a refresh is in-flight.
	updated := c.processNewValue(UpdateSourceTryUpdate, newValue, at)

	// Schedule background push when value was actually updated (increase OR decrease).
	// With unified semantics, all value changes are propagated to remote.
	// Note: Value can be 0 for valid cases like earliest block = genesis.
	if updated {
		c.scheduleBackgroundPushCurrent()
	}
	return c.value.Load()
}

func (c *counterInt64) TryUpdateIfStale(ctx context.Context, staleness time.Duration, executeNewValueFn func(ctx context.Context) (int64, error)) (int64, error) {
	ctx, span := common.StartSpan(ctx, "CounterInt64.TryUpdateIfStale",
		trace.WithAttributes(
			attribute.String("key", c.key),
			attribute.Int64("staleness_ms", staleness.Milliseconds()),
		),
	)
	defer span.End()

	if !c.IsStale(staleness) {
		span.SetAttributes(attribute.Bool("skipped_not_stale", true))
		return c.value.Load(), nil
	}

	// Track time waiting for mutex
	_, mutexSpan := common.StartSpan(ctx, "CounterInt64.TryUpdateIfStale.AcquireMutex")
	c.updateMu.Lock()
	mutexSpan.End()
	defer c.updateMu.Unlock()

	// Double-check staleness after acquiring mutex
	if !c.IsStale(staleness) {
		span.SetAttributes(attribute.Bool("skipped_not_stale_after_mutex", true))
		return c.value.Load(), nil
	}

	initialVal := c.value.Load()
	span.SetAttributes(attribute.Int64("initial_value", initialVal))
	// IMPORTANT: Foreground path MUST be local-only and bounded by updateMaxWait.
	// Do NOT acquire distributed locks or do any remote I/O here, otherwise degraded shared state
	// (e.g. Redis lock acquisition) can block normal request flow.
	span.SetAttributes(attribute.Bool("foreground_remote_io_disabled", true))

	// Execute the refresh function (e.g., RPC call to get latest block) in background.
	// fallbackTimeout is sized for remote storage ops; honor a longer caller
	// deadline since the refresh itself can legitimately take longer. The
	// parent stays appCtx so the fetch survives foreground cancellation.
	fnTimeout := c.registry.fallbackTimeout
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining > fnTimeout {
			fnTimeout = remaining
		}
	}

	resultCh := make(chan refreshResult, 1)
	go func() {
		fnCtx, fnCancel := context.WithTimeout(c.registry.appCtx, fnTimeout)
		defer fnCancel()

		// Create a span for the actual RPC/refresh call - this is usually what takes time
		_, fnSpan := common.StartSpan(ctx, "CounterInt64.TryUpdateIfStale.ExecuteRefresh",
			trace.WithAttributes(
				attribute.String("key", c.key),
				attribute.Int64("timeout_ms", fnTimeout.Milliseconds()),
			),
		)
		started := time.Now()
		// A panic in the refresh fails it, so its caller and the async
		// helper get an error instead of the process crashing.
		defer func() {
			if rec := recover(); rec != nil {
				telemetry.MetricUnexpectedPanicTotal.WithLabelValues(
					"shared-state-refresh",
					"key:"+c.key,
					common.ErrorFingerprint(rec),
				).Inc()
				c.registry.logger.Error().Interface("panic", rec).Str("key", c.key).Str("stack", string(debug.Stack())).
					Msg("unexpected panic in a shared counter refresh")
				fnSpan.End()
				resultCh <- refreshResult{err: fmt.Errorf("shared counter refresh of %s panicked: %v", c.key, rec), started: started}
			}
		}()
		value, err := executeNewValueFn(fnCtx)
		if err != nil {
			fnSpan.SetAttributes(attribute.String("error", err.Error()))
		} else {
			fnSpan.SetAttributes(attribute.Int64("result_value", value))
		}
		fnSpan.End()

		resultCh <- refreshResult{val: value, err: err, started: started}
	}()

	timer := time.NewTimer(c.registry.updateMaxWait)
	defer timer.Stop()
	span.SetAttributes(attribute.String("role", "local"))

	select {
	case r := <-resultCh:
		span.SetAttributes(
			attribute.String("result_path", "got_result"),
			attribute.Bool("async_refresh", false),
		)
		// Always behave like "follower" in the synchronous path: local update only,
		// remote reconciliation/push happens asynchronously via scheduleBackgroundPushCurrent().
		return c.applyRefreshResult(initialVal, r)
	case <-timer.C:
		span.SetAttributes(
			attribute.String("result_path", "timeout"),
			attribute.Bool("async_refresh", true),
		)
		c.markFreshAttempt()
		c.continueAsyncRefresh(resultCh, fnTimeout)
		return initialVal, nil
	}
}

func (c *counterInt64) OnValue(cb func(int64)) {
	c.callbackMu.Lock()
	defer c.callbackMu.Unlock()
	c.valueCallbacks = append(c.valueCallbacks, cb)
}

func (c *counterInt64) OnLargeRollback(cb func(currentVal, newVal int64)) {
	c.callbackMu.Lock()
	defer c.callbackMu.Unlock()
	c.largeRollbackCallbacks = append(c.largeRollbackCallbacks, cb)
}

func (c *counterInt64) OnRewind(cb func(newVal int64)) {
	c.callbackMu.Lock()
	defer c.callbackMu.Unlock()
	c.rewindCallbacks = append(c.rewindCallbacks, cb)
}

func (c *counterInt64) Rewind(newValue int64) bool {
	current := c.value.Load()
	if c.ignoreRollbackOf <= 0 || newValue <= 0 || newValue >= current {
		return false
	}
	if !c.applyRewind(UpdateSourceRewind, current, newValue, 0) {
		return false
	}
	c.scheduleBackgroundPushCurrent()
	return true
}

// applyRewind moves the value down to newVal as a rewind; remoteTs is the
// remote state's order key, 0 for a local rewind, which takes a fresh one.
// OnRewind fires before OnValue, so the health tracker holds the rewound
// head when OnValue's readers wake.
func (c *counterInt64) applyRewind(source string, currentValue, newVal, remoteTs int64) bool {
	c.setDrop(dropRewind)
	for {
		if c.value.CompareAndSwap(currentValue, newVal) {
			if remoteTs > 0 {
				c.updateUpdatedAtMs(remoteTs)
			} else {
				c.allocateUpdatedAtMs()
			}
			c.markReported(time.Now())
			c.registry.logger.Debug().
				Str("source", source).
				Str("key", c.key).
				Int64("from", currentValue).
				Int64("to", newVal).
				Msg("counter rewound")
			c.callbackMu.RLock()
			callbacks := make([]func(int64), len(c.rewindCallbacks))
			copy(callbacks, c.rewindCallbacks)
			c.callbackMu.RUnlock()
			for _, cb := range callbacks {
				if cb != nil {
					cb(newVal)
				}
			}
			c.triggerValueCallback(newVal)
			return true
		}
		currentValue = c.value.Load()
		if newVal >= currentValue {
			return false
		}
	}
}

func (c *counterInt64) markFreshAttempt() {
	c.attemptAtUnixMs.Store(time.Now().UnixMilli())
}

type refreshResult struct {
	val     int64
	err     error
	started time.Time
}

func (c *counterInt64) applyRefreshResult(initialVal int64, r refreshResult) (int64, error) {
	if r.err != nil {
		c.markFreshAttempt()
		return initialVal, r.err
	}
	updated := c.processNewValue(UpdateSourceTryUpdateIfStale, r.val, r.started)
	if updated {
		c.scheduleBackgroundPushCurrent()
	}
	return c.value.Load(), nil
}

func (c *counterInt64) continueAsyncRefresh(resultCh <-chan refreshResult, fnTimeout time.Duration) {
	go func() {
		// Bound the helper's lifetime. The paired refresh worker is capped by
		// fnCtx (fnTimeout), but if a downstream call ignores its context the
		// worker may never send and this goroutine would leak permanently.
		// Cap at fnTimeout + a small buffer; drop the eventual value in the
		// pathological case — the next TryUpdateIfStale will refresh again.
		timer := time.NewTimer(fnTimeout + time.Second)
		defer timer.Stop()

		select {
		case r, ok := <-resultCh:
			if !ok {
				return
			}
			// Apply result locally; remote push is scheduled async and MUST NOT block request flow.
			c.updateMu.Lock()
			_, _ = c.applyRefreshResult(c.value.Load(), r)
			c.updateMu.Unlock()
		case <-timer.C:
			return
		case <-c.registry.appCtx.Done():
			return
		}
	}()
}

func (c *counterInt64) tryAcquireLock(ctx context.Context) func() {
	lock, err := c.registry.connector.Lock(ctx, c.key, c.registry.lockTtl)
	if err != nil {
		// Log differently based on error type
		if strings.Contains(err.Error(), "lock already taken") {
			c.registry.logger.Debug().Err(err).Str("key", c.key).Msg("lock held by another instance, proceeding with local lock")
		} else if errors.Is(err, context.DeadlineExceeded) {
			c.registry.logger.Warn().Err(err).Str("key", c.key).Msg("lock acquisition timed out waiting for other instance")
		} else {
			c.registry.logger.Warn().Err(err).Str("key", c.key).Msg("failed to remotely lock counter will only use local lock")
		}
	}
	if lock != nil && !lock.IsNil() {
		c.registry.logger.Debug().Str("key", c.key).Msg("acquired remote lock for counter")
		return func() {
			unlockCtx, cancel := context.WithTimeout(c.registry.appCtx, c.registry.lockTtl)
			defer cancel()
			if err := lock.Unlock(unlockCtx); err != nil {
				// "lock was already expired" is expected when operations take longer than anticipated
				if strings.Contains(err.Error(), "lock was already expired") {
					c.registry.logger.Debug().Err(err).Str("key", c.key).Int64("lock_ttl_ms", c.registry.lockTtl.Milliseconds()).Msg("lock expired during operations (expected behavior)")
				} else {
					c.registry.logger.Debug().Err(err).Str("key", c.key).Int64("lock_ttl_ms", c.registry.lockTtl.Milliseconds()).Msg("failed to unlock counter, so it will be expired after ttl")
				}
			} else {
				c.registry.logger.Debug().Str("key", c.key).Msg("released remote lock for counter")
			}
		}
	}
	return nil
}

func (c *counterInt64) localState() CounterInt64State {
	v := c.value.Load()
	return CounterInt64State{
		Value:     v,
		UpdatedAt: c.updatedAtMs(),
		UpdatedBy: c.registry.instanceId,
		Rewind:    dropKind(c.drop.Load()) == dropRewind,
	}
}

func (c *counterInt64) tryGetRemoteState(ctx context.Context) (CounterInt64State, bool) {
	remoteVal, err := c.registry.connector.Get(ctx, ConnectorMainIndex, c.key, "value", nil)
	if err != nil {
		if common.HasErrorCode(err, common.ErrCodeRecordNotFound) {
			c.registry.logger.Debug().Err(err).Str("key", c.key).Msg("remote counter value not found, will initialize it")
		} else {
			c.registry.logger.Warn().Err(err).Str("key", c.key).Msg("failed to get remote counter value")
		}
		return CounterInt64State{}, false
	}
	st, ok := parseCounterState(remoteVal)
	if !ok {
		c.registry.logger.Debug().Str("key", c.key).Msg("failed to parse remote counter state (treating as missing)")
	}
	return st, ok
}

func (c *counterInt64) updateRemoteState(ctx context.Context, st CounterInt64State) {
	if st.UpdatedBy == "" {
		st.UpdatedBy = c.registry.instanceId
	}
	payload, err := common.SonicCfg.Marshal(st)
	if err != nil {
		c.registry.logger.Debug().Err(err).
			Str("key", c.key).
			Int64("value", st.Value).
			Msg("failed to marshal counter state for remote update")
		return
	}

	err = c.registry.connector.Set(ctx, c.key, "value", payload, nil)
	if err == nil {
		err = c.registry.connector.PublishCounterInt64(ctx, c.key, st)
	}
	if err != nil {
		c.registry.logger.Debug().Err(err).
			Str("key", c.key).
			Int64("value", st.Value).
			Msg("failed to update remote counter value")
	} else {
		c.registry.logger.Debug().
			Str("key", c.key).
			Int64("value", st.Value).
			Int64("updatedAt", st.UpdatedAt).
			Str("updatedBy", st.UpdatedBy).
			Msg("published counter value to remote")
	}
}

// scheduleBackgroundPushCurrent dedupes and pushes the current local value to the
// remote store under a lock, without blocking the caller.
func (c *counterInt64) scheduleBackgroundPushCurrent() {
	// Always mark that a push is needed.
	c.bgPushRequested.Store(true)

	// Only one goroutine at a time runs the push loop.
	if !c.bgPushInProgress.TryLock() {
		return
	}

	go func() {
		defer func() {
			c.bgPushInProgress.Unlock()
			// Close the race window: if someone set bgPushRequested between our Swap(false)
			// returning false and their TryLock failing, respawn a worker.
			if c.bgPushRequested.Load() {
				c.scheduleBackgroundPushCurrent()
			}
		}()

		for {
			// Coalesce: if no push is requested at loop start, we're done.
			if !c.bgPushRequested.Swap(false) {
				return
			}

			// Snapshot local state (atomic reads; never block request flow here)
			local := c.localState()
			// Skip only if UpdatedAt indicates uninitialized; Value can be 0 (e.g., earliest = genesis)
			if local.UpdatedAt <= 0 {
				continue
			}

			// Best-effort fast propagation (no distributed lock): publish the latest state so
			// other instances can update their local counters quickly via WatchCounterInt64.
			pubCtx, pubCancel := context.WithTimeout(c.registry.appCtx, c.registry.lockMaxWait)
			_ = c.registry.connector.PublishCounterInt64(pubCtx, c.key, local)
			pubCancel()

			// Acquire distributed lock with a bounded wait budget.
			// NOTE: This is a background operation and MUST NOT block request flow.
			lockCtx, lockCancel := context.WithTimeout(c.registry.appCtx, c.registry.lockMaxWait)
			unlock := c.tryAcquireLock(lockCtx)
			lockCancel()
			if unlock == nil {
				// Could not acquire lock quickly; rely on publish-only propagation for now.
				// If newer updates arrive, scheduleBackgroundPushCurrent() will set bgPushRequested
				// and this loop will run again.
				continue
			}

			func() {
				defer unlock()

				// Reconcile with remote under the distributed lock.
				getCtx, getCancel := context.WithTimeout(c.registry.appCtx, c.registry.fallbackTimeout)
				remote, remoteOk := c.tryGetRemoteState(getCtx)
				getCancel()

				// Adopt the newer state by UpdatedAt ordering.
				if remoteOk && remote.UpdatedAt > local.UpdatedAt {
					c.updateMu.Lock()
					_ = c.processNewState(UpdateSourceRemoteCheck, remote)
					c.updateMu.Unlock()
					local = c.localState()
				}

				// Remote ahead is forward progress: adopt it instead of
				// overwriting it with a lower local value. ignoreRollbackOf
				// only applies to decreases (see processNewValue); capping
				// adoption by that threshold let a newer-timestamp lagging
				// writer push a regression that peers then treated as a
				// large rollback.
				// Only head counters are max-only; the others keep timestamp
				// order, and a drop outranks the older states it left.
				if remoteOk && c.ignoreRollbackOf > 0 && !c.dropped() && remote.Value > local.Value {
					c.processNewValue(UpdateSourceRemoteCheck, remote.Value, time.Now())
					return
				}

				// If our local state is newer (or remote missing), push it to remote.
				// Skip only if UpdatedAt indicates uninitialized; Value can be 0 (e.g., earliest = genesis)
				if local.UpdatedAt <= 0 {
					return
				}

				if !remoteOk || local.UpdatedAt > remote.UpdatedAt {
					pctx, cancel := context.WithTimeout(c.registry.appCtx, c.registry.fallbackTimeout)
					c.updateRemoteState(pctx, local)
					cancel()
				}
			}()
		}
	}()
}
