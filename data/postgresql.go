package data

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	id          string
	logger      *zerolog.Logger
	conn        *pgxpool.Pool
	initializer *util.Initializer

	minConns int32
	maxConns int32
	table    string

	cleanupTicker *time.Ticker
	initTimeout   time.Duration
	getTimeout    time.Duration
	setTimeout    time.Duration
}

func NewPostgreSQLConnector(
	appCtx context.Context,
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

	// 1) Create an Initializer to handle (re)connecting
	connector.initializer = util.NewInitializer(appCtx, &lg, nil)

	// 2) Define our connection task.
	connectTask := util.NewBootstrapTask(fmt.Sprintf("postgres-connect/%s", id), func(ctx context.Context) error {
		return connector.connectTask(ctx, cfg)
	})

	// 3) Execute the task. If it fails on the first attempt, we log it but still return the connector.
	if err := connector.initializer.ExecuteTasks(appCtx, connectTask); err != nil {
		lg.Error().Err(err).Msg("failed to initialize PostgreSQL on first attempt (will retry in background)")
		// Return the connector so the app can proceed, but note that it's not ready yet.
		return connector, nil
	}

	return connector, nil
}

// connectTask attempts to create a pgxpool connection, create necessary tables/indexes, etc.
func (p *PostgreSQLConnector) connectTask(ctx context.Context, cfg *common.PostgreSQLConnectorConfig) error {
	config, err := pgxpool.ParseConfig(cfg.ConnectionUri)
	if err != nil {
		return fmt.Errorf("failed to parse connection URI: %w", err)
	}
	config.MinConns = p.minConns
	config.MaxConns = p.maxConns
	config.MaxConnLifetime = 5 * time.Hour
	config.MaxConnIdleTime = 30 * time.Minute

	// Respect initTimeout for the dial operation
	dialCtx, cancel := context.WithTimeout(ctx, p.initTimeout)
	defer cancel()

	// Attempt to connect
	conn, err := pgxpool.ConnectConfig(dialCtx, config)
	if err != nil {
		return fmt.Errorf("failed to connect to PostgreSQL: %w", err)
	}

	// Ensure required table/columns/indexes exist
	if err := p.initDBObjects(dialCtx, conn); err != nil {
		conn.Close()
		return err
	}

	// Store the newly established connection
	p.conn = conn
	p.logger.Info().Str("table", p.table).Msg("successfully connected to PostgreSQL")

	// Attempt to set up pg_cron or start local cleanup
	if err := p.configureCleanup(dialCtx, conn); err != nil {
		// Not a fatal error - just log a warning
		p.logger.Warn().Err(err).Msg("failed to configure pg_cron for cleanup; fallback to local ticker")
	}

	// If we are *not* using pg_cron, we still have a non-nil ticker,
	// so we spawn the local cleanup routine:
	if p.cleanupTicker != nil {
		go p.startCleanup(ctx)
	}

	return nil
}

// initDBObjects ensures the table + columns + indexes exist
func (p *PostgreSQLConnector) initDBObjects(ctx context.Context, conn *pgxpool.Pool) error {
	// Create table if not exists
	_, err := conn.Exec(ctx, fmt.Sprintf(`
        CREATE TABLE IF NOT EXISTS %s (
            partition_key TEXT,
            range_key TEXT,
            value TEXT,
            expires_at TIMESTAMP WITH TIME ZONE,
            PRIMARY KEY (partition_key, range_key)
        )
    `, p.table))
	if err != nil {
		return fmt.Errorf("failed to create table: %w", err)
	}

	// Add expires_at if missing (note: in Postgres 9.6+ "ADD COLUMN IF NOT EXISTS" is not standard,
	// but your snippet used it; presumably it works in your environment)
	_, err = conn.Exec(ctx, fmt.Sprintf(`
        ALTER TABLE %s
        ADD COLUMN IF NOT EXISTS expires_at TIMESTAMP WITH TIME ZONE
    `, p.table))
	if err != nil {
		return fmt.Errorf("failed to add expires_at column: %w", err)
	}

	// Create index for reverse lookups
	_, err = conn.Exec(ctx, fmt.Sprintf(`
        CREATE INDEX IF NOT EXISTS idx_reverse ON %s (partition_key, range_key) INCLUDE (value)
    `, p.table))
	if err != nil {
		return fmt.Errorf("failed to create reverse index: %w", err)
	}

	// Create index for TTL cleanup
	_, err = conn.Exec(ctx, fmt.Sprintf(`
        CREATE INDEX IF NOT EXISTS idx_expires_at ON %s (expires_at)
        WHERE expires_at IS NOT NULL
    `, p.table))
	if err != nil {
		return fmt.Errorf("failed to create TTL index: %w", err)
	}

	return nil
}

