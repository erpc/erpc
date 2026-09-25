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

func newLeaseRedis(t *testing.T, m *miniredis.Miniredis) *RedisConnector {
	t.Helper()
	lg := zerolog.New(io.Discard)
	cfg := &common.RedisConnectorConfig{
		Addr: m.Addr(), ConnPoolSize: 5,
		InitTimeout: common.Duration(2 * time.Second), GetTimeout: common.Duration(time.Second),
		SetTimeout: common.Duration(time.Second), LockRetryInterval: common.Duration(20 * time.Millisecond),
	}
	require.NoError(t, cfg.SetDefaults())
	c, err := NewRedisConnector(context.Background(), &lg, "lease-test", cfg)
	require.NoError(t, err)
	return c
}

func TestHeadPollLease_OneHolderRenewAndTakeover(t *testing.T) {
	m, err := miniredis.Run()
	require.NoError(t, err)
	defer m.Close()
	ctx := context.Background()
	ttl := 2 * time.Second
	var aEvents []bool
	a := NewHeadPollLease(newLeaseRedis(t, m), "hp/k", ttl, nil, func(h bool) { aEvents = append(aEvents, h) })
	b := NewHeadPollLease(newLeaseRedis(t, m), "hp/k", ttl, nil, nil)

	a.Step(ctx)
	require.True(t, a.Held())
	b.Step(ctx)
	require.False(t, b.Held(), "only one holder")

	// Renew keeps a as holder past the original TTL.
	for i := 0; i < 3; i++ {
		m.FastForward(ttl / 2)
		a.Step(ctx)
		require.True(t, a.Held())
		b.Step(ctx)
		require.False(t, b.Held())
	}

	// Crash takeover: a stops renewing (no unlock); after Redis TTL b acquires.
	m.FastForward(ttl + time.Millisecond)
	b.Step(ctx)
	require.True(t, b.Held())
	// a's renew now fails and it drops the claim immediately.
	a.Step(ctx)
	require.False(t, a.Held())
	require.Equal(t, []bool{true, false}, dedupe(aEvents))

	// Graceful stop releases for immediate takeover.
	b.Stop()
	require.False(t, b.Held())
	a.Step(ctx)
	require.True(t, a.Held())
	a.Stop()
}

func TestHeadPollLease_HeldExpiresLocallyWithoutRenew(t *testing.T) {
	m, err := miniredis.Run()
	require.NoError(t, err)
	defer m.Close()
	a := NewHeadPollLease(newLeaseRedis(t, m), "hp/x", 2*time.Second, nil, nil)
	now := time.Now()
	a.nowFn = func() time.Time { return now }
	a.Step(context.Background())
	require.True(t, a.Held())
	// Local clock beyond ttl-margin: never claims a possibly-expired lease.
	a.nowFn = func() time.Time { return now.Add(1900 * time.Millisecond) }
	require.False(t, a.Held())
	a.Stop()
}

func TestHeadPollLease_RedisDownFailsOpen(t *testing.T) {
	m, err := miniredis.Run()
	require.NoError(t, err)
	a := NewHeadPollLease(newLeaseRedis(t, m), "hp/y", 2*time.Second, nil, nil)
	a.Step(context.Background())
	require.True(t, a.Held())
	m.Close()
	a.Step(context.Background())
	require.False(t, a.Held(), "renew error must drop the claim")
	a.Stop()
}

func dedupe(in []bool) []bool {
	var out []bool
	for _, v := range in {
		if len(out) == 0 || out[len(out)-1] != v {
			out = append(out, v)
		}
	}
	return out
}
