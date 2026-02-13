package data

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	promUtil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

type lockOverrideConnector struct {
	inner Connector
	lock  func(ctx context.Context, key string, ttl time.Duration) (DistributedLock, error)
}

func (c *lockOverrideConnector) Id() string { return c.inner.Id() }
func (c *lockOverrideConnector) Get(ctx context.Context, index, partitionKey, rangeKey string, metadata interface{}) ([]byte, error) {
	return c.inner.Get(ctx, index, partitionKey, rangeKey, metadata)
}
func (c *lockOverrideConnector) Set(ctx context.Context, partitionKey, rangeKey string, value []byte, ttl *time.Duration) error {
	return c.inner.Set(ctx, partitionKey, rangeKey, value, ttl)
}
func (c *lockOverrideConnector) Delete(ctx context.Context, partitionKey, rangeKey string) error {
	return c.inner.Delete(ctx, partitionKey, rangeKey)
}
func (c *lockOverrideConnector) List(ctx context.Context, index string, limit int, paginationToken string) ([]KeyValuePair, string, error) {
	return c.inner.List(ctx, index, limit, paginationToken)
}
func (c *lockOverrideConnector) Lock(ctx context.Context, key string, ttl time.Duration) (DistributedLock, error) {
	if c.lock != nil {
		return c.lock(ctx, key, ttl)
	}
	return c.inner.Lock(ctx, key, ttl)
}
func (c *lockOverrideConnector) WatchCounterInt64(ctx context.Context, key string) (<-chan CounterInt64State, func(), error) {
	return c.inner.WatchCounterInt64(ctx, key)
}
func (c *lockOverrideConnector) PublishCounterInt64(ctx context.Context, key string, value CounterInt64State) error {
	return c.inner.PublishCounterInt64(ctx, key, value)
}

type blockingUnlockLock struct {
	inner       DistributedLock
	unlockCalled chan struct{}
	allowUnlock  chan struct{}
}

func (l *blockingUnlockLock) IsNil() bool { return l == nil || l.inner == nil || l.inner.IsNil() }
func (l *blockingUnlockLock) Unlock(ctx context.Context) error {
	select {
	case l.unlockCalled <- struct{}{}:
	default:
	}
	<-l.allowUnlock
	return l.inner.Unlock(ctx)
}

func newMiniredis(t *testing.T) *miniredis.Miniredis {
	t.Helper()
	s, err := miniredis.Run()
	require.NoError(t, err)
	return s
}

