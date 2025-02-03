package data

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
)

type sharedStateRegistry struct {
	logger    *zerolog.Logger
	connector Connector
	variables sync.Map // map[string]*counterInt64

	// Fallback settings
	fallbackTimeout  time.Duration
	debounceInterval time.Duration
}

func NewSharedStateRegistry(
	logger *zerolog.Logger,
	connector Connector,
	fallbackTimeout time.Duration,
	debounceInterval time.Duration,
) SharedStateRegistry {
	return &sharedStateRegistry{
		logger:           logger,
		connector:        connector,
		fallbackTimeout:  fallbackTimeout,
		debounceInterval: debounceInterval,
	}
}

func (r *sharedStateRegistry) GetCounterInt64(key string) CounterInt64SharedVariable {
	value, loaded := r.variables.LoadOrStore(key, &counterInt64{
		registry: r,
		key:      key,
	})
	counter := value.(*counterInt64)

	// Setup sync only once per counter
	if !loaded {
		go r.setupCounterSync(context.Background(), counter)
	}
	return counter
}

func (r *sharedStateRegistry) setupCounterSync(ctx context.Context, counter *counterInt64) {
	updates, cleanup, err := r.connector.WatchCounterInt64(ctx, counter.key)
	if err != nil {
		r.logger.Error().Err(err).Str("key", counter.key).Msg("failed to setup counter sync")
		return
	}
	defer cleanup()

	for {
		select {
		case <-ctx.Done():
			return
		case newValue, ok := <-updates:
			if !ok {
				return
			}
			counter.mu.Lock()
			if newValue > counter.value {
				counter.value = newValue
				counter.lastUpdated = time.Now()
			}
			counter.mu.Unlock()
		}
	}
}

func (r *sharedStateRegistry) Close() error {
	// Cleanup any resources if needed
	return nil
}

func (r *sharedStateRegistry) updateCounter(ctx context.Context, counter *counterInt64, newValue int64) (int64, bool, error) {
	// First check if the new value is higher than our local cache
	counter.mu.RLock()
	currentValue := counter.value
	counter.mu.RUnlock()

	if newValue <= currentValue {
		return currentValue, false, nil
	}

	// Try to update in remote storage first
	ctx, cancel := context.WithTimeout(ctx, r.fallbackTimeout)
	defer cancel()

	updated, err := r.updateRemoteCounter(ctx, counter.key, currentValue, newValue)
	if err != nil {
		// On error, fall back to local update if the value is higher
		r.logger.Warn().Err(err).
			Str("key", counter.key).
			Int64("currentValue", currentValue).
			Int64("newValue", newValue).
			Msg("failed to update remote counter, falling back to local")

		counter.mu.Lock()
		if newValue > counter.value {
			counter.value = newValue
			counter.lastUpdated = time.Now()
			counter.mu.Unlock()
			return newValue, true, nil
		}
		counter.mu.Unlock()
		return counter.value, false, nil
	}

	if updated {
		counter.mu.Lock()
		counter.value = newValue
		counter.lastUpdated = time.Now()
		counter.mu.Unlock()
		return newValue, true, nil
	}

	return currentValue, false, nil
}

func (r *sharedStateRegistry) updateRemoteCounter(ctx context.Context, key string, currentValue, newValue int64) (bool, error) {
	// Try to acquire a lock first
	lock, err := r.connector.Lock(ctx, fmt.Sprintf("lock:%s", key), r.fallbackTimeout)
	if err != nil {
		return false, fmt.Errorf("failed to acquire lock: %w", err)
	}
	defer lock.Unlock(ctx)

	// Read current remote value
	remoteVal, err := r.connector.Get(ctx, ConnectorMainIndex, key, "value")
	if err != nil && !common.HasErrorCode(err, common.ErrCodeRecordNotFound) {
		return false, fmt.Errorf("failed to get remote value: %w", err)
	}

	var remoteValue int64
	if remoteVal != "" {
		if _, err := fmt.Sscanf(remoteVal, "%d", &remoteValue); err != nil {
			return false, fmt.Errorf("failed to parse remote value: %w", err)
		}
	}

	// Only update if new value is higher than both current and remote
	if newValue > remoteValue && newValue > currentValue {
		err = r.connector.Set(ctx, key, "value", fmt.Sprintf("%d", newValue), nil)
		if err != nil {
			return false, fmt.Errorf("failed to set remote value: %w", err)
		}
		err = r.connector.PublishCounterInt64(ctx, key, newValue)
		if err != nil {
			return false, fmt.Errorf("failed to publish counter value: %w", err)
		}
		return true, nil
	}

	return false, nil
}
