package data

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/jackc/pgx/v4/pgxpool"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var pgTableSeq atomic.Int64

// newPgTable returns a table name unique to this test binary run so tests
// sharing one Postgres server never see each other's rows.
func newPgTable(prefix string) string {
	return fmt.Sprintf("%s_%d_%d", prefix, time.Now().UnixNano()%1_000_000, pgTableSeq.Add(1))
}

func pgConnectorConfig(uri, table string) *common.PostgreSQLConnectorConfig {
	return &common.PostgreSQLConnectorConfig{
		Table:         table,
		ConnectionUri: uri,
		InitTimeout:   common.Duration(5 * time.Second),
		GetTimeout:    common.Duration(2 * time.Second),
		SetTimeout:    common.Duration(2 * time.Second),
		MinConns:      1,
		MaxConns:      4,
	}
}

// newReadyPgConnector builds a connector against the shared Postgres server
// and waits until it reports ready.
func newReadyPgConnector(t *testing.T, cfg *common.PostgreSQLConnectorConfig) (context.Context, *PostgreSQLConnector) {
	t.Helper()
	logger := zerolog.New(io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c, err := NewPostgreSQLConnector(ctx, &logger, "pg-"+cfg.Table, cfg)
	require.NoError(t, err)
	require.Equal(t, util.StateReady, c.initializer.State(), "errors: %v", c.initializer.Errors())
	return ctx, c
}

// rawPgPool opens a pool directly against the shared server, for arranging
// rows the connector API cannot produce (already-expired rows, legacy schema).
func rawPgPool(t *testing.T, uri string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.Connect(context.Background(), uri)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

func insertExpiredPgRow(t *testing.T, pool *pgxpool.Pool, table, pk, rk, value string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), fmt.Sprintf(
		`INSERT INTO %s (partition_key, range_key, value, expires_at) VALUES ($1, $2, $3, NOW() - INTERVAL '1 hour')`, table),
		pk, rk, []byte(value))
	require.NoError(t, err)
}

func TestPostgreSQLConnector_SetGetDelete(t *testing.T) {
	uri := sharedPostgresURI(t)
	table := newPgTable("pg_crud")
	ctx, c := newReadyPgConnector(t, pgConnectorConfig(uri, table))

	t.Run("missing key is record-not-found", func(t *testing.T) {
		_, err := c.Get(ctx, ConnectorMainIndex, "nope", "nope", nil)
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeRecordNotFound), "got %v", err)
	})

	t.Run("set without ttl then overwrite with ttl", func(t *testing.T) {
		require.NoError(t, c.Set(ctx, "pk1", "rk1", []byte("v1"), nil))
		got, err := c.Get(ctx, ConnectorMainIndex, "pk1", "rk1", nil)
		require.NoError(t, err)
		assert.Equal(t, []byte("v1"), got)

		ttl := time.Hour
		require.NoError(t, c.Set(ctx, "pk1", "rk1", []byte("v2"), &ttl))
		got, err = c.Get(ctx, ConnectorMainIndex, "pk1", "rk1", nil)
		require.NoError(t, err)
		assert.Equal(t, []byte("v2"), got)
	})

	t.Run("binary values round-trip byte-exact", func(t *testing.T) {
		val := []byte{0x00, 0xff, 0x10, 0x00, 0x7f}
		require.NoError(t, c.Set(ctx, "bin", "rk", val, nil))
		got, err := c.Get(ctx, ConnectorMainIndex, "bin", "rk", nil)
		require.NoError(t, err)
		assert.Equal(t, val, got)
	})

	t.Run("entry with short ttl expires", func(t *testing.T) {
		ttl := 50 * time.Millisecond
		require.NoError(t, c.Set(ctx, "short", "rk", []byte("soon-gone"), &ttl))
		require.Eventually(t, func() bool {
			_, err := c.Get(ctx, ConnectorMainIndex, "short", "rk", nil)
			return common.HasErrorCode(err, common.ErrCodeRecordNotFound)
		}, 5*time.Second, 20*time.Millisecond)
	})

	t.Run("zero ttl stores without expiry", func(t *testing.T) {
		zero := time.Duration(0)
		require.NoError(t, c.Set(ctx, "zero", "rk", []byte("forever"), &zero))
		got, err := c.Get(ctx, ConnectorMainIndex, "zero", "rk", nil)
		require.NoError(t, err)
		assert.Equal(t, []byte("forever"), got)
	})

	t.Run("delete removes the entry and is idempotent", func(t *testing.T) {
		require.NoError(t, c.Set(ctx, "del", "rk", []byte("x"), nil))
		require.NoError(t, c.Delete(ctx, "del", "rk"))
		_, err := c.Get(ctx, ConnectorMainIndex, "del", "rk", nil)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeRecordNotFound), "got %v", err)
		require.NoError(t, c.Delete(ctx, "del", "rk"))
	})
}

