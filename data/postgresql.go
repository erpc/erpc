package data

import (
	"context"
	"io"
	"strings"

	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/config"
	"github.com/jackc/pgx/v4"
	"github.com/jackc/pgx/v4/pgxpool"
)

const (
	PostgreSQLStoreDriver = "postgresql"
)

type PostgreSQLStore struct {
	cfg  *config.PostgreSQLStoreConfig
	conn *pgxpool.Pool
}

type PostgreSQLValueWriter struct {
	ctx    context.Context
	store  *PostgreSQLStore
	conn   *pgxpool.Pool
	key    string
	buffer strings.Builder
}

func (w *PostgreSQLValueWriter) Write(p []byte) (n int, err error) {
	w.buffer.Write(p)
	return len(p), nil
}

func (w *PostgreSQLValueWriter) Close() error {
	_, err := w.conn.Exec(w.ctx, "INSERT INTO "+w.store.cfg.Table+" (key, value) VALUES ($1, $2) ON CONFLICT (key) DO UPDATE SET value = $2", w.key, w.buffer.String())
	return err
}

func NewPostgreSQLStore(cfg *config.PostgreSQLStoreConfig) (*PostgreSQLStore, error) {
	conn, err := pgxpool.Connect(context.Background(), cfg.ConnectionUri)
	if err != nil {
		return nil, err
	}

	_, err = conn.Exec(context.Background(), "CREATE TABLE IF NOT EXISTS "+cfg.Table+" (key VARCHAR(1024) PRIMARY KEY, value TEXT)")
	if err != nil {
		return nil, err
	}

	return &PostgreSQLStore{cfg: cfg, conn: conn}, nil
}

func (p *PostgreSQLStore) Get(ctx context.Context, key string) (string, error) {
	var value string
	err := p.conn.QueryRow(ctx, "SELECT value FROM "+p.cfg.Table+" WHERE key = $1", key).Scan(&value)
	if err != nil {
		if err == pgx.ErrNoRows {
			return "", common.NewErrRecordNotFound(key, PostgreSQLStoreDriver)
		}
		return "", err
	}
	return value, nil
}

func (d *PostgreSQLStore) GetWithReader(ctx context.Context, key string) (io.Reader, error) {
	value, err := d.Get(ctx, key)
	if err != nil {
		return nil, err
	}

	return strings.NewReader(value), nil
}

func (p *PostgreSQLStore) Set(ctx context.Context, key string, value string) (int, error) {
	x, err := p.conn.Exec(ctx, "INSERT INTO "+p.cfg.Table+" (key, value) VALUES ($1, $2) ON CONFLICT (key) DO UPDATE SET value = $2", key, value)
	return int(x.RowsAffected()), err
}

func (p *PostgreSQLStore) SetWithWriter(ctx context.Context, key string) (io.WriteCloser, error) {
	return &PostgreSQLValueWriter{store: p, ctx: ctx, conn: p.conn, key: key}, nil
}

func (p *PostgreSQLStore) Scan(ctx context.Context, prefix string) ([]string, error) {
	rows, err := p.conn.Query(ctx, "SELECT key FROM "+p.cfg.Table+" WHERE key LIKE $1", prefix+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return keys, nil
}

func (p *PostgreSQLStore) Delete(ctx context.Context, key string) error {
	_, err := p.conn.Exec(ctx, "DELETE FROM "+p.cfg.Table+" WHERE key = $1", key)
	return err
}
