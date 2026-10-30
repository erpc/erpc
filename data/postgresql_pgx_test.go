package data

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/util"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withQuery appends a raw query string to a connection URI.
func withQuery(uri, query string) string {
	if strings.Contains(uri, "?") {
		return uri + "&" + query
	}
	return uri + "?" + query
}

func TestParsePoolConfig_LegacyV4Keys(t *testing.T) {
	const base = "postgres://u:p@db:5432/x?sslmode=disable"
	cases := []struct {
		query    string
		mode     pgx.QueryExecMode
		legacy   []string
		errMatch string
	}{
		{"", pgx.QueryExecModeCacheStatement, nil, ""},
		{"statement_cache_mode=prepare", pgx.QueryExecModeCacheStatement, []string{"statement_cache_mode"}, ""},
		{"statement_cache_mode=describe", pgx.QueryExecModeCacheDescribe, []string{"statement_cache_mode"}, ""},
		{"prefer_simple_protocol=true", pgx.QueryExecModeSimpleProtocol, []string{"prefer_simple_protocol"}, ""},
		{"prefer_simple_protocol=false", pgx.QueryExecModeCacheStatement, []string{"prefer_simple_protocol"}, ""},
		// v4 checked the simple protocol before the statement cache.
		{"statement_cache_mode=describe&prefer_simple_protocol=true", pgx.QueryExecModeSimpleProtocol, []string{"statement_cache_mode", "prefer_simple_protocol"}, ""},
		// An explicit v5 mode wins over a legacy key.
		{"statement_cache_mode=describe&default_query_exec_mode=exec", pgx.QueryExecModeExec, []string{"statement_cache_mode"}, ""},
		// A zero capacity disabled the v4 cache; the v5 equivalent is describe_exec.
		{"statement_cache_capacity=0", pgx.QueryExecModeDescribeExec, nil, ""},
		{"statement_cache_mode=describe&description_cache_capacity=0", pgx.QueryExecModeDescribeExec, []string{"statement_cache_mode"}, ""},
		{"statement_cache_mode=bogus", 0, nil, "invalid statement_cache_mode"},
		{"prefer_simple_protocol=maybe", 0, nil, "invalid prefer_simple_protocol"},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			cfg, legacy, err := parsePoolConfig(withQuery(base, tc.query))
			if tc.errMatch != "" {
				require.ErrorContains(t, err, tc.errMatch)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.mode, cfg.ConnConfig.DefaultQueryExecMode)
			assert.Equal(t, tc.legacy, legacy)
			assert.NotContains(t, cfg.ConnConfig.RuntimeParams, "statement_cache_mode")
			assert.NotContains(t, cfg.ConnConfig.RuntimeParams, "prefer_simple_protocol")
		})
	}
}

// Connection URIs written for pgx v4 must keep connecting: pgx v5 would pass
// the unknown keys to the server, which rejects them as unknown parameters.
func TestPostgreSQLConnector_LegacyV4URIKeysConnect(t *testing.T) {
	uri := sharedPostgresURI(t)
	for _, q := range []string{
		"statement_cache_mode=describe",
		"statement_cache_mode=prepare",
		"prefer_simple_protocol=true",
		"statement_cache_capacity=0",
		"default_query_exec_mode=exec",
	} {
		t.Run(q, func(t *testing.T) {
			cfg := pgConnectorConfig(withQuery(uri, q), newPgTable("pg_legacy"))
			ctx, c := newReadyPgConnector(t, cfg)

			val := []byte{0x00, 0x01, 0xfe, 0xff}
			ttl := time.Hour
			require.NoError(t, c.Set(ctx, "pk", "rk", val, &ttl))
			got, err := c.Get(ctx, ConnectorMainIndex, "pk", "rk", nil)
			require.NoError(t, err)
			assert.Equal(t, val, got)

			lock, err := c.Lock(ctx, "legacy-lock", time.Second)
			require.NoError(t, err)
			require.NoError(t, lock.Unlock(ctx))

			items, _, err := c.List(ctx, ConnectorMainIndex, 10, "")
			require.NoError(t, err)
			assert.Len(t, items, 1)
		})
	}
}

