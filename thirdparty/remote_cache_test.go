package thirdparty

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func cacheIdle[T any](c *RemoteDataCache[T], key string) bool {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	_, busy := c.inflight[key]
	return !busy
}

func TestRemoteDataCache_LookupAndHas(t *testing.T) {
	c := NewRemoteDataCache[int]("test")

	val, fresh := c.Lookup("a", time.Hour)
	assert.Equal(t, 0, val)
	assert.False(t, fresh)
	assert.False(t, c.Has("a"))

	primeRemoteCache(t, c, "a", 7)
	assert.True(t, c.Has("a"))
	assert.False(t, c.Has("b"), "other keys stay absent once a snapshot exists")

	val, fresh = c.Lookup("b", time.Hour)
	assert.Equal(t, 0, val)
	assert.False(t, fresh)

	val, fresh = c.Lookup("a", time.Nanosecond)
	assert.Equal(t, 7, val)
	assert.False(t, fresh, "an entry older than the recheck interval is stale")
}

func TestRemoteDataCache_PublishKeepsOtherKeys(t *testing.T) {
	c := NewRemoteDataCache[string]("test")
	primeRemoteCache(t, c, "a", "one")
	require.Eventually(t, func() bool { return cacheIdle(c, "a") }, 5*time.Second, 10*time.Millisecond)
	primeRemoteCache(t, c, "b", "two")

	va, _ := c.Lookup("a", time.Hour)
	vb, _ := c.Lookup("b", time.Hour)
	assert.Equal(t, "one", va)
	assert.Equal(t, "two", vb)
}

func TestRemoteDataCache_EnsureFresh(t *testing.T) {
	logger := zerolog.Nop()

	t.Run("cold returns zero and triggers refresh", func(t *testing.T) {
		c := NewRemoteDataCache[int]("test")
		var calls atomic.Int32
		val, ok := c.EnsureFresh(&logger, "k", time.Hour, func(ctx context.Context) (int, error) {
			calls.Add(1)
			return 42, nil
		})
		assert.Equal(t, 0, val)
		assert.False(t, ok)
		require.Eventually(t, func() bool {
			v, fresh := c.Lookup("k", time.Hour)
			return fresh && v == 42
		}, 5*time.Second, 10*time.Millisecond)
		assert.Equal(t, int32(1), calls.Load())
	})

	t.Run("fresh value returned without refresh", func(t *testing.T) {
		c := NewRemoteDataCache[int]("test")
		primeRemoteCache(t, c, "k", 5)
		require.Eventually(t, func() bool { return cacheIdle(c, "k") }, 5*time.Second, 10*time.Millisecond)
		val, ok := c.EnsureFresh(&logger, "k", time.Hour, func(ctx context.Context) (int, error) {
			t.Error("fetcher must not run for a fresh value")
			return 0, nil
		})
		assert.Equal(t, 5, val)
		assert.True(t, ok)
		assert.True(t, cacheIdle(c, "k"), "no refresh is started for a fresh value")
	})

	t.Run("stale value returned and refreshed", func(t *testing.T) {
		c := NewRemoteDataCache[int]("test")
		primeRemoteCache(t, c, "k", 5)
		require.Eventually(t, func() bool { return cacheIdle(c, "k") }, 5*time.Second, 10*time.Millisecond)
		val, ok := c.EnsureFresh(&logger, "k", time.Nanosecond, func(ctx context.Context) (int, error) {
			return 6, nil
		})
		assert.Equal(t, 5, val)
		assert.True(t, ok)
		require.Eventually(t, func() bool {
			v, _ := c.Lookup("k", time.Hour)
			return v == 6
		}, 5*time.Second, 10*time.Millisecond)
	})
}

func TestRemoteDataCache_FetcherErrorKeepsSnapshot(t *testing.T) {
	logger := zerolog.Nop()
	c := NewRemoteDataCache[int]("test")
	primeRemoteCache(t, c, "k", 1)
	require.Eventually(t, func() bool { return cacheIdle(c, "k") }, 5*time.Second, 10*time.Millisecond)

	done := make(chan struct{})
	c.TriggerAsyncRefresh(&logger, "k", func(ctx context.Context) (int, error) {
		defer close(done)
		return 99, errors.New("upstream down")
	})
	<-done
	require.Eventually(t, func() bool { return cacheIdle(c, "k") }, 5*time.Second, 10*time.Millisecond)

	val, fresh := c.Lookup("k", time.Hour)
	assert.Equal(t, 1, val)
	assert.True(t, fresh, "failed refresh does not touch fetchedAt either")
}

func TestRemoteDataCache_SingleFlight(t *testing.T) {
	logger := zerolog.Nop()
	c := NewRemoteDataCache[int]("test")

	release := make(chan struct{})
	started := make(chan struct{})
	var calls atomic.Int32
	c.TriggerAsyncRefresh(&logger, "k", func(ctx context.Context) (int, error) {
		calls.Add(1)
		close(started)
		<-release
		return 1, nil
	})
	<-started

	c.TriggerAsyncRefresh(&logger, "k", func(ctx context.Context) (int, error) {
		calls.Add(1)
		return 2, nil
	})
	assert.False(t, cacheIdle(c, "k"))

	close(release)
	require.Eventually(t, func() bool {
		v, fresh := c.Lookup("k", time.Hour)
		return fresh && v == 1 && cacheIdle(c, "k")
	}, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, int32(1), calls.Load(), "second trigger while the first is in flight is a no-op")

	// Once idle, a new trigger runs again.
	primeRemoteCache(t, c, "k", 3)
	v, _ := c.Lookup("k", time.Hour)
	assert.Equal(t, 3, v)
}

func TestRemoteDataCache_PanicRecovered(t *testing.T) {
	logger := zerolog.Nop()
	c := NewRemoteDataCache[int]("test")

	c.TriggerAsyncRefresh(&logger, "k", func(ctx context.Context) (int, error) {
		panic("fetcher exploded")
	})
	require.Eventually(t, func() bool { return cacheIdle(c, "k") }, 5*time.Second, 10*time.Millisecond)
	assert.False(t, c.Has("k"))

	// The in-flight marker was cleared, so a later refresh still publishes.
	primeRemoteCache(t, c, "k", 8)
	v, fresh := c.Lookup("k", time.Hour)
	assert.Equal(t, 8, v)
	assert.True(t, fresh)
}