func newRedisSSR(t *testing.T, ctx context.Context, addr, id string, lockTtl, lockMaxWait, updateMaxWait, fallbackTimeout time.Duration) SharedStateRegistry {
	t.Helper()

	cfg := &common.SharedStateConfig{
		ClusterKey:      "test",
		FallbackTimeout: common.Duration(fallbackTimeout),
		LockTtl:         common.Duration(lockTtl),
		LockMaxWait:     common.Duration(lockMaxWait),
		UpdateMaxWait:   common.Duration(updateMaxWait),
		Connector: &common.ConnectorConfig{
			Id:     id,
			Driver: common.DriverRedis,
			Redis: &common.RedisConnectorConfig{
				URI:               fmt.Sprintf("redis://%s", addr),
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

func forceStale(c *counterInt64) {
	c.updatedAtUnixMs.Store(time.Now().Add(-10 * time.Second).UnixMilli())
}

func metricDelta(t *testing.T, outcome string, before float64) float64 {
	t.Helper()
	return metricValue(t, outcome) - before
}

func metricValue(t *testing.T, outcome string) float64 {
	t.Helper()
	m, err := telemetry.MetricSharedStatePollLockTotal.GetMetricWithLabelValues(outcome)
	require.NoError(t, err)
	return promUtil.ToFloat64(m)
}

func TestIntegrationPollLock_ContentionWait_GetsFresh_Redis(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := newMiniredis(t)
	defer s.Close()

	ssrA := newRedisSSR(t, ctx, s.Addr(), "test-redis-a", 2*time.Second, 200*time.Millisecond, 2*time.Second, 2*time.Second)
	ssrB := newRedisSSR(t, ctx, s.Addr(), "test-redis-b", 2*time.Second, 200*time.Millisecond, 2*time.Second, 2*time.Second)

	counterA := ssrA.GetCounterInt64("poll-lock-contention", 1024).(*counterInt64)
	counterB := ssrB.GetCounterInt64("poll-lock-contention", 1024).(*counterInt64)

	// Handshake: ensure B receives updates from A.
	counterA.TryUpdate(ctx, 1)
	require.Eventually(t, func() bool { return counterB.GetValue() == 1 }, 2*time.Second, 10*time.Millisecond)

	forceStale(counterA)
	forceStale(counterB)

	beforeContention := metricValue(t, "contention")
	beforeAcquired := metricValue(t, "acquired")

	var callsA atomic.Int32
	fnA := func(ctx context.Context) (int64, error) {
		callsA.Add(1)
		time.Sleep(200 * time.Millisecond)
		return 2, nil
	}
	var callsB atomic.Int32
	fnB := func(ctx context.Context) (int64, error) {
		callsB.Add(1)
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
		valA, errA = counterA.TryUpdateIfStale(callCtx, 5*time.Second, fnA)
	}()
	go func() {
		defer wg.Done()
		valB, errB = counterB.TryUpdateIfStale(callCtx, 5*time.Second, fnB)
	}()

	wg.Wait()

	require.NoError(t, errA)
	require.NoError(t, errB)
	assert.Equal(t, int64(2), valA)
	assert.Equal(t, int64(2), valB)
	assert.Equal(t, int32(1), callsA.Load()+callsB.Load(), "expected exactly one refreshFn execution across instances")

	assert.GreaterOrEqual(t, metricDelta(t, "acquired", beforeAcquired), 1.0)
	assert.GreaterOrEqual(t, metricDelta(t, "contention", beforeContention), 1.0)
}

func TestIntegrationPollLock_UnchangedValue_PublishesFreshness_Redis(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := newMiniredis(t)
	defer s.Close()

	ssrA := newRedisSSR(t, ctx, s.Addr(), "test-redis-a", 2*time.Second, 1*time.Second, 2*time.Second, 2*time.Second)
	ssrB := newRedisSSR(t, ctx, s.Addr(), "test-redis-b", 2*time.Second, 1*time.Second, 2*time.Second, 2*time.Second)

	counterA := ssrA.GetCounterInt64("poll-lock-unchanged", 1024).(*counterInt64)
	counterB := ssrB.GetCounterInt64("poll-lock-unchanged", 1024).(*counterInt64)

	// Handshake.
	counterA.TryUpdate(ctx, 1)
	require.Eventually(t, func() bool { return counterB.GetValue() == 1 }, 2*time.Second, 10*time.Millisecond)

	forceStale(counterA)
	forceStale(counterB)

	var refreshCalls atomic.Int32
	refreshFn := func(ctx context.Context) (int64, error) {
		refreshCalls.Add(1)
		time.Sleep(200 * time.Millisecond)
		return 1, nil // unchanged
	}

	callCtx, callCancel := context.WithTimeout(ctx, 5*time.Second)
	defer callCancel()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = counterA.TryUpdateIfStale(callCtx, 5*time.Second, refreshFn)
	}()
	go func() {
		defer wg.Done()
		_, _ = counterB.TryUpdateIfStale(callCtx, 5*time.Second, refreshFn)
	}()
	wg.Wait()

	require.Equal(t, int32(1), refreshCalls.Load(), "expected exactly one refresh across instances")

	// If freshness was published, both should be non-stale and subsequent callers should not re-poll.
	staleness := 5 * time.Second
	require.Eventually(t, func() bool {
		return !counterA.IsStale(staleness) && !counterB.IsStale(staleness)
	}, 2*time.Second, 10*time.Millisecond, "expected counters to become fresh via shared state propagation")

	_, err := counterB.TryUpdateIfStale(callCtx, staleness, func(ctx context.Context) (int64, error) {
		refreshCalls.Add(1)
		return 2, nil
	})
	require.NoError(t, err)
	require.Equal(t, int32(1), refreshCalls.Load(), "expected no second refresh after unchanged freshness publish")
}

func TestIntegrationPollLock_ContentionWait_Timeout_Redis(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := newMiniredis(t)
	defer s.Close()

	// Short updateMaxWait so contender times out quickly; long fallback so holder keeps lock.
	ssrA := newRedisSSR(t, ctx, s.Addr(), "test-redis-a", 3*time.Second, 200*time.Millisecond, 150*time.Millisecond, 5*time.Second)
	ssrB := newRedisSSR(t, ctx, s.Addr(), "test-redis-b", 3*time.Second, 200*time.Millisecond, 150*time.Millisecond, 5*time.Second)

	counterA := ssrA.GetCounterInt64("poll-lock-timeout", 1024).(*counterInt64)
	counterB := ssrB.GetCounterInt64("poll-lock-timeout", 1024).(*counterInt64)

	// Handshake.
	counterA.TryUpdate(ctx, 1)
	require.Eventually(t, func() bool { return counterB.GetValue() == 1 }, 2*time.Second, 10*time.Millisecond)

	forceStale(counterA)
	forceStale(counterB)

	hold := make(chan struct{})
	fnA := func(ctx context.Context) (int64, error) {
		<-hold
		return 2, nil
	}
	var callsB atomic.Int32
	fnB := func(ctx context.Context) (int64, error) {
		callsB.Add(1)
		return 999, nil
	}

	beforeTimeout := metricValue(t, "contention_timeout")
	beforeContention := metricValue(t, "contention")

	callCtx, callCancel := context.WithTimeout(ctx, 5*time.Second)
	defer callCancel()

	// Start holder first to ensure poll lock is held.
	go func() {
		_, _ = counterA.TryUpdateIfStale(callCtx, 5*time.Second, fnA)
	}()

	// Wait until poll lock key exists in Redis (holder acquired).
	require.Eventually(t, func() bool {
		for _, k := range s.Keys() {
			if strings.Contains(k, "lock:test/poll-lock-timeout/poll") {
				return true
			}
		}
		return false
	}, 2*time.Second, 10*time.Millisecond, "expected poll lock key to exist")

	valB, errB := counterB.TryUpdateIfStale(callCtx, 5*time.Second, fnB)

	assert.NoError(t, errB)
	assert.Equal(t, int64(1), valB, "contender should return current value on contention timeout")
	assert.Equal(t, int32(0), callsB.Load(), "contender should not run refreshFn on contention timeout")

	assert.GreaterOrEqual(t, metricDelta(t, "contention", beforeContention), 1.0)
	assert.GreaterOrEqual(t, metricDelta(t, "contention_timeout", beforeTimeout), 1.0)

	close(hold)
}

func TestIntegrationPollLock_Unavailable_FallsBackToLocalRefresh(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := newMiniredis(t)
	defer s.Close()

	ssr := newRedisSSR(t, ctx, s.Addr(), "test-redis", 2*time.Second, 200*time.Millisecond, 500*time.Millisecond, 2*time.Second)
	reg := ssr.(*sharedStateRegistry)

	orig := reg.connector
	reg.connector = &lockOverrideConnector{
		inner: orig,
		lock: func(ctx context.Context, key string, ttl time.Duration) (DistributedLock, error) {
			if strings.HasSuffix(key, "/poll") {
				return nil, errors.New("boom")
			}
			return orig.Lock(ctx, key, ttl)
		},
	}

	counter := ssr.GetCounterInt64("poll-lock-unavailable", 1024).(*counterInt64)
	forceStale(counter)

	beforeUnavailable := metricValue(t, "unavailable")

	var calls atomic.Int32
	val, err := counter.TryUpdateIfStale(ctx, 5*time.Second, func(ctx context.Context) (int64, error) {
		calls.Add(1)
		return 7, nil
	})

	require.NoError(t, err)
	assert.Equal(t, int64(7), val)
	assert.Equal(t, int32(1), calls.Load())
	assert.GreaterOrEqual(t, metricDelta(t, "unavailable", beforeUnavailable), 1.0)
}

func TestIntegrationPollLock_SkippedFresh_AsyncUnlock_NoBlock(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := newMiniredis(t)
	defer s.Close()

	ssrA := newRedisSSR(t, ctx, s.Addr(), "test-redis-a", 2*time.Second, 200*time.Millisecond, 2*time.Second, 2*time.Second)
	ssrB := newRedisSSR(t, ctx, s.Addr(), "test-redis-b", 2*time.Second, 200*time.Millisecond, 2*time.Second, 2*time.Second)

	regB := ssrB.(*sharedStateRegistry)
	innerB := regB.connector

	lockEntered := make(chan struct{}, 1)
	allowLock := make(chan struct{})
	unlockCalled := make(chan struct{}, 1)
	allowUnlock := make(chan struct{})

	regB.connector = &lockOverrideConnector{
		inner: innerB,
		lock: func(ctx context.Context, key string, ttl time.Duration) (DistributedLock, error) {
			if strings.HasSuffix(key, "/poll") {
				select {
				case lockEntered <- struct{}{}:
				default:
				}
				<-allowLock
				l, err := innerB.Lock(ctx, key, ttl)
				if err != nil {
					return nil, err
				}
				return &blockingUnlockLock{
					inner:        l,
					unlockCalled: unlockCalled,
					allowUnlock:  allowUnlock,
				}, nil
			}
			return innerB.Lock(ctx, key, ttl)
		},
	}

	counterA := ssrA.GetCounterInt64("poll-lock-skipped-fresh", 1024).(*counterInt64)
	counterB := ssrB.GetCounterInt64("poll-lock-skipped-fresh", 1024).(*counterInt64)

	// Handshake.
	counterA.TryUpdate(ctx, 1)
	require.Eventually(t, func() bool { return counterB.GetValue() == 1 }, 2*time.Second, 10*time.Millisecond)

	forceStale(counterA)
	forceStale(counterB)

	beforeSkippedFresh := metricValue(t, "skipped_fresh")

	done := make(chan struct{})
	var (
		val int64
		err error
	)
	var refreshCalls atomic.Int32
	go func() {
		defer close(done)
		val, err = counterB.TryUpdateIfStale(ctx, 5*time.Second, func(ctx context.Context) (int64, error) {
			refreshCalls.Add(1)
			return 123, nil
		})
	}()

	// Ensure B is attempting poll lock.
	require.Eventually(t, func() bool {
		select {
		case <-lockEntered:
			return true
		default:
			return false
		}
	}, 2*time.Second, 10*time.Millisecond)

	// Publish fresh value while B is blocked inside Lock().
	counterA.TryUpdate(ctx, 2)
	require.Eventually(t, func() bool { return counterB.GetValue() == 2 }, 2*time.Second, 10*time.Millisecond)

	// Allow poll lock acquisition to proceed; it should immediately take skipped_fresh path.
	close(allowLock)

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("TryUpdateIfStale did not return promptly (likely blocked on unlock)")
	}

	require.NoError(t, err)
	assert.Equal(t, int64(2), val)
	assert.Equal(t, int32(0), refreshCalls.Load(), "skipped_fresh should not execute refreshFn")

	// Ensure unlock is attempted asynchronously (it should be called even while blocked).
	require.Eventually(t, func() bool {
		select {
		case <-unlockCalled:
			return true
		default:
			return false
		}
	}, 2*time.Second, 10*time.Millisecond)

	close(allowUnlock)
	assert.GreaterOrEqual(t, metricDelta(t, "skipped_fresh", beforeSkippedFresh), 1.0)
}
