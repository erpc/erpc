package data

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

func TestIntegrationPollLock_CrossInstanceDedup_Redis(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s, err := miniredis.Run()
	require.NoError(t, err)
	defer s.Close()

	newSSR := func(id string) SharedStateRegistry {
		cfg := &common.SharedStateConfig{
			ClusterKey:      "test",
			FallbackTimeout: common.Duration(2 * time.Second),
			LockTtl:         common.Duration(2 * time.Second),
			LockMaxWait:     common.Duration(200 * time.Millisecond),
			// Must be long enough for: refreshFn + background push/publish + pubsub propagation.
			UpdateMaxWait: common.Duration(2 * time.Second),
			Connector: &common.ConnectorConfig{
				Id:     id,
				Driver: common.DriverRedis,
				Redis: &common.RedisConnectorConfig{
					URI:               fmt.Sprintf("redis://%s", s.Addr()),
					InitTimeout:       common.Duration(1 * time.Second),
					GetTimeout:        common.Duration(1 * time.Second),
					SetTimeout:        common.Duration(1 * time.Second),
					LockRetryInterval: common.Duration(50 * time.Millisecond),
				},
			},
		}
		require.NoError(t, cfg.SetDefaults("test"))

		ssr, err := NewSharedStateRegistry(ctx, &log.Logger, cfg)
		require.NoError(t, err)
		return ssr
	}

	ssrA := newSSR("test-redis-a")
	ssrB := newSSR("test-redis-b")

	counterA := ssrA.GetCounterInt64("poll-lock", 1024).(*counterInt64)
	counterB := ssrB.GetCounterInt64("poll-lock", 1024).(*counterInt64)

	// Handshake: ensure B receives updates from A (pubsub/watch + initial fetch).
	counterA.TryUpdate(ctx, 1)
	require.Eventually(t, func() bool {
		return counterB.GetValue() == 1
	}, 2*time.Second, 10*time.Millisecond, "expected counterB to observe counterA update via shared state")

	// Force both sides stale so both attempt refresh concurrently.
	staleTs := time.Now().Add(-10 * time.Second).UnixMilli()
	counterA.updatedAtUnixMs.Store(staleTs)
	counterB.updatedAtUnixMs.Store(staleTs)

	var refreshCalls atomic.Int32
	refreshFn := func(ctx context.Context) (int64, error) {
		refreshCalls.Add(1)
		time.Sleep(200 * time.Millisecond)
		return 2, nil
	}

	callCtx, callCancel := context.WithTimeout(ctx, 5*time.Second)
	defer callCancel()

	var wg sync.WaitGroup
	wg.Add(2)

	var (
		valA int64
		valB int64
		errA error
		errB error
	)

	go func() {
		defer wg.Done()
		valA, errA = counterA.TryUpdateIfStale(callCtx, 5*time.Second, refreshFn)
	}()
	go func() {
		defer wg.Done()
		valB, errB = counterB.TryUpdateIfStale(callCtx, 5*time.Second, refreshFn)
	}()

	wg.Wait()

	require.NoError(t, errA)
	require.NoError(t, errB)
	assert.Equal(t, int64(2), valA)
	assert.Equal(t, int64(2), valB)
	assert.Equal(t, int32(1), refreshCalls.Load(), "expected exactly one refreshFn execution across instances")

	require.Eventually(t, func() bool {
		return counterA.GetValue() == 2 && counterB.GetValue() == 2
	}, 2*time.Second, 10*time.Millisecond, "expected both counters to converge via shared state")
}