func TestPostgreSQLConnector_WildcardGet(t *testing.T) {
	uri := sharedPostgresURI(t)
	table := newPgTable("pg_wild")
	ctx, c := newReadyPgConnector(t, pgConnectorConfig(uri, table))
	pool := rawPgPool(t, uri)

	rows := []struct{ pk, rk, v string }{
		{"evm:1:100", "eth_getBlockByNumber:abc", "b100"},
		{"evm:1:200", "eth_getBlockByNumber:abc", "b200"},
		{"evm:1:300", "eth_call:foo_bar", "underscore"},
		{"evm:1:300", "eth_call:foo-bar", "dash"},
		{"evm:2:999", "eth_getBlockByNumber:abc", "other-chain"},
	}
	for _, r := range rows {
		require.NoError(t, c.Set(ctx, r.pk, r.rk, []byte(r.v), nil))
	}
	// An expired row sorting above every live one must be skipped.
	insertExpiredPgRow(t, pool, table, "evm:1:999", "eth_getBlockByNumber:abc", "expired")

	cases := []struct {
		name    string
		index   string
		pk, rk  string
		want    string
		missing bool
	}{
		{name: "reverse index picks highest live partition", index: ConnectorReverseIndex, pk: "evm:1:*", rk: "eth_getBlockByNumber:abc", want: "b200"},
		{name: "reverse index is scoped to the chain prefix", index: ConnectorReverseIndex, pk: "evm:2:*", rk: "eth_getBlockByNumber:abc", want: "other-chain"},
		{name: "reverse index with no match is not-found", index: ConnectorReverseIndex, pk: "evm:3:*", rk: "eth_getBlockByNumber:abc", missing: true},
		{name: "main index range prefix", index: ConnectorMainIndex, pk: "evm:1:100", rk: "eth_getBlockByNumber:*", want: "b100"},
		{name: "underscore in range pattern is literal", index: ConnectorMainIndex, pk: "evm:1:300", rk: "eth_call:foo_b*", want: "underscore"},
		{name: "dash pattern does not match underscore row", index: ConnectorMainIndex, pk: "evm:1:300", rk: "eth_call:foo-b*", want: "dash"},
		{name: "main index only expired match is not-found", index: ConnectorMainIndex, pk: "evm:1:999", rk: "eth_getBlockByNumber:*", missing: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := c.Get(ctx, tc.index, tc.pk, tc.rk, nil)
			if tc.missing {
				assert.True(t, common.HasErrorCode(err, common.ErrCodeRecordNotFound), "got %v", err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, string(got))
		})
	}

	t.Run("expired exact key is not-found", func(t *testing.T) {
		_, err := c.Get(ctx, ConnectorMainIndex, "evm:1:999", "eth_getBlockByNumber:abc", nil)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeRecordNotFound), "got %v", err)
	})
}

func TestPostgreSQLConnector_List(t *testing.T) {
	uri := sharedPostgresURI(t)
	table := newPgTable("pg_list")
	ctx, c := newReadyPgConnector(t, pgConnectorConfig(uri, table))
	pool := rawPgPool(t, uri)

	for i := 1; i <= 3; i++ {
		require.NoError(t, c.Set(ctx, fmt.Sprintf("pk%d", i), "rk", []byte(fmt.Sprintf("v%d", i)), nil))
	}
	insertExpiredPgRow(t, pool, table, "pk0", "rk", "expired")

	t.Run("returns live rows ordered by key", func(t *testing.T) {
		items, next, err := c.List(ctx, ConnectorMainIndex, 10, "")
		require.NoError(t, err)
		assert.Empty(t, next)
		require.Len(t, items, 3)
		for i, it := range items {
			assert.Equal(t, fmt.Sprintf("pk%d", i+1), it.PartitionKey)
			assert.Equal(t, "rk", it.RangeKey)
			assert.Equal(t, fmt.Sprintf("v%d", i+1), string(it.Value))
		}
	})

	t.Run("offset token resumes after the given position", func(t *testing.T) {
		token := base64.StdEncoding.EncodeToString([]byte(`{"offset":2}`))
		items, next, err := c.List(ctx, ConnectorMainIndex, 10, token)
		require.NoError(t, err)
		assert.Empty(t, next)
		require.Len(t, items, 1)
		assert.Equal(t, "pk3", items[0].PartitionKey)
	})

	t.Run("limit caps the page", func(t *testing.T) {
		items, _, err := c.List(ctx, ConnectorMainIndex, 2, "")
		require.NoError(t, err)
		require.Len(t, items, 2)
		assert.Equal(t, "pk1", items[0].PartitionKey)
		assert.Equal(t, "pk2", items[1].PartitionKey)
	})

	t.Run("token that is not base64 is rejected", func(t *testing.T) {
		_, _, err := c.List(ctx, ConnectorMainIndex, 2, "%%%not-base64")
		require.ErrorContains(t, err, "invalid pagination token")
	})

	t.Run("token that is not json is rejected", func(t *testing.T) {
		token := base64.StdEncoding.EncodeToString([]byte("not-json"))
		_, _, err := c.List(ctx, ConnectorMainIndex, 2, token)
		require.ErrorContains(t, err, "invalid pagination token format")
	})
}

