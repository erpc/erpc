package common

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBatchUpstreamSelectionCache_ContextRoundTrip(t *testing.T) {
	cache := NewBatchUpstreamSelectionCache()
	ctx := WithBatchUpstreamSelectionCache(context.Background(), cache)

	require.NotNil(t, BatchUpstreamSelectionCacheFromContext(ctx))
	assert.Nil(t, BatchUpstreamSelectionCacheFromContext(context.Background()))
}

func TestBatchUpstreamSelectionCache_ResolveConcurrentSingleLoaderCall(t *testing.T) {
	cache := NewBatchUpstreamSelectionCache()
	key := BatchUpstreamSelectionKey{
		NetworkID: "evm:1",
		Method:    "eth_getBalance",
		Finality:  DataFinalityStateUnfinalized,
	}
	mockUpstream := &mockUpstreamForSelection{id: "upstream-a"}

	var loaderCalls atomic.Int32

	const workers = 16
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			ups, hit, err := cache.Resolve(key, func() ([]Upstream, error) {
				loaderCalls.Add(1)
				return []Upstream{mockUpstream}, nil
			})
			require.NoError(t, err)
			require.Len(t, ups, 1)
			assert.Equal(t, "upstream-a", ups[0].Id())
			_ = hit
		}()
	}
	wg.Wait()

	assert.Equal(t, int32(1), loaderCalls.Load())
}

func TestBatchUpstreamSelectionCache_ResolveLoaderPanicReturnsErrorAndUnblocksWaiters(t *testing.T) {
	cache := NewBatchUpstreamSelectionCache()
	key := BatchUpstreamSelectionKey{
		NetworkID: "evm:1",
		Method:    "eth_getBalance",
		Finality:  DataFinalityStateUnfinalized,
	}

	errCh := make(chan error, 2)
	started := make(chan struct{})
	release := make(chan struct{})
	var secondLoaderCalled atomic.Bool
	secondUpstream := &mockUpstreamForSelection{id: "upstream-second"}

	go func() {
		_, _, err := cache.Resolve(key, func() ([]Upstream, error) {
			close(started)
			<-release
			panic("boom")
		})
		errCh <- err
	}()

	<-started
	go func() {
		_, _, err := cache.Resolve(key, func() ([]Upstream, error) {
			secondLoaderCalled.Store(true)
			return []Upstream{secondUpstream}, nil
		})
		errCh <- err
	}()

	close(release)

	panicErrs := 0
	nilErrs := 0
	for i := 0; i < 2; i++ {
		select {
		case err := <-errCh:
			if err == nil {
				nilErrs++
				continue
			}
			assert.True(t, strings.Contains(err.Error(), "loader panic"))
			panicErrs++
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for concurrent resolvers to finish")
		}
	}

	assert.GreaterOrEqual(t, panicErrs, 1)
	if secondLoaderCalled.Load() {
		assert.Equal(t, 1, nilErrs, "when second loader executes, one resolver should succeed")
	} else {
		assert.Equal(t, 0, nilErrs, "when second resolver waits on inflight, both should observe panic error")
	}

	thirdUpstream := &mockUpstreamForSelection{id: "upstream-third"}
	ups, _, err := cache.Resolve(key, func() ([]Upstream, error) {
		return []Upstream{thirdUpstream}, nil
	})
	require.NoError(t, err)
	require.Len(t, ups, 1)
	if secondLoaderCalled.Load() {
		assert.Equal(t, "upstream-second", ups[0].Id())
	} else {
		assert.Equal(t, "upstream-third", ups[0].Id())
	}
}
