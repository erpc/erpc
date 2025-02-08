package data

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
)

type SharedStateRegistry interface {
	GetCounterInt64(key string) CounterInt64SharedVariable
	Close() error
}

type sharedStateRegistry struct {
	appCtx             context.Context
	logger             *zerolog.Logger
	connector          Connector
	variables          sync.Map // map[string]*counterInt64
	fallbackTimeout    time.Duration
	toleratedStaleness time.Duration
}

func NewSharedStateRegistry(
	appCtx context.Context,
	logger *zerolog.Logger,
	cfg *common.SharedStateConfig,
) (SharedStateRegistry, error) {
	lg := logger.With().Str("component", "sharedState").Logger()
	connector, err := NewConnector(appCtx, &lg, cfg.Connector)
	if err != nil {
		return nil, fmt.Errorf("failed to create connector: %w", err)
	}

	return &sharedStateRegistry{
		appCtx:          appCtx,
		logger:          &lg,
		connector:       connector,
		fallbackTimeout: cfg.FallbackTimeout,
	}, nil
}

func (r *sharedStateRegistry) GetCounterInt64(key string) CounterInt64SharedVariable {
	value, alreadySetup := r.variables.LoadOrStore(key, &counterInt64{
		registry: r,
		key:      key,
	})
	counter := value.(*counterInt64)

	// Setup sync only once per counter
	if !alreadySetup {
		go r.setupCounterSync(r.appCtx, counter)
		v, err := r.fetchValue(r.appCtx, key)
		if err != nil {
			r.logger.Error().Err(err).Str("key", key).Msg("failed to fetch initial value for counter")
		} else {
			r.logger.Debug().Str("key", key).Int64("value", v).Msg("fetched initial value for counter")
		}
		if v > 0 {
			counter.mu.Lock()
			defer counter.mu.Unlock()
			counter.setValue(v)
		}
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
			counter.mu.RLock()
			r.logger.Debug().Str("key", counter.key).Int64("currentValue", counter.value).Msg("stopping counter sync for shared state due to context cancellation")
			counter.mu.RUnlock()
			return
		case newValue, ok := <-updates:
			if !ok {
				return
			}
			counter.mu.Lock()
			r.logger.Debug().Str("key", counter.key).Int64("currentValue", counter.value).Int64("newValue", newValue).Msg("received new value from shared state")
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

func (r *sharedStateRegistry) fetchValue(ctx context.Context, key string) (int64, error) {
	tctx, cancel := context.WithTimeout(ctx, r.fallbackTimeout)
	defer cancel()
	remoteVal, err := r.connector.Get(tctx, ConnectorMainIndex, key, "value")
	if err != nil {
		return 0, fmt.Errorf("failed to get remote value: %w", err)
	}

	var remoteValue int64
	if remoteVal != "" {
		if _, err := fmt.Sscanf(remoteVal, "%d", &remoteValue); err != nil {
			return 0, fmt.Errorf("failed to parse remote value: %w", err)
		}
	}

	return remoteValue, nil
}