func TestPostgreSQLConnector_Unlock(t *testing.T) {
	uri := sharedPostgresURI(t)
	ctx, c := newReadyPgConnector(t, pgConnectorConfig(uri, newPgTable("pg_unlock")))

	lock, err := c.Lock(ctx, "k", time.Second)
	require.NoError(t, err)
	require.NoError(t, lock.Unlock(ctx))
	// A second unlock has no transaction to commit.
	require.ErrorContains(t, lock.Unlock(ctx), "no active transaction")

	var nilLock *postgresLock
	assert.True(t, nilLock.IsNil())
}

func TestPostgreSQLConnector_WatchAndPublishCounter(t *testing.T) {
	uri := sharedPostgresURI(t)
	ctx, c := newReadyPgConnector(t, pgConnectorConfig(uri, newPgTable("pg_watch")))
	key := "shared/state-poller:evm:1/latest"

	initial := CounterInt64State{Value: 10, UpdatedAt: 1000, UpdatedBy: "a"}
	payload, err := common.SonicCfg.Marshal(initial)
	require.NoError(t, err)
	require.NoError(t, c.Set(ctx, key, "value", payload, nil))

	updates, cleanup, err := c.WatchCounterInt64(ctx, key)
	require.NoError(t, err)

	select {
	case st := <-updates:
		assert.Equal(t, initial, st, "watch must deliver the stored value first")
	case <-time.After(5 * time.Second):
		t.Fatal("no initial value delivered")
	}

	// A second watcher on the same key reuses the existing listener.
	updates2, cleanup2, err := c.WatchCounterInt64(ctx, key)
	require.NoError(t, err)
	<-updates2 // initial value

	next := CounterInt64State{Value: 11, UpdatedAt: 2000, UpdatedBy: "b"}
	require.Eventually(t, func() bool {
		require.NoError(t, c.PublishCounterInt64(ctx, key, next))
		select {
		case st := <-updates:
			return st == next
		case <-time.After(200 * time.Millisecond):
			return false
		}
	}, 10*time.Second, 10*time.Millisecond, "published value must reach watchers")

	select {
	case st := <-updates2:
		assert.Equal(t, next, st)
	case <-time.After(5 * time.Second):
		t.Fatal("second watcher did not receive publish")
	}

	cleanup()
	_, open := <-updates
	assert.False(t, open, "cleanup must close the updates channel")
	cleanup2()
}

func TestPostgreSQLConnector_WatchCounterSkipsUnparsableStoredValue(t *testing.T) {
	uri := sharedPostgresURI(t)
	ctx, c := newReadyPgConnector(t, pgConnectorConfig(uri, newPgTable("pg_watchbad")))

	require.NoError(t, c.Set(ctx, "k", "value", []byte("not-json"), nil))
	st, ok, err := c.getCurrentValue(ctx, "k")
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Equal(t, CounterInt64State{}, st)

	_, ok, err = c.getCurrentValue(ctx, "absent")
	require.NoError(t, err)
	assert.False(t, ok, "absent key is reported as missing, not as an error")
}

