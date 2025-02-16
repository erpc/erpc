package data

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/erpc/erpc/common"
)

type SharedVariable interface {
	IsStale(staleness time.Duration) bool
}

type CounterInt64SharedVariable interface {
	SharedVariable
	GetValue() int64
	TryUpdateIfStale(ctx context.Context, staleness time.Duration, getNewValue func() (int64, error)) (int64, error)
	TryUpdate(ctx context.Context, newValue int64) int64
	OnValue(callback func(int64))
}

type baseSharedVariable struct {
	lastUpdated time.Time
}

func (v *baseSharedVariable) IsStale(staleness time.Duration) bool {
	return time.Since(v.lastUpdated) > staleness
}

type counterInt64 struct {
	baseSharedVariable
	registry *sharedStateRegistry
	key      string
	value    int64
	mu       sync.RWMutex
	callback func(int64)
}

func (c *counterInt64) GetValue() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.value
}

func (c *counterInt64) TryUpdate(ctx context.Context, newValue int64) int64 {
	rctx, cancel := context.WithTimeout(ctx, c.registry.fallbackTimeout)
	defer cancel()

	// Try remote lock first
	lock, err := c.registry.connector.Lock(rctx, c.key, c.registry.lockTtl)
	if err != nil {
		// Fallback to local lock if remote lock fails
		c.mu.Lock()
		defer c.mu.Unlock()

		if newValue > c.value {
			c.setValue(newValue)
		}
		return c.value
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), c.registry.lockTtl)
		defer cancel()
		if err := lock.Unlock(unlockCtx); err != nil {
			c.registry.logger.Error().Err(err).Str("key", c.key).Msg("failed to unlock counter")
		}
	}()

	// Get remote value
	remoteVal, err := c.registry.connector.Get(rctx, ConnectorMainIndex, c.key, "value")
	if err != nil && !common.HasErrorCode(err, common.ErrCodeRecordNotFound) {
		// Fallback to local update on error
		c.mu.Lock()
		defer c.mu.Unlock()

		if newValue > c.value {
			c.setValue(newValue)
		}
		return c.value
	}

	var remoteValue int64
	if remoteVal != "" {
		if _, err := fmt.Sscanf(remoteVal, "%d", &remoteValue); err != nil {
			// Fallback to local update on parse error
			c.mu.Lock()
			defer c.mu.Unlock()

			if newValue > c.value {
				c.setValue(newValue)
			}
			return c.value
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Use highest value among local, remote, and new
	highestValue := c.value
	if remoteValue > highestValue {
		highestValue = remoteValue
	}
	if newValue > highestValue {
		highestValue = newValue

		go func() {
			// Only update remote if we're using the new value
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

	if highestValue > 0 {
		c.setValue(highestValue)
	}
	return c.value
}

func (c *counterInt64) TryUpdateIfStale(ctx context.Context, staleness time.Duration, getNewValue func() (int64, error)) (int64, error) {
	// Quick check if value is not stale using read lock
	c.mu.RLock()
	if !c.IsStale(staleness) {
		value := c.value
		c.mu.RUnlock()
		return value, nil
	}
	c.mu.RUnlock()

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
			return c.value, nil
		}

		newValue, err := getNewValue()
		if err != nil {
			return c.value, err
		}

		if newValue > c.value {
			c.setValue(newValue)
		}
		return c.value, nil
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), c.registry.lockTtl)
		defer cancel()
		if err := lock.Unlock(unlockCtx); err != nil {
			c.registry.logger.Error().Err(err).Str("key", c.key).Msg("failed to unlock counter")
		}
	}()

	// Get remote value
	remoteVal, err := c.registry.connector.Get(rctx, ConnectorMainIndex, c.key, "value")
	if err != nil && !common.HasErrorCode(err, common.ErrCodeRecordNotFound) {
		// Fallback to local update on error
		c.mu.Lock()
		defer c.mu.Unlock()

		if !c.IsStale(staleness) {
			return c.value, nil
		}

		newValue, err := getNewValue()
		if err != nil {
			return c.value, err
		}

		if newValue > c.value {
			c.setValue(newValue)
		}
		return c.value, nil
	}

	var remoteValue int64
	if remoteVal != "" {
		if _, err := fmt.Sscanf(remoteVal, "%d", &remoteValue); err != nil {
			return 0, fmt.Errorf("failed to parse remote value: %w", err)
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Update local value if remote is higher
	if remoteValue > c.value {
		c.setValue(remoteValue)
	}

	// Check staleness again under lock
	if !c.IsStale(staleness) {
		return c.value, nil
	}

	// Get new value
	newValue, err := getNewValue()
	if err != nil {
		return c.value, err
	}

	// Update if new value is higher
	if newValue > c.value {
		c.setValue(newValue)
		go func() {
			err := c.registry.connector.Set(ctx, c.key, "value", fmt.Sprintf("%d", newValue), nil)
			if err == nil {
				err = c.registry.connector.PublishCounterInt64(ctx, c.key, newValue)
			}
			if err != nil {
				c.registry.logger.Warn().Err(err).
					Str("key", c.key).
					Int64("value", newValue).
					Msg("failed to update remote value")
			}
		}()
	}

	return c.value, nil
}

func (c *counterInt64) OnValue(cb func(int64)) {
	c.callback = cb
}

func (c *counterInt64) setValue(val int64) {
	c.value = val
	c.lastUpdated = time.Now()
	if c.callback != nil {
		c.callback(val)
	}
}
