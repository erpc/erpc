package data

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"strings"
	"sync"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/jackc/pgx/v4"
	"github.com/jackc/pgx/v4/pgxpool"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

const (
	PostgreSQLDriverName = "postgresql"
)

var _ Connector = (*PostgreSQLConnector)(nil)

type PostgreSQLConnector struct {
	id            string
	logger        *zerolog.Logger
	conn          *pgxpool.Pool
	connMu        sync.RWMutex
	initializer   *util.Initializer
	minConns      int32
	maxConns      int32
	table         string
	cleanupTicker *time.Ticker
	initTimeout   time.Duration
	getTimeout    time.Duration
	setTimeout    time.Duration
	listeners     sync.Map      // map[string]*pgxListener
	listenerPool  *pgxpool.Pool // Separate pool for LISTEN connections
}

type pgxListener struct {
	mu            sync.Mutex
	conn          *pgx.Conn
	watchers      []chan CounterInt64State
	cacheWatchers []chan CacheInvalidationEvent
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

func (p *PostgreSQLConnector) connectTask(ctx context.Context, cfg *common.PostgreSQLConnectorConfig) error {
	p.connMu.Lock()
	defer p.connMu.Unlock()

	// Defer creation of listener pool until it's actually needed by WatchCounterInt64
	p.listenerPool = nil

	config, err := pgxpool.ParseConfig(cfg.ConnectionUri)
	if err != nil {
		return common.NewTaskFatal(fmt.Errorf("failed to parse connection URI: %w", err))
	}
	config.MinConns = p.minConns
	config.MaxConns = p.maxConns
	config.MaxConnLifetime = 5 * time.Hour
	config.MaxConnIdleTime = 30 * time.Minute

	ctx, cancel := context.WithTimeout(ctx, p.initTimeout)
	defer cancel()

	conn, err := pgxpool.ConnectConfig(ctx, config)
	if err != nil {
		return err
	}

	// Create table if not exists with TTL column
	_, err = conn.Exec(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			partition_key TEXT,
			range_key TEXT,
			value BYTEA,
			expires_at TIMESTAMP WITH TIME ZONE,
			PRIMARY KEY (partition_key, range_key)
		)
	`, cfg.Table))
	if err != nil {
		return err
	}

	// Migrate existing TEXT column to BYTEA if needed
	var dataType string
	err = conn.QueryRow(ctx, `
		SELECT data_type 
		FROM information_schema.columns 
		WHERE table_name = $1 AND column_name = 'value'
	`, cfg.Table).Scan(&dataType)

	if err == nil && dataType == "text" {
		// Migration needed
		p.logger.Info().Msg("migrating value column from TEXT to BYTEA")

		// Add temporary column
		_, err = conn.Exec(ctx, fmt.Sprintf(`
			ALTER TABLE %s ADD COLUMN IF NOT EXISTS value_new BYTEA
		`, cfg.Table))
		if err != nil {
			return fmt.Errorf("failed to add temporary column: %w", err)
		}

		// Copy data (converting text to bytea)
		_, err = conn.Exec(ctx, fmt.Sprintf(`
			UPDATE %s SET value_new = value::bytea WHERE value IS NOT NULL
		`, cfg.Table))
		if err != nil {
			return fmt.Errorf("failed to migrate data: %w", err)
		}

		// Drop old column and rename new one
		_, err = conn.Exec(ctx, fmt.Sprintf(`
			ALTER TABLE %s DROP COLUMN value;
			ALTER TABLE %s RENAME COLUMN value_new TO value;
		`, cfg.Table, cfg.Table))
		if err != nil {
			return fmt.Errorf("failed to complete migration: %w", err)
		}

		p.logger.Info().Msg("successfully migrated value column to BYTEA")
	}

	// Add expires_at column if it doesn't exist
	_, err = conn.Exec(ctx, fmt.Sprintf(`
        ALTER TABLE %s
        ADD COLUMN IF NOT EXISTS expires_at TIMESTAMP WITH TIME ZONE
    `, cfg.Table))
	if err != nil {
		return fmt.Errorf("failed to add expires_at column: %w", err)
	}

	// Create index for reverse lookups (range_key first to support queries that filter by range_key)
	_, err = conn.Exec(ctx, fmt.Sprintf(`
		CREATE INDEX IF NOT EXISTS idx_reverse ON %s (range_key, partition_key)
	`, cfg.Table))
	if err != nil {
		return fmt.Errorf("failed to create reverse index: %w", err)
	}

	// Create index for TTL cleanup
	_, err = conn.Exec(ctx, fmt.Sprintf(`
		CREATE INDEX IF NOT EXISTS idx_expires_at ON %s (expires_at)
		WHERE expires_at IS NOT NULL
	`, cfg.Table))
	if err != nil {
		return fmt.Errorf("failed to create TTL index: %w", err)
	}

	// Try to set up pg_cron cleanup job if extension exists
	var hasPgCron bool
	err = conn.QueryRow(ctx, `
        SELECT EXISTS (
            SELECT 1 FROM pg_extension WHERE extname = 'pg_cron'
        )
    `).Scan(&hasPgCron)
	if err != nil {
		p.logger.Warn().Err(err).Msg("failed to check for pg_cron extension")
	}

	if hasPgCron {
		// Create cleanup job using pg_cron
		_, err = conn.Exec(ctx, fmt.Sprintf(`
            SELECT cron.schedule('*/5 * * * *', $$
                DELETE FROM %s
                WHERE expires_at IS NOT NULL AND expires_at <= NOW() AT TIME ZONE 'UTC'
            $$)
        `, p.table))
		if err != nil {
			p.logger.Warn().Err(err).Msg("failed to create pg_cron cleanup job, falling back to local cleanup")
		} else {
			p.logger.Info().Msg("successfully configured pg_cron cleanup job")
			// Don't start the local cleanup routine since we're using pg_cron
			p.cleanupTicker = nil
		}
	}

	p.conn = conn
	p.logger.Info().Str("table", p.table).Msg("successfully connected to postgres")

	// If we are *not* using pg_cron, we still have a non-nil ticker,
	// so we spawn the local cleanup routine:
	if p.cleanupTicker != nil {
		go p.startCleanup(ctx)
	}
	return nil
}

func (p *PostgreSQLConnector) Id() string {
	return p.id
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

	p.connMu.RLock()
	defer p.connMu.RUnlock()

	if p.conn == nil {
		err := fmt.Errorf("PostgreSQLConnector not connected yet")
		common.SetTraceSpanError(span, err)
		return err
	}

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

	var err error
	if expiresAt != nil {
		_, err = p.conn.Exec(ctx, fmt.Sprintf(`
			INSERT INTO %s (partition_key, range_key, value, expires_at)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (partition_key, range_key) DO UPDATE
			SET value = $3, expires_at = $4
		`, p.table), partitionKey, rangeKey, value, expiresAt)
	} else {
		_, err = p.conn.Exec(ctx, fmt.Sprintf(`
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

	p.connMu.RLock()
	defer p.connMu.RUnlock()

	if p.conn == nil {
		err := fmt.Errorf("PostgreSQLConnector not connected yet")
		common.SetTraceSpanError(span, err)
		return nil, err
	}

	var query string
	var args []interface{}

	ctx, cancel := context.WithTimeout(ctx, p.getTimeout)
	defer cancel()

	if strings.HasSuffix(partitionKey, "*") || strings.HasSuffix(rangeKey, "*") {
		return p.getWithWildcard(ctx, index, partitionKey, rangeKey)
	}

	query = fmt.Sprintf(`
		SELECT value FROM %s
		WHERE partition_key = $1 AND range_key = $2
		AND (expires_at IS NULL OR expires_at > NOW() AT TIME ZONE 'UTC')
	`, p.table)
	args = []interface{}{partitionKey, rangeKey}

	p.logger.Debug().Str("query", query).Interface("args", args).Msg("getting item from postgres")

	var value []byte
	err := p.conn.QueryRow(ctx, query, args...).Scan(&value)

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

	p.connMu.RLock()
	defer p.connMu.RUnlock()

	if p.conn == nil {
		err := fmt.Errorf("PostgreSQLConnector not connected yet")
		common.SetTraceSpanError(span, err)
		return nil, err
	}

	// Generate consistent hash for the key as advisory lock ID
	h := fnv.New64a()
	_, err := h.Write([]byte(key))
	if err != nil {
		common.SetTraceSpanError(span, err)
		return nil, fmt.Errorf("failed to generate advisory lock ID: %w", err)
	}
	lockID := int64(h.Sum64()) // #nosec

	// Start a transaction
	tx, err := p.conn.Begin(ctx)
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
		conn:   p.conn,
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

	p.connMu.RLock()
	defer p.connMu.RUnlock()

	if p.conn == nil {
		err := fmt.Errorf("postgres not connected yet")
		common.SetTraceSpanError(span, err)
		return err
	}

	p.logger.Debug().Str("key", key).Int64("value", value.Value).Msg("publishing counter update to postgres")

	channel := sanitizeChannelName(fmt.Sprintf("counter_%s", key))
	payload, err := common.SonicCfg.Marshal(value)
	if err != nil {
		common.SetTraceSpanError(span, err)
		return err
	}
	_, err = p.conn.Exec(ctx, "SELECT pg_notify($1, $2)", channel, string(payload))

	if err != nil {
		common.SetTraceSpanError(span, err)
	}

	return err
}

func (p *PostgreSQLConnector) taskId() string {
	return fmt.Sprintf("postgres-connect/%s", p.id)
}

func (p *PostgreSQLConnector) handleConnectionFailure(err error) {
	if strings.Contains(err.Error(), "connection") {
		s := p.initializer.State()
		if s != util.StateInitializing &&
			s != util.StateRetrying {
			// p.conn = nil
			p.logger.Warn().Err(err).Str("state", s.String()).Msg("postgres connection lost; marking connector as failed for reinitialization")
			p.initializer.MarkTaskAsFailed(p.taskId(), err)
		} else {
			p.logger.Warn().Err(err).Str("state", s.String()).Msg("postgres connection lost; and will not be retried due to connector state")
		}
	}
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

		p.connMu.Lock()
		// Lazily initialize listenerPool using the main pool's connection string
		if p.listenerPool == nil {
			if p.conn == nil {
				p.connMu.Unlock()
				time.Sleep(5 * time.Second)
				continue
			}
			cfg, err := pgxpool.ParseConfig(p.conn.Config().ConnString())
			if err != nil {
				p.connMu.Unlock()
				return nil, err
			}
			cfg.MaxConns = p.maxConns
			pool, err := pgxpool.ConnectConfig(ctx, cfg)
			if err != nil {
				p.connMu.Unlock()
				time.Sleep(5 * time.Second)
				continue
			}
			p.listenerPool = pool
		}
		conn, err := p.listenerPool.Acquire(ctx)
		p.connMu.Unlock()

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

func (p *PostgreSQLConnector) getWithWildcard(ctx context.Context, index, partitionKey, rangeKey string) ([]byte, error) {
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
	err := p.conn.QueryRow(ctx, query, args...).Scan(&value)

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

	p.connMu.RLock()
	defer p.connMu.RUnlock()

	if p.conn == nil {
		err := fmt.Errorf("PostgreSQLConnector not connected yet")
		common.SetTraceSpanError(span, err)
		return err
	}

	p.logger.Debug().Str("partitionKey", partitionKey).Str("rangeKey", rangeKey).Msg("deleting from postgres")

	ctx, cancel := context.WithTimeout(ctx, p.setTimeout)
	defer cancel()

	_, err := p.conn.Exec(ctx, fmt.Sprintf(`
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

	p.connMu.RLock()
	defer p.connMu.RUnlock()

	if p.conn == nil {
		err := fmt.Errorf("PostgreSQLConnector not connected yet")
		common.SetTraceSpanError(span, err)
		return nil, "", err
	}

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

	rows, err := p.conn.Query(ctx, query, limit+1, offset) // Get one extra to check if there are more
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

// WatchCacheInvalidation subscribes to cache invalidation events for a given channel.
// Returns a channel that receives invalidation events, a cleanup function, and an error.
func (p *PostgreSQLConnector) WatchCacheInvalidation(ctx context.Context, channel string) (<-chan CacheInvalidationEvent, func(), error) {
	updates := make(chan CacheInvalidationEvent, 10)

	// Create or get listener for this channel
	listener, err := p.getOrCreateCacheInvalidationListener(ctx, channel)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create cache invalidation listener: %w", err)
	}

	// Add watcher to listener
	listener.mu.Lock()
	listener.cacheWatchers = append(listener.cacheWatchers, updates)
	watcherCount := len(listener.cacheWatchers)
	listener.mu.Unlock()

	p.logger.Debug().Str("channel", channel).Int("watchers", watcherCount).Msg("starting cache invalidation watcher")

	cleanup := func() {
		listener.mu.Lock()
		defer listener.mu.Unlock()

		// Remove this watcher
		for i, ch := range listener.cacheWatchers {
			if ch == updates {
				listener.cacheWatchers = append(listener.cacheWatchers[:i], listener.cacheWatchers[i+1:]...)
				break
			}
		}

		close(updates)
	}

	return updates, cleanup, nil
}

// PublishCacheInvalidation publishes a cache invalidation event to a channel.
func (p *PostgreSQLConnector) PublishCacheInvalidation(ctx context.Context, channel string, event CacheInvalidationEvent) error {
	ctx, span := common.StartSpan(ctx, "PostgreSQLConnector.PublishCacheInvalidation",
		trace.WithAttributes(
			attribute.String("channel", channel),
			attribute.String("key", event.Key),
		),
	)
	defer span.End()

	p.connMu.RLock()
	defer p.connMu.RUnlock()

	if p.conn == nil {
		err := fmt.Errorf("postgres not connected yet")
		common.SetTraceSpanError(span, err)
		return err
	}

	p.logger.Debug().Str("channel", channel).Str("key", event.Key).Msg("publishing cache invalidation event to postgres")

	pgChannel := sanitizeChannelName(fmt.Sprintf("cache_inv_%s", channel))
	payload, err := common.SonicCfg.Marshal(event)
	if err != nil {
		common.SetTraceSpanError(span, err)
		return err
	}
	_, err = p.conn.Exec(ctx, "SELECT pg_notify($1, $2)", pgChannel, string(payload))

	if err != nil {
		common.SetTraceSpanError(span, err)
	}

	return err
}

func (p *PostgreSQLConnector) getOrCreateCacheInvalidationListener(ctx context.Context, channel string) (*pgxListener, error) {
	pgChannel := sanitizeChannelName(fmt.Sprintf("cache_inv_%s", channel))

	// Try to load existing listener first
	if l, ok := p.listeners.Load(pgChannel); ok {
		return l.(*pgxListener), nil
	}

	listener := &pgxListener{
		cacheWatchers: make([]chan CacheInvalidationEvent, 0),
	}

	// Try to atomically store the new listener
	// If another goroutine already stored one, use that instead
	actual, loaded := p.listeners.LoadOrStore(pgChannel, listener)
	if loaded {
		// Another goroutine created the listener first, use that one
		return actual.(*pgxListener), nil
	}

	conn, err := p.connectListener(ctx, pgChannel)
	if err != nil {
		p.listeners.Delete(pgChannel) // Clean up the placeholder
		return nil, err
	}

	go func() {
		for {
			if err := ctx.Err(); err != nil {
				p.logger.Debug().Err(err).Str("channel", channel).Msg("stopping cache invalidation listener due to context termination")
				return
			}

			notification, err := conn.Conn().WaitForNotification(ctx)
			if err != nil {
				// Try to reconnect
				p.logger.Warn().Err(err).Str("channel", channel).Msg("lost postgres connection for cache invalidation, attempting reconnect")
				if newConn, err := p.connectListener(ctx, pgChannel); err == nil {
					p.logger.Debug().Str("channel", channel).Msg("successfully reconnected to postgres cache invalidation channel")
					conn = newConn
					continue
				}
				return
			}

			p.logger.Trace().Str("channel", channel).Interface("payload", notification).Msg("received cache invalidation notification")

			// Parse and broadcast event
			var event CacheInvalidationEvent
			if err := common.SonicCfg.Unmarshal([]byte(notification.Payload), &event); err != nil {
				p.logger.Warn().Err(err).Str("channel", channel).Str("payload", notification.Payload).Msg("failed to unmarshal cache invalidation event")
				continue
			}
			if event.Key != "" {
				listener.mu.Lock()
				for _, ch := range listener.cacheWatchers {
					select {
					case ch <- event:
					default:
						// Channel full, skip to avoid blocking
					}
				}
				listener.mu.Unlock()
			}
		}
	}()

	p.logger.Debug().Str("channel", channel).Msg("successfully created postgres cache invalidation listener")
	listener.conn = conn.Conn()
	return listener, nil
}
