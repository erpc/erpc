package data

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"
)

func newLeaseTestRegistry(t *testing.T, ctx context.Context, cfg *common.ConnectorConfig) SharedStateRegistry {
	t.Helper()
	r, err := NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{ClusterKey: "lease-test", Connector: cfg})
	require.NoError(t, err)
	return r
}

func exerciseLease(t *testing.T, a, b SharedStateRegistry, key string) {
	ctx := context.Background()
	ttl := 300 * time.Millisecond

	la, err := a.AcquireLease(ctx, key, ttl)
	require.NoError(t, err)
	require.NotNil(t, la, "first instance takes the free lease")

	lb, err := b.AcquireLease(ctx, key, ttl)
	require.NoError(t, err)
	require.Nil(t, lb, "second instance must not take a held lease")

	ok, err := la.Renew(ctx, ttl)
	require.NoError(t, err)
	require.True(t, ok)

	// Holder dies (stops renewing): the other instance takes over after TTL.
	require.Eventually(t, func() bool {
		lb, err = b.AcquireLease(ctx, key, ttl)
		return err == nil && lb != nil
	}, 3*time.Second, 20*time.Millisecond)

	ok, err = la.Renew(ctx, ttl)
	require.NoError(t, err)
	require.False(t, ok, "the stale holder learns it lost the lease on renew")
	require.NoError(t, la.Release(ctx), "releasing a lost lease is a no-op")

	// Explicit release hands over immediately.
	require.NoError(t, lb.Release(ctx))
	la, err = a.AcquireLease(ctx, key, ttl)
	require.NoError(t, err)
	require.NotNil(t, la)
}

func TestSharedStateLease_Redis(t *testing.T) {
	mr := miniredis.RunT(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mk := func(id string) SharedStateRegistry {
		rc := &common.RedisConnectorConfig{Addr: mr.Addr(), ConnPoolSize: 5}
		require.NoError(t, rc.SetDefaults())
		return newLeaseTestRegistry(t, ctx, &common.ConnectorConfig{Id: id, Driver: common.DriverRedis, Redis: rc})
	}
	a, b := mk("a"), mk("b")
	require.Eventually(t, func() bool {
		l, err := a.AcquireLease(ctx, "probe", time.Second)
		if l != nil {
			_ = l.Release(ctx)
		}
		return err == nil
	}, 5*time.Second, 20*time.Millisecond)
	// miniredis TTLs advance only via FastForward; drive time in the background.
	go func() {
		tk := time.NewTicker(10 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
				mr.FastForward(10 * time.Millisecond)
			}
		}
	}()
	exerciseLease(t, a, b, "redis-key")
}

func TestSharedStateLease_Memory(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mk := func() SharedStateRegistry {
		return newLeaseTestRegistry(t, ctx, &common.ConnectorConfig{
			Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{MaxItems: 100, MaxTotalSize: "1MB"},
		})
	}
	exerciseLease(t, mk(), mk(), "memory-key")
}
