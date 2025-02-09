package data

import (
	"context"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/jackc/pgx/v4"
	"github.com/jackc/pgx/v4/pgxpool"
	"github.com/rs/zerolog"
)

const (
	PostgreSQLDriverName = "postgresql"
)

var _ Connector = (*PostgreSQLConnector)(nil)

type PostgreSQLConnector struct {
	id            string
	logger        *zerolog.Logger
	conn          *pgxpool.Pool
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
	mu       sync.Mutex
	conn     *pgx.Conn
	watchers []chan int64
}

type postgresLock struct {
	conn   *pgxpool.Pool
	lockID uint64
	logger *zerolog.Logger
}

func NewPostgreSQLConnector(
	ctx context.Context,
	logger *zerolog.Logger,
	id string,
	cfg *common.PostgreSQLConnectorConfig,
) (*PostgreSQLConnector, error) {
	lg := logger.With().Str("connector", id).Logger()
	lg.Debug().Interface("config", cfg).Msg("creating PostgreSQLConnector")

	connector := &PostgreSQLConnector{
		id:            id,
		logger:        &lg,
		table:         cfg.Table,
		minConns:      cfg.MinConns,
		maxConns:      cfg.MaxConns,
		initTimeout:   cfg.InitTimeout,
		getTimeout:    cfg.GetTimeout,
		setTimeout:    cfg.SetTimeout,
		cleanupTicker: time.NewTicker(5 * time.Minute),
	}
	listenerConfig, err := pgxpool.ParseConfig(cfg.ConnectionUri)
	if err != nil {
		return nil, err
	}
	listenerConfig.MaxConns = cfg.MaxConns
	listenerPool, err := pgxpool.ConnectConfig(ctx, listenerConfig)
	if err != nil {
		return nil, err
	}
	connector.listenerPool = listenerPool

	// create an Initializer to handle (re)connecting
	connector.initializer = util.NewInitializer(ctx, &lg, nil)

	connectTask := util.NewBootstrapTask(fmt.Sprintf("postgres-connect/%s", id), func(ctx context.Context) error {
		return connector.connectTask(ctx, cfg)
	})

	if err := connector.initializer.ExecuteTasks(ctx, connectTask); err != nil {
		lg.Error().Err(err).Msg("failed to initialize PostgreSQL on first attempt (will retry in background)")
		// Return the connector so the app can proceed, but note that it's not ready yet.
		return connector, nil
	}

	return connector, nil
}

