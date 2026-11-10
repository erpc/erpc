package data

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/dgraph-io/ristretto/v2/z"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

func TestRedisConnector_Purge_SuffixIsolation(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	ctx := context.Background()
	logger := zerolog.Nop()
	cfg := &common.RedisConnectorConfig{
		Addr:         mr.Addr(),
		ConnPoolSize: 5,
		InitTimeout:  common.Duration(2 * time.Second),
		GetTimeout:   common.Duration(2 * time.Second),
		SetTimeout:   common.Duration(2 * time.Second),
	}
	require.NoError(t, cfg.SetDefaults())
	conn, err := NewRedisConnector(ctx, &logger, "redis-cache", cfg)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return conn.initializer.State() == util.StateReady
	}, 5*time.Second, 50*time.Millisecond)

	rangeKey := "eth_blockNumber:deadbeef"
	require.NoError(t, conn.Set(WithReverseIndex(ctx, "evm:1", ""), "evm:1:100", rangeKey, []byte("unsuffixed"), nil))
	require.NoError(t, conn.Set(WithReverseIndex(ctx, "evm:1", "systx"), "evm:1:systx:100", rangeKey, []byte("suffixed"), nil))

	t.Run("dryRun counts unsuffixed only", func(t *testing.T) {
		matched, deleted, err := conn.DeleteByPattern(ctx, PatternDeleteOpts{
			MainPatterns:   []string{"evm:1:*"},
			RviPatterns:    []string{"rvi#evm:1:*#*"},
			DryRun:         true,
			UnsuffixedOnly: true,
			NetworkId:      "evm:1",
		})
		require.NoError(t, err)
		require.Equal(t, int64(0), deleted)
		require.GreaterOrEqual(t, matched, int64(2)) // main + rvi
		// Sibling still present
		_, err = conn.Get(ctx, ConnectorMainIndex, "evm:1:systx:100", rangeKey, nil)
		require.NoError(t, err)
		_, err = conn.Get(ctx, ConnectorMainIndex, "evm:1:100", rangeKey, nil)
		require.NoError(t, err)
	})

	t.Run("real unsuffixed purge keeps systx sibling", func(t *testing.T) {
		_, deleted, err := conn.DeleteByPattern(ctx, PatternDeleteOpts{
			MainPatterns:   []string{"evm:1:*"},
			RviPatterns:    []string{"rvi#evm:1:*#*"},
			DryRun:         false,
			UnsuffixedOnly: true,
			NetworkId:      "evm:1",
		})
		require.NoError(t, err)
		require.GreaterOrEqual(t, deleted, int64(2))

		_, err = conn.Get(ctx, ConnectorMainIndex, "evm:1:100", rangeKey, nil)
		require.Error(t, err)
		val, err := conn.Get(ctx, ConnectorMainIndex, "evm:1:systx:100", rangeKey, nil)
		require.NoError(t, err)
		require.Equal(t, []byte("suffixed"), val)
	})

	t.Run("suffixed purge only hits systx", func(t *testing.T) {
		require.NoError(t, conn.Set(WithReverseIndex(ctx, "evm:1", ""), "evm:1:200", rangeKey, []byte("again"), nil))
		matched, deleted, err := conn.DeleteByPattern(ctx, PatternDeleteOpts{
			MainPatterns:   []string{"evm:1:systx:*"},
			RviPatterns:    []string{"rvi#evm:1:systx:*#*"},
			DryRun:         false,
			UnsuffixedOnly: false,
			NetworkId:      "evm:1",
			Suffix:         "systx",
		})
		require.NoError(t, err)
		require.GreaterOrEqual(t, matched, int64(2))
		require.Equal(t, matched, deleted)

		_, err = conn.Get(ctx, ConnectorMainIndex, "evm:1:systx:100", rangeKey, nil)
		require.Error(t, err)
		val, err := conn.Get(ctx, ConnectorMainIndex, "evm:1:200", rangeKey, nil)
		require.NoError(t, err)
		require.Equal(t, []byte("again"), val)
	})
}

