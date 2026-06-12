package data

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/jackc/pgconn"
	"github.com/jackc/pgx/v4"
	"github.com/jackc/pgx/v4/pgxpool"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

const (
	PostgreSQLDriverName = "postgresql"
)

// ErrConnectorNotReady is returned by every entry point on PostgreSQLConnector
// when no pool is currently available (first connect in flight, or all
// reconnects have failed so far). Callers can use errors.Is to distinguish
// this from real transport-layer failures so they can apply the right
// telemetry label and avoid re-triggering the reconnect cascade.
var ErrConnectorNotReady = errors.New("PostgreSQLConnector not connected yet")

var _ Connector = (*PostgreSQLConnector)(nil)

type PostgreSQLConnector struct {
	id     string
	logger *zerolog.Logger
	// appCtx is the long-lived context passed to NewPostgreSQLConnector. It
	// outlives any single connectTask invocation and is what background
	// goroutines (cleanup ticker, listener pumps) observe for shutdown.
	// Earlier code passed the per-attempt timeout context to startCleanup,
	// which caused the cleanup goroutine to exit microseconds after each
	// successful connect.
	appCtx context.Context
	conn   *pgxpool.Pool
	connMu sync.RWMutex
	// schemaApplied + schemaMu gate one-time schema setup (CREATE TABLE /
	// CREATE INDEX / pg_cron). The DDL is mostly idempotent but
	// (a) re-running on every reconnect adds load to the database and
	// pooler at exactly the moments when both are already strained, and
	// (b) `cron.schedule` is NOT idempotent — every call inserts a new
	// pg_cron job. We serialize the check-and-set under schemaMu so two
	// concurrent connectTask calls cannot both observe schemaApplied==false
	// and run the non-idempotent migration steps in parallel. (A bare
	// atomic.Bool would have a race between Load() and Store() that two
	// concurrent goroutines could pass through, both calling cron.schedule.)
	schemaMu      sync.Mutex
	schemaApplied bool
	// cleanupOnce gates the local expired-items goroutine so repeated
	// reconnects don't accumulate copies of it.
	cleanupOnce sync.Once
	// lastFailureMarkNanos records when the last MarkTaskAsFailed fired,
	// in nanoseconds since Unix epoch. Used by handleConnectionFailure to
	// coalesce concurrent failures so a burst of N failing Get/Set calls
	// triggers ONE reconnect, not N. See the 2026-05-13 incident notes on
	// handleConnectionFailure for why this matters (each MarkTaskAsFailed
	// fires an Error log in the initializer, and during the cascade those
	// logs were the dominant contributor to stdout fd-lock contention).
	lastFailureMarkNanos atomic.Int64
	initializer          *util.Initializer
	minConns             int32
	maxConns             int32
	table                string
	cleanupTicker        *time.Ticker
	initTimeout          time.Duration
	getTimeout           time.Duration
	setTimeout           time.Duration
	listeners            sync.Map      // map[string]*pgxListener
	listenerPool         *pgxpool.Pool // Separate pool for LISTEN connections
}

// failureMarkCooldown bounds how often handleConnectionFailure may trigger a
// reconnect via MarkTaskAsFailed. The cooldown only matters during a real
// outage — during steady-state operation the connector is in StateReady and
// the typed predicate filters out the noise that would otherwise feed it.
// Sized to be long enough that thousands of concurrent failures collapse to
// one mark, short enough that an actual transient failure still triggers
// recovery within a request budget.
const failureMarkCooldown = 1 * time.Second

type pgxListener struct {
	mu       sync.Mutex
	conn     *pgx.Conn
	watchers []chan CounterInt64State
}

var _ DistributedLock = &postgresLock{}

type postgresLock struct {
	conn   *pgxpool.Pool
	lockID int64
	logger *zerolog.Logger
	tx     pgx.Tx
}

func (l *postgresLock) IsNil() bool {
	return l == nil || l.conn == nil
}

func NewPostgreSQLConnector(
	ctx context.Context,
	logger *zerolog.Logger,
	id string,
	cfg *common.PostgreSQLConnectorConfig,
) (*PostgreSQLConnector, error) {
	lg := logger.With().Str("connector", id).Logger()
	lg.Debug().Interface("config", cfg).Msg("creating postgresql connector")

	connector := &PostgreSQLConnector{
		id:            id,
		logger:        &lg,
		appCtx:        ctx,
		table:         cfg.Table,
		minConns:      cfg.MinConns,
		maxConns:      cfg.MaxConns,
		initTimeout:   cfg.InitTimeout.Duration(),
		getTimeout:    cfg.GetTimeout.Duration(),
		setTimeout:    cfg.SetTimeout.Duration(),
		cleanupTicker: time.NewTicker(5 * time.Minute),
		connMu:        sync.RWMutex{},
	}

	// create an Initializer to handle (re)connecting
	connector.initializer = util.NewInitializer(ctx, &lg, nil)

	connectTask := util.NewBootstrapTask(connector.taskId(), func(ctx context.Context) error {
		return connector.connectTask(ctx, cfg)
	})

	if err := connector.initializer.ExecuteTasks(ctx, connectTask); err != nil {
		lg.Error().Err(err).Msg("failed to initialize postgres on first attempt (will retry in background)")
		// Return the connector so the app can proceed, but note that it's not ready yet.
		return connector, nil
	}

	return connector, nil
}