func (p *PostgreSQLConnector) connectTask(ctx context.Context, cfg *common.PostgreSQLConnectorConfig) error {
	config, err := pgxpool.ParseConfig(cfg.ConnectionUri)
	if err != nil {
		return fmt.Errorf("failed to parse connection URI: %w", err)
	}
	config.MinConns = p.minConns
	config.MaxConns = p.maxConns
	config.MaxConnLifetime = 5 * time.Hour
	config.MaxConnIdleTime = 30 * time.Minute

	ctx, cancel := context.WithTimeout(ctx, p.initTimeout)
	defer cancel()

	conn, err := pgxpool.ConnectConfig(ctx, config)
	if err != nil {
		return fmt.Errorf("failed to connect to PostgreSQL: %w", err)
	}

	// Create table if not exists with TTL column
	_, err = conn.Exec(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			partition_key TEXT,
			range_key TEXT,
			value TEXT,
			expires_at TIMESTAMP WITH TIME ZONE,
			PRIMARY KEY (partition_key, range_key)
		)
	`, cfg.Table))
	if err != nil {
		return fmt.Errorf("failed to create table: %w", err)
	}

	// Add expires_at column if it doesn't exist
	_, err = conn.Exec(ctx, fmt.Sprintf(`
        ALTER TABLE %s
        ADD COLUMN IF NOT EXISTS expires_at TIMESTAMP WITH TIME ZONE
    `, cfg.Table))
	if err != nil {
		return fmt.Errorf("failed to add expires_at column: %w", err)
	}

	// Create index for reverse lookups
	_, err = conn.Exec(ctx, fmt.Sprintf(`
		CREATE INDEX IF NOT EXISTS idx_reverse ON %s (partition_key, range_key) INCLUDE (value)
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
	p.logger.Info().Str("table", p.table).Msg("successfully connected to PostgreSQL")

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

func (p *PostgreSQLConnector) Set(ctx context.Context, partitionKey, rangeKey, value string, ttl *time.Duration) error {
	if p.conn == nil {
		return fmt.Errorf("PostgreSQLConnector not connected yet")
	}

	p.logger.Debug().Msgf("writing to PostgreSQL with partition key: %s and range key: %s", partitionKey, rangeKey)

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

	return err
}

func (p *PostgreSQLConnector) Get(ctx context.Context, index, partitionKey, rangeKey string) (string, error) {
	if p.conn == nil {
		return "", fmt.Errorf("PostgreSQLConnector not connected yet")
	}

	var query string
	var args []interface{}

	ctx, cancel := context.WithTimeout(ctx, p.getTimeout)
	defer cancel()

	if strings.HasSuffix(partitionKey, "*") || strings.HasSuffix(rangeKey, "*") {
		return p.getWithWildcard(ctx, index, partitionKey, rangeKey)
	}

	if index == ConnectorReverseIndex {
		query = fmt.Sprintf(`
            SELECT value FROM %s
            WHERE range_key = $1 AND partition_key = $2
            AND (expires_at IS NULL OR expires_at > NOW() AT TIME ZONE 'UTC')
        `, p.table)
		args = []interface{}{rangeKey, partitionKey}
	} else {
		query = fmt.Sprintf(`
            SELECT value FROM %s
            WHERE partition_key = $1 AND range_key = $2
            AND (expires_at IS NULL OR expires_at > NOW() AT TIME ZONE 'UTC')
        `, p.table)
		args = []interface{}{partitionKey, rangeKey}
	}

	p.logger.Debug().Msgf("getting item from PostgreSQL with query: %s args: %v", query, args)

	var value string
	err := p.conn.QueryRow(ctx, query, args...).Scan(&value)

	if err == pgx.ErrNoRows {
		return "", common.NewErrRecordNotFound(partitionKey, rangeKey, PostgreSQLDriverName)
	} else if err != nil {
		return "", err
	}

	return value, nil
}

func (p *PostgreSQLConnector) Lock(ctx context.Context, key string, ttl time.Duration) (DistributedLock, error) {
	// Generate consistent hash for the key as advisory lock ID
	h := fnv.New64a()
	_, err := h.Write([]byte(key))
	if err != nil {
		return nil, fmt.Errorf("failed to generate advisory lock ID: %w", err)
	}
	lockID := h.Sum64()

	// Try to acquire advisory lock with timeout
	ctx, cancel := context.WithTimeout(ctx, ttl)
	defer cancel()

	// pg_try_advisory_lock returns true if lock acquired, false if not
	var acquired bool
	err = p.conn.QueryRow(ctx, `
        SELECT pg_try_advisory_lock($1)
    `, lockID).Scan(&acquired)

	if err != nil {
		return nil, fmt.Errorf("failed to acquire advisory lock: %w", err)
	}

	if !acquired {
		return nil, fmt.Errorf("failed to acquire lock: timeout")
	}

	return &postgresLock{
		conn:   p.conn,
		lockID: lockID,
		logger: p.logger,
	}, nil
}

func (l *postgresLock) Unlock(ctx context.Context) error {
	// Release advisory lock
	var released bool
	err := l.conn.QueryRow(ctx, `
        SELECT pg_advisory_unlock($1)
    `, l.lockID).Scan(&released)

	if err != nil {
		return fmt.Errorf("failed to release advisory lock: %w", err)
	}

	if !released {
		l.logger.Warn().Uint64("lockID", l.lockID).Msg("lock was already released")
	}

	return nil
}

func (p *PostgreSQLConnector) WatchCounterInt64(ctx context.Context, key string) (<-chan int64, func(), error) {
	updates := make(chan int64, 1)

	// Create or get listener for this key
	listener, err := p.getOrCreateListener(ctx, key)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create listener: %w", err)
	}

	// Add watcher to listener
	listener.mu.Lock()
	listener.watchers = append(listener.watchers, updates)
	listener.mu.Unlock()

	// Start fallback polling
	ticker := time.NewTicker(30 * time.Second)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if val, err := p.getCurrentValue(ctx, key); err == nil {
					select {
					case updates <- val:
					default:
					}
				}
			}
		}
	}()

	// Send initial value
	if val, err := p.getCurrentValue(ctx, key); err == nil {
		updates <- val
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

		// If no more watchers, remove listener
		if len(listener.watchers) == 0 {
			p.listeners.Delete(key)
			if listener.conn != nil {
				err := listener.conn.Close(context.Background())
				if err != nil {
					p.logger.Warn().Err(err).Str("key", key).Msg("failed to close listener connection")
				}
			}
		}

		close(updates)
	}

	return updates, cleanup, nil
}

