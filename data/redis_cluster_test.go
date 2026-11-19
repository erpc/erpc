package data

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

func newClusterRedisConnector(t *testing.T, uri string) (context.Context, *RedisConnector) {
	t.Helper()
	cfg := &common.RedisConnectorConfig{
		URI:               uri,
		InitTimeout:       common.Duration(5 * time.Second),
		GetTimeout:        common.Duration(2 * time.Second),
		SetTimeout:        common.Duration(2 * time.Second),
		LockRetryInterval: common.Duration(50 * time.Millisecond),
	}
	require.NoError(t, cfg.SetDefaults())
	logger := zerolog.New(io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c, err := NewRedisConnector(ctx, &logger, "redis-cluster-test", cfg)
	require.NoError(t, err)
	return ctx, c
}

func TestRedisConnector_StandaloneKeepsSingleNodeClient(t *testing.T) {
	_, c, _ := newReadyRedisConnector(t, nil)
	_, ok := c.client.(*redis.Client)
	assert.True(t, ok, "a server without cluster mode keeps the single-node client")
}

func TestRedisConnector_ClusterMode(t *testing.T) {
	addr := sharedValkeyClusterAddr(t)
	ctx, c := newClusterRedisConnector(t, "redis://"+addr)
	require.Equal(t, util.StateReady, c.initializer.State(), "errors: %v", c.initializer.Errors())

	cluster, ok := c.client.(*redis.ClusterClient)
	require.True(t, ok, "cluster mode is detected from the seed node")

	prefix := fmt.Sprintf("t%d", time.Now().UnixNano())
	const n = 60

	t.Run("keys spread over every master are written and read back", func(t *testing.T) {
		for i := range n {
			require.NoError(t, c.Set(ctx, fmt.Sprintf("%s-%d", prefix, i), "rk", []byte(fmt.Sprintf("v%d", i)), nil))
		}
		for i := range n {
			v, err := c.Get(ctx, ConnectorMainIndex, fmt.Sprintf("%s-%d", prefix, i), "rk", nil)
			require.NoError(t, err)
			assert.Equal(t, fmt.Sprintf("v%d", i), string(v))
		}

		owners := map[string]bool{}
		for i := range n {
			node, err := cluster.MasterForKey(ctx, fmt.Sprintf("%s-%d:rk", prefix, i))
			require.NoError(t, err)
			owners[node.Options().Addr] = true
		}
		assert.Len(t, owners, 3, "the test keys must land on all three masters")
	})

	t.Run("list pages through every master exactly once", func(t *testing.T) {
		seen := map[string]int{}
		token := ""
		for range 1000 {
			items, next, err := c.List(ctx, ConnectorMainIndex, 7, token)
			require.NoError(t, err)
			for _, it := range items {
				seen[it.PartitionKey+":"+it.RangeKey]++
			}
			if next == "" {
				break
			}
			token = next
		}
		for i := range n {
			assert.Equal(t, 1, seen[fmt.Sprintf("%s-%d:rk", prefix, i)], "key %d", i)
		}
	})

	t.Run("token of a master that left resumes on the next address", func(t *testing.T) {
		items, next, err := c.List(ctx, ConnectorMainIndex, 7, "127.0.0.1:1@42")
		require.NoError(t, err)
		assert.True(t, len(items) > 0 || next != "", "the walk continues on the first master after the stale address")
	})

	t.Run("malformed tokens are rejected", func(t *testing.T) {
		for _, tok := range []string{"42", "@42", "127.0.0.1:1@x"} {
			_, _, err := c.List(ctx, ConnectorMainIndex, 1, tok)
			require.ErrorContains(t, err, "invalid pagination token", tok)
		}
	})

	t.Run("reverse index resolves across slots", func(t *testing.T) {
		rctx := WithReverseIndex(ctx, "evm:"+prefix, "")
		require.NoError(t, c.Set(rctx, "evm:"+prefix+":100", "rk", []byte("vblock"), nil))
		v, err := c.Get(ctx, ConnectorReverseIndex, common.CachePartitionKey("evm:"+prefix, "", "*"), "rk", nil)
		require.NoError(t, err)
		assert.Equal(t, "vblock", string(v))
	})

	t.Run("delete removes the key", func(t *testing.T) {
		require.NoError(t, c.Delete(ctx, prefix+"-0", "rk"))
		_, err := c.Get(ctx, ConnectorMainIndex, prefix+"-0", "rk", nil)
		require.True(t, common.HasErrorCode(err, common.ErrCodeRecordNotFound), "got %v", err)
	})

	t.Run("locks are exclusive", func(t *testing.T) {
		lock, err := c.Lock(ctx, prefix+"-lock", 2*time.Second)
		require.NoError(t, err)
		_, err = c.Lock(ctx, prefix+"-lock", 200*time.Millisecond)
		require.Error(t, err)
		require.NoError(t, lock.Unlock(ctx))
	})

	t.Run("counter updates reach a subscriber on another connector", func(t *testing.T) {
		_, other := newClusterRedisConnector(t, "redis://"+addr)
		require.Equal(t, util.StateReady, other.initializer.State())

		key := prefix + "/latest"
		updates, cleanup, err := other.WatchCounterInt64(ctx, key)
		require.NoError(t, err)
		defer cleanup()

		want := CounterInt64State{Value: 42, UpdatedAt: time.Now().UnixMilli(), UpdatedBy: "pub"}
		deadline := time.After(10 * time.Second)
		tick := time.NewTicker(200 * time.Millisecond)
		defer tick.Stop()
		for {
			require.NoError(t, c.PublishCounterInt64(ctx, key, want))
			select {
			case st := <-updates:
				if st == want {
					return
				}
			case <-tick.C:
			case <-deadline:
				t.Fatal("published value not delivered across the cluster")
			}
		}
	})
}

func TestRedisConnector_ClusterModeSelectsDatabase(t *testing.T) {
	addr := sharedValkeyClusterAddr(t)
	ctx, db3 := newClusterRedisConnector(t, "redis://"+addr+"/3")
	require.Equal(t, util.StateReady, db3.initializer.State(), "errors: %v", db3.initializer.Errors())
	_, db0 := newClusterRedisConnector(t, "redis://"+addr+"/0")
	require.Equal(t, util.StateReady, db0.initializer.State(), "errors: %v", db0.initializer.Errors())

	prefix := fmt.Sprintf("db%d", time.Now().UnixNano())
	for i := range 30 {
		pk := fmt.Sprintf("%s-%d", prefix, i)
		require.NoError(t, db3.Set(ctx, pk, "rk", []byte("in-db3"), nil))

		v, err := db3.Get(ctx, ConnectorMainIndex, pk, "rk", nil)
		require.NoError(t, err)
		assert.Equal(t, "in-db3", string(v))

		_, err = db0.Get(ctx, ConnectorMainIndex, pk, "rk", nil)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeRecordNotFound), "key %d leaked into database 0: %v", i, err)
	}
}

