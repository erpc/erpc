package data

import (
	"context"
	"errors"
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
	TryUpdate(ctx context.Context, newValue int64) int64
	OnValue(callback func(int64))
	OnLargeRollback(callback func(currentVal, newVal int64))
}

type baseSharedVariable struct {
	// updatedAtUnixMs tracks the last-known update timestamp for this shared variable.
	// It is used for staleness checks (TryUpdateIfStale debouncing).
	//
	// Semantics: unix milliseconds, monotonic increasing within a process.
	updatedAtUnixMs atomic.Int64
}

func (v *baseSharedVariable) IsStale(staleness time.Duration) bool {
	updatedAt := v.updatedAtUnixMs.Load()
	if updatedAt <= 0 {
		return true
	}
	return time.Since(time.UnixMilli(updatedAt)) > staleness
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

type counterInt64 struct {
	baseSharedVariable
	registry               *sharedStateRegistry
	key                    string
	value                  atomic.Int64
	updateMu               sync.Mutex
	valueCallbacks         []func(int64)
	ignoreRollbackOf       int64
	largeRollbackCallbacks []func(localVal, newVal int64)
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

func (c *counterInt64) processNewState(source string, st CounterInt64State) bool {
	// processNewState handles updates from remote sources (WatchCounterInt64, reconciliation).
	// It applies the SAME rollback handling as processNewValue for consistency:
	// - ignoreRollbackOf = 0: Accept all changes (for earliest block)
	// - ignoreRollbackOf > 0: Ignore small rollbacks (for latest/finalized - avoids noise)
	//
	// This ensures consistent "only-increasing" semantics for latest/finalized across
	// all instances, preventing noise from propagating via shared state.

	// Track if this is a remote update (with external timestamp) vs local update.
	// For remote updates, we only CHECK timestamp ordering here - we DON'T update our local
	// timestamp until we know we're going to accept the value change (forward progress or large rollback).
	//
	// SPECIAL CASE - Small rollback rejection: When we reject a small rollback from a remote update
	// with a newer timestamp, we advance our local timestamp past the remote's timestamp. This ensures
	// our higher local value will "win" reconciliation in scheduleBackgroundPushCurrent and get pushed
	// to remote, since local.UpdatedAt will be > remote.UpdatedAt.
	isRemoteUpdate := st.UpdatedAt > 0

	if !isRemoteUpdate {
		// For local updates, allow UpdatedAt=0 and allocate a unique timestamp.
		st.UpdatedAt = c.allocateUpdatedAtMs()
	} else {
		// For remote updates, just check timestamp ordering - don't update local timestamp yet.
		currentTs := c.updatedAtMs()
		if st.UpdatedAt <= currentTs {
			// Reject stale/out-of-order states by UpdatedAt ordering.
			// This provides idempotency and prevents stale remote values from overriding fresh local detections.
			return false
		}
	}

	currentValue := c.value.Load()
	newVal := st.Value

	if newVal == currentValue {
		// Values appear equal - no value change needed.
		// For remote updates, we want to update timestamp since remote is fresher.
		// However, we must verify the value hasn't changed concurrently to avoid a TOCTOU race:
		// a concurrent TryUpdate could change the value between our Load() and this check,
		// causing us to incorrectly set the remote's stale timestamp for a different local value.
		if isRemoteUpdate {
			if c.updateUpdatedAtMs(st.UpdatedAt) {
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
		// Forward progress: always accept
		for {
			if c.value.CompareAndSwap(currentValue, newVal) {
				// Update timestamp AFTER accepting the value (for remote updates)
				if isRemoteUpdate {
					c.updateUpdatedAtMs(st.UpdatedAt)
				}
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

	// Large rollback exceeds threshold - apply it and trigger callback
	for {
		if c.value.CompareAndSwap(currentValue, newVal) {
			// Update timestamp AFTER accepting the value (for remote updates)
			if isRemoteUpdate {
				c.updateUpdatedAtMs(st.UpdatedAt)
			}
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
// an external timestamp. The ignoreRollbackOf parameter controls rollback behavior:
// - ignoreRollbackOf = 0: Accept all changes (used for earliest block - can decrease due to pruning)
// - ignoreRollbackOf > 0: Ignore small rollbacks (noise from lagging RPCs), trigger callback for large ones
//
// This is different from processNewState which uses timestamp ordering for remote reconciliation.
func (c *counterInt64) processNewValue(source string, newVal int64) bool {
	currentValue := c.value.Load()
	if newVal == currentValue {
		// Value is the same, but mark as fresh to prevent repeated refresh attempts
		c.allocateUpdatedAtMs()
		return false
	}

	if newVal > currentValue {
		// Forward progress: always accept
		for {
			if c.value.CompareAndSwap(currentValue, newVal) {
				c.allocateUpdatedAtMs() // Mark as fresh
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
	for {
		if c.value.CompareAndSwap(currentValue, newVal) {
			c.allocateUpdatedAtMs() // Mark as fresh
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

func (c *counterInt64) TryUpdate(ctx context.Context, newValue int64) int64 {
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
	updated := c.processNewValue(UpdateSourceTryUpdate, newValue)

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
	muLocked := true
	defer func() {
		if muLocked {
			c.updateMu.Unlock()
		}
	}()

	// Double-check staleness after acquiring mutex
	if !c.IsStale(staleness) {
		span.SetAttributes(attribute.Bool("skipped_not_stale_after_mutex", true))
		return c.value.Load(), nil
	}

	initialVal := c.value.Load()
	span.SetAttributes(attribute.Int64("initial_value", initialVal))

	// Try distributed poll lock to coordinate refresh across instances.
	// Only one instance per variable should execute the expensive RPC call;
	// others receive the result via shared state pubsub.
	// Bounded by lockMaxWait. Primarily called from background polling goroutines,
	// but can also be reached from request-handler paths
	// (e.g., EvmAssertBlockAvailability with forceFreshIfStale=true).
	lockCtx, lockCancel := context.WithTimeout(c.registry.appCtx, c.registry.lockMaxWait)
	unlock, isContention := c.tryAcquirePollLock(lockCtx)
	lockCancel()

	if unlock == nil {
		if isContention {
			// Another instance is refreshing — release local mutex and wait briefly
			// for the pubsub update rather than returning stale immediately.
			// This matters on request-handler paths (forceFreshIfStale) where stale
			// values cause wrong availability decisions.
			c.updateMu.Unlock()
			muLocked = false
			span.SetAttributes(attribute.String("poll_lock", "contention"))
			telemetry.MetricSharedStatePollLockTotal.WithLabelValues(c.key, "contention").Inc()

			waitTimer := time.NewTimer(c.registry.updateMaxWait)
			defer waitTimer.Stop()
			ticker := time.NewTicker(5 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					// Caller's context cancelled — return current value without advancing timestamp.
					span.SetAttributes(attribute.String("contention_exit", "context_cancelled"))
					return c.value.Load(), ctx.Err()
				case <-waitTimer.C:
					// Timed out waiting for pubsub update — return current value (may still be stale).
					// Do NOT call markFreshAttempt(): we didn't actually refresh, so we should not
					// advance updatedAtUnixMs. This allows the next caller to retry immediately
					// and avoids suppressing pubsub updates that arrive slightly late.
					span.SetAttributes(attribute.Bool("contention_got_fresh", false))
					return c.value.Load(), nil
				case <-ticker.C:
					if !c.IsStale(staleness) {
						span.SetAttributes(attribute.Bool("contention_got_fresh", true))
						return c.value.Load(), nil
					}
				}
			}
		}
		// Infrastructure error — fall through to local refresh (graceful degradation).
		span.SetAttributes(attribute.String("poll_lock", "unavailable"))
		telemetry.MetricSharedStatePollLockTotal.WithLabelValues(c.key, "unavailable").Inc()
	} else {
		// Re-check staleness after acquiring distributed lock (third check: pre-mutex, post-mutex,
		// post-lock) — pubsub update may have arrived while waiting for the lock.
		if !c.IsStale(staleness) {
			unlock()
			span.SetAttributes(attribute.String("poll_lock", "skipped_fresh"))
			telemetry.MetricSharedStatePollLockTotal.WithLabelValues(c.key, "skipped_fresh").Inc()
			return c.value.Load(), nil
		}
		span.SetAttributes(attribute.String("poll_lock", "acquired"))
		telemetry.MetricSharedStatePollLockTotal.WithLabelValues(c.key, "acquired").Inc()
	}

	// Execute the refresh function (e.g., RPC call to get latest block) in background.
	// The poll lock (if acquired) is held until the RPC completes, preventing other
	// instances from redundantly polling the same upstream.
	// Pass unlock as parameter to avoid fragile closure capture.
	unlockFn := unlock
	resultCh := make(chan refreshResult, 1)
	go func() {
		if unlockFn != nil {
			defer unlockFn()
		}

		fnCtx, fnCancel := context.WithTimeout(c.registry.appCtx, c.registry.fallbackTimeout)
		defer fnCancel()

		_, fnSpan := common.StartSpan(ctx, "CounterInt64.TryUpdateIfStale.ExecuteRefresh",
			trace.WithAttributes(
				attribute.String("key", c.key),
				attribute.Int64("timeout_ms", c.registry.fallbackTimeout.Milliseconds()),
			),
		)
		value, err := executeNewValueFn(fnCtx)
		if err != nil {
			fnSpan.SetAttributes(attribute.String("error", err.Error()))
		} else {
			fnSpan.SetAttributes(attribute.Int64("result_value", value))
		}
		fnSpan.End()

		resultCh <- refreshResult{val: value, err: err}
	}()

	timer := time.NewTimer(c.registry.updateMaxWait)
	defer timer.Stop()

	select {
	case r := <-resultCh:
		span.SetAttributes(
			attribute.String("result_path", "got_result"),
			attribute.Bool("async_refresh", false),
		)
		return c.applyRefreshResult(initialVal, r)
	case <-timer.C:
		span.SetAttributes(
			attribute.String("result_path", "timeout"),
			attribute.Bool("async_refresh", true),
		)
		c.markFreshAttempt()
		c.continueAsyncRefresh(resultCh)
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

func (c *counterInt64) markFreshAttempt() {
	c.allocateUpdatedAtMs()
}

type refreshResult struct {
	val int64
	err error
}

func (c *counterInt64) applyRefreshResult(initialVal int64, r refreshResult) (int64, error) {
	if r.err != nil {
		c.markFreshAttempt()
		return initialVal, r.err
	}
	updated := c.processNewValue(UpdateSourceTryUpdateIfStale, r.val)
	if updated {
		c.scheduleBackgroundPushCurrent()
	}
	return c.value.Load(), nil
}

func (c *counterInt64) continueAsyncRefresh(resultCh <-chan refreshResult) {
	go func() {
		r, ok := <-resultCh
		if !ok {
			return
		}
		// Apply result locally; remote push is scheduled async and MUST NOT block request flow.
		c.updateMu.Lock()
		_, _ = c.applyRefreshResult(c.value.Load(), r)
		c.updateMu.Unlock()
	}()
}

func (c *counterInt64) tryAcquireLock(ctx context.Context) func() {
	lock, err := c.registry.connector.Lock(ctx, c.key, c.registry.lockTtl)
	if err != nil {
		if errors.Is(err, ErrLockContention) {
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
				if errors.Is(err, ErrLockExpired) {
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

// tryAcquirePollLock attempts to acquire a distributed lock for poll coordination.
// Uses a separate key ("/poll" suffix) from the reconciliation lock to avoid contention.
// Returns (unlockFn, isContention):
//   - (fn, false) → lock acquired, caller must call fn() to release
//   - (nil, true) → another instance holds the lock (contention) — caller should wait for pubsub result
//   - (nil, false) → infrastructure error (shared state unavailable) — caller should fall through to local refresh
func (c *counterInt64) tryAcquirePollLock(ctx context.Context) (func(), bool) {
	pollKey := c.key + "/poll"
	// 2x lockTtl: poll RPCs may be slower than reconciliation operations,
	// and we want the lock to survive a slow but successful poll. If the poll
	// exceeds this TTL, the lock expires harmlessly and the unlock logs at Debug level.
	pollTTL := 2 * c.registry.lockTtl

	lock, err := c.registry.connector.Lock(ctx, pollKey, pollTTL)
	if err != nil {
		// ErrLockContention is a definitive signal that another instance holds the lock.
		// All connector implementations return this sentinel for contention cases
		// (Redis via redsync ErrTaken, DynamoDB via ConditionalCheckFailed retries,
		// PostgreSQL via pg_try_advisory_xact_lock, Memory via TryLock failures).
		if errors.Is(err, ErrLockContention) {
			c.registry.logger.Debug().Err(err).Str("key", pollKey).Msg("poll lock held by another instance, waiting for pubsub")
			return nil, true
		}
		// DeadlineExceeded is ambiguous: could be slow/down infrastructure OR contention
		// where the context expired before the connector could return ErrLockContention.
		// Fall through to local refresh for graceful degradation — this ensures forward
		// progress when the shared state backend is genuinely unavailable.
		if errors.Is(err, context.DeadlineExceeded) {
			c.registry.logger.Debug().Err(err).Str("key", pollKey).Msg("poll lock timed out (ambiguous), falling through to local refresh")
		} else {
			c.registry.logger.Warn().Err(err).Str("key", pollKey).Msg("poll lock unavailable (infrastructure error), falling through to local refresh")
		}
		return nil, false
	}
	if lock == nil || lock.IsNil() {
		c.registry.logger.Warn().Str("key", pollKey).Msg("connector returned nil lock without error, falling through to local refresh")
		return nil, false
	}

	c.registry.logger.Debug().Str("key", pollKey).Msg("acquired poll lock for refresh coordination")
	return func() {
		// Unlock timeout uses base lockTtl (not pollTTL) since unlock is a fast single-key operation.
		unlockCtx, cancel := context.WithTimeout(c.registry.appCtx, c.registry.lockTtl)
		defer cancel()
		if err := lock.Unlock(unlockCtx); err != nil {
			if errors.Is(err, ErrLockExpired) {
				c.registry.logger.Debug().Err(err).Str("key", pollKey).Msg("poll lock expired during refresh (expected for slow RPCs)")
			} else {
				c.registry.logger.Warn().Err(err).Str("key", pollKey).Msg("failed to unlock poll lock, will be held until TTL expiry")
			}
		}
	}, false
}

func (c *counterInt64) localState() CounterInt64State {
	return CounterInt64State{
		Value:     c.value.Load(),
		UpdatedAt: c.updatedAtMs(),
		UpdatedBy: c.registry.instanceId,
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
	var st CounterInt64State
	if err := common.SonicCfg.Unmarshal(remoteVal, &st); err != nil {
		// No backward compatibility: treat parse errors as missing
		c.registry.logger.Debug().Err(err).Str("key", c.key).Msg("failed to parse remote counter state (treating as missing)")
		return CounterInt64State{}, false
	}
	if st.UpdatedAt <= 0 {
		return CounterInt64State{}, false
	}
	return st, true
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