func TestMemoryConnector_Purge_SuffixIsolation(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()
	conn, err := NewMemoryConnector(ctx, &logger, "mem", &common.MemoryConnectorConfig{
		MaxItems:     1000,
		MaxTotalSize: "10MB",
	})
	require.NoError(t, err)

	rangeKey := "eth_call:abcd"
	require.NoError(t, conn.Set(WithReverseIndex(ctx, "evm:1", ""), "evm:1:50", rangeKey, []byte("a"), nil))
	require.NoError(t, conn.Set(WithReverseIndex(ctx, "evm:1", "systx"), "evm:1:systx:50", rangeKey, []byte("b"), nil))
	conn.cache.Wait()

	matched, deleted, err := conn.DeleteByPattern(ctx, PatternDeleteOpts{
		MainPatterns:   []string{"evm:1:*"},
		RviPatterns:    []string{"rvi#evm:1:*#*"},
		DryRun:         false,
		UnsuffixedOnly: true,
		NetworkId:      "evm:1",
	})
	require.NoError(t, err)
	require.GreaterOrEqual(t, matched, int64(2))
	require.Equal(t, matched, deleted)

	_, err = conn.Get(ctx, ConnectorMainIndex, "evm:1:50", rangeKey, nil)
	require.Error(t, err)
	val, err := conn.Get(ctx, ConnectorMainIndex, "evm:1:systx:50", rangeKey, nil)
	require.NoError(t, err)
	require.Equal(t, []byte("b"), val)

	_, deleted, err = conn.DeleteByPattern(ctx, PatternDeleteOpts{
		MainPatterns: []string{"evm:1:systx:*"},
		RviPatterns:  []string{"rvi#evm:1:systx:*#*"},
		NetworkId:    "evm:1",
		Suffix:       "systx",
	})
	require.NoError(t, err)
	require.GreaterOrEqual(t, deleted, int64(1))
	_, err = conn.Get(ctx, ConnectorMainIndex, "evm:1:systx:50", rangeKey, nil)
	require.Error(t, err)
}

func TestMemoryConnector_Purge_ColonInMethod(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()
	conn, err := NewMemoryConnector(ctx, &logger, "mem-svm", &common.MemoryConnectorConfig{
		MaxItems: 1000, MaxTotalSize: "10MB",
	})
	require.NoError(t, err)

	rk := "custom:method:deadbeef"
	rkShort := "custom:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	require.NoError(t, conn.Set(ctx, "svm:mainnet-beta:42", rk, []byte("keep-me-gone"), nil))
	require.NoError(t, conn.Set(ctx, "svm:mainnet-beta:42", rkShort, []byte("short-method"), nil))
	require.NoError(t, conn.Set(ctx, "svm:mainnet-beta:systx:42", rk, []byte("sibling"), nil))
	conn.cache.Wait()

	_, deleted, err := conn.DeleteByPattern(ctx, PatternDeleteOpts{
		MainPatterns:   []string{"svm:mainnet-beta:*:custom:method:*"},
		DryRun:         false,
		UnsuffixedOnly: true,
		NetworkId:      "svm:mainnet-beta",
		Method:         "custom:method",
	})
	require.NoError(t, err)
	require.Equal(t, int64(1), deleted)

	_, err = conn.Get(ctx, ConnectorMainIndex, "svm:mainnet-beta:42", rk, nil)
	require.Error(t, err)
	val, err := conn.Get(ctx, ConnectorMainIndex, "svm:mainnet-beta:42", rkShort, nil)
	require.NoError(t, err, "method=custom:method must not delete method=custom")
	require.Equal(t, []byte("short-method"), val)
	val, err = conn.Get(ctx, ConnectorMainIndex, "svm:mainnet-beta:systx:42", rk, nil)
	require.NoError(t, err)
	require.Equal(t, []byte("sibling"), val)

	// Network-wide unsuffixed purge removes colon-in-method keys but not suffix siblings.
	_, deleted, err = conn.DeleteByPattern(ctx, PatternDeleteOpts{
		MainPatterns:   []string{"svm:mainnet-beta:*"},
		DryRun:         false,
		UnsuffixedOnly: true,
		NetworkId:      "svm:mainnet-beta",
	})
	require.NoError(t, err)
	require.GreaterOrEqual(t, deleted, int64(1))
	_, err = conn.Get(ctx, ConnectorMainIndex, "svm:mainnet-beta:42", rkShort, nil)
	require.Error(t, err)
	val, err = conn.Get(ctx, ConnectorMainIndex, "svm:mainnet-beta:systx:42", rk, nil)
	require.NoError(t, err)
	require.Equal(t, []byte("sibling"), val)
}

