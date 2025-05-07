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
	lastUpdated atomic.Int64 // Unix nanoseconds
}

func (v *baseSharedVariable) IsStale(staleness time.Duration) bool {
	return time.Since(time.Unix(0, v.lastUpdated.Load())) > staleness
}

type counterInt64 struct {
	baseSharedVariable
	registry              *sharedStateRegistry
	key                   string
	value                 atomic.Int64
	mu                    sync.Mutex // still needed for complex operations
	valueCallback         func(int64)
	ignoreRollbackOf      int64
	largeRollbackCallback func(localVal, newVal int64)
}

func (c *counterInt64) GetValue() int64 {
	return c.value.Load()
}

func (c *counterInt64) processNewValue(newVal int64) bool {
	// This function is designed to be called from within c.mu.Lock().
	// It returns true if the local value was actually updated.
	currentValue := c.value.Load()
	updated := false
	if newVal >= currentValue {
		c.setValue(newVal)
		updated = true
	} else if currentValue > newVal && (currentValue-newVal > c.ignoreRollbackOf) {
		c.setValue(newVal)
		if c.largeRollbackCallback != nil {
			c.largeRollbackCallback(currentValue, newVal)
		}
		updated = true
	}
	if updated {
		c.lastUpdated.Store(time.Now().UnixNano())
	}
	return updated
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

	c.mu.Lock()
	defer c.mu.Unlock()

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
		// Only update remote if local value was updated AND remote lock was acquired
		c.updateRemoteUpdate(span, newValue)
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

	c.mu.Lock()
	defer c.mu.Unlock()

	// Double-check staleness under lock
	if !c.IsStale(staleness) {
		return c.value.Load(), nil
	}

	rctx, cancel := context.WithTimeout(ctx, c.registry.fallbackTimeout)
	defer cancel()

	// Lock remotely if possible in best-effort mode
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
	newValue, err := executeNewValueFn(ctx)
	if err != nil {
		return c.value.Load(), err
	}

	// Update and publish if new value is applicable AND remote lock was acquired
	if c.processNewValue(newValue) && unlock != nil {
		c.updateRemoteUpdate(span, newValue)
	}

	// Return the final value from local storage, no matter how it was updated.
	return c.value.Load(), nil
}

func (c *counterInt64) OnValue(cb func(int64)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.valueCallback = cb
}

func (c *counterInt64) OnLargeRollback(cb func(currentVal, newVal int64)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.largeRollbackCallback = cb
}

func (c *counterInt64) setValue(val int64) {
	c.value.Store(val)
	if c.valueCallback != nil {
		c.valueCallback(val)
	}
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
		c.registry.logger.Warn().Err(err).Str("key", c.key).Msg("failed to get remote counter value")
	} else {
		remoteValueInt, err := strconv.ParseInt(remoteVal, 0, 0)
		if err != nil {
			c.registry.logger.Warn().Err(err).Str("key", c.key).Msg("failed to parse remote counter value")
		} else {
			return remoteValueInt
		}
	}
	return 0
}

func (c *counterInt64) updateRemoteUpdate(span trace.Span, newValue int64) {
	setCtx, setCancel := context.WithCancel(c.registry.appCtx)
	defer setCancel()
	setCtx = trace.ContextWithSpanContext(setCtx, span.SpanContext())
	err := c.registry.connector.Set(setCtx, c.key, "value", fmt.Sprintf("%d", newValue), nil)
	if err == nil {
		err = c.registry.connector.PublishCounterInt64(setCtx, c.key, newValue)
	}
	if err != nil {
		c.registry.logger.Warn().Err(err).
			Str("key", c.key).
			Int64("value", newValue).
			Msg("failed to update remote counter value")
	}
}
