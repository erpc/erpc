package data

import (
	"context"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// TestPostgreSQLConnector_SkipSchemaSetup_CleanupTicker verifies that the local
// expired-row cleanup ticker is armed only when the connector owns the schema.
// On a read-only replica (SkipSchemaSetup=true) the ticker must be nil so the
// startCleanup goroutine never issues its periodic DELETE (a write that either
// fails or gets needlessly cross-region write-forwarded). This is deterministic
// and needs no database — the ticker is decided in the constructor before the
// first connect attempt.
func TestPostgreSQLConnector_SkipSchemaSetup_CleanupTicker(t *testing.T) {
	logger := zerolog.New(io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Unreachable address so the initial connect fails fast (no server). The
	// constructor still returns the connector and the cleanupTicker decision
	// has already been made regardless of connectivity.
	cfg := func(skip bool) *common.PostgreSQLConnectorConfig {
		return &common.PostgreSQLConnectorConfig{
			Table:           "test_skip_cleanup",
			ConnectionUri:   "postgres://user:pass@127.0.0.1:9876/bogusdb?sslmode=disable",
			InitTimeout:     common.Duration(500 * time.Millisecond),
			GetTimeout:      common.Duration(500 * time.Millisecond),
			SetTimeout:      common.Duration(500 * time.Millisecond),
			MinConns:        1,
			MaxConns:        1,
			SkipSchemaSetup: skip,
		}
	}

	t.Run("reader skips cleanup ticker", func(t *testing.T) {
		conn, err := NewPostgreSQLConnector(ctx, &logger, "test-skip-true", cfg(true))
		require.NoError(t, err)
		require.NotNil(t, conn)
		require.Nil(t, conn.cleanupTicker, "cleanupTicker must be nil when SkipSchemaSetup is true")
	})

	t.Run("writer arms cleanup ticker", func(t *testing.T) {
		conn, err := NewPostgreSQLConnector(ctx, &logger, "test-skip-false", cfg(false))
		require.NoError(t, err)
		require.NotNil(t, conn)
		require.NotNil(t, conn.cleanupTicker, "cleanupTicker must be set when SkipSchemaSetup is false")
		conn.cleanupTicker.Stop()
	})
}

// TestPostgreSQLConnector_SkipSchemaSetup_NoDDL proves that SkipSchemaSetup
// prevents the connector from running any startup DDL: with it enabled the
// connector connects but does NOT create its table, while the default (writer)
// path does. Mirrors a read-only Aurora global secondary, where CREATE TABLE
// fails with SQLSTATE 25006 and DDL is not write-forwarded.
//
// Requires a writable Postgres reachable via POSTGRES_TEST_URI; skipped
// otherwise (same convention as the other PostgreSQL integration tests).
func TestPostgreSQLConnector_SkipSchemaSetup_NoDDL(t *testing.T) {
	uri := os.Getenv("POSTGRES_TEST_URI")
	if uri == "" {
		t.Skip("Skipping PostgreSQL test - POSTGRES_TEST_URI not set")
	}

	logger := zerolog.New(io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	table := fmt.Sprintf("test_skip_schema_%d", time.Now().UnixNano())

	cfg := func(skip bool, id string) *common.PostgreSQLConnectorConfig {
		return &common.PostgreSQLConnectorConfig{
			Table:           table,
			ConnectionUri:   uri,
			InitTimeout:     common.Duration(10 * time.Second),
			GetTimeout:      common.Duration(5 * time.Second),
			SetTimeout:      common.Duration(5 * time.Second),
			MinConns:        1,
			MaxConns:        5,
			SkipSchemaSetup: skip,
		}
	}

	tableExists := func(c *PostgreSQLConnector) bool {
		var exists bool
		err := c.conn.QueryRow(ctx,
			`SELECT EXISTS (SELECT FROM information_schema.tables WHERE table_name = $1)`,
			table).Scan(&exists)
		require.NoError(t, err)
		return exists
	}

	// Reader: connects, but must not create the table.
	reader, err := NewPostgreSQLConnector(ctx, &logger, "test-pg-skip", cfg(true, "test-pg-skip"))
	require.NoError(t, err)
	require.NotNil(t, reader)
	require.NotNil(t, reader.conn, "connector should connect even with schema setup skipped")
	require.False(t, tableExists(reader), "SkipSchemaSetup=true must not create the table")

	// Writer: default path creates the table and is fully functional.
	writer, err := NewPostgreSQLConnector(ctx, &logger, "test-pg-write", cfg(false, "test-pg-write"))
	require.NoError(t, err)
	require.NotNil(t, writer)
	require.True(t, tableExists(writer), "SkipSchemaSetup=false must create the table")
	t.Cleanup(func() {
		_, _ = writer.conn.Exec(context.Background(), fmt.Sprintf("DROP TABLE IF EXISTS %s", table))
	})

	require.NoError(t, writer.Set(ctx, "pk", "rk", []byte("v"), nil))
	got, err := writer.Get(ctx, "", "pk", "rk", nil)
	require.NoError(t, err)
	require.Equal(t, []byte("v"), got)
}
