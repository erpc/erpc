package data

import (
	"context"
	"fmt"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
)

type SharedStateRegistry interface {
	GetCounterInt64(key string, ignoreRollbackOf int64) CounterInt64SharedVariable
	GetLockTtl() time.Duration
	GetFallbackTimeout() time.Duration
	// TryLock attempts to acquire a distributed lock with the given key.
	// Returns the lock if acquired, or nil if the lock is held by another instance.
	// The lock should be renewed periodically and released when no longer needed.
	TryLock(ctx context.Context, key string) (DistributedLock, error)
}

type sharedStateRegistry struct {
	appCtx          context.Context
	logger          *zerolog.Logger
	clusterKey      string
	instanceId      string
	connector       Connector
	variables       sync.Map // map[string]*counterInt64
	fallbackTimeout time.Duration
	lockTtl         time.Duration
	lockMaxWait     time.Duration
	updateMaxWait   time.Duration
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

	fallbackTimeout := cfg.FallbackTimeout.Duration()
	if fallbackTimeout <= 0 {
		fallbackTimeout = 3 * time.Second
	}
	lockTtl := cfg.LockTtl.Duration()
	if lockTtl <= 0 {
		lockTtl = 4 * time.Second
	}
	lockMaxWait := cfg.LockMaxWait.Duration()
	if lockMaxWait <= 0 {
		lockMaxWait = 100 * time.Millisecond
	}
	updateMaxWait := cfg.UpdateMaxWait.Duration()
	if updateMaxWait <= 0 {
		updateMaxWait = 50 * time.Millisecond
	}

	instanceId := resolveSharedStateInstanceID()

	return &sharedStateRegistry{
		appCtx:          appCtx,
		logger:          &lg,
		clusterKey:      cfg.ClusterKey,
		instanceId:      instanceId,
		connector:       connector,
		fallbackTimeout: fallbackTimeout,
		lockTtl:         lockTtl,
		lockMaxWait:     lockMaxWait,
		updateMaxWait:   updateMaxWait,
		initializer:     util.NewInitializer(appCtx, &lg, nil),
	}, nil
}

func resolveSharedStateInstanceID() string {
	if id := strings.TrimSpace(os.Getenv("INSTANCE_ID")); id != "" {
		return id
	}
	if id := strings.TrimSpace(os.Getenv("POD_NAME")); id != "" {
		return id
	}
	if id := strings.TrimSpace(os.Getenv("HOSTNAME")); id != "" {
		return id
	}
	if hn, err := os.Hostname(); err == nil {
		if hn = strings.TrimSpace(hn); hn != "" {
			return hn
		}
	}
	return "unknown"
}

func (r *sharedStateRegistry) GetCounterInt64(key string, ignoreRollbackOf int64) CounterInt64SharedVariable {
	fkey := fmt.Sprintf("%s/%s", r.clusterKey, key)
	value, alreadySetup := r.variables.LoadOrStore(fkey, &counterInt64{
		registry:         r,
		key:              fkey,
		ignoreRollbackOf: ignoreRollbackOf,
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
			raw, err := r.connector.Get(ctx, ConnectorMainIndex, counter.key, "value", nil)
			if err != nil {
				if common.HasErrorCode(err, common.ErrCodeRecordNotFound) {
					r.logger.Debug().Str("key", counter.key).Msg("no remote initial value found for counter")
					return nil
				}
				r.logger.Error().Err(err).Str("key", counter.key).Msg("failed to fetch initial value for counter")
				return err
			}

			var st CounterInt64State
			if err := common.SonicCfg.Unmarshal(raw, &st); err != nil || st.UpdatedAt <= 0 {
				// No backward compatibility: treat parse errors as missing
				r.logger.Debug().Str("key", counter.key).Msg("initial counter value is not a valid JSON state; treating as missing")
				return nil
			}

			r.logger.Debug().
				Str("key", counter.key).
				Int64("value", st.Value).
				Int64("updatedAt", st.UpdatedAt).
				Str("updatedBy", st.UpdatedBy).
				Msg("fetched initial value for counter")
			counter.processNewState(UpdateSourceInitialFetch, st)
			return nil
		},
	)
}

func (r *sharedStateRegistry) initCounterSync(counter *counterInt64) error {
	defer func() {
		if rc := recover(); rc != nil {
			telemetry.MetricUnexpectedPanicTotal.WithLabelValues(
				"shared-state-counter-sync",
				fmt.Sprintf("connector:%s cluster:%s", r.connector.Id(), r.clusterKey),
				common.ErrorFingerprint(rc),
			).Inc()
			r.logger.Error().
				Interface("panic", rc).
				Str("stack", string(debug.Stack())).
				Str("key", counter.key).
				Msg("unexpected panic in shared state counter sync")
			err := fmt.Errorf("unexpected panic in shared state counter sync: %v stack: %s", rc, string(debug.Stack()))
			r.initializer.MarkTaskAsFailed(r.getCounterSyncTaskName(counter), err)
		}
	}()

	// Initial setup using the provided context
	updates, cleanup, err := r.connector.WatchCounterInt64(r.appCtx, counter.key)
	if err != nil {
		r.logger.Error().Err(err).Str("key", counter.key).Msg("failed to setup counter sync")
		return err
	}

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

				r.logger.Debug().
					Str("key", counter.key).
					Int64("value", newValue.Value).
					Int64("updatedAt", newValue.UpdatedAt).
					Str("updatedBy", newValue.UpdatedBy).
					Msg("received new value from shared state sync")
				counter.processNewState(UpdateSourceRemoteSync, newValue)
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
	remoteVal, err := r.connector.Get(ctx, ConnectorMainIndex, key, "value", nil)
	if err != nil {
		return 0, err
	}

	var remoteValue int64
	if len(remoteVal) > 0 {
		if _, err := fmt.Sscanf(string(remoteVal), "%d", &remoteValue); err != nil {
			return 0, err
		}
	}

	return remoteValue, nil
}

func (r *sharedStateRegistry) GetLockTtl() time.Duration {
	return r.lockTtl
}

func (r *sharedStateRegistry) GetFallbackTimeout() time.Duration {
	return r.fallbackTimeout
}

func (r *sharedStateRegistry) TryLock(ctx context.Context, key string) (DistributedLock, error) {
	return r.connector.Lock(ctx, key, r.lockTtl)
}
