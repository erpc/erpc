package data

import (
	"context"
	"errors"
	"fmt"
	"strconv"
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
	// SetLocalValue updates the local in-memory value synchronously without any network calls.
	// Returns true if the value was actually updated (newValue > current).
	// Use this for fast path updates where remote sync can happen asynchronously.
	SetLocalValue(newValue int64) bool
	OnValue(callback func(int64))
	OnLargeRollback(callback func(currentVal, newVal int64))
}

type baseSharedVariable struct {
	lastProcessed   time.Time
	lastProcessedMu sync.RWMutex
}

func (v *baseSharedVariable) IsStale(staleness time.Duration) bool {
	v.lastProcessedMu.RLock()
	defer v.lastProcessedMu.RUnlock()
	if v.lastProcessed.IsZero() {
		return true
	}
	return time.Since(v.lastProcessed) > staleness
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
	bgUpdateInProgress     sync.Mutex
}

func (c *counterInt64) GetValue() int64 {
	return c.value.Load()
}

// SetLocalValue updates only the local in-memory atomic value without any network/distributed operations.
// This is a fast, synchronous update for cases where immediate local visibility is needed
// and remote sync can happen asynchronously later.
// Returns true if the value was actually updated (newValue > currentValue).
func (c *counterInt64) SetLocalValue(newValue int64) bool {
	currentValue := c.value.Load()
	if newValue <= currentValue {
		return false
	}
	// Use CompareAndSwap for thread-safety without holding the mutex long
	// If another goroutine updated it in the meantime, we retry
	for {
		if c.value.CompareAndSwap(currentValue, newValue) {
			// Mark as fresh so TryUpdateIfStale won't trigger unnecessary polls
			c.lastProcessedMu.Lock()
			c.lastProcessed = time.Now()
			c.lastProcessedMu.Unlock()

			c.registry.logger.Trace().
				Str("key", c.key).
				Int64("from", currentValue).
				Int64("to", newValue).
				Msg("local counter value set synchronously")
			c.triggerValueCallback(newValue)
			return true
		}
		// Someone else updated it, re-check
		currentValue = c.value.Load()
		if newValue <= currentValue {
			return false // Our value is no longer an improvement
		}
	}
}