// The connect task must fail when no connection can be opened, as pgx v4's
// ConnectConfig did; pgx v5's pool constructor alone dials in the background.
func TestPostgreSQLConnector_ConnectFailsFast(t *testing.T) {
	uri := sharedPostgresURI(t)
	u, err := url.Parse(uri)
	require.NoError(t, err)
	u.User = url.UserPassword("postgres", "wrong-password")
	logger := zerolog.New(io.Discard)

	for name, badURI := range map[string]string{
		"bad password":     u.String(),
		"unknown database": strings.Replace(uri, "/postgres?", "/no_such_db?", 1),
		"port closed":      "postgres://postgres:password@127.0.0.1:1/postgres?sslmode=disable",
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c, err := NewPostgreSQLConnector(ctx, &logger, "pg-failfast", pgConnectorConfig(badURI, newPgTable("pg_failfast")))
			require.NoError(t, err)
			assert.NotEqual(t, util.StateReady, c.initializer.State())
			assert.ErrorIs(t, c.Set(ctx, "pk", "rk", []byte("v"), nil), ErrConnectorNotReady)
		})
	}
}

// Empty and nil values are stored and read back as they were in pgx v4.
func TestPostgreSQLConnector_EmptyAndNilValues(t *testing.T) {
	uri := sharedPostgresURI(t)
	table := newPgTable("pg_empty")
	ctx, c := newReadyPgConnector(t, pgConnectorConfig(uri, table))
	pool := rawPgPool(t, uri)

	require.NoError(t, c.Set(ctx, "empty", "rk", []byte{}, nil))
	require.NoError(t, c.Set(ctx, "nil", "rk", nil, nil))

	var emptyIsNull, nilIsNull bool
	require.NoError(t, pool.QueryRow(ctx, fmt.Sprintf(`SELECT value IS NULL FROM %s WHERE partition_key = 'empty'`, table)).Scan(&emptyIsNull))
	require.NoError(t, pool.QueryRow(ctx, fmt.Sprintf(`SELECT value IS NULL FROM %s WHERE partition_key = 'nil'`, table)).Scan(&nilIsNull))
	assert.False(t, emptyIsNull, "an empty value is stored as empty bytea")
	assert.True(t, nilIsNull, "a nil value is stored as NULL")

	got, err := c.Get(ctx, ConnectorMainIndex, "empty", "rk", nil)
	require.NoError(t, err)
	assert.NotNil(t, got)
	assert.Empty(t, got)

	got, err = c.Get(ctx, ConnectorMainIndex, "nil", "rk", nil)
	require.NoError(t, err)
	assert.Nil(t, got)
}

// A listener whose backend is terminated must reconnect and keep delivering.
func TestPostgreSQLConnector_ListenerSurvivesBackendTermination(t *testing.T) {
	uri := sharedPostgresURI(t)
	ctx, c := newReadyPgConnector(t, pgConnectorConfig(uri, newPgTable("pg_listen_kill")))
	pool := rawPgPool(t, uri)
	key := "listener-kill/latest"
	channel := sanitizeChannelName(fmt.Sprintf("counter_%s", key))

	updates, cleanup, err := c.WatchCounterInt64(ctx, key)
	require.NoError(t, err)
	defer cleanup()

	publishUntilSeen := func(st CounterInt64State) {
		t.Helper()
		require.Eventually(t, func() bool {
			require.NoError(t, c.PublishCounterInt64(ctx, key, st))
			for {
				select {
				case got := <-updates:
					if got == st {
						return true
					}
				case <-time.After(200 * time.Millisecond):
					return false
				}
			}
		}, 30*time.Second, 10*time.Millisecond)
	}
	publishUntilSeen(CounterInt64State{Value: 1, UpdatedAt: 1000, UpdatedBy: "a"})

	var killed int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(pg_terminate_backend(pid))::int FROM pg_stat_activity
		WHERE pid <> pg_backend_pid() AND query = $1`, "LISTEN "+channel).Scan(&killed))
	require.Equal(t, 1, killed, "exactly one listener backend must be terminated")

	publishUntilSeen(CounterInt64State{Value: 2, UpdatedAt: 2000, UpdatedBy: "b"})
}