func TestRedisClusterOptions_CarriesConnectionSettings(t *testing.T) {
	creds := func(context.Context) (string, string, error) { return "u", "token", nil }
	o := &redis.Options{
		Addr:                       "cfg.example:6379",
		Username:                   "u",
		Password:                   "p",
		CredentialsProviderContext: creds,
		TLSConfig:                  &tls.Config{ServerName: "cfg.example"},
		DialTimeout:                3 * time.Second,
		ReadTimeout:                time.Second,
		WriteTimeout:               2 * time.Second,
		ContextTimeoutEnabled:      true,
		PoolSize:                   8,
		ConnMaxLifetime:            11 * time.Hour,
		ConnMaxLifetimeJitter:      30 * time.Minute,
	}
	co := redisClusterOptions(o)
	assert.Equal(t, []string{"cfg.example:6379"}, co.Addrs)
	assert.Equal(t, "u", co.Username)
	assert.Equal(t, "p", co.Password)
	assert.NotNil(t, co.CredentialsProviderContext)
	assert.Same(t, o.TLSConfig, co.TLSConfig)
	assert.Equal(t, o.DialTimeout, co.DialTimeout)
	assert.Equal(t, o.ReadTimeout, co.ReadTimeout)
	assert.Equal(t, o.WriteTimeout, co.WriteTimeout)
	assert.True(t, co.ContextTimeoutEnabled)
	assert.Equal(t, 8, co.PoolSize)
	assert.Equal(t, o.ConnMaxLifetime, co.ConnMaxLifetime)
	assert.Equal(t, o.ConnMaxLifetimeJitter, co.ConnMaxLifetimeJitter)
}
