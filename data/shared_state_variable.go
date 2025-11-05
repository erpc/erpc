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

func (c *counterInt64) processNewValue(source string, newVal int64) bool {
	// This function is designed to be called from within c.updateMu.Lock().
	// It returns true if the local value was actually updated.
	currentValue := c.value.Load()
	updated := false
	if newVal > currentValue {
		c.value.Store(newVal)
		updated = true
		c.registry.logger.Debug().Str("source", source).Str("key", c.key).Int64("from", currentValue).Int64("to", newVal).Msg("counter value increased")
		c.triggerValueCallback(newVal)
	} else if currentValue > newVal && (currentValue-newVal > c.ignoreRollbackOf) {
		c.value.Store(newVal)
		updated = true
		c.registry.logger.Warn().Str("source", source).Str("key", c.key).Int64("from", currentValue).Int64("to", newVal).Int64("rollback", currentValue-newVal).Msg("counter value rolled back")
		c.triggerValueCallback(newVal)
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

	// Quick check if value is not stale using atomic read
	if !c.IsStale(staleness) {
		return c.value.Load(), nil
	}

	c.updateMu.Lock()
	defer c.updateMu.Unlock()

	// Double-check staleness under lock
	if !c.IsStale(staleness) {
		return c.value.Load(), nil
	}

	// Best-effort foreground lock and user-function budgets
	lockWait := c.registry.lockMaxWait
	lockCtx, lockCancel := context.WithTimeout(ctx, lockWait)
	defer lockCancel()
	unlock := c.tryAcquireLock(lockCtx)

	// Run user refresh in the background; only wait up to updateMaxWait for the caller
	type res struct {
		val int64
		err error
	}
	resultCh := make(chan res, 1)
	go func() {
		v, e := executeNewValueFn(c.registry.appCtx)
		resultCh <- res{val: v, err: e}
	}()

	fnWait := c.registry.updateMaxWait
	timer := time.NewTimer(fnWait)
	defer timer.Stop()

	select {
	case r := <-resultCh:
		if r.err != nil {
			// Debounce to avoid herd on repeated failures
			c.lastProcessedMu.Lock()
			c.lastProcessed = time.Now()
			c.lastProcessedMu.Unlock()
			if unlock != nil {
				unlock()
			}
			return c.value.Load(), r.err
		}

		updated := c.processNewValue(UpdateSourceTryUpdateIfStale, r.val)
		if unlock != nil && updated {
			c.reconcileAndPushWithLockNoLocalLock(unlock)
		} else if updated {
			c.scheduleBackgroundPushCurrent()
		} else if unlock != nil {
			unlock()
		}
		return c.value.Load(), nil

	case <-timer.C:
		// Caller budget exhausted; continue in background and return current local value
		go func(unlockFn func()) {
			r := <-resultCh
			if r.err != nil {
				if unlockFn != nil {
					unlockFn()
				}
				return
			}
			c.updateMu.Lock()
			updated := c.processNewValue(UpdateSourceTryUpdateIfStale, r.val)
			if unlockFn != nil && updated {
				c.reconcileAndPushWithLockNoLocalLock(unlockFn)
			} else if updated {
				c.scheduleBackgroundPushCurrent()
			} else if unlockFn != nil {
				unlockFn()
			}
			c.updateMu.Unlock()
		}(unlock)

		return c.value.Load(), nil
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
	if remoteVal > 0 {
		_ = c.processNewValue(UpdateSourceRemoteCheck, remoteVal)
	}

	current := c.value.Load()
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
		// Acquire lock with background budget
		lockCtx, lockCancel := context.WithTimeout(c.registry.appCtx, c.registry.lockTtl)
		unlock := c.tryAcquireLock(lockCtx)
		lockCancel()
		if unlock == nil {
			return
		}
		defer unlock()

		// Reconcile with remote before pushing to ensure monotonicity
		getCtx, getCancel := context.WithTimeout(c.registry.appCtx, c.registry.fallbackTimeout)
		remote := c.tryGetRemoteValue(getCtx)
		getCancel()

		// Update local with remote if it is ahead
		c.updateMu.Lock()
		if remote > 0 {
			_ = c.processNewValue(UpdateSourceRemoteCheck, remote)
		}
		current := c.value.Load()
		c.updateMu.Unlock()

		if current <= 0 {
			return
		}
		pctx, cancel := context.WithTimeout(c.registry.appCtx, c.registry.fallbackTimeout)
		c.updateRemoteUpdate(pctx, current)
		cancel()
	}()
}
