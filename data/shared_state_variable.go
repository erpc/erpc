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

// counterState is a counter's value with how it got there, swapped as one
// pointer so the two never disagree (spec §14). While a head counter holds
// a drop, a higher remote state stamped no later than the local one, or a
// local rise whose evidence predates dropAt, is the branch the drop left.
type counterState struct {
	v    int64
	drop dropKind
	// dropAt is this instance's clock, unix ms, when the drop was applied.
	dropAt int64
}

func (s *counterState) value() int64 {
	if s == nil {
		return 0
	}
	return s.v
}

func (s *counterState) dropped() bool { return s != nil && s.drop != dropNone }

func (s *counterState) rewound() bool { return s != nil && s.drop == dropRewind }

type counterInt64 struct {
	baseSharedVariable
	// reportAtUnixMs is ReportAt.
	reportAtUnixMs atomic.Int64
	// attemptAtUnixMs is this instance's clock when its last refresh ended
	// (an answer, an error or the foreground timeout): it debounces the
	// next refresh, since a report dated by a slow fetch's send time can
	// be older than the debounce at once.
	attemptAtUnixMs atomic.Int64
	registry               *sharedStateRegistry
	key                    string
	state                  atomic.Pointer[counterState]
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
	return c.state.Load().value()
}

// dropTo is the state of a head counter dropped to v; other counters keep
// no drop, their decreases are ordinary (earliest, cordon, floors).
func (c *counterInt64) dropTo(v int64, k dropKind) *counterState {
	if c.ignoreRollbackOf <= 0 {
		return &counterState{v: v}
	}
	return &counterState{v: v, drop: k, dropAt: time.Now().UnixMilli()}
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

// processNewState applies a remote state (pubsub, initial fetch, reconcile).
// Head counters (ignoreRollbackOf > 0: latest, finalized) are max-only on
// every path: a higher value wins whatever its timestamp, a drop of up to
// ignoreRollbackOf is noise, and timestamps decide only larger drops (spec
// §14, #1243, #222). Other counters (earliest, cordon, floors) take any
// change, in timestamp order. Each rule reads the state its CAS replaces.
//
// A rejected drop with a newer timestamp moves the order key past it, so
// the higher local value wins the next reconcile and goes back to remote.
func (c *counterInt64) processNewState(source string, st CounterInt64State) bool {
	isRemoteUpdate := st.UpdatedAt > 0
	if !isRemoteUpdate {
		st.UpdatedAt = c.allocateUpdatedAtMs()
	}
	newVal := st.Value
	for {
		cur := c.state.Load()
		currentValue := cur.value()
		newer := !isRemoteUpdate || st.UpdatedAt > c.updatedAtMs()

		if c.ignoreRollbackOf == 0 && !newer && newVal != currentValue {
			countRejected(rejectStaleTimestamp)
			return false
		}

		if newVal == currentValue {
			// A remote state of the same value only moves the order key. A
			// concurrent change after the load takes a fresh key, so the key
			// never dates a different local value by the remote's clock.
			if isRemoteUpdate && c.updateUpdatedAtMs(st.UpdatedAt) {
				c.markReported(time.Now())
				if c.GetValue() != newVal {
					c.allocateUpdatedAtMs()
				}
			}
			return false
		}

		if newVal > currentValue {
			if !newer && cur.dropped() {
				countRejected(rejectStaleTimestamp)
				return false
			}
			if !c.state.CompareAndSwap(cur, &counterState{v: newVal}) {
				continue
			}
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

		// A head counter never legitimately moves to zero or below: zero is
		// the zero value of an unset state (an instance that pushes before
		// its first poll, an empty record with a live timestamp), and as a
		// rollback the size of the chain it would pass the large-rollback
		// rule on every instance (#258, #1084). Earliest counters
		// (ignoreRollbackOf 0) may hold 0 on an archive node. The order key
		// moves past it as for a small rollback, so reconcile does not hand
		// the zero back.
		if newVal <= 0 && c.ignoreRollbackOf > 0 {
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

		// A rewind-marked drop newer than the local state is another
		// instance's rollback write: taken whatever its size, never pushed
		// back, counted against no upstream.
		if st.Rewind && c.ignoreRollbackOf > 0 {
			if !newer {
				countRejected(rejectStaleTimestamp)
				return false
			}
			if !c.state.CompareAndSwap(cur, c.dropTo(newVal, dropRewind)) {
				continue
			}
			c.rewound(source, currentValue, newVal, st.UpdatedAt)
			return true
		}

		gap := currentValue - newVal
		if gap <= c.ignoreRollbackOf {
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

		// Timestamps decide large drops: an older one is a state this
		// instance already moved past.
		if !newer {
			countRejected(rejectStaleTimestamp)
			return false
		}
		if !c.state.CompareAndSwap(cur, c.dropTo(newVal, dropRollback)) {
			continue
		}
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
		c.triggerLargeRollback(currentValue, newVal)
		return true
	}
}

// processNewValue applies a local report (TryUpdate, a refresh, the
// reconcile adopt); at is when its evidence was taken (ReportAt). Earliest
// counters (ignoreRollbackOf 0) take any change; head counters ignore drops
// of up to ignoreRollbackOf as noise from lagging nodes and take larger ones
// as rollbacks. While a head counter holds a drop, a rise whose evidence
// predates it (a poll sent before the switch) is the branch the drop left.
func (c *counterInt64) processNewValue(source string, newVal int64, at time.Time) bool {
	for {
		cur := c.state.Load()
		currentValue := cur.value()
		if newVal == currentValue {
			// Same value: it is fresh evidence, which debounces refreshes.
			c.allocateUpdatedAtMs()
			c.markReported(at)
			return false
		}

		if newVal > currentValue {
			if cur.dropped() && at.UnixMilli() < cur.dropAt {
				c.registry.logger.Trace().
					Str("source", source).
					Str("key", c.key).
					Int64("currentValue", currentValue).
					Int64("newVal", newVal).
					Msg("rise from evidence older than the drop ignored (local)")
				return false
			}
			if !c.state.CompareAndSwap(cur, &counterState{v: newVal}) {
				continue
			}
			c.allocateUpdatedAtMs()
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

		// See processNewState: zero is an unset state, not a head.
		if newVal <= 0 && c.ignoreRollbackOf > 0 {
			c.registry.logger.Warn().
				Str("source", source).
				Str("key", c.key).
				Int64("currentValue", currentValue).
				Int64("newVal", newVal).
				Msg("rejected non-positive value for a head counter (local)")
			return false
		}

		gap := currentValue - newVal
		if gap <= c.ignoreRollbackOf {
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

		if !c.state.CompareAndSwap(cur, c.dropTo(newVal, dropRollback)) {
			continue
		}
		c.allocateUpdatedAtMs()
		c.markReported(at)
		c.registry.logger.Trace().
			Str("source", source).
			Str("key", c.key).
			Int64("from", currentValue).
			Int64("to", newVal).
			Int64("gap", gap).
			Msg("large rollback applied (local)")
		c.triggerValueCallback(newVal)
		c.triggerLargeRollback(currentValue, newVal)
		return true
	}
}

func (c *counterInt64) triggerLargeRollback(currentVal, newVal int64) {
	c.callbackMu.RLock()
	callbacks := make([]func(localVal, newVal int64), len(c.largeRollbackCallbacks))
	copy(callbacks, c.largeRollbackCallbacks)
	c.callbackMu.RUnlock()
	for _, cb := range callbacks {
		if cb != nil {
			cb(currentVal, newVal)
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
	if common.IsTracingDetailed() {
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
	return c.GetValue()
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
		return c.GetValue(), nil
	}

	// Track time waiting for mutex
	_, mutexSpan := common.StartSpan(ctx, "CounterInt64.TryUpdateIfStale.AcquireMutex")
	c.updateMu.Lock()
	mutexSpan.End()
	defer c.updateMu.Unlock()

	// Double-check staleness after acquiring mutex
	if !c.IsStale(staleness) {
		span.SetAttributes(attribute.Bool("skipped_not_stale_after_mutex", true))
		return c.GetValue(), nil
	}

	initialVal := c.GetValue()
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
	if c.ignoreRollbackOf <= 0 || newValue <= 0 {
		return false
	}
	for {
		cur := c.state.Load()
		current := cur.value()
		if newValue >= current {
			return false
		}
		if !c.state.CompareAndSwap(cur, c.dropTo(newValue, dropRewind)) {
			continue
		}
		c.rewound(UpdateSourceRewind, current, newValue, 0)
		c.scheduleBackgroundPushCurrent()
		return true
	}
}

// rewound is the bookkeeping of a rewind its CAS applied; remoteTs is the
// remote state's order key, 0 for a local rewind, which takes a fresh one.
// OnRewind fires before OnValue, so the health tracker holds the rewound
// head when OnValue's readers wake.
func (c *counterInt64) rewound(source string, currentValue, newVal, remoteTs int64) {
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
	c.markFreshAttempt()
	if r.err != nil {
		return initialVal, r.err
	}
	updated := c.processNewValue(UpdateSourceTryUpdateIfStale, r.val, r.started)
	if updated {
		c.scheduleBackgroundPushCurrent()
	}
	return c.GetValue(), nil
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
			_, _ = c.applyRefreshResult(c.GetValue(), r)
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
	s := c.state.Load()
	return CounterInt64State{
		Value:     s.value(),
		UpdatedAt: c.updatedAtMs(),
		UpdatedBy: c.registry.instanceId,
		Rewind:    s.rewound(),
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
	st, ok := parseCounterState(c.key, remoteVal)
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
				if remoteOk && c.ignoreRollbackOf > 0 && !c.state.Load().dropped() && remote.Value > local.Value {
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