func (c *counterInt64) processNewValue(source string, newVal int64) bool {
	// This function is designed to be called from within c.updateMu.Lock().
	// It returns true if the local value was actually updated.
	// Uses CAS to safely coordinate with SetLocalValue which bypasses updateMu.
	currentValue := c.value.Load()
	updated := false
	if newVal > currentValue {
		// Use CAS loop to handle race with SetLocalValue
		for {
			if c.value.CompareAndSwap(currentValue, newVal) {
				updated = true
				c.registry.logger.Trace().Str("source", source).Str("key", c.key).Int64("from", currentValue).Int64("to", newVal).Msg("counter value increased")
				c.triggerValueCallback(newVal)
				break
			}
			// CAS failed - someone else updated (likely SetLocalValue)
			currentValue = c.value.Load()
			if newVal <= currentValue {
				// Our value is no longer an improvement, abort
				c.registry.logger.Trace().Str("source", source).Str("key", c.key).Int64("newVal", newVal).Int64("currentValue", currentValue).Msg("CAS failed, value no longer an improvement")
				break
			}
			// Retry with updated currentValue
		}
	} else if currentValue > newVal && (currentValue-newVal > c.ignoreRollbackOf) {
		// Rollback detection: The new value is significantly lower than current.
		// This could indicate a blockchain reorg OR stale data racing with SetLocalValue.
		//
		// We do NOT apply the rollback here because:
		// 1. SetLocalValue may have set a higher value from fresh RPC data
		// 2. Even remote-sync updates might carry stale data from lagging upstreams
		// 3. For block numbers (the primary use case), values should only increase
		//
		// If a real reorg happened, subsequent polls will naturally pick up the
		// correct lower value through the normal forward-progress path.
		// We still trigger the rollback callbacks to notify listeners of the detection.
		c.registry.logger.Trace().
			Str("source", source).
			Str("key", c.key).
			Int64("currentValue", currentValue).
			Int64("newVal", newVal).
			Int64("gap", currentValue-newVal).
			Msg("rollback detected but not applied to preserve SetLocalValue invariant")

		c.callbackMu.RLock()
		callbacks := make([]func(localVal, newVal int64), len(c.largeRollbackCallbacks))
		copy(callbacks, c.largeRollbackCallbacks)
		c.callbackMu.RUnlock()
		for _, cb := range callbacks {
			if cb != nil {
				cb(currentValue, newVal)
			}
		}
	}

	// We have just consulted a fresh source; refresh the timestamp
	// so the value is considered fresh for the debounce window even
	// when it did not change.
	c.lastProcessedMu.Lock()
	c.lastProcessed = time.Now()
	c.lastProcessedMu.Unlock()

	c.registry.logger.Trace().Str("source", source).Str("key", c.key).Str("ptr", fmt.Sprintf("%p", c)).Int64("currentValue", currentValue).Int64("newVal", newVal).Bool("updated", updated).Msg("processed new value")
	return updated
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

	c.updateMu.Lock()
	defer c.updateMu.Unlock()

	// Best-effort foreground lock (short wait)
	lockWait := c.registry.lockMaxWait
	lockCtx, lockCancel := context.WithTimeout(ctx, lockWait)
	defer lockCancel()
	unlock := c.tryAcquireLock(lockCtx)
	prev := c.value.Load()
	updated := c.processNewValue(UpdateSourceTryUpdate, newValue)

	if unlock != nil && updated {
		defer unlock()
		current := c.value.Load()
		// Read remote under lock; we'll only adopt it if this is an increase and remote is ahead.
		getCtx, getCancel := context.WithTimeout(c.registry.appCtx, c.registry.fallbackTimeout)
		remoteVal := c.tryGetRemoteValue(getCtx)
		getCancel()
		// If this was an increase, prefer a higher remote value when present.
		// If this was a rollback (current < prev), keep the local value by design.
		if current >= prev && remoteVal > current {
			_ = c.processNewValue(UpdateSourceRemoteCheck, remoteVal)
			current = c.value.Load()
		}
		pctx, cancel := context.WithTimeout(c.registry.appCtx, c.registry.fallbackTimeout)
		defer cancel()
		c.updateRemoteUpdate(pctx, current)
	} else if unlock == nil && updated {
		c.scheduleBackgroundPushCurrent()
	} else if unlock != nil {
		// Not updating locally, but we still reconcile from remote to unstale if needed
		getCtx, getCancel := context.WithTimeout(c.registry.appCtx, c.registry.fallbackTimeout)
		remoteVal := c.tryGetRemoteValue(getCtx)
		getCancel()
		if remoteVal > 0 {
			_ = c.processNewValue(UpdateSourceRemoteCheck, remoteVal)
		}
		unlock()
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
		return c.value.Load(), nil
	}

	c.updateMu.Lock()
	defer c.updateMu.Unlock()

	if !c.IsStale(staleness) {
		return c.value.Load(), nil
	}

	initialVal := c.value.Load()

	lockCtx, lockCancel := context.WithTimeout(ctx, c.registry.lockMaxWait)
	unlock := c.tryAcquireLock(lockCtx)
	lockCancel()

	resultCh := make(chan refreshResult, 1)
	go func() {
		fnCtx, fnCancel := context.WithTimeout(c.registry.appCtx, c.registry.fallbackTimeout)
		defer fnCancel()
		value, err := executeNewValueFn(fnCtx)
		resultCh <- refreshResult{val: value, err: err}
	}()

	timer := time.NewTimer(c.registry.updateMaxWait)
	defer timer.Stop()

	if unlock == nil {
		select {
		case r := <-resultCh:
			return c.handleFollowerResult(initialVal, r)
		case <-timer.C:
			c.markFreshAttempt()
			c.continueAsyncRefresh(resultCh)
			return initialVal, nil
		}
	}

	select {
	case r := <-resultCh:
		return c.handleLeaderResult(initialVal, unlock, r)
	case <-timer.C:
		c.markFreshAttempt()
		unlock()
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
	c.lastProcessedMu.Lock()
	c.lastProcessed = time.Now()
	c.lastProcessedMu.Unlock()
}

type refreshResult struct {
	val int64
	err error
}

func (c *counterInt64) handleFollowerResult(initialVal int64, r refreshResult) (int64, error) {
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

func (c *counterInt64) handleLeaderResult(initialVal int64, unlock func(), r refreshResult) (int64, error) {
	if r.err != nil {
		c.markFreshAttempt()
		unlock()
		return initialVal, r.err
	}
	updated := c.processNewValue(UpdateSourceTryUpdateIfStale, r.val)
	if updated {
		c.reconcileAndPushWithLockNoLocalLock(unlock)
		return c.value.Load(), nil
	}
	unlock()
	return c.value.Load(), nil
}

func (c *counterInt64) continueAsyncRefresh(resultCh <-chan refreshResult) {
	if !c.bgUpdateInProgress.TryLock() {
		go func() {
			<-resultCh
		}()
		return
	}
	go func() {
		defer c.bgUpdateInProgress.Unlock()
		r, ok := <-resultCh
		if !ok || r.err != nil {
			return
		}

		c.updateMu.Lock()
		updated := c.processNewValue(UpdateSourceTryUpdateIfStale, r.val)
		c.updateMu.Unlock()
		if !updated {
			return
		}

		lockCtx, lockCancel := context.WithTimeout(c.registry.appCtx, c.registry.lockTtl)
		unlock := c.tryAcquireLock(lockCtx)
		lockCancel()
		if unlock == nil {
			return
		}

		c.updateMu.Lock()
		c.reconcileAndPushWithLockNoLocalLock(unlock)
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

func (c *counterInt64) tryGetRemoteValue(ctx context.Context) int64 {
	remoteVal, err := c.registry.connector.Get(ctx, ConnectorMainIndex, c.key, "value", nil)
	if err != nil {
		if common.HasErrorCode(err, common.ErrCodeRecordNotFound) {
			c.registry.logger.Debug().Err(err).Str("key", c.key).Msg("remote counter value not found, will initialize it")
		} else {
			c.registry.logger.Warn().Err(err).Str("key", c.key).Msg("failed to get remote counter value")
		}
	} else {
		remoteValueInt, err := strconv.ParseInt(string(remoteVal), 0, 0)
		if err != nil {
			c.registry.logger.Warn().Err(err).Str("key", c.key).Msg("failed to parse remote counter value")
		} else {
			return remoteValueInt
		}
	}
	return 0
}

func (c *counterInt64) updateRemoteUpdate(ctx context.Context, newValue int64) {
	err := c.registry.connector.Set(ctx, c.key, "value", []byte(fmt.Sprintf("%d", newValue)), nil)
	if err == nil {
		err = c.registry.connector.PublishCounterInt64(ctx, c.key, newValue)
	}
	if err != nil {
		c.registry.logger.Debug().Err(err).
			Str("key", c.key).
			Int64("value", newValue).
			Msg("failed to update remote counter value")
	} else {
		c.registry.logger.Debug().Str("key", c.key).Int64("value", newValue).Msg("published counter value to remote")
	}
}

// reconcileAndPushWithLockNoLocalLock assumes updateMu is already held by the caller and a
// distributed lock has been acquired (represented by unlock). It reconciles the local value
// with remote (taking the higher value) and pushes the final value, then releases the lock.
func (c *counterInt64) reconcileAndPushWithLockNoLocalLock(unlock func()) {
	defer unlock()

	// Read remote while we still hold the distributed lock
	getCtx, getCancel := context.WithTimeout(c.registry.appCtx, c.registry.fallbackTimeout)
	remoteVal := c.tryGetRemoteValue(getCtx)
	getCancel()
	current := c.value.Load()
	if remoteVal > current {
		_ = c.processNewValue(UpdateSourceRemoteCheck, remoteVal)
		current = c.value.Load()
	}
	if current <= 0 {
		return
	}
	pctx, cancel := context.WithTimeout(c.registry.appCtx, c.registry.fallbackTimeout)
	c.updateRemoteUpdate(pctx, current)
	cancel()
}

// scheduleBackgroundPushCurrent dedupes and pushes the current local value to the
// remote store under a lock, without blocking the caller.
func (c *counterInt64) scheduleBackgroundPushCurrent() {
	if !c.bgUpdateInProgress.TryLock() {
		return
	}
	go func() {
		defer c.bgUpdateInProgress.Unlock()

		// IMPORTANT: Acquire updateMu FIRST to maintain consistent lock ordering
		// This prevents deadlock with TryUpdate/TryUpdateIfStale which also acquire
		// updateMu before distributed lock
		c.updateMu.Lock()

		// Read current value while holding updateMu
		current := c.value.Load()
		if current <= 0 {
			c.updateMu.Unlock()
			return
		}

		// Now acquire distributed lock with background budget
		lockCtx, lockCancel := context.WithTimeout(c.registry.appCtx, c.registry.lockTtl)
		unlock := c.tryAcquireLock(lockCtx)
		lockCancel()
		if unlock == nil {
			c.updateMu.Unlock()
			return
		}

		// Reconcile with remote while holding both locks
		getCtx, getCancel := context.WithTimeout(c.registry.appCtx, c.registry.fallbackTimeout)
		remoteVal := c.tryGetRemoteValue(getCtx)
		getCancel()
		if remoteVal > c.value.Load() {
			_ = c.processNewValue(UpdateSourceRemoteCheck, remoteVal)
		}
		current = c.value.Load()

		// Push the reconciled value while still holding both locks
		// This ensures the value we push is exactly what we reconciled
		if current > 0 {
			pctx, cancel := context.WithTimeout(c.registry.appCtx, c.registry.fallbackTimeout)
			c.updateRemoteUpdate(pctx, current)
			cancel()
		}

		// Release distributed lock before local lock to respect acquisition order
		unlock()
		c.updateMu.Unlock()
	}()
}