// connectTask establishes (or re-establishes) the pgxpool used by this
// connector. It is invoked once by the initial bootstrap call and again by
// the util.Initializer auto-retry loop whenever handleConnectionFailure
// marks the task as failed.
//
// All the slow work below — pgxpool.ConnectConfig (TCP dial, TLS, auth,
// opening MinConns connections) and the one-time schema setup — runs WITHOUT
// the connMu write lock. The previous design held connMu.Lock() for the
// entire ~5s body of this function, which serialized every Get/Set/Lock
// behind each reconnect attempt and was the proximate cause of the
// 2026-05-13 fd-lock cascade: traces surfaced as
// `PostgreSQLConnector not connected yet` with duration ≈ initTimeout
// because the connMu.RLock() inside Get was blocked on the connectTask's
// WRITE lock for ~5s.
//
// The write lock is now scoped down to the brief field-swap at the end
// (nanoseconds). The schema setup is gated by ensureSchema so it runs at
// most once per process lifetime — re-running DDL on every reconnect was
// also part of the cascade (especially the non-idempotent `cron.schedule`
// call). Old pools are closed AFTER releasing the lock so a slow Close
// (drain of in-flight queries) cannot stall the hot read path.
func (p *PostgreSQLConnector) connectTask(ctx context.Context, cfg *common.PostgreSQLConnectorConfig) error {
	config, err := pgxpool.ParseConfig(cfg.ConnectionUri)
	if err != nil {
		return common.NewTaskFatal(fmt.Errorf("failed to parse connection URI: %w", err))
	}
	config.MinConns = p.minConns
	config.MaxConns = p.maxConns
	config.MaxConnLifetime = 5 * time.Hour
	config.MaxConnIdleTime = 30 * time.Minute

	if iam := cfg.IAMAuth; iam != nil && iam.Enabled {
		sess, err := createAWSSession(iam.Auth, iam.Region)
		if err != nil {
			return common.NewTaskFatal(fmt.Errorf("rds iam: failed to create AWS session: %w", err))
		}
		// BeforeConnect is called on every new pgxpool connection, ensuring each
		// connection uses a fresh IAM token (tokens are valid for 15 minutes but
		// only checked at connection establishment time).
		config.BeforeConnect = newRDSBeforeConnect(sess, iam)
		p.logger.Info().
			Str("endpoint", iam.Endpoint).
			Str("region", iam.Region).
			Str("dbUser", iam.DBUser).
			Msg("PostgreSQL IAM auth enabled (RDS)")
	}

	connectCtx, cancel := context.WithTimeout(ctx, p.initTimeout)
	defer cancel()

	newConn, err := pgxpool.ConnectConfig(connectCtx, config)
	if err != nil {
		return err
	}

	// Apply schema at most once successfully per process. ensureSchema
	// serializes the check-and-set under a mutex so two concurrent
	// connectTask calls can't both run the migration in parallel — only
	// the `CREATE TABLE IF NOT EXISTS` / `CREATE INDEX IF NOT EXISTS` parts
	// are safe under that race; the TEXT→BYTEA `DROP COLUMN` and
	// `cron.schedule` steps are not.
	if err := p.ensureSchema(connectCtx, newConn, cfg); err != nil {
		// The pool we just opened is about to be discarded — close it
		// synchronously so its connections are released back to the
		// pooler rather than lingering until GC.
		newConn.Close()
		return err
	}

	// Publish the new pool. This is the only critical section: readers see
	// a consistent snapshot of (conn, listenerPool) and the swap is
	// nanoseconds, not seconds.
	p.connMu.Lock()
	oldConn := p.conn
	oldListenerPool := p.listenerPool
	p.conn = newConn
	// Force lazy re-creation of the listener pool against the fresh main
	// pool on the next WatchCounterInt64 call.
	p.listenerPool = nil
	p.connMu.Unlock()

	// Close the old pools OUTSIDE the lock. pgxpool.Pool.Close blocks until
	// in-flight queries return, which can take seconds; doing it under the
	// lock would defeat the entire purpose of the swap. Doing it
	// synchronously (rather than in `go oldConn.Close()`) is preferred
	// because the lock is already released — readers proceed against the
	// new pool — and the initializer sees connectTask fully complete only
	// after the previous pool has drained, which keeps semantics
	// deterministic.
	if oldConn != nil {
		oldConn.Close()
	}
	if oldListenerPool != nil {
		oldListenerPool.Close()
	}

	p.logger.Info().Str("table", p.table).Msg("successfully connected to postgres")

	// Spawn the local expired-items cleanup goroutine at most once per
	// connector lifetime, regardless of how many times connectTask runs.
	// Use p.appCtx — NOT the per-attempt ctx — so the goroutine survives
	// the `defer cancel()` above and lives for the connector's actual
	// lifetime. The nil check lives inside the Once closure so the gate
	// fires exactly once even on the pg_cron path (where applySchema sets
	// cleanupTicker=nil).
	p.cleanupOnce.Do(func() {
		if p.cleanupTicker != nil {
			go p.startCleanup(p.appCtx)
		}
	})
	return nil
}

// ensureSchema runs applySchema at most once successfully per process
// lifetime. The check-and-set is serialized under schemaMu so concurrent
// connectTask callers can't race past the gate and both run the
// (partially non-idempotent) migration in parallel. On failure the bool
// stays false and the next connectTask will retry.
func (p *PostgreSQLConnector) ensureSchema(ctx context.Context, conn *pgxpool.Pool, cfg *common.PostgreSQLConnectorConfig) error {
	p.schemaMu.Lock()
	defer p.schemaMu.Unlock()
	if p.schemaApplied {
		return nil
	}
	if err := p.applySchema(ctx, conn, cfg); err != nil {
		return err
	}
	p.schemaApplied = true
	return nil
}

