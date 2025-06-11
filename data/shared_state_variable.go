package data

import (
	"context"
	"fmt"
	"strconv"
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
}

func (c *counterInt64) GetValue() int64 {
	return c.value.Load()
}

func (c *counterInt64) processNewValue(newVal int64) bool {
	// This function is designed to be called from within c.updateMu.Lock().
	// It returns true if the local value was actually updated.
	currentValue := c.value.Load()
	updated := false
	if newVal > currentValue {
		c.value.Store(newVal)
		updated = true
		c.triggerValueCallback(newVal)
	} else if currentValue > newVal && (currentValue-newVal > c.ignoreRollbackOf) {
		c.value.Store(newVal)
		updated = true
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

	c.registry.logger.Trace().Str("key", c.key).Str("ptr", fmt.Sprintf("%p", c)).Int64("currentValue", currentValue).Int64("newVal", newVal).Bool("updated", updated).Msg("processed new value")
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

	rctx, cancel := context.WithTimeout(ctx, c.registry.fallbackTimeout)
	defer cancel()

	// Try remote lock as a best effort
	unlock := c.tryAcquireLock(rctx)
	if unlock != nil {
		// If remote lock was acquired, we need to unlock it after the whole operation is complete
		defer unlock()

		// Get current remote value if it exists
		remoteVal := c.tryGetRemoteValue(rctx)
		if remoteVal > 0 {
			// Use highest value local vs remote if applicable
			c.processNewValue(remoteVal)
		}
	}

	// Compare local vs new value
	if c.processNewValue(newValue) && unlock != nil {
		// Only update remote if local value was updated AND remote lock was acquired.
		// We create a new context so the 'set' timeout restarts here (not and punished by slow lock/get).
		pctx, cancel := context.WithTimeout(ctx, c.registry.fallbackTimeout)
		defer cancel()
		// The reason we don't run this within a "goroutine" is because we want to do this while "lock" is held.
		// The reason we don't care about errors (such as deadline exceeded) is because remote updates are best-effort,
		// as we don't want to block the usual flow of the program.
		c.updateRemoteUpdate(pctx, newValue)
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

	// Lock remotely if possible in best-effort mode
	rctx, cancel := context.WithTimeout(ctx, c.registry.fallbackTimeout)
	defer cancel()
	unlock := c.tryAcquireLock(rctx)
	if unlock != nil {
		// If remote lock was acquired, we need to unlock it after the whole operation is complete
		defer unlock()

		// Get existing remote value in case it is already updated by another instance
		if val := c.tryGetRemoteValue(rctx); val > 0 {
			if c.processNewValue(val) {
				return c.value.Load(), nil
			}
		}
	}

	// Refresh the new value via user-defined func because local value is stale AND remote value is not up-to-date
	c.registry.logger.Trace().
		Str("key", c.key).
		Str("ptr", fmt.Sprintf("%p", c)).
		Int64("c.value", c.value.Load()).
		Bool("unlockable", unlock != nil).
		Msg("refreshing new value via user-defined func because local value is stale AND remote value is not up-to-date")
	newValue, err := executeNewValueFn(ctx)
	if err != nil {
		// Avoid thundering herd in case of errors (wait equal to debounce window)
		c.lastProcessedMu.Lock()
		c.lastProcessed = time.Now()
		c.lastProcessedMu.Unlock()
		return c.value.Load(), err
	}
	c.registry.logger.Trace().Str("key", c.key).Str("ptr", fmt.Sprintf("%p", c)).Bool("unlockable", unlock != nil).Int64("newValue", newValue).Msg("received new value from user-defined func")

	// Process the new value locally
	if c.processNewValue(newValue) && unlock != nil {
		// Only update remote if local value was updated AND remote lock was acquired.
		// We create a new context so the 'set' timeout restarts here (not and punished by slow lock/get).
		pctx, cancel := context.WithTimeout(ctx, c.registry.fallbackTimeout)
		defer cancel()
		// The reason we don't run this within a "goroutine" is because we want to do this while "lock" is held.
		// The reason we don't care about errors (such as deadline exceeded) is because remote updates are best-effort,
		// as we don't want to block the usual flow of the program.
		c.updateRemoteUpdate(pctx, newValue)
	}

	// Return the final value from local storage, no matter how it was updated.
	return c.value.Load(), nil
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
		c.registry.logger.Warn().Err(err).Str("key", c.key).Msg("failed to remotely lock counter will only use local lock")
	}
	if lock != nil && !lock.IsNil() {
		return func() {
			unlockCtx, cancel := context.WithTimeout(c.registry.appCtx, c.registry.lockTtl)
			defer cancel()
			if err := lock.Unlock(unlockCtx); err != nil {
				c.registry.logger.Warn().Err(err).Str("key", c.key).Int64("lock_ttl_ms", c.registry.lockTtl.Milliseconds()).Msg("failed to unlock counter, so it will be expired after ttl")
			}
		}
	}
	return nil
}

func (c *counterInt64) tryGetRemoteValue(ctx context.Context) int64 {
	remoteVal, err := c.registry.connector.Get(ctx, ConnectorMainIndex, c.key, "value")
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
		c.registry.logger.Warn().Err(err).
			Str("key", c.key).
			Int64("value", newValue).
			Msg("failed to update remote counter value")
	}
}
