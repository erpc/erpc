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

func TestCordonStore_MemoryDriverHasNoStore(t *testing.T) {
	logger := zerolog.New(io.Discard)
	cfg := &common.SharedStateConfig{Connector: &common.ConnectorConfig{Driver: common.DriverMemory}}
	require.NoError(t, cfg.SetDefaults("c"))
	ssr, err := NewSharedStateRegistry(t.Context(), &logger, cfg)
	require.NoError(t, err)
	require.Nil(t, ssr.Cordons(), "process-local shared state must not offer cordon persistence")
}

// Two registries over one Redis stand in for two replicas: a write on one is
// the next read on the other, deletes collapse empty maps, and a foreign
// project's record is never touched.
func TestCordonStore_TwoReplicasShareOneRecord(t *testing.T) {
	m, err := miniredis.Run()
	require.NoError(t, err)
	defer m.Close()
	ctx := context.Background()

	a := newRedisSharedState(t, m.Addr()).Cordons()
	b := newRedisSharedState(t, m.Addr()).Cordons()
	require.NotNil(t, a)
	require.NotNil(t, b)

	got, err := b.Get(ctx, "main")
	require.NoError(t, err)
	require.Empty(t, got, "missing record reads as empty, not error")

	e1 := common.CordonEntry{Reason: "incident-1", CordonedAtMs: 1000}
	e2 := common.CordonEntry{Reason: "p95 > 30s", CordonedAtMs: 2000}
	require.NoError(t, a.Set(ctx, "main", "alchemy", "*", e1))
	require.NoError(t, a.Set(ctx, "main", "drpc", "eth_getLogs", e2))
	require.NoError(t, a.Set(ctx, "other", "alchemy", "*", e1))

	got, err = b.Get(ctx, "main")
	require.NoError(t, err)
	require.Equal(t, ProjectCordons{
		"alchemy": {"*": e1},
		"drpc":    {"eth_getLogs": e2},
	}, got)

	// Reason edit from the other replica keeps the record coherent.
	e1b := common.CordonEntry{Reason: "incident-1 (extended)", CordonedAtMs: 1000}
	require.NoError(t, b.Set(ctx, "main", "alchemy", "*", e1b))
	got, err = a.Get(ctx, "main")
	require.NoError(t, err)
	require.Equal(t, e1b, got["alchemy"]["*"])

	require.NoError(t, b.Delete(ctx, "main", "drpc", "eth_getLogs"))
	require.NoError(t, b.Delete(ctx, "main", "drpc", "never-set"), "deleting an absent entry is a no-op")
	got, err = a.Get(ctx, "main")
	require.NoError(t, err)
	require.Equal(t, ProjectCordons{"alchemy": {"*": e1b}}, got, "empty upstream map is dropped")

	require.NoError(t, a.Delete(ctx, "main", "alchemy", "*"))
	got, err = b.Get(ctx, "main")
	require.NoError(t, err)
	require.Empty(t, got, "last entry removed deletes the record")

	got, err = b.Get(ctx, "other")
	require.NoError(t, err)
	require.Equal(t, ProjectCordons{"alchemy": {"*": e1}}, got, "other project untouched")
}

func TestCordonStore_WriteFailsWhenRedisDown(t *testing.T) {
	m, err := miniredis.Run()
	require.NoError(t, err)
	store := newRedisSharedState(t, m.Addr()).Cordons()
	m.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	require.Error(t, store.Set(ctx, "main", "alchemy", "*", common.CordonEntry{Reason: "x", CordonedAtMs: 1}))
	_, err = store.Get(ctx, "main")
	require.Error(t, err)
}
