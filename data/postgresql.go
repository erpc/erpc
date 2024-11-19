package data

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/erpc/erpc/common"
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
	table         string
	cleanupTicker *time.Ticker
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
		cleanupTicker: time.NewTicker(5 * time.Minute),
	}

	// Attempt the actual connecting in background to avoid blocking the main thread.
	go func() {
		for i := 0; i < 30; i++ {
			select {
			case <-ctx.Done():
				lg.Error().Msg("context cancelled while attempting to connect to PostgreSQL")
				return
			default:
				lg.Debug().Msgf("attempting to connect to PostgreSQL (attempt %d of 30)", i+1)
				err := connector.connect(ctx, cfg)
				if err == nil {
					// Start TTL cleanup routine after successful connection
					go connector.startCleanup(ctx)
					return
				}
				lg.Warn().Err(err).Msgf("failed to connect to PostgreSQL (attempt %d of 30)", i+1)
				time.Sleep(10 * time.Second)
			}
		}
		lg.Error().Msg("failed to connect to PostgreSQL after maximum attempts")
	}()

	return connector, nil
}

func (p *PostgreSQLConnector) connect(ctx context.Context, cfg *common.PostgreSQLConnectorConfig) error {
	conn, err := pgxpool.Connect(ctx, cfg.ConnectionUri)
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
	if ttl != nil {
		t := time.Now().UTC().Add(*ttl)
		expiresAt = &t
	}

	_, err := p.conn.Exec(ctx, fmt.Sprintf(`
		INSERT INTO %s (partition_key, range_key, value, expires_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (partition_key, range_key) DO UPDATE
		SET value = $3, expires_at = $4
	`, p.table), partitionKey, rangeKey, value, expiresAt)

	return err
}

func (p *PostgreSQLConnector) Get(ctx context.Context, index, partitionKey, rangeKey string) (string, error) {
	if p.conn == nil {
		return "", fmt.Errorf("PostgreSQLConnector not connected yet")
	}

	var query string
	var args []interface{}

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
		return "", common.NewErrRecordNotFound(fmt.Sprintf("PK: %s RK: %s", partitionKey, rangeKey), PostgreSQLDriverName)
	} else if err != nil {
		return "", err
	}

	return value, nil
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
		return "", common.NewErrRecordNotFound(fmt.Sprintf("PK: %s RK: %s", partitionKey, rangeKey), PostgreSQLDriverName)
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
