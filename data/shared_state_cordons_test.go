package data

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

func newRedisSharedState(t *testing.T, addr string) SharedStateRegistry {
	t.Helper()
	logger := zerolog.New(io.Discard)
	cfg := &common.SharedStateConfig{
		ClusterKey: "cluster-a",
		Connector: &common.ConnectorConfig{
			Driver: common.DriverRedis,
			Redis: &common.RedisConnectorConfig{
				Addr:         addr,
				ConnPoolSize: 5,
				InitTimeout:  common.Duration(2 * time.Second),
				GetTimeout:   common.Duration(2 * time.Second),
				SetTimeout:   common.Duration(2 * time.Second),
			},
		},
	}
	require.NoError(t, cfg.SetDefaults("cluster-a"))
	ssr, err := NewSharedStateRegistry(t.Context(), &logger, cfg)
	require.NoError(t, err)
	return ssr
}

func newMemorySharedState(t *testing.T) SharedStateRegistry {
	t.Helper()
	logger := zerolog.New(io.Discard)
	cfg := &common.SharedStateConfig{Connector: &common.ConnectorConfig{Driver: common.DriverMemory}}
	require.NoError(t, cfg.SetDefaults("c"))
	ssr, err := NewSharedStateRegistry(t.Context(), &logger, cfg)
	require.NoError(t, err)
	return ssr
}

// The store contract, run against both implementations: versions grow on
// every write, a reason edit keeps the original start, deletes are
// idempotent, empty maps collapse, and projects never touch each other.
func TestCordonStore_Contract(t *testing.T) {
	m, err := miniredis.Run()
	require.NoError(t, err)
	defer m.Close()

	for name, store := range map[string]CordonStore{
		"memory": newMemorySharedState(t).Cordons(),
		"redis":  newRedisSharedState(t, m.Addr()).Cordons(),
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()

			snap, err := store.Get(ctx, "main")
			require.NoError(t, err)
			require.Equal(t, int64(0), snap.Version)
			require.Empty(t, snap.Cordons)

			snap, err = store.Set(ctx, "main", "alchemy", "*", "incident-1")
			require.NoError(t, err)
			require.Equal(t, int64(1), snap.Version)
			first := snap.Cordons["alchemy"]["*"]
			require.Equal(t, "incident-1", first.Reason)
			require.NotZero(t, first.CordonedAtMs)

			time.Sleep(2 * time.Millisecond)
			snap, err = store.Set(ctx, "main", "alchemy", "*", "incident-1 (extended)")
			require.NoError(t, err)
			require.Equal(t, int64(2), snap.Version)
			require.Equal(t, first.CordonedAtMs, snap.Cordons["alchemy"]["*"].CordonedAtMs, "reason edit keeps the start")
			require.Equal(t, "incident-1 (extended)", snap.Cordons["alchemy"]["*"].Reason)

			snap, err = store.Set(ctx, "main", "drpc", "eth_getLogs", "slow")
			require.NoError(t, err)
			require.Equal(t, int64(3), snap.Version)
			_, err = store.Set(ctx, "other", "alchemy", "*", "foreign")
			require.NoError(t, err)

			got, err := store.Get(ctx, "main")
			require.NoError(t, err)
			require.Equal(t, snap, got, "Get returns exactly what the last write returned")
			require.Len(t, got.Cordons, 2)

			snap, err = store.Delete(ctx, "main", "drpc", "never-set")
			require.NoError(t, err)
			require.Equal(t, int64(4), snap.Version, "a no-op delete still advances the version")
			snap, err = store.Delete(ctx, "main", "drpc", "eth_getLogs")
			require.NoError(t, err)
			require.NotContains(t, snap.Cordons, "drpc", "empty upstream map is dropped")
			snap, err = store.Delete(ctx, "main", "alchemy", "*")
			require.NoError(t, err)
			require.Empty(t, snap.Cordons)
			require.Equal(t, int64(6), snap.Version, "record survives empty so the version stays monotonic")

			got, err = store.Get(ctx, "other")
			require.NoError(t, err)
			require.Equal(t, "foreign", got.Cordons["alchemy"]["*"].Reason)
		})
	}
}

// Two registries over one Redis stand in for two replicas: a write on one is
// the next read on the other.
func TestCordonStore_TwoReplicasShareOneRecord(t *testing.T) {
	m, err := miniredis.Run()
	require.NoError(t, err)
	defer m.Close()
	ctx := context.Background()

	a := newRedisSharedState(t, m.Addr()).Cordons()
	b := newRedisSharedState(t, m.Addr()).Cordons()

	wrote, err := a.Set(ctx, "main", "alchemy", "*", "incident-1")
	require.NoError(t, err)
	read, err := b.Get(ctx, "main")
	require.NoError(t, err)
	require.Equal(t, wrote, read)

	wrote, err = b.Set(ctx, "main", "alchemy", "*", "incident-1 (edited on b)")
	require.NoError(t, err)
	require.Equal(t, read.Cordons["alchemy"]["*"].CordonedAtMs, wrote.Cordons["alchemy"]["*"].CordonedAtMs,
		"an edit from a replica that never held the cordon locally still keeps the persisted start")
}

func TestCordonStore_RedisDownFailsWrites(t *testing.T) {
	m, err := miniredis.Run()
	require.NoError(t, err)
	store := newRedisSharedState(t, m.Addr()).Cordons()
	m.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err = store.Set(ctx, "main", "alchemy", "*", "x")
	require.Error(t, err)
	_, err = store.Get(ctx, "main")
	require.Error(t, err)
}