// applySchema runs the idempotent one-time schema setup against the
// supplied pool. It is intentionally split from connectTask so that
// reconnects can rebuild only the pool without re-issuing DDL — see the
// long comment on connectTask for why this matters in production.
func (p *PostgreSQLConnector) applySchema(ctx context.Context, conn *pgxpool.Pool, cfg *common.PostgreSQLConnectorConfig) error {
	// Create table if not exists with TTL column
	if _, err := conn.Exec(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			partition_key TEXT,
			range_key TEXT,
			value BYTEA,
			expires_at TIMESTAMP WITH TIME ZONE,
			PRIMARY KEY (partition_key, range_key)
		)
	`, cfg.Table)); err != nil {
		return err
	}

	// Migrate existing TEXT column to BYTEA if needed
	var dataType string
	err := conn.QueryRow(ctx, `
		SELECT data_type 
		FROM information_schema.columns 
		WHERE table_name = $1 AND column_name = 'value'
	`, cfg.Table).Scan(&dataType)

	if err == nil && dataType == "text" {
		p.logger.Info().Msg("migrating value column from TEXT to BYTEA")

		if _, err := conn.Exec(ctx, fmt.Sprintf(`
			ALTER TABLE %s ADD COLUMN IF NOT EXISTS value_new BYTEA
		`, cfg.Table)); err != nil {
			return fmt.Errorf("failed to add temporary column: %w", err)
		}

		if _, err := conn.Exec(ctx, fmt.Sprintf(`
			UPDATE %s SET value_new = value::bytea WHERE value IS NOT NULL
		`, cfg.Table)); err != nil {
			return fmt.Errorf("failed to migrate data: %w", err)
		}

		if _, err := conn.Exec(ctx, fmt.Sprintf(`
			ALTER TABLE %s DROP COLUMN value;
			ALTER TABLE %s RENAME COLUMN value_new TO value;
		`, cfg.Table, cfg.Table)); err != nil {
			return fmt.Errorf("failed to complete migration: %w", err)
		}

		p.logger.Info().Msg("successfully migrated value column to BYTEA")
	}

	// Add expires_at column if it doesn't exist
	if _, err := conn.Exec(ctx, fmt.Sprintf(`
        ALTER TABLE %s
        ADD COLUMN IF NOT EXISTS expires_at TIMESTAMP WITH TIME ZONE
    `, cfg.Table)); err != nil {
		return fmt.Errorf("failed to add expires_at column: %w", err)
	}

	// Create index for reverse lookups (range_key first to support queries that filter by range_key)
	if _, err := conn.Exec(ctx, fmt.Sprintf(`
		CREATE INDEX IF NOT EXISTS idx_reverse ON %s (range_key, partition_key)
	`, cfg.Table)); err != nil {
		return fmt.Errorf("failed to create reverse index: %w", err)
	}

	// Create index for TTL cleanup
	if _, err := conn.Exec(ctx, fmt.Sprintf(`
		CREATE INDEX IF NOT EXISTS idx_expires_at ON %s (expires_at)
		WHERE expires_at IS NOT NULL
	`, cfg.Table)); err != nil {
		return fmt.Errorf("failed to create TTL index: %w", err)
	}

	// Try to set up pg_cron cleanup job if extension exists
	var hasPgCron bool
	if err := conn.QueryRow(ctx, `
        SELECT EXISTS (
            SELECT 1 FROM pg_extension WHERE extname = 'pg_cron'
        )
    `).Scan(&hasPgCron); err != nil {
		p.logger.Warn().Err(err).Msg("failed to check for pg_cron extension")
	}

	if hasPgCron {
		if _, err := conn.Exec(ctx, fmt.Sprintf(`
            SELECT cron.schedule('*/5 * * * *', $$
                DELETE FROM %s
                WHERE expires_at IS NOT NULL AND expires_at <= NOW() AT TIME ZONE 'UTC'
            $$)
        `, p.table)); err != nil {
			p.logger.Warn().Err(err).Msg("failed to create pg_cron cleanup job, falling back to local cleanup")
		} else {
			p.logger.Info().Msg("successfully configured pg_cron cleanup job")
			// Don't start the local cleanup routine since we're using pg_cron.
			// Safe to mutate here: initSchema is only ever called once
			// (gated on schemaInitialized) and before connectTask spawns
			// the cleanup goroutine.
			p.cleanupTicker = nil
		}
	}

	return nil
}

func (p *PostgreSQLConnector) Id() string {
	return p.id
}

// acquirePool takes the connMu read lock and returns the live pgxpool
// snapshot together with a release function that the caller MUST defer.
// It centralises the not-ready check so every entry point (Get/Set/Lock/
// Delete/List/PublishCounterInt64) emits the same ErrConnectorNotReady
// sentinel and the same span attribution.
//
// If the pool is nil (first init in flight, or reconnect storm has not
// recovered yet), the read lock is released immediately and the span is
// tagged before returning. Callers should treat the returned error
// identically to any other connector error path; the sentinel is
// errors.Is-comparable to ErrConnectorNotReady so the consumer-side auth
// strategy can classify it as `db_not_ready` instead of `db_connection`.
//
// Usage:
//
//	pool, release, err := p.acquirePool(span)
//	if err != nil { return err }
//	defer release()
//	// pool is safe to use until release() is called.
func (p *PostgreSQLConnector) acquirePool(span trace.Span) (*pgxpool.Pool, func(), error) {
	p.connMu.RLock()
	if p.conn == nil {
		p.connMu.RUnlock()
		common.SetTraceSpanError(span, ErrConnectorNotReady)
		return nil, nil, ErrConnectorNotReady
	}
	return p.conn, p.connMu.RUnlock, nil
}

func (p *PostgreSQLConnector) Set(ctx context.Context, partitionKey, rangeKey string, value []byte, ttl *time.Duration) error {
	ctx, span := common.StartSpan(ctx, "PostgreSQLConnector.Set")
	defer span.End()

	if common.IsTracingDetailed {
		span.SetAttributes(
			attribute.String("partition_key", partitionKey),
			attribute.String("range_key", rangeKey),
			attribute.Int("value_size", len(value)),
		)
	}

	pool, release, err := p.acquirePool(span)
	if err != nil {
		return err
	}
	defer release()

	if len(value) < 1024 {
		p.logger.Debug().Int("length", len(value)).Str("partitionKey", partitionKey).Str("rangeKey", rangeKey).Msg("writing to postgres")
	} else {
		p.logger.Debug().Int("length", len(value)).Str("partitionKey", partitionKey).Str("rangeKey", rangeKey).Msg("writing to postgres")
	}

	var expiresAt *time.Time
	if ttl != nil && *ttl > 0 {
		t := time.Now().UTC().Add(*ttl)
		expiresAt = &t
	}

	ctx, cancel := context.WithTimeout(ctx, p.setTimeout)
	defer cancel()

	if expiresAt != nil {
		_, err = pool.Exec(ctx, fmt.Sprintf(`
			INSERT INTO %s (partition_key, range_key, value, expires_at)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (partition_key, range_key) DO UPDATE
			SET value = $3, expires_at = $4
		`, p.table), partitionKey, rangeKey, value, expiresAt)
	} else {
		_, err = pool.Exec(ctx, fmt.Sprintf(`
			INSERT INTO %s (partition_key, range_key, value)
			VALUES ($1, $2, $3)
			ON CONFLICT (partition_key, range_key) DO UPDATE
			SET value = $3
		`, p.table), partitionKey, rangeKey, value)
	}

	if err != nil {
		p.handleConnectionFailure(err)
		common.SetTraceSpanError(span, err)
	}

	return err
}

func (p *PostgreSQLConnector) Get(ctx context.Context, index, partitionKey, rangeKey string, _ interface{}) ([]byte, error) {
	ctx, span := common.StartSpan(ctx, "PostgreSQLConnector.Get")
	defer span.End()

	if common.IsTracingDetailed {
		span.SetAttributes(
			attribute.String("index", index),
			attribute.String("partition_key", partitionKey),
			attribute.String("range_key", rangeKey),
		)
	}

	pool, release, err := p.acquirePool(span)
	if err != nil {
		return nil, err
	}
	defer release()

	var query string
	var args []interface{}

	ctx, cancel := context.WithTimeout(ctx, p.getTimeout)
	defer cancel()

	if strings.HasSuffix(partitionKey, "*") || strings.HasSuffix(rangeKey, "*") {
		return p.getWithWildcard(ctx, pool, index, partitionKey, rangeKey)
	}

	query = fmt.Sprintf(`
		SELECT value FROM %s
		WHERE partition_key = $1 AND range_key = $2
		AND (expires_at IS NULL OR expires_at > NOW() AT TIME ZONE 'UTC')
	`, p.table)
	args = []interface{}{partitionKey, rangeKey}

	p.logger.Debug().Str("query", query).Interface("args", args).Msg("getting item from postgres")

	var value []byte
	err = pool.QueryRow(ctx, query, args...).Scan(&value)

	if err != nil {
		p.handleConnectionFailure(err)
		common.SetTraceSpanError(span, err)
	}

	if err == pgx.ErrNoRows {
		err := common.NewErrRecordNotFound(partitionKey, rangeKey, PostgreSQLDriverName)
		common.SetTraceSpanError(span, err)
		return nil, err
	}

	if common.IsTracingDetailed {
		span.SetAttributes(
			attribute.Int("value_size", len(value)),
		)
	}

	return value, err
}

func (p *PostgreSQLConnector) Lock(ctx context.Context, key string, ttl time.Duration) (DistributedLock, error) {
	ctx, span := common.StartSpan(ctx, "PostgreSQLConnector.Lock")
	defer span.End()

	if common.IsTracingDetailed {
		span.SetAttributes(
			attribute.String("lock_key", key),
			attribute.Int64("ttl_ms", ttl.Milliseconds()),
		)
	}

	pool, release, err := p.acquirePool(span)
	if err != nil {
		return nil, err
	}
	defer release()

	// Generate consistent hash for the key as advisory lock ID
	h := fnv.New64a()
	if _, err := h.Write([]byte(key)); err != nil {
		common.SetTraceSpanError(span, err)
		return nil, fmt.Errorf("failed to generate advisory lock ID: %w", err)
	}
	lockID := int64(h.Sum64()) // #nosec

	// Start a transaction
	tx, err := pool.Begin(ctx)
	if err != nil {
		p.handleConnectionFailure(err)
		common.SetTraceSpanError(span, err)
		return nil, fmt.Errorf("failed to begin transaction: %w", err)
	}

	// Try to acquire transaction-level advisory lock
	var acquired bool
	err = tx.QueryRow(ctx, `
        SELECT pg_try_advisory_xact_lock($1)
    `, lockID).Scan(&acquired)

	if err != nil {
		p.handleConnectionFailure(err)
		go tx.Rollback(context.Background())
		common.SetTraceSpanError(span, err)
		return nil, fmt.Errorf("failed to acquire advisory lock: %w", err)
	}

	if !acquired {
		go tx.Rollback(context.Background())
		err := fmt.Errorf("failed to acquire lock: already locked")
		common.SetTraceSpanError(span, err)
		return nil, err
	}

	p.logger.Trace().Str("key", key).Int64("lockID", lockID).Msg("distributed lock acquired")

	return &postgresLock{
		conn:   pool,
		lockID: lockID,
		logger: p.logger,
		tx:     tx,
	}, nil
}

func (l *postgresLock) Unlock(ctx context.Context) error {
	ctx, span := common.StartSpan(ctx, "PostgreSQLConnector.Unlock",
		trace.WithAttributes(
			attribute.Int64("lockID", l.lockID),
		),
	)
	defer span.End()

	if l.tx == nil {
		err := fmt.Errorf("no active transaction")
		common.SetTraceSpanError(span, err)
		return err
	}

	// Commit or rollback the transaction will automatically release the advisory lock
	err := l.tx.Commit(ctx)
	if err != nil {
		// Try to rollback if commit fails
		rollbackErr := l.tx.Rollback(ctx)
		if rollbackErr != nil {
			l.logger.Error().Err(rollbackErr).Msg("failed to rollback transaction after commit failure")
		}
		common.SetTraceSpanError(span, err)
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	l.logger.Trace().Int64("lockID", l.lockID).Msg("distributed lock released")

	l.tx = nil
	return nil
}

func (p *PostgreSQLConnector) WatchCounterInt64(ctx context.Context, key string) (<-chan CounterInt64State, func(), error) {
	updates := make(chan CounterInt64State, 1)

	// Create or get listener for this key
	listener, err := p.getOrCreateListener(ctx, key)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create listener: %w", err)
	}

	// Add watcher to listener
	listener.mu.Lock()
	listener.watchers = append(listener.watchers, updates)
	listener.mu.Unlock()

	p.logger.Debug().Str("key", key).Int("watchers", len(listener.watchers)).Msg("starting watcher for key")

	// Start fallback polling
	ticker := time.NewTicker(30 * time.Second)
	go func() {
		for {
			select {
			case <-ctx.Done():
				p.logger.Debug().Str("key", key).Msg("stopping watcher for key due to context termination")
				return
			case <-ticker.C:
				if st, ok, err := p.getCurrentValue(ctx, key); err == nil && ok {
					select {
					case updates <- st:
					default:
					}
				} else {
					p.logger.Warn().Err(err).Str("key", key).Msg("failed to proactively get current value from postgres")
				}
			}
		}
	}()

	// Send initial value
	if st, ok, err := p.getCurrentValue(ctx, key); err == nil && ok {
		updates <- st
	}

	cleanup := func() {
		ticker.Stop()

		listener.mu.Lock()
		defer listener.mu.Unlock()

		// Remove this watcher
		for i, ch := range listener.watchers {
			if ch == updates {
				listener.watchers = append(listener.watchers[:i], listener.watchers[i+1:]...)
				break
			}
		}

		close(updates)
	}

	return updates, cleanup, nil
}

func (p *PostgreSQLConnector) PublishCounterInt64(ctx context.Context, key string, value CounterInt64State) error {
	ctx, span := common.StartSpan(ctx, "PostgreSQLConnector.PublishCounterInt64",
		trace.WithAttributes(
			attribute.String("key", key),
		),
	)
	defer span.End()

	if common.IsTracingDetailed {
		span.SetAttributes(
			attribute.Int64("value", value.Value),
			attribute.Int64("updated_at", value.UpdatedAt),
			attribute.String("updated_by", value.UpdatedBy),
		)
	}

	pool, release, err := p.acquirePool(span)
	if err != nil {
		return err
	}
	defer release()

	p.logger.Debug().Str("key", key).Int64("value", value.Value).Msg("publishing counter update to postgres")

	channel := sanitizeChannelName(fmt.Sprintf("counter_%s", key))
	payload, err := common.SonicCfg.Marshal(value)
	if err != nil {
		common.SetTraceSpanError(span, err)
		return err
	}
	_, err = pool.Exec(ctx, "SELECT pg_notify($1, $2)", channel, string(payload))

	if err != nil {
		common.SetTraceSpanError(span, err)
	}

	return err
}

func (p *PostgreSQLConnector) taskId() string {
	return fmt.Sprintf("postgres-connect/%s", p.id)
}

func (p *PostgreSQLConnector) handleConnectionFailure(err error) {
	if !isPostgresConnectionError(err) {
		return
	}
	s := p.initializer.State()
	if s == util.StateInitializing || s == util.StateRetrying {
		// Demoted to Debug: during the 2026-05-13 cascade this branch fired
		// thousands of times per second once the reconnect loop kicked in,
		// and the Warn-level fan-out into stdout was the primary contributor
		// to the fd-lock contention that ultimately leaked goroutines.
		// Once the connector is already initializing/retrying there is no
		// additional action to take here.
		p.logger.Debug().Err(err).Str("state", s.String()).Msg("postgres connection error during reinit; not re-marking")
		return
	}
	// Coalesce concurrent failures: only one goroutine in a cooldown window
	// gets to call MarkTaskAsFailed (which logs at Error and triggers the
	// initializer auto-retry). With a typical edge fleet of 1000+ in-flight
	// auth queries, an unfiltered transition from Ready → Failed otherwise
	// produces 1000+ identical "marking task as failed" Error logs in the
	// same millisecond.
	now := time.Now().UnixNano()
	last := p.lastFailureMarkNanos.Load()
	if now-last < int64(failureMarkCooldown) {
		// Another goroutine already triggered (or is about to) within the
		// cooldown window. Nothing to do — the initializer's auto-retry
		// loop is already in motion.
		return
	}
	if !p.lastFailureMarkNanos.CompareAndSwap(last, now) {
		// Lost the race against another concurrent failure handler.
		return
	}
	p.logger.Warn().Err(err).Str("state", s.String()).Msg("postgres connection lost; marking connector as failed for reinitialization")
	p.initializer.MarkTaskAsFailed(p.taskId(), err)
}

// isPostgresConnectionError reports whether err indicates a real transport-layer
// connection failure that warrants tearing the pool down and reinitializing it.
//
// It is deliberately strict — root cause analysis of the 2026-05-13 edge-prod
// incident traced the cascade to the previous predicate matching the bare
// word "connection" anywhere in the error string. That matched:
//   - "PostgreSQLConnector not connected yet" (our own sentinel during reconnect)
//   - "too many connections for role" (application-level capacity, not a broken
//     transport)
//   - "connection limit exceeded for non-superusers"
//
// All three trigger the reconnect loop, which holds connMu and re-runs schema
// migration, which makes the next batch of queries even more likely to surface
// "not connected yet" — a self-sustaining cascade.
//
// We now opt-in only the error shapes that definitively mean "this socket is
// gone": typed pgconn 08* SQLSTATEs, kernel-level connection errors, and a
// narrow allowlist of substrings for opaque transport faults that don't
// unwrap to a typed error.
func isPostgresConnectionError(err error) bool {
	if err == nil {
		return false
	}
	// Caller-side context errors never indicate a broken transport — they
	// just mean the caller gave up waiting. Reconnecting on these would
	// be a pure footgun.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// Record-not-found is a business signal, not a transport failure.
	if errors.Is(err, pgx.ErrNoRows) || common.HasErrorCode(err, common.ErrCodeRecordNotFound) {
		return false
	}
	// Our own sentinel: if we surface this it means a reconnect is already
	// in flight, and triggering another MarkTaskAsFailed is exactly the
	// feedback loop the 2026-05-13 incident traced to.
	if errors.Is(err, ErrConnectorNotReady) {
		return false
	}
	// pgconn surfaces server-side errors with five-character SQLSTATEs.
	// The 08xxx class — "Connection Exception" — is the only class that
	// means the underlying connection is broken; every other class
	// (constraint violations, syntax errors, permission errors, capacity
	// errors like "too many connections") is application-level and must
	// NOT trigger pool reinit.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return strings.HasPrefix(pgErr.Code, "08")
	}
	// Kernel-level transport faults that pgx wraps but doesn't classify.
	if errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ETIMEDOUT) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	// Last-resort substring match for transport faults that don't unwrap to
	// a typed error. The bare word "connection" is deliberately NOT in this
	// list — see the long comment above for why.
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "connection refused"),
		strings.Contains(msg, "connection reset"),
		strings.Contains(msg, "broken pipe"),
		strings.Contains(msg, "no route to host"),
		strings.Contains(msg, "tls handshake"),
		strings.Contains(msg, "i/o timeout"),
		strings.Contains(msg, "use of closed network connection"),
		strings.Contains(msg, "unexpectedly closed"):
		return true
	}
	return false
}

func (p *PostgreSQLConnector) getOrCreateListener(ctx context.Context, key string) (*pgxListener, error) {
	if l, ok := p.listeners.Load(key); ok {
		return l.(*pgxListener), nil
	}

	listener := &pgxListener{}
	channel := sanitizeChannelName(fmt.Sprintf("counter_%s", key))

	conn, err := p.connectListener(ctx, channel)
	if err != nil {
		return nil, err
	}

	go func() {
		for {
			if err := ctx.Err(); err != nil {
				p.logger.Debug().Err(err).Str("key", key).Msg("stopping postgres listener due to context termination")
				return
			}

			notification, err := conn.Conn().WaitForNotification(ctx)
			if err != nil {
				// Try to reconnect
				p.logger.Warn().Err(err).Str("key", key).Msg("lost postgres connection, attempting reconnect")
				if newConn, err := p.connectListener(ctx, channel); err == nil {
					p.logger.Debug().Str("key", key).Msg("successfully reconnected to postgres channel")
					conn = newConn
					continue
				}
				return
			}

			p.logger.Trace().Str("key", key).Interface("payload", notification).Msg("received postgres notification")

			// Parse and broadcast state
			var st CounterInt64State
			if err := common.SonicCfg.Unmarshal([]byte(notification.Payload), &st); err == nil && st.UpdatedAt > 0 {
				listener.mu.Lock()
				for _, ch := range listener.watchers {
					select {
					case ch <- st:
					default:
					}
				}
				listener.mu.Unlock()
			}
		}
	}()

	p.logger.Debug().Str("key", key).Msg("successfully created postgres listener for key")
	listener.conn = conn.Conn()
	p.listeners.Store(key, listener)
	return listener, nil
}

func (p *PostgreSQLConnector) connectListener(ctx context.Context, channel string) (*pgxpool.Conn, error) {
	for {
		if err := ctx.Err(); err != nil {
			p.logger.Debug().Err(err).Str("channel", channel).Msg("stopping postgres listener reconnection due to context termination")
			return nil, err
		}
		p.logger.Trace().Str("channel", channel).Msg("attempting to connect to postgres channel")

		// Snapshot the current pool state under RLock so we don't hold the
		// WRITE lock across the slow pgxpool.ConnectConfig dial below. The
		// previous design held connMu.Lock() for the entire body of this
		// function — including ConnectConfig and Acquire — which
		// serialized every Get/Set/Lock behind any listener that needed to
		// build a new pool. This is the same anti-pattern that connectTask
		// used to have (see the long comment there for incident context).
		p.connMu.RLock()
		listenerPool := p.listenerPool
		mainConn := p.conn
		p.connMu.RUnlock()

		if listenerPool == nil {
			if mainConn == nil {
				time.Sleep(5 * time.Second)
				continue
			}
			lcfg, err := pgxpool.ParseConfig(mainConn.Config().ConnString())
			if err != nil {
				return nil, err
			}
			lcfg.MaxConns = p.maxConns
			// Build the new listener pool OUTSIDE any lock — this is the
			// slow operation (TCP dial + TLS + auth + MinConns conns).
			newPool, err := pgxpool.ConnectConfig(ctx, lcfg)
			if err != nil {
				time.Sleep(5 * time.Second)
				continue
			}
			// Brief WRITE lock only to install. Handle the race where
			// another goroutine on this connector finished its own build
			// first.
			p.connMu.Lock()
			if p.listenerPool == nil {
				p.listenerPool = newPool
				listenerPool = newPool
			} else {
				listenerPool = p.listenerPool
			}
			p.connMu.Unlock()
			// If we lost the race, close our orphan pool.
			if listenerPool != newPool {
				newPool.Close()
			}
		}

		// listenerPool is non-nil. Acquire is fine outside the lock —
		// pgxpool has its own internal synchronization.
		conn, err := listenerPool.Acquire(ctx)
		if err != nil {
			p.logger.Trace().Err(err).Str("channel", channel).Msg("failed to acquire postgres listener connection, will retry")
			time.Sleep(time.Second * 5)
			continue
		}

		_, err = conn.Exec(ctx, fmt.Sprintf("LISTEN %s", channel))
		if err != nil {
			p.logger.Trace().Err(err).Str("channel", channel).Msg("failed to listen to postgres channel, will retry")
			conn.Release()
			time.Sleep(time.Second * 5)
			continue
		}

		p.logger.Debug().Str("channel", channel).Msg("connected listener to postgres channel")
		return conn, nil
	}
}

func (p *PostgreSQLConnector) getCurrentValue(ctx context.Context, key string) (CounterInt64State, bool, error) {
	ctx, span := common.StartDetailSpan(ctx, "PostgreSQLConnector.getCurrentValue",
		trace.WithAttributes(
			attribute.String("key", key),
		),
	)
	defer span.End()

	p.logger.Trace().Str("key", key).Msg("proactively getting current value from postgres")
	val, err := p.Get(ctx, ConnectorMainIndex, key, "value", nil)
	if err != nil {
		common.SetTraceSpanError(span, err)
		if common.HasErrorCode(err, common.ErrCodeRecordNotFound) {
			return CounterInt64State{}, false, nil
		}
		return CounterInt64State{}, false, err
	}

	var st CounterInt64State
	if err := common.SonicCfg.Unmarshal(val, &st); err != nil || st.UpdatedAt <= 0 {
		// No backward compatibility: treat parse errors as missing
		return CounterInt64State{}, false, nil
	}

	span.SetAttributes(attribute.Int64("value", st.Value))
	return st, true, nil
}

// getWithWildcard takes an already-acquired pool from the caller so it
// shares the same RLock-scope and skips a redundant nil check. Caller is
// responsible for ensuring `pool` is non-nil and that the connMu read lock
// is held for the duration of this call.
func (p *PostgreSQLConnector) getWithWildcard(ctx context.Context, pool *pgxpool.Pool, index, partitionKey, rangeKey string) ([]byte, error) {
	ctx, span := common.StartDetailSpan(ctx, "PostgreSQLConnector.getWithWildcard",
		trace.WithAttributes(
			attribute.String("index", index),
			attribute.String("partition_key", partitionKey),
			attribute.String("range_key", rangeKey),
		),
	)
	defer span.End()

	var query string
	var args []interface{}

	if index == ConnectorReverseIndex {
		query = fmt.Sprintf(`
			SELECT value FROM %s
			WHERE range_key = $1 AND partition_key LIKE $2
			  AND (expires_at IS NULL OR expires_at > NOW() AT TIME ZONE 'UTC')
			ORDER BY partition_key DESC
			LIMIT 1
		`, p.table)
		args = []interface{}{
			strings.ReplaceAll(rangeKey, "*", "%"),
			strings.ReplaceAll(partitionKey, "*", "%"),
		}
	} else {
		query = fmt.Sprintf(`
			SELECT value FROM %s
			WHERE partition_key = $1 AND range_key LIKE $2
			  AND (expires_at IS NULL OR expires_at > NOW() AT TIME ZONE 'UTC')
			ORDER BY partition_key DESC
			LIMIT 1
		`, p.table)
		args = []interface{}{
			strings.ReplaceAll(partitionKey, "*", "%"),
			strings.ReplaceAll(rangeKey, "*", "%"),
		}
	}

	p.logger.Debug().Str("query", query).Interface("args", args).Msg("getting item from postgres with wildcard")

	var value []byte
	err := pool.QueryRow(ctx, query, args...).Scan(&value)

	if err == pgx.ErrNoRows {
		err := common.NewErrRecordNotFound(partitionKey, rangeKey, PostgreSQLDriverName)
		common.SetTraceSpanError(span, err)
		return nil, err
	} else if err != nil {
		common.SetTraceSpanError(span, err)
		return nil, err
	}

	if common.IsTracingDetailed {
		span.SetAttributes(attribute.Int("value_size", len(value)))
	}
	return value, nil
}

func (p *PostgreSQLConnector) startCleanup(ctx context.Context) {
	// Skip cleanup routine if we're using pg_cron
	if p.cleanupTicker == nil {
		p.logger.Debug().Msg("skipping local cleanup routine (using pg_cron)")
		return
	}

	p.logger.Debug().Msg("starting local expired items cleanup routine")
	for {
		select {
		case <-ctx.Done():
			p.logger.Debug().Msg("stopping cleanup routine due to context cancellation")
			return
		case <-p.cleanupTicker.C:
			if err := p.cleanupExpired(ctx); err != nil {
				p.logger.Error().Err(err).Msg("failed to cleanup expired items")
			}
		}
	}
}

func (p *PostgreSQLConnector) cleanupExpired(ctx context.Context) error {
	p.connMu.RLock()
	defer p.connMu.RUnlock()

	result, err := p.conn.Exec(ctx, fmt.Sprintf(`
		DELETE FROM %s
		WHERE expires_at IS NOT NULL AND expires_at <= NOW() AT TIME ZONE 'UTC'
	`, p.table))
	if err != nil {
		return err
	}

	if rowsAffected := result.RowsAffected(); rowsAffected > 0 {
		p.logger.Debug().Int64("count", rowsAffected).Msg("removed expired items")
	}
	return nil
}

// sanitizeChannelName converts any string into a valid postgres channel name
// postgres identifiers must start with a letter or underscore and can only contain
// letters, digits, and underscores
func sanitizeChannelName(key string) string {
	// Replace any non-alphanumeric character with underscore
	sanitized := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			return r
		}
		return '_'
	}, key)

	// Ensure it starts with a letter or underscore
	if len(sanitized) > 0 && !((sanitized[0] >= 'a' && sanitized[0] <= 'z') ||
		(sanitized[0] >= 'A' && sanitized[0] <= 'Z') || sanitized[0] == '_') {
		sanitized = "_" + sanitized
	}

	// Truncate if too long (postgres has a 63-byte limit for identifiers)
	if len(sanitized) > 63 {
		sanitized = sanitized[:63]
	}

	return sanitized
}

func (p *PostgreSQLConnector) Delete(ctx context.Context, partitionKey, rangeKey string) error {
	ctx, span := common.StartSpan(ctx, "PostgreSQLConnector.Delete")
	defer span.End()

	if common.IsTracingDetailed {
		span.SetAttributes(
			attribute.String("partition_key", partitionKey),
			attribute.String("range_key", rangeKey),
		)
	}

	pool, release, err := p.acquirePool(span)
	if err != nil {
		return err
	}
	defer release()

	p.logger.Debug().Str("partitionKey", partitionKey).Str("rangeKey", rangeKey).Msg("deleting from postgres")

	ctx, cancel := context.WithTimeout(ctx, p.setTimeout)
	defer cancel()

	_, err = pool.Exec(ctx, fmt.Sprintf(`
		DELETE FROM %s 
		WHERE partition_key = $1 AND range_key = $2
	`, p.table), partitionKey, rangeKey)

	if err != nil {
		p.handleConnectionFailure(err)
		common.SetTraceSpanError(span, err)
	}

	return err
}

func (p *PostgreSQLConnector) List(ctx context.Context, index string, limit int, paginationToken string) ([]KeyValuePair, string, error) {
	ctx, span := common.StartSpan(ctx, "PostgreSQLConnector.List")
	defer span.End()

	if common.IsTracingDetailed {
		span.SetAttributes(
			attribute.String("index", index),
			attribute.Int("limit", limit),
		)
	}

	pool, release, err := p.acquirePool(span)
	if err != nil {
		return nil, "", err
	}
	defer release()

	ctx, cancel := context.WithTimeout(ctx, p.getTimeout)
	defer cancel()

	// Parse pagination token - we'll use base64 encoded JSON with offset info
	offset := 0
	if paginationToken != "" {
		decoded, err := base64.StdEncoding.DecodeString(paginationToken)
		if err != nil {
			return nil, "", fmt.Errorf("invalid pagination token: %w", err)
		}
		var tokenData map[string]int
		err = json.Unmarshal(decoded, &tokenData)
		if err != nil {
			return nil, "", fmt.Errorf("invalid pagination token format: %w", err)
		}
		if o, ok := tokenData["offset"]; ok {
			offset = o
		}
	}

	query := fmt.Sprintf(`
		SELECT partition_key, range_key, value 
		FROM %s 
		WHERE expires_at IS NULL OR expires_at > NOW() AT TIME ZONE 'UTC'
		ORDER BY partition_key, range_key
		LIMIT $1 OFFSET $2
	`, p.table)

	p.logger.Debug().Str("query", query).Int("limit", limit).Int("offset", offset).Msg("listing from postgres")

	rows, err := pool.Query(ctx, query, limit+1, offset) // Get one extra to check if there are more
	if err != nil {
		p.handleConnectionFailure(err)
		common.SetTraceSpanError(span, err)
		return nil, "", err
	}
	defer rows.Close()

	results := make([]KeyValuePair, 0, limit)
	count := 0

	for rows.Next() {
		if count >= limit {
			break // We got the extra record, so there are more results
		}

		var partitionKey, rangeKey string
		var value []byte

		err := rows.Scan(&partitionKey, &rangeKey, &value)
		if err != nil {
			return nil, "", fmt.Errorf("failed to scan row: %w", err)
		}

		results = append(results, KeyValuePair{
			PartitionKey: partitionKey,
			RangeKey:     rangeKey,
			Value:        value,
		})
		count++
	}

	if err := rows.Err(); err != nil {
		p.handleConnectionFailure(err)
		common.SetTraceSpanError(span, err)
		return nil, "", err
	}

	// Prepare next token
	nextToken := ""
	if count == limit {
		// Check if there's a next page by seeing if we got more than limit records
		hasMore := false
		for rows.Next() {
			hasMore = true
			break
		}

		if hasMore {
			tokenData := map[string]int{"offset": offset + limit}
			tokenBytes, err := json.Marshal(tokenData)
			if err != nil {
				return nil, "", fmt.Errorf("failed to create pagination token: %w", err)
			}
			nextToken = base64.StdEncoding.EncodeToString(tokenBytes)
		}
	}

	return results, nextToken, nil
}
