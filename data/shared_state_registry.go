package data

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
)

type SharedStateRegistry interface {
	GetCounterInt64(key string) CounterInt64SharedVariable
}

type sharedStateRegistry struct {
	appCtx          context.Context
	logger          *zerolog.Logger
	clusterKey      string
	connector       Connector
	variables       sync.Map // map[string]*counterInt64
	fallbackTimeout time.Duration
	lockTtl         time.Duration
	initializer     *util.Initializer
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
		clusterKey:      cfg.ClusterKey,
		connector:       connector,
		fallbackTimeout: cfg.FallbackTimeout,
		lockTtl:         cfg.LockTtl,
		initializer:     util.NewInitializer(appCtx, &lg, nil),
	}, nil
}

func (r *sharedStateRegistry) GetCounterInt64(key string) CounterInt64SharedVariable {
	fkey := fmt.Sprintf("%s/%s", r.clusterKey, key)
	value, alreadySetup := r.variables.LoadOrStore(fkey, &counterInt64{
		registry: r,
		key:      fkey,
	})
	counter := value.(*counterInt64)

	// Setup sync only once per counter
	if !alreadySetup {
		go func() {
			err := r.initializer.ExecuteTasks(
				r.appCtx,
				r.buildCounterSyncTask(counter),
				r.buildInitialValueTask(counter),
			)
			if err != nil {
				r.logger.Error().Err(err).Str("key", fkey).Msg("failed to setup shared counter on initial attempt (will retry in background)")
			}
		}()
	}

	return counter
}

func (r *sharedStateRegistry) buildCounterSyncTask(counter *counterInt64) *util.BootstrapTask {
	return util.NewBootstrapTask(
		r.getCounterSyncTaskName(counter),
		func(ctx context.Context) error {
			return r.initCounterSync(counter)
		},
	)
}

func (r *sharedStateRegistry) buildInitialValueTask(counter *counterInt64) *util.BootstrapTask {
	return util.NewBootstrapTask(
		r.getInitialValueTaskName(counter),
		func(ctx context.Context) error {
			v, err := r.fetchValue(ctx, counter.key)
			if err != nil {
				if common.HasErrorCode(err, common.ErrCodeRecordNotFound) {
					r.logger.Debug().Str("key", counter.key).Msg("no local initial value found for counter")
					return nil
				} else {
					r.logger.Error().Err(err).Str("key", counter.key).Msg("failed to fetch initial value for counter")
					return err
				}
			}
			r.logger.Debug().Str("key", counter.key).Int64("value", v).Msg("fetched initial value for counter")
			if v > 0 {
				counter.mu.Lock()
				defer counter.mu.Unlock()
				if v > counter.value {
					counter.setValue(v)
				}
			}
			return nil
		},
	)
}

func (r *sharedStateRegistry) initCounterSync(counter *counterInt64) error {
	defer func() {
		if rc := recover(); rc != nil {
			err := fmt.Errorf("panic in counter sync: %v", rc)
			r.logger.Error().Interface("panic", rc).Str("key", counter.key).Msg("counter sync panic")
			r.initializer.MarkTaskAsFailed(r.getCounterSyncTaskName(counter), err)
		}
	}()

	// Initial setup using the provided context
	updates, cleanup, err := r.connector.WatchCounterInt64(r.appCtx, counter.key)
	if err != nil {
		r.logger.Error().Err(err).Str("key", counter.key).Msg("failed to setup counter sync")
		return err
	}

	// Start the watch loop in a goroutine
	go func() {
		if cleanup != nil {
			defer cleanup()
		}
		for {
			select {
			case <-r.appCtx.Done():
				return

			case newValue, ok := <-updates:
				if !ok {
					err := fmt.Errorf("shared int64 counter sync channel closed unexpectedly")
					r.initializer.MarkTaskAsFailed(r.getCounterSyncTaskName(counter), err)
					return
				}

				counter.mu.Lock()
				if newValue > counter.value {
					r.logger.Debug().Str("key", counter.key).Int64("currentValue", counter.value).Int64("newValue", newValue).Msg("received new value from shared state")
					counter.setValue(newValue)
				}
				counter.mu.Unlock()
			}
		}
	}()

	return nil
}

func (r *sharedStateRegistry) getCounterSyncTaskName(counter *counterInt64) string {
	return fmt.Sprintf("counterSync/%s", counter.key)
}

func (r *sharedStateRegistry) getInitialValueTaskName(counter *counterInt64) string {
	return fmt.Sprintf("initialValue/%s", counter.key)
}

func (r *sharedStateRegistry) fetchValue(ctx context.Context, key string) (int64, error) {
	remoteVal, err := r.connector.Get(ctx, ConnectorMainIndex, key, "value")
	if err != nil {
		return 0, err
	}

	var remoteValue int64
	if remoteVal != "" {
		if _, err := fmt.Sscanf(remoteVal, "%d", &remoteValue); err != nil {
			return 0, err
		}
	}

	return remoteValue, nil
}
