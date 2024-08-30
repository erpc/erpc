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
	cfg    *common.PostgreSQLConnectorConfig
	logger *zerolog.Logger
	conn   *pgxpool.Pool
	table  string
}

func NewPostgreSQLConnector(ctx context.Context, logger *zerolog.Logger, cfg *common.PostgreSQLConnectorConfig) (*PostgreSQLConnector, error) {
	logger.Debug().Msgf("creating PostgreSQLConnector with for table: %s", cfg.Table)
	p := &PostgreSQLConnector{
		cfg:    cfg,
		logger: logger,
		table:  cfg.Table,
	}

	// Attempt the actual connecting in background to avoid blocking the main thread.
	// Retry every 10 seconds until success and give up after 30 failed attempts.
	go func() {
		for i := 0; i < 30; i++ {
			select {
			case <-ctx.Done():
				logger.Error().Msg("Context cancelled while attempting to connect to PostgreSQL")
				return
			default:
				logger.Debug().Msgf("attempting to connect to PostgreSQL (attempt %d of 30)", i+1)
				err := p.connect(ctx)
				if err == nil {
					return
				}
				logger.Warn().Msgf("failed to connect to PostgreSQL (attempt %d of 30): %s", i+1, err)
				time.Sleep(10 * time.Second)
			}
		}
		logger.Error().Msg("Failed to connect to PostgreSQL after maximum attempts")
	}()

	return p, nil
}

func (p *PostgreSQLConnector) connect(ctx context.Context) error {
	conn, err := pgxpool.Connect(ctx, p.cfg.ConnectionUri)
	if err != nil {
		return fmt.Errorf("failed to connect to PostgreSQL: %w", err)
	}

	// Create table if not exists
	_, err = conn.Exec(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			partition_key TEXT,
			range_key TEXT,
			value TEXT,
			PRIMARY KEY (partition_key, range_key)
		)
	`, p.cfg.Table))
	if err != nil {
		return fmt.Errorf("failed to create table: %w", err)
	}

	// Create index for reverse lookups
	_, err = conn.Exec(ctx, fmt.Sprintf(`
		CREATE INDEX IF NOT EXISTS idx_reverse ON %s (range_key, partition_key)
	`, p.cfg.Table))
	if err != nil {
		return fmt.Errorf("failed to create reverse index: %w", err)
	}

	p.conn = conn

	return nil
}

func (p *PostgreSQLConnector) SetTTL(_ string, _ string) error {
	p.logger.Debug().Msgf("Method TTLs not implemented for PostgresSQLConnector")
	return nil
}

func (p *PostgreSQLConnector) HasTTL(_ string) bool {
	return false
}

func (p *PostgreSQLConnector) Set(ctx context.Context, partitionKey, rangeKey, value string) error {
	if p.conn == nil {
		return fmt.Errorf("PostgreSQLConnector not connected yet")
	}

	p.logger.Debug().Msgf("writing to PostgreSQL with partition key: %s and range key: %s", partitionKey, rangeKey)

	_, err := p.conn.Exec(ctx, fmt.Sprintf(`
		INSERT INTO %s (partition_key, range_key, value)
		VALUES ($1, $2, $3)
		ON CONFLICT (partition_key, range_key) DO UPDATE
		SET value = $3
	`, p.table), partitionKey, rangeKey, value)

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
		`, p.table)
		args = []interface{}{rangeKey, partitionKey}
	} else {
		query = fmt.Sprintf(`
			SELECT value FROM %s
			WHERE partition_key = $1 AND range_key = $2
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

func (p *PostgreSQLConnector) Delete(ctx context.Context, index, partitionKey, rangeKey string) error {
	if p.conn == nil {
		return fmt.Errorf("PostgreSQLConnector not connected yet")
	}

	if strings.HasSuffix(rangeKey, "*") {
		return p.deleteWithPrefix(ctx, index, partitionKey, rangeKey)
	} else {
		return p.deleteSingleItem(ctx, partitionKey, rangeKey)
	}
}

func (p *PostgreSQLConnector) deleteSingleItem(ctx context.Context, partitionKey, rangeKey string) error {
	_, err := p.conn.Exec(ctx, fmt.Sprintf(`
		DELETE FROM %s
		WHERE partition_key = $1 AND range_key = $2
	`, p.table), partitionKey, rangeKey)

	return err
}

func (p *PostgreSQLConnector) deleteWithPrefix(ctx context.Context, index, partitionKey, rangeKey string) error {
	var query string
	var args []interface{}

	if index == ConnectorReverseIndex {
		query = fmt.Sprintf(`
			DELETE FROM %s
			WHERE range_key LIKE $1 AND partition_key = $2
		`, p.table)
		args = []interface{}{
			strings.ReplaceAll(rangeKey, "*", "%"),
			partitionKey,
		}
	} else {
		query = fmt.Sprintf(`
			DELETE FROM %s
			WHERE partition_key = $1 AND range_key LIKE $2
		`, p.table)
		args = []interface{}{
			partitionKey,
			strings.ReplaceAll(rangeKey, "*", "%"),
		}
	}

	_, err := p.conn.Exec(ctx, query, args...)
	return err
}