func (p *PostgreSQLConnector) PublishCounterInt64(ctx context.Context, key string, value int64) error {
	channel := fmt.Sprintf("counter_%s", key)
	_, err := p.conn.Exec(ctx, fmt.Sprintf("NOTIFY %s, '%d'", channel, value))
	return err
}

func (p *PostgreSQLConnector) getOrCreateListener(ctx context.Context, key string) (*pgxListener, error) {
	// Check if listener exists
	if l, ok := p.listeners.Load(key); ok {
		return l.(*pgxListener), nil
	}

	// Create new listener
	listener := &pgxListener{}

	// Establish dedicated connection for LISTEN
	conn, err := p.listenerPool.Acquire(ctx)
	if err != nil {
		return nil, err
	}

	// Set up LISTEN
	channel := fmt.Sprintf("counter_%s", key)
	_, err = conn.Exec(ctx, fmt.Sprintf("LISTEN %s", channel))
	if err != nil {
		conn.Release()
		return nil, err
	}

	// Start notification handler
	go func() {
		for {
			notification, err := conn.Conn().WaitForNotification(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				// Try to reconnect
				p.logger.Warn().Err(err).Str("key", key).Msg("lost connection, attempting reconnect")
				if newConn, err := p.reconnectListener(ctx, channel); err == nil {
					conn = newConn
					continue
				}
				return
			}

			// Parse and broadcast value
			if val, err := strconv.ParseInt(notification.Payload, 10, 64); err == nil {
				listener.mu.Lock()
				for _, ch := range listener.watchers {
					select {
					case ch <- val:
					default:
					}
				}
				listener.mu.Unlock()
			}
		}
	}()

	listener.conn = conn.Conn()
	p.listeners.Store(key, listener)
	return listener, nil
}

func (p *PostgreSQLConnector) reconnectListener(ctx context.Context, channel string) (*pgxpool.Conn, error) {
	for i := 0; i < 3; i++ {
		conn, err := p.listenerPool.Acquire(ctx)
		if err != nil {
			time.Sleep(time.Second * time.Duration(i+1))
			continue
		}

		_, err = conn.Exec(ctx, fmt.Sprintf("LISTEN %s", channel))
		if err != nil {
			conn.Release()
			time.Sleep(time.Second * time.Duration(i+1))
			continue
		}

		return conn, nil
	}
	return nil, fmt.Errorf("failed to reconnect after 3 attempts")
}

func (p *PostgreSQLConnector) getCurrentValue(ctx context.Context, key string) (int64, error) {
	val, err := p.Get(ctx, ConnectorMainIndex, key, "value")
	if err != nil {
		if common.HasErrorCode(err, common.ErrCodeRecordNotFound) {
			return 0, nil
		}
		return 0, err
	}
	return strconv.ParseInt(val, 10, 64)
}

func (p *PostgreSQLConnector) getWithWildcard(ctx context.Context, index, partitionKey, rangeKey string) (string, error) {
	var query string
	var args []interface{}

	if index == ConnectorReverseIndex {
		query = fmt.Sprintf(`
			SELECT value FROM %s
			WHERE range_key = $1 AND partition_key LIKE $2
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
			LIMIT 1
		`, p.table)
		args = []interface{}{
			strings.ReplaceAll(partitionKey, "*", "%"),
			strings.ReplaceAll(rangeKey, "*", "%"),
		}
	}

	p.logger.Debug().Msgf("getting item from PostgreSQL with wildcard query: %s args: %v", query, args)

	var value string
	err := p.conn.QueryRow(ctx, query, args...).Scan(&value)

	if err == pgx.ErrNoRows {
		return "", common.NewErrRecordNotFound(partitionKey, rangeKey, PostgreSQLDriverName)
	} else if err != nil {
		return "", err
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