func TestPostgreSQLConnector_MigratesLegacyTextValueColumn(t *testing.T) {
	uri := sharedPostgresURI(t)
	table := newPgTable("pg_legacy")
	pool := rawPgPool(t, uri)
	_, err := pool.Exec(context.Background(), fmt.Sprintf(
		`CREATE TABLE %s (partition_key TEXT, range_key TEXT, value TEXT, PRIMARY KEY (partition_key, range_key))`, table))
	require.NoError(t, err)
	_, err = pool.Exec(context.Background(), fmt.Sprintf(`INSERT INTO %s VALUES ('pk', 'rk', 'legacy')`, table))
	require.NoError(t, err)

	ctx, c := newReadyPgConnector(t, pgConnectorConfig(uri, table))

	got, err := c.Get(ctx, ConnectorMainIndex, "pk", "rk", nil)
	require.NoError(t, err)
	assert.Equal(t, []byte("legacy"), got, "legacy text rows must survive the BYTEA migration")

	var dataType string
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT data_type FROM information_schema.columns WHERE table_name = $1 AND column_name = 'value'`, table).Scan(&dataType))
	assert.Equal(t, "bytea", dataType)

	ttl := time.Hour
	require.NoError(t, c.Set(ctx, "pk2", "rk", []byte{0x00, 0x01}, &ttl), "migrated table must accept writes with ttl")
}

func TestPostgreSQLConnector_ReconnectSwapsPoolWithoutReapplyingSchema(t *testing.T) {
	uri := sharedPostgresURI(t)
	cfg := pgConnectorConfig(uri, newPgTable("pg_reconnect"))
	ctx, c := newReadyPgConnector(t, cfg)
	require.NoError(t, c.Set(ctx, "pk", "rk", []byte("before"), nil))

	c.connMu.RLock()
	oldPool := c.conn
	c.connMu.RUnlock()

	// Drop the table: if the second connect re-ran DDL it would recreate it.
	pool := rawPgPool(t, uri)
	_, err := pool.Exec(context.Background(), fmt.Sprintf("DROP TABLE %s", cfg.Table))
	require.NoError(t, err)

	require.NoError(t, c.connectTask(ctx, cfg))

	c.connMu.RLock()
	newPool := c.conn
	c.connMu.RUnlock()
	assert.NotSame(t, oldPool, newPool, "reconnect must publish a fresh pool")

	_, err = c.Get(ctx, ConnectorMainIndex, "pk", "rk", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not exist", "schema must not be re-applied on reconnect")
}

func TestPostgreSQLConnector_ConnectionLossTriggersRecovery(t *testing.T) {
	uri := sharedPostgresURI(t)
	ctx, c := newReadyPgConnector(t, pgConnectorConfig(uri, newPgTable("pg_recover")))

	// A non-transport error never demotes a ready connector.
	c.handleConnectionFailure(errors.New("syntax error at or near"))
	require.Equal(t, util.StateReady, c.initializer.State())

	c.handleConnectionFailure(io.EOF)
	require.NotEqual(t, util.StateReady, c.initializer.State(), "transport failure must mark the connector for reconnect")

	// The pool stays usable while the initializer reconnects in the background.
	require.NoError(t, c.Set(ctx, "pk", "rk", []byte("v"), nil))

	require.Eventually(t, func() bool {
		return c.initializer.State() == util.StateReady
	}, 20*time.Second, 50*time.Millisecond, "auto-retry must bring the connector back to ready")

	got, err := c.Get(ctx, ConnectorMainIndex, "pk", "rk", nil)
	require.NoError(t, err)
	assert.Equal(t, []byte("v"), got)
}

func TestPostgreSQLConnector_CleanupExpiredRemovesOnlyExpiredRows(t *testing.T) {
	uri := sharedPostgresURI(t)
	table := newPgTable("pg_cleanup")
	ctx, c := newReadyPgConnector(t, pgConnectorConfig(uri, table))
	pool := rawPgPool(t, uri)

	require.NoError(t, c.Set(ctx, "live", "rk", []byte("v"), nil))
	insertExpiredPgRow(t, pool, table, "dead1", "rk", "x")
	insertExpiredPgRow(t, pool, table, "dead2", "rk", "x")

	countRows := func() int {
		var n int
		require.NoError(t, pool.QueryRow(context.Background(), fmt.Sprintf("SELECT COUNT(*) FROM %s", table)).Scan(&n))
		return n
	}
	require.Equal(t, 3, countRows())

	require.NoError(t, c.cleanupExpired(ctx))
	assert.Equal(t, 1, countRows())
	require.NoError(t, c.cleanupExpired(ctx), "nothing left to delete is not an error")

	t.Run("cleanup loop runs on its ticker and stops with ctx", func(t *testing.T) {
		insertExpiredPgRow(t, pool, table, "dead3", "rk", "x")
		lg := zerolog.Nop()
		loopCtx, stop := context.WithCancel(context.Background())
		p := &PostgreSQLConnector{logger: &lg, conn: pool, table: table, cleanupTicker: time.NewTicker(10 * time.Millisecond)}
		defer p.cleanupTicker.Stop()
		done := make(chan struct{})
		go func() { p.startCleanup(loopCtx); close(done) }()
		require.Eventually(t, func() bool { return countRows() == 1 }, 5*time.Second, 10*time.Millisecond)
		stop()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("cleanup loop did not stop on ctx cancel")
		}
	})

	t.Run("cleanup loop without ticker returns immediately", func(t *testing.T) {
		lg := zerolog.Nop()
		p := &PostgreSQLConnector{logger: &lg}
		done := make(chan struct{})
		go func() { p.startCleanup(context.Background()); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("startCleanup with nil ticker must return")
		}
	})
}

func TestPostgreSQLConnector_SchemaFailureLeavesConnectorNotReady(t *testing.T) {
	uri := sharedPostgresURI(t)
	logger := zerolog.New(io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A hyphen is not valid in an unquoted identifier, so CREATE TABLE fails.
	c, err := NewPostgreSQLConnector(ctx, &logger, "pg-bad-table", pgConnectorConfig(uri, "bad-table"))
	require.NoError(t, err)
	assert.NotEqual(t, util.StateReady, c.initializer.State())
	err = c.Set(ctx, "pk", "rk", []byte("v"), nil)
	assert.ErrorIs(t, err, ErrConnectorNotReady, "the half-open pool must be discarded")
}

func TestPostgreSQLConnector_SkipSchemaSetupOnMissingTable(t *testing.T) {
	uri := sharedPostgresURI(t)
	cfg := pgConnectorConfig(uri, newPgTable("pg_noschema"))
	cfg.SkipSchemaSetup = true
	ctx, c := newReadyPgConnector(t, cfg)

	_, err := c.Get(ctx, ConnectorMainIndex, "pk", "rk", nil)
	require.Error(t, err)
	assert.False(t, common.HasErrorCode(err, common.ErrCodeRecordNotFound), "a missing table is a query error, not a miss")
	assert.Equal(t, util.StateReady, c.initializer.State(), "an application-level error must not trigger reconnect")
}

func TestPostgreSQLConnector_InitFailures(t *testing.T) {
	logger := zerolog.New(io.Discard)

	t.Run("unparsable uri is fatal", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		c, err := NewPostgreSQLConnector(ctx, &logger, "pg-bad-uri", pgConnectorConfig("postgres://%zz", "t"))
		require.NoError(t, err)
		assert.Equal(t, util.StateFatal, c.initializer.State())
	})

	t.Run("unsupported iam auth mode is fatal", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		cfg := pgConnectorConfig("postgres://u@127.0.0.1:1/db?sslmode=disable", "t")
		cfg.IAMAuth = &common.PostgreSQLIAMAuthConfig{Enabled: true, Region: "us-east-1", Auth: &common.AwsAuthConfig{Mode: "bogus"}}
		c, err := NewPostgreSQLConnector(ctx, &logger, "pg-bad-iam", cfg)
		require.NoError(t, err)
		assert.Equal(t, util.StateFatal, c.initializer.State())
	})

	t.Run("unreachable host returns promptly and every entry point reports not-ready", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		cfg := pgConnectorConfig("postgres://u:p@127.0.0.1:1/db?sslmode=disable", "t")
		cfg.InitTimeout = common.Duration(300 * time.Millisecond)
		start := time.Now()
		c, err := NewPostgreSQLConnector(ctx, &logger, "pg-unreachable", cfg)
		require.NoError(t, err)
		assert.Less(t, time.Since(start), 5*time.Second, "constructor must not hang on an unreachable host")

		assert.ErrorIs(t, c.Set(ctx, "pk", "rk", nil, nil), ErrConnectorNotReady)
		_, err = c.Get(ctx, ConnectorMainIndex, "pk", "rk", nil)
		assert.ErrorIs(t, err, ErrConnectorNotReady)
		_, err = c.Lock(ctx, "k", time.Second)
		assert.ErrorIs(t, err, ErrConnectorNotReady)
		assert.ErrorIs(t, c.Delete(ctx, "pk", "rk"), ErrConnectorNotReady)
		_, _, err = c.List(ctx, ConnectorMainIndex, 1, "")
		assert.ErrorIs(t, err, ErrConnectorNotReady)
		assert.ErrorIs(t, c.PublishCounterInt64(ctx, "k", CounterInt64State{}), ErrConnectorNotReady)
		assert.Equal(t, "pg-unreachable", c.Id())
	})
}
