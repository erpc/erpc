package data

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/common"
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

	// For local updates, allow UpdatedAt=0 and allocate a unique timestamp.
	// For remote updates, UpdatedAt must be present and valid.
	if st.UpdatedAt <= 0 {
		st.UpdatedAt = c.allocateUpdatedAtMs()
	} else if !c.updateUpdatedAtMs(st.UpdatedAt) {
		// Reject stale/out-of-order states by UpdatedAt ordering.
		// This provides idempotency and prevents stale remote values from overriding fresh local detections.
		return false
	}

	currentValue := c.value.Load()
	newVal := st.Value
	updated := false

	if newVal == currentValue {
		// No change needed
		return false
	}

	if newVal > currentValue {
		// Forward progress: always accept
		for {
			if c.value.CompareAndSwap(currentValue, newVal) {
				updated = true
				c.registry.logger.Trace().
					Str("source", source).
					Str("key", c.key).
					Int64("from", currentValue).
					Int64("to", newVal).
					Int64("updatedAt", st.UpdatedAt).
					Msg("counter value increased (remote)")
				c.triggerValueCallback(newVal)
				break
			}
			currentValue = c.value.Load()
			if newVal <= currentValue {
				break
			}
		}
		return updated
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
			Msg("small rollback ignored (remote)")
		return false
	}

	// Large rollback exceeds threshold - apply it and trigger callback
	for {
		if c.value.CompareAndSwap(currentValue, newVal) {
			updated = true
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
			break
		}
		currentValue = c.value.Load()
		if newVal >= currentValue {
			break
		}
	}
	return updated
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
	if updated && newValue > 0 {
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

	// Execute the refresh function (e.g., RPC call to get latest block) in background
	resultCh := make(chan refreshResult, 1)
	go func() {
		fnCtx, fnCancel := context.WithTimeout(c.registry.appCtx, c.registry.fallbackTimeout)
		defer fnCancel()

		// Create a span for the actual RPC/refresh call - this is usually what takes time
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
			if local.Value <= 0 || local.UpdatedAt <= 0 {
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
				if local.Value <= 0 || local.UpdatedAt <= 0 {
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
