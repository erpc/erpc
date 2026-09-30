package erpc

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/erpc/erpc/blockstore"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

func init() { util.ConfigureTestLogger() }

func TestBlockStoreFingerprintIncludesCachePolicy(t *testing.T) {
	project := &common.ProjectConfig{}
	network := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm: &common.EvmNetworkConfig{BlockStore: &common.EvmBlockStoreConfig{
			Enabled: true, Depth: 64, MaxBytes: 1024, MaxBlockBytes: 512,
		}},
	}
	fingerprint := func() string {
		hash, err := blockStoreFingerprint(project, network)
		require.NoError(t, err)
		return hash
	}
	base := fingerprint()
	network.Evm.BlockStore.Depth++
	require.NotEqual(t, base, fingerprint(), "different cache depths must use distinct fleet scopes")
	network.Evm.BlockStore.Depth--
	network.Evm.BlockStore.MaxBytes++
	require.NotEqual(t, base, fingerprint(), "different cache capacities must use distinct fleet scopes")
	network.Evm.BlockStore.MaxBytes--
	network.Evm.BlockStore.MaxBlockBytes++
	require.NotEqual(t, base, fingerprint(), "different maximum block sizes must use distinct fleet scopes")
}

func TestBlockStoreFleetStoreLeaseFencingAndPartition(t *testing.T) {
	mr := miniredis.RunT(t)
	ctx := context.Background()
	cfg := &common.RedisConnectorConfig{URI: "redis://" + mr.Addr()}
	require.NoError(t, cfg.SetDefaults())
	logger := zerolog.New(io.Discard)
	rc, err := data.NewRedisConnector(ctx, &logger, "blockstore-fleet-test", cfg)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return rc.Client() != nil }, 3*time.Second, 10*time.Millisecond)
	store := &blockStoreConnectorStore{connector: rc, redis: rc}
	scope := blockstore.Scope{Namespace: "test", ProjectId: "project", NetworkId: "evm:1"}

	_, err = store.Acquire(ctx, scope, time.Nanosecond)
	require.Error(t, err)
	lockKey, _, err := store.fleetKeys(scope)
	require.NoError(t, err)
	require.False(t, mr.Exists(lockKey), "sub-millisecond acquisition must not create a persistent lock")

	first, err := store.Acquire(ctx, scope, time.Second)
	require.NoError(t, err)
	require.NotNil(t, first)
	blocked, err := store.Acquire(ctx, scope, time.Second)
	require.NoError(t, err)
	require.Nil(t, blocked)

	renewed, err := first.Renew(ctx, 2*time.Second)
	require.NoError(t, err)
	require.True(t, renewed)
	require.Error(t, func() error {
		_, err := first.Renew(ctx, time.Nanosecond)
		return err
	}())
	require.Error(t, func() error {
		_, err := first.Publish(ctx, &blockstore.Snapshot{Head: 1, Hashes: []string{"x"}, At: time.Now()}, time.Nanosecond)
		return err
	}())

	snap := &blockstore.Snapshot{Head: 2, Hashes: []string{"a", "b", "c"}, At: time.Now(), Incomplete: true}
	published, err := first.Publish(ctx, snap, 3*time.Second)
	require.NoError(t, err)
	require.True(t, published)
	got, err := store.ReadSnapshot(ctx, scope)
	require.NoError(t, err)
	require.Equal(t, snap.Head, got.Head)
	require.Equal(t, snap.Hashes, got.Hashes)
	require.True(t, got.Incomplete, "snapshot JSON must preserve the incomplete flag")

	lockKey, snapshotKey, err := store.fleetKeys(scope)
	require.NoError(t, err)
	require.Equal(t, int64(2000), mr.TTL(lockKey).Milliseconds())
	require.Equal(t, int64(3000), mr.TTL(snapshotKey).Milliseconds())

	otherScope := scope
	otherScope.NetworkId = "evm:2"
	other, err := store.Acquire(ctx, otherScope, time.Second)
	require.NoError(t, err)
	require.NotNil(t, other)

	mr.FastForward(3 * time.Second)
	takeover, err := store.Acquire(ctx, scope, time.Second)
	require.NoError(t, err)
	require.NotNil(t, takeover)
	staleWrite, err := first.Publish(ctx, &blockstore.Snapshot{Head: 9, Hashes: []string{"x"}, At: time.Now()}, time.Minute)
	require.NoError(t, err)
	require.False(t, staleWrite)
	staleRenew, err := first.Renew(ctx, time.Second)
	require.NoError(t, err)
	require.False(t, staleRenew)
	require.NoError(t, first.Release(ctx))
	stillHeld, err := store.Acquire(ctx, scope, time.Second)
	require.NoError(t, err)
	require.Nil(t, stillHeld)

	newSnap := &blockstore.Snapshot{Head: 4, Hashes: []string{"d"}, At: time.Now()}
	ok, err := takeover.Publish(ctx, newSnap, time.Second)
	require.NoError(t, err)
	require.True(t, ok)
	value, err := rc.Get(ctx, data.ConnectorMainIndex, store.mustPartition(t, scope), "fleet-snapshot", nil)
	require.NoError(t, err)
	var decoded blockstore.Snapshot
	require.NoError(t, json.Unmarshal(value, &decoded))
	require.Equal(t, int64(4), decoded.Head)
	require.NoError(t, takeover.Release(ctx))
	require.NoError(t, other.Release(ctx))
}

func (s *blockStoreConnectorStore) mustPartition(t *testing.T, scope blockstore.Scope) string {
	t.Helper()
	partition, err := s.partition(scope)
	require.NoError(t, err)
	return partition
}
