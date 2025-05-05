package data

import (
	"context"
	"fmt"
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
	lastProcessed atomic.Int64 // Unix nanoseconds
}

func (v *baseSharedVariable) IsStale(staleness time.Duration) bool {
	lastProcessedNano := v.lastProcessed.Load()
	return time.Since(time.Unix(0, lastProcessedNano)) > staleness
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

func (c *counterInt64) maybeUpdateValue(currentVal, newVal int64) bool {
	// This function is designed to be called from within c.mu.Lock().
	// It returns true if the local value was actually updated.
	updated := false
	if newVal > currentVal {
		c.setValue(newVal)
		updated = true
	} else if currentVal > newVal && (currentVal-newVal > c.ignoreRollbackOf) {
		c.setValue(newVal)
		if c.largeRollbackCallback != nil {
			c.largeRollbackCallback(currentVal, newVal)
		}
		updated = true
	}
	// Even when the value did not change, record the attempt so the debounce timer moves forward.
	if !updated {
		c.lastProcessed.Store(time.Now().UnixNano())
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

	rctx, cancel := context.WithTimeout(ctx, c.registry.fallbackTimeout)
	defer cancel()

	// Try remote lock first
	lock, err := c.registry.connector.Lock(rctx, c.key, c.registry.lockTtl)
	if err != nil {
		// Fallback to local lock if remote lock fails
		c.mu.Lock()
		defer c.mu.Unlock()

		currentValue := c.value.Load()
		_ = c.maybeUpdateValue(currentValue, newValue)
		return c.value.Load()
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), c.registry.lockTtl)
		defer cancel()
		if err := lock.Unlock(unlockCtx); err != nil {
			c.registry.logger.Warn().Err(err).Str("key", c.key).Int64("lock_ttl_ms", c.registry.lockTtl.Milliseconds()).Msg("failed to unlock counter, so it will be expired after ttl")
		}
	}()

	// Get remote value
	remoteVal, err := c.registry.connector.Get(rctx, ConnectorMainIndex, c.key, "value")
	if err != nil && !common.HasErrorCode(err, common.ErrCodeRecordNotFound) {
		// Fallback to local update on error
		c.mu.Lock()
		defer c.mu.Unlock()

		currentValue := c.value.Load()
		_ = c.maybeUpdateValue(currentValue, newValue)
		return c.value.Load()
	}

	var remoteValue int64
	if remoteVal != "" {
		if _, err := fmt.Sscanf(remoteVal, "%d", &remoteValue); err != nil {
			// Fallback to local update on parse error
			c.mu.Lock()
			defer c.mu.Unlock()

			currentValue := c.value.Load()
			_ = c.maybeUpdateValue(currentValue, remoteValue)
			return c.value.Load()
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Use highest value among local, remote, and new
	currentValue := c.value.Load()

	if remoteValue > currentValue {
		c.setValue(remoteValue)
		currentValue = remoteValue
	}

	if c.maybeUpdateValue(currentValue, newValue) {
		go func() {
			// Only update remote if we're using the new value
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
					Msg("failed to update remote value")
			}
		}()
	}

	return c.value.Load()
}

func (c *counterInt64) TryUpdateIfStale(ctx context.Context, staleness time.Duration, getNewValue func(ctx context.Context) (int64, error)) (int64, error) {
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

	rctx, cancel := context.WithTimeout(ctx, c.registry.fallbackTimeout)
	defer cancel()

	// Try remote lock first
	lock, err := c.registry.connector.Lock(rctx, c.key, c.registry.lockTtl)
	if err != nil {
		// Fallback to local lock if remote lock fails
		c.mu.Lock()
		defer c.mu.Unlock()

		// Double-check staleness under lock
		if !c.IsStale(staleness) {
			c.registry.logger.Info().Str("key", c.key).Msg("isNotStale xxxx1")
			return c.value.Load(), nil
		}

		c.registry.logger.Info().Str("key", c.key).Msg("isStale xxxx1")

		c.registry.logger.Info().Str("key", c.key).Msg("getNewValue xxxx1")
		newValue, err := getNewValue(ctx)
		if err != nil {
			// mark attempt so debounce interval is respected even on failure
			c.lastProcessed.Store(time.Now().UnixNano())
			return c.value.Load(), err
		}

		currentValue := c.value.Load()
		_ = c.maybeUpdateValue(currentValue, newValue)
		return c.value.Load(), nil
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), c.registry.lockTtl)
		defer cancel()
		if err := lock.Unlock(unlockCtx); err != nil {
			c.registry.logger.Warn().Err(err).Str("key", c.key).Msg("failed to unlock counter")
		}
	}()

	// Get remote value
	remoteVal, err := c.registry.connector.Get(rctx, ConnectorMainIndex, c.key, "value")
	if err != nil && !common.HasErrorCode(err, common.ErrCodeRecordNotFound) {
		// Fallback to local update on error
		c.mu.Lock()
		defer c.mu.Unlock()

		if !c.IsStale(staleness) {
			c.registry.logger.Info().Str("key", c.key).Msg("isNotStale xxxx2")
			return c.value.Load(), nil
		}

		c.registry.logger.Info().Str("key", c.key).Msg("isStale xxxx2")

		c.registry.logger.Info().Str("key", c.key).Msg("getNewValue xxxx2")
		newValue, err := getNewValue(ctx)
		if err != nil {
			// mark attempt so debounce interval is respected even on failure
			c.lastProcessed.Store(time.Now().UnixNano())
			return c.value.Load(), err
		}

		currentValue := c.value.Load()
		_ = c.maybeUpdateValue(currentValue, newValue)
		return c.value.Load(), nil
	}

	var remoteValue int64
	if remoteVal != "" {
		if _, err := fmt.Sscanf(remoteVal, "%d", &remoteValue); err != nil {
			// ensure failures still refresh lastUpdated
			c.lastProcessed.Store(time.Now().UnixNano())
			return 0, fmt.Errorf("failed to parse remote value: %w", err)
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Update local value if remote is higher
	currentValue := c.value.Load()
	if remoteValue > currentValue {
		c.setValue(remoteValue)
	}

	// Check staleness again under lock
	if !c.IsStale(staleness) {
		c.registry.logger.Info().Str("key", c.key).Msg("isNotStale xxxx3")
		return c.value.Load(), nil
	}

	c.registry.logger.Info().Str("key", c.key).Msg("isStale xxxx3")

	// Get new value
	c.registry.logger.Info().Str("key", c.key).Msg("getNewValue xxxx3")
	newValue, err := getNewValue(ctx)
	if err != nil {
		// mark attempt so debounce interval is respected even on failure
		c.lastProcessed.Store(time.Now().UnixNano())
		return c.value.Load(), err
	}

	// Update if new value is higher
	currentValue = c.value.Load() // Re-read in case it changed
	if c.maybeUpdateValue(currentValue, newValue) {
		go func() {
			err := c.registry.connector.Set(c.registry.appCtx, c.key, "value", fmt.Sprintf("%d", newValue), nil)
			if err == nil {
				err = c.registry.connector.PublishCounterInt64(c.registry.appCtx, c.key, newValue)
			}
			if err != nil {
				c.registry.logger.Warn().Err(err).
					Str("key", c.key).
					Int64("value", newValue).
					Msg("failed to update remote value")
			}
		}()
	}

	return c.value.Load(), nil
}

func (c *counterInt64) OnValue(cb func(int64)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.valueCallback = cb
}

func (c *counterInt64) setValue(val int64) {
	c.value.Store(val)
	c.lastProcessed.Store(time.Now().UnixNano())
	if c.valueCallback != nil {
		c.valueCallback(val)
	}
}

func (c *counterInt64) OnLargeRollback(cb func(currentVal, newVal int64)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.largeRollbackCallback = cb
}