// configureCleanup checks for pg_cron and configures it if available; otherwise local ticker is used.
func (p *PostgreSQLConnector) configureCleanup(ctx context.Context, conn *pgxpool.Pool) error {
	var hasPgCron bool
	err := conn.QueryRow(ctx, `
        SELECT EXISTS (
            SELECT 1 FROM pg_extension WHERE extname = 'pg_cron'
        )
    `).Scan(&hasPgCron)
	if err != nil {
		return fmt.Errorf("failed checking for pg_cron extension: %w", err)
	}

	if !hasPgCron {
		// We'll rely on the local ticker cleanup
		p.logger.Debug().Msg("pg_cron not installed; will use local ticker-based cleanup")
		return nil
	}

	// Try to schedule the cron job
	_, err = conn.Exec(ctx, fmt.Sprintf(`
        SELECT cron.schedule('*/5 * * * *', $$
            DELETE FROM %s
            WHERE expires_at IS NOT NULL AND expires_at <= NOW() AT TIME ZONE 'UTC'
        $$)
    `, p.table))
	if err != nil {
		return fmt.Errorf("failed to create pg_cron cleanup job: %w", err)
	}

	p.logger.Info().Msg("successfully configured pg_cron cleanup job")
	// Disable local ticker, so we don't do double-cleanup
	if p.cleanupTicker != nil {
		p.cleanupTicker.Stop()
		p.cleanupTicker = nil
	}
	return nil
}

// startCleanup runs the local ticker-based cleanup if pg_cron is not used.
func (p *PostgreSQLConnector) startCleanup(ctx context.Context) {
	if p.cleanupTicker == nil {
		p.logger.Debug().Msg("skipping local cleanup routine (pg_cron in use)")
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
				// Potentially mark connection lost if it's a fatal error?
				p.markConnectionAsLostIfNecessary(err)
			}
		}
	}
}