func TestMemoryConnector_Purge_KeyIndexDropsOnEvict(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()
	// Tiny MaxTotalSize so subsequent Sets force eviction of the first key.
	conn, err := NewMemoryConnector(ctx, &logger, "mem-evict", &common.MemoryConnectorConfig{
		MaxItems: 10, MaxTotalSize: "1KB",
	})
	require.NoError(t, err)

	require.NoError(t, conn.Set(ctx, "evm:1:1", "eth_call:aa", []byte("first"), nil))
	conn.cache.Wait()
	h, _ := z.KeyToHash("evm:1:1:eth_call:aa")
	_, loaded := conn.hashIndex.Load(h)
	require.True(t, loaded, "key should be indexed after Set")

	payload := make([]byte, 400)
	for i := 0; i < 20; i++ {
		pk := fmt.Sprintf("evm:1:%d", i+10)
		require.NoError(t, conn.Set(ctx, pk, "eth_call:bb", payload, nil))
	}
	conn.cache.Wait()

	require.Eventually(t, func() bool {
		conn.cache.Wait()
		_, still := conn.hashIndex.Load(h)
		return !still
	}, 3*time.Second, 50*time.Millisecond, "OnEvict/OnReject should drop evicted keys from the purge index")
}

func TestPurgeConnectors_UnsupportedReported(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()
	mem, err := NewMemoryConnector(ctx, &logger, "mem", &common.MemoryConnectorConfig{
		MaxItems: 100, MaxTotalSize: "1MB",
	})
	require.NoError(t, err)

	polMem, err := NewCachePolicy(&common.CachePolicyConfig{Connector: "mem", Network: "*", Method: "*"}, mem)
	require.NoError(t, err)
	stub := NewMockConnector("no-purge")
	stub.On("Lock", ctx, "cache-purge/evm:1", cachePurgeLockTTL).Return(nil, fmt.Errorf("no lock"))
	polStub, err := NewCachePolicy(&common.CachePolicyConfig{Connector: "no-purge", Network: "*", Method: "*"}, stub)
	require.NoError(t, err)

	results := PurgeConnectors(ctx, []*CachePolicy{polMem, polStub}, common.CachePurgeScope{
		NetworkId: "evm:1",
		DryRun:    true,
	})
	require.Len(t, results, 2)
	var sawOK, sawUnsupported bool
	for _, r := range results {
		if r.ConnectorId == "mem" {
			sawOK = true
			require.NoError(t, r.Err)
		}
		if r.ConnectorId == "no-purge" {
			sawUnsupported = true
			require.Error(t, r.Err)
		}
	}
	require.True(t, sawOK)
	require.True(t, sawUnsupported)
}

func TestPurgeConnectors_GrpcReadOnlyReportsUnsupported(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()
	grpc := &GrpcConnector{id: "grpc-cache", logger: &logger}
	require.True(t, grpc.ReadOnly())

	pol, err := NewCachePolicy(&common.CachePolicyConfig{Connector: "grpc-cache", Network: "*", Method: "*"}, grpc)
	require.NoError(t, err)

	results := PurgeConnectors(ctx, []*CachePolicy{pol}, common.CachePurgeScope{
		NetworkId: "evm:1",
		DryRun:    true,
	})
	require.Len(t, results, 1, "read-only gRPC must appear in results, not be skipped")
	require.Equal(t, "grpc-cache", results[0].ConnectorId)
	require.ErrorIs(t, results[0].Err, ErrPatternDeleteUnsupported)
}

func TestHoldPurgeDistributedLock_SkipsPostgres(t *testing.T) {
	require.False(t, holdPurgeDistributedLock(&PostgreSQLConnector{}))
	require.True(t, holdPurgeDistributedLock(&RedisConnector{}))
	require.True(t, holdPurgeDistributedLock(&MemoryConnector{}))
}
