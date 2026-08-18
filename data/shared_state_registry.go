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

// CounterValueSchemaVersion identifies the on-the-wire format this registry uses
// to persist (Set) and publish (PublishCounterInt64) counter values. Callers
// embed it in the counter key namespace (see the evm state poller) so that erpc
// instances running incompatible counter formats never read or write the same
// key.
//
// History:
//   - v1 (erpc <= 0.0.62): bare integer string, e.g. "12345", parsed via Sscanf/ParseInt.
//   - v2 (erpc >= 0.0.63): JSON CounterInt64State {value, updatedAt, updatedBy}.
//
// The two formats collide on a shared key: a v1 reader hitting a v2 JSON value
// fails with "expected integer" and never seeds the counter, breaking
// cross-instance block-tip coordination (and a v1 pub/sub subscriber silently
// drops v2 messages). Bump this whenever the persisted value or the pub/sub
// payload changes incompatibly. Crossing a version boundary cold-starts each
// counter once; it re-seeds within a poll interval and the foreground request
// path is local-only (see CounterInt64.TryUpdateIfStale), so requests are
// unaffected.
const CounterValueSchemaVersion = "v2"

type SharedStateRegistry interface {
	GetCounterInt64(key string, ignoreRollbackOf int64) CounterInt64SharedVariable
	GetLockTtl() time.Duration
	GetFallbackTimeout() time.Duration
	// Cordon-state persistence. All methods no-op when registry is nil.
	SetCordonState(ctx context.Context, projectId, upstreamId string, entry CordonStateEntry) error
	DeleteCordonState(ctx context.Context, projectId, upstreamId, method string) error
	LoadCordonStates(ctx context.Context, projectId, upstreamId string) ([]CordonStateEntry, error)
	// WatchCordonNotifications returns a counter used as a change-notification
	// channel. Register OnValue callbacks; value is a unix-ms timestamp that
	// bumps on every cordon/uncordon, broadcasting to all replicas via pubsub.
	WatchCordonNotifications(projectId, upstreamId string) CounterInt64SharedVariable
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

// cordonMapKey returns (partitionKey, rangeKey) for the cordon map blob.
// All cordons for an upstream are stored as a single JSON map at this key,
// avoiding a full SCAN on LoadCordonStates.
func (r *sharedStateRegistry) cordonMapKey(projectId, upstreamId string) (string, string) {
	return fmt.Sprintf("%s/cordon-map/%s/%s", r.clusterKey, projectId, upstreamId), "methods"
}

func (r *sharedStateRegistry) cordonNotifyKey(projectId, upstreamId string) string {
	return fmt.Sprintf("cordon-notify/%s/%s", projectId, upstreamId)
}

// readCordonMap fetches the current map[method]CordonStateEntry from Redis.
// Returns an empty map (not nil) when the key does not exist.
func (r *sharedStateRegistry) readCordonMap(ctx context.Context, pk, rk string) (map[string]CordonStateEntry, error) {
	raw, err := r.connector.Get(ctx, ConnectorMainIndex, pk, rk, nil)
	if err != nil {
		if common.HasErrorCode(err, common.ErrCodeRecordNotFound) {
			return map[string]CordonStateEntry{}, nil
		}
		return nil, err
	}
	var m map[string]CordonStateEntry
	if err := common.SonicCfg.Unmarshal(raw, &m); err != nil {
		return map[string]CordonStateEntry{}, nil
	}
	return m, nil
}

func (r *sharedStateRegistry) writeCordonMap(ctx context.Context, pk, rk string, m map[string]CordonStateEntry) error {
	if len(m) == 0 {
		// Clean up rather than persist an empty map.
		err := r.connector.Delete(ctx, pk, rk)
		if err != nil && !common.HasErrorCode(err, common.ErrCodeRecordNotFound) {
			return err
		}
		return nil
	}
	payload, err := common.SonicCfg.Marshal(m)
	if err != nil {
		return err
	}
	return r.connector.Set(ctx, pk, rk, payload, nil)
}

func (r *sharedStateRegistry) bumpCordonNotify(ctx context.Context, projectId, upstreamId string) {
	counter := r.GetCounterInt64(r.cordonNotifyKey(projectId, upstreamId), 0)
	counter.TryUpdate(ctx, time.Now().UnixMilli())
}

func (r *sharedStateRegistry) SetCordonState(ctx context.Context, projectId, upstreamId string, entry CordonStateEntry) error {
	pk, rk := r.cordonMapKey(projectId, upstreamId)
	// ponytail: optimistic read-modify-write; concurrent cordons could race, but
	// cordon ops are rare operator actions so last-write-wins is acceptable.
	m, err := r.readCordonMap(ctx, pk, rk)
	if err != nil {
		return err
	}
	m[entry.Method] = entry
	if err := r.writeCordonMap(ctx, pk, rk, m); err != nil {
		return err
	}
	r.bumpCordonNotify(ctx, projectId, upstreamId)
	return nil
}

func (r *sharedStateRegistry) DeleteCordonState(ctx context.Context, projectId, upstreamId, method string) error {
	pk, rk := r.cordonMapKey(projectId, upstreamId)
	m, err := r.readCordonMap(ctx, pk, rk)
	if err != nil {
		return err
	}
	delete(m, method)
	if err := r.writeCordonMap(ctx, pk, rk, m); err != nil {
		return err
	}
	r.bumpCordonNotify(ctx, projectId, upstreamId)
	return nil
}

func (r *sharedStateRegistry) LoadCordonStates(ctx context.Context, projectId, upstreamId string) ([]CordonStateEntry, error) {
	pk, rk := r.cordonMapKey(projectId, upstreamId)
	m, err := r.readCordonMap(ctx, pk, rk)
	if err != nil {
		return nil, err
	}
	entries := make([]CordonStateEntry, 0, len(m))
	for _, e := range m {
		entries = append(entries, e)
	}
	return entries, nil
}

func (r *sharedStateRegistry) WatchCordonNotifications(projectId, upstreamId string) CounterInt64SharedVariable {
	return r.GetCounterInt64(r.cordonNotifyKey(projectId, upstreamId), 0)
}