// cleanupExpired does a DELETE for expired records
func (p *PostgreSQLConnector) cleanupExpired(ctx context.Context) error {
	if p.conn == nil {
		return fmt.Errorf("not connected, cannot cleanup")
	}

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

// Id is part of a hypothetical Connector interface
func (p *PostgreSQLConnector) Id() string {
	return p.id
}

// checkReady ensures the initializer is in StateReady and the pool is not nil
func (p *PostgreSQLConnector) checkReady() error {
	if p.initializer == nil {
		return fmt.Errorf("initializer not set")
	}
	state := p.initializer.State()
	if state != util.StateReady {
		return fmt.Errorf("postgres is not connected (initializer state=%d)", state)
	}
	if p.conn == nil {
		return fmt.Errorf("postgres connection pool not initialized yet")
	}
	return nil
}

// markConnectionAsLostIfNecessary triggers a re-init if a non-trivial error occurs
func (p *PostgreSQLConnector) markConnectionAsLostIfNecessary(err error) {
	if p.initializer == nil || err == nil {
		return
	}
	// We can ignore certain errors, like context canceled or NoRows, etc.
	if errors.Is(err, context.Canceled) {
		return
	}
	p.initializer.MarkTaskAsFailed(
		fmt.Sprintf("postgres-connect/%s", p.id),
		fmt.Errorf("connection lost or postgres error: %w", err),
	)
}

// Set writes data to the DB. We check readiness first; if an error is encountered, we mark the connection as lost.
func (p *PostgreSQLConnector) Set(ctx context.Context, partitionKey, rangeKey, value string, ttl *time.Duration) error {
	if err := p.checkReady(); err != nil {
		return err
	}

	p.logger.Debug().Msgf("writing to PostgreSQL with partition key: %s and range key: %s", partitionKey, rangeKey)

	var expiresAt *time.Time
	if ttl != nil && *ttl > 0 {
		t := time.Now().UTC().Add(*ttl)
		expiresAt = &t
	}

	ctx, cancel := context.WithTimeout(ctx, p.setTimeout)
	defer cancel()

	var execErr error
	if expiresAt != nil {
		_, execErr = p.conn.Exec(ctx, fmt.Sprintf(`
            INSERT INTO %s (partition_key, range_key, value, expires_at)
            VALUES ($1, $2, $3, $4)
            ON CONFLICT (partition_key, range_key) DO UPDATE
            SET value = $3, expires_at = $4
        `, p.table), partitionKey, rangeKey, value, expiresAt)
	} else {
		_, execErr = p.conn.Exec(ctx, fmt.Sprintf(`
            INSERT INTO %s (partition_key, range_key, value)
            VALUES ($1, $2, $3)
            ON CONFLICT (partition_key, range_key) DO UPDATE
            SET value = $3
        `, p.table), partitionKey, rangeKey, value)
	}
	if execErr != nil {
		p.logger.Warn().Err(execErr).Msg("failed to SET in PostgreSQL, marking connection lost")
		p.markConnectionAsLostIfNecessary(execErr)
	}
	return execErr
}

// Get retrieves data. If there's a wildcard, we route to getWithWildcard.
// If an error occurs that implies a broken connection, we mark as lost.
func (p *PostgreSQLConnector) Get(ctx context.Context, index, partitionKey, rangeKey string) (string, error) {
	if err := p.checkReady(); err != nil {
		return "", err
	}

	// If wildcard, defer to getWithWildcard
	if strings.Contains(partitionKey, "*") || strings.Contains(rangeKey, "*") {
		return p.getWithWildcard(ctx, index, partitionKey, rangeKey)
	}

	ctx, cancel := context.WithTimeout(ctx, p.getTimeout)
	defer cancel()

	var query string
	var args []interface{}

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

	var val string
	err := p.conn.QueryRow(ctx, query, args...).Scan(&val)
	if err == pgx.ErrNoRows {
		return "", common.NewErrRecordNotFound(partitionKey, rangeKey, PostgreSQLDriverName)
	} else if err != nil {
		p.logger.Warn().Err(err).
			Str("partitionKey", partitionKey).
			Str("rangeKey", rangeKey).
			Msg("failed to GET in PostgreSQL, marking connection lost")
		p.markConnectionAsLostIfNecessary(err)
		return "", err
	}
	return val, nil
}

func (p *PostgreSQLConnector) getWithWildcard(ctx context.Context, index, partitionKey, rangeKey string) (string, error) {
	if err := p.checkReady(); err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(ctx, p.getTimeout)
	defer cancel()

	var query string
	var args []interface{}

	if index == ConnectorReverseIndex {
		query = fmt.Sprintf(`
            SELECT value FROM %s
            WHERE range_key = $1 AND partition_key LIKE $2
              AND (expires_at IS NULL OR expires_at > NOW() AT TIME ZONE 'UTC')
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
            LIMIT 1
        `, p.table)
		args = []interface{}{
			strings.ReplaceAll(partitionKey, "*", "%"),
			strings.ReplaceAll(rangeKey, "*", "%"),
		}
	}

	p.logger.Debug().Msgf("getting item (wildcard) from PostgreSQL query: %s args: %v", query, args)

	var val string
	err := p.conn.QueryRow(ctx, query, args...).Scan(&val)
	if err == pgx.ErrNoRows {
		return "", common.NewErrRecordNotFound(partitionKey, rangeKey, PostgreSQLDriverName)
	} else if err != nil {
		p.logger.Warn().Err(err).
			Str("partitionKey", partitionKey).
			Str("rangeKey", rangeKey).
			Msg("failed to GET (wildcard) in PostgreSQL, marking connection lost")
		p.markConnectionAsLostIfNecessary(err)
		return "", err
	}
	return val, nil
}
