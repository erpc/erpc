package erpc

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/telemetry"
	promUtil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// fanOutPolicies wires both mock connectors as cache policies for the same
// network+method so the cache layer fans out across them.
func fanOutPolicies(t *testing.T, conns []*data.MockConnector) []*data.CachePolicy {
	t.Helper()
	policies := make([]*data.CachePolicy, 0, len(conns))
	for _, c := range conns {
		p, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Network:   "evm:123",
			Method:    "eth_getBlockByNumber",
			Connector: c.Id(),
		}, c)
		require.NoError(t, err)
		policies = append(policies, p)
	}
	return policies
}

func newGetBlockByNumberRequest(t *testing.T, network *Network, cache common.CacheDAL) *common.NormalizedRequest {
	t.Helper()
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["0x1",false],"id":1}`))
	req.SetNetwork(network)
	req.SetCacheDal(cache)
	return req
}

func TestEvmJsonRpcCache_FanOut_FirstHitWins(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conns, network, _, cache := createCacheTestFixtures(ctx, []upsTestCfg{
		{id: "upsA", syncing: common.EvmSyncingStateUnknown, finBn: 10, lstBn: 15},
	})
	cache.SetPolicies(fanOutPolicies(t, conns))

	expected := `{"number":"0x1","hash":"0xfromA"}`
	conns[0].On("Get", mock.Anything, mock.Anything, "evm:123:1", mock.Anything, mock.Anything).
		Return([]byte(expected), nil)
	conns[1].On("Get", mock.Anything, mock.Anything, "evm:123:1", mock.Anything, mock.Anything).
		Return([]byte(`{"number":"0x1","hash":"0xfromB"}`), nil).Maybe()

	resp, err := cache.Get(context.Background(), newGetBlockByNumberRequest(t, network, cache))

	require.NoError(t, err)
	require.NotNil(t, resp)
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	// We don't assert which connector wins — only that we got one of the hits.
	got := jrr.GetResultString()
	require.Truef(t,
		got == expected || got == `{"number":"0x1","hash":"0xfromB"}`,
		"unexpected response: %s", got,
	)
	require.True(t, resp.FromCache())
}

func TestEvmJsonRpcCache_FanOut_AllMissReturnsNil(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conns, network, _, cache := createCacheTestFixtures(ctx, []upsTestCfg{
		{id: "upsA", syncing: common.EvmSyncingStateUnknown, finBn: 10, lstBn: 15},
	})
	cache.SetPolicies(fanOutPolicies(t, conns))

	notFound := common.NewErrRecordNotFound("evm:123:1", "k", "mock")
	conns[0].On("Get", mock.Anything, mock.Anything, "evm:123:1", mock.Anything, mock.Anything).
		Return(nil, notFound)
	conns[1].On("Get", mock.Anything, mock.Anything, "evm:123:1", mock.Anything, mock.Anything).
		Return(nil, notFound)

	resp, err := cache.Get(context.Background(), newGetBlockByNumberRequest(t, network, cache))

	require.NoError(t, err)
	require.Nil(t, resp, "all-miss should fall through to upstream layer")
	conns[0].AssertCalled(t, "Get", mock.Anything, mock.Anything, "evm:123:1", mock.Anything, mock.Anything)
	conns[1].AssertCalled(t, "Get", mock.Anything, mock.Anything, "evm:123:1", mock.Anything, mock.Anything)
}

func TestEvmJsonRpcCache_FanOut_OneMissOneHit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conns, network, _, cache := createCacheTestFixtures(ctx, []upsTestCfg{
		{id: "upsA", syncing: common.EvmSyncingStateUnknown, finBn: 10, lstBn: 15},
	})
	cache.SetPolicies(fanOutPolicies(t, conns))

	notFound := common.NewErrRecordNotFound("evm:123:1", "k", "mock1")
	conns[0].On("Get", mock.Anything, mock.Anything, "evm:123:1", mock.Anything, mock.Anything).
		Return(nil, notFound)

	cached := `{"number":"0x1","hash":"0xabc"}`
	conns[1].On("Get", mock.Anything, mock.Anything, "evm:123:1", mock.Anything, mock.Anything).
		Return([]byte(cached), nil)

	resp, err := cache.Get(context.Background(), newGetBlockByNumberRequest(t, network, cache))

	require.NoError(t, err)
	require.NotNil(t, resp)
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	assert.Equal(t, cached, jrr.GetResultString())
}

func TestEvmJsonRpcCache_FanOut_OneErrorOneHit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conns, network, _, cache := createCacheTestFixtures(ctx, []upsTestCfg{
		{id: "upsA", syncing: common.EvmSyncingStateUnknown, finBn: 10, lstBn: 15},
	})
	cache.SetPolicies(fanOutPolicies(t, conns))

	conns[0].On("Get", mock.Anything, mock.Anything, "evm:123:1", mock.Anything, mock.Anything).
		Return(nil, errors.New("connection refused"))

	cached := `{"number":"0x1","hash":"0xabc"}`
	conns[1].On("Get", mock.Anything, mock.Anything, "evm:123:1", mock.Anything, mock.Anything).
		Return([]byte(cached), nil)

	resp, err := cache.Get(context.Background(), newGetBlockByNumberRequest(t, network, cache))

	require.NoError(t, err)
	require.NotNil(t, resp, "errors on one connector must not block hits from peers")
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	assert.Equal(t, cached, jrr.GetResultString())
}

func TestEvmJsonRpcCache_FanOut_AllErrorsFallThrough(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conns, network, _, cache := createCacheTestFixtures(ctx, []upsTestCfg{
		{id: "upsA", syncing: common.EvmSyncingStateUnknown, finBn: 10, lstBn: 15},
	})
	cache.SetPolicies(fanOutPolicies(t, conns))

	conns[0].On("Get", mock.Anything, mock.Anything, "evm:123:1", mock.Anything, mock.Anything).
		Return(nil, errors.New("connection refused"))
	conns[1].On("Get", mock.Anything, mock.Anything, "evm:123:1", mock.Anything, mock.Anything).
		Return(nil, errors.New("i/o timeout"))

	resp, err := cache.Get(context.Background(), newGetBlockByNumberRequest(t, network, cache))

	require.NoError(t, err, "Get must not surface connector errors — caller falls through to upstream")
	require.Nil(t, resp)
}

func TestEvmJsonRpcCache_FanOut_RunsConcurrently(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conns, network, _, cache := createCacheTestFixtures(ctx, []upsTestCfg{
		{id: "upsA", syncing: common.EvmSyncingStateUnknown, finBn: 10, lstBn: 15},
	})
	cache.SetPolicies(fanOutPolicies(t, conns))

	// Both connectors take 100ms; if run sequentially the call would take ≥200ms.
	// Parallel fan-out keeps it under ~150ms.
	const delay = 100 * time.Millisecond
	for _, c := range conns {
		c.On("Get", mock.Anything, mock.Anything, "evm:123:1", mock.Anything, mock.Anything).
			After(delay).
			Return(nil, common.NewErrRecordNotFound("evm:123:1", "k", c.Id()))
	}

	t0 := time.Now()
	resp, err := cache.Get(context.Background(), newGetBlockByNumberRequest(t, network, cache))
	elapsed := time.Since(t0)

	require.NoError(t, err)
	require.Nil(t, resp)
	assert.Less(t, elapsed, 180*time.Millisecond,
		"fan-out should run connectors concurrently (took %v)", elapsed)
}

// FirstHitCancelsPeer verifies that once one connector returns a hit, peer
// connectors observe context cancellation. The slow connector here would take
// 1s if not cancelled — the assertion is that the call returns much earlier.
func TestEvmJsonRpcCache_FanOut_FirstHitCancelsPeer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conns, network, _, cache := createCacheTestFixtures(ctx, []upsTestCfg{
		{id: "upsA", syncing: common.EvmSyncingStateUnknown, finBn: 10, lstBn: 15},
	})
	cache.SetPolicies(fanOutPolicies(t, conns))

	cached := `{"number":"0x1","hash":"0xfast"}`
	conns[0].On("Get", mock.Anything, mock.Anything, "evm:123:1", mock.Anything, mock.Anything).
		Return([]byte(cached), nil) // immediate hit

	var slowSawCancel atomic.Bool
	conns[1].On("Get", mock.Anything, mock.Anything, "evm:123:1", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			callCtx := args.Get(0).(context.Context)
			select {
			case <-callCtx.Done():
				slowSawCancel.Store(true)
			case <-time.After(time.Second):
			}
		}).
		Return(nil, common.NewErrRecordNotFound("evm:123:1", "k", "mock2")).Maybe()

	t0 := time.Now()
	resp, err := cache.Get(context.Background(), newGetBlockByNumberRequest(t, network, cache))
	elapsed := time.Since(t0)

	require.NoError(t, err)
	require.NotNil(t, resp)
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	assert.Equal(t, cached, jrr.GetResultString())
	assert.Less(t, elapsed, 500*time.Millisecond,
		"slow peer should be cancelled once fast peer wins (took %v)", elapsed)
	// Allow up to 200ms for the slow goroutine to observe cancellation.
	assert.Eventually(t, slowSawCancel.Load, 200*time.Millisecond, 5*time.Millisecond,
		"slow peer goroutine should observe context cancellation after first hit wins")
}

func TestEvmJsonRpcCache_FanOut_ParentContextCancellationStopsAll(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conns, network, _, cache := createCacheTestFixtures(ctx, []upsTestCfg{
		{id: "upsA", syncing: common.EvmSyncingStateUnknown, finBn: 10, lstBn: 15},
	})
	cache.SetPolicies(fanOutPolicies(t, conns))

	for _, c := range conns {
		c.On("Get", mock.Anything, mock.Anything, "evm:123:1", mock.Anything, mock.Anything).
			Run(func(args mock.Arguments) {
				callCtx := args.Get(0).(context.Context)
				<-callCtx.Done()
			}).
			Return(nil, context.Canceled).Maybe()
	}

	getCtx, cancelGet := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancelGet()
	}()
	t0 := time.Now()
	_, _ = cache.Get(getCtx, newGetBlockByNumberRequest(t, network, cache))
	elapsed := time.Since(t0)
	assert.Less(t, elapsed, 500*time.Millisecond,
		"caller-cancelled context should unwind all goroutines promptly (took %v)", elapsed)
}

// ParentContextDeadlineStopsAll covers the deadline path: the parent context
// expires (rather than being explicitly cancelled), which propagates to peer
// connectors as context.DeadlineExceeded — distinct from context.Canceled.
// The cancellation guard must treat both alike, otherwise spurious connector
// error metrics get emitted and the goroutine takes the noisy error path.
func TestEvmJsonRpcCache_FanOut_ParentContextDeadlineStopsAll(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conns, network, _, cache := createCacheTestFixtures(ctx, []upsTestCfg{
		{id: "upsA", syncing: common.EvmSyncingStateUnknown, finBn: 10, lstBn: 15},
	})
	cache.SetPolicies(fanOutPolicies(t, conns))

	for _, c := range conns {
		c.On("Get", mock.Anything, mock.Anything, "evm:123:1", mock.Anything, mock.Anything).
			Run(func(args mock.Arguments) {
				callCtx := args.Get(0).(context.Context)
				<-callCtx.Done()
			}).
			Return(nil, context.DeadlineExceeded).Maybe()
	}

	beforeErr := promUtil.CollectAndCount(telemetry.MetricCacheGetErrorTotal)

	getCtx, cancelGet := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelGet()
	t0 := time.Now()
	_, _ = cache.Get(getCtx, newGetBlockByNumberRequest(t, network, cache))
	elapsed := time.Since(t0)

	// Allow goroutines a brief moment to drain any (incorrect) metric emission.
	time.Sleep(50 * time.Millisecond)
	afterErr := promUtil.CollectAndCount(telemetry.MetricCacheGetErrorTotal)

	assert.Less(t, elapsed, 500*time.Millisecond,
		"deadline-expired parent context should unwind all goroutines promptly (took %v)", elapsed)
	assert.Equal(t, beforeErr, afterErr,
		"deadline-cancelled connector calls must not emit cache_get_error_total — they are external cancellations, not connector failures")
}

func TestEvmJsonRpcCache_FanOut_RespectsSkipCacheReadDirective(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conns, network, _, cache := createCacheTestFixtures(ctx, []upsTestCfg{
		{id: "upsA", syncing: common.EvmSyncingStateUnknown, finBn: 10, lstBn: 15},
	})
	cache.SetPolicies(fanOutPolicies(t, conns))

	cached := `{"number":"0x1","hash":"0xfromB"}`
	conns[1].On("Get", mock.Anything, mock.Anything, "evm:123:1", mock.Anything, mock.Anything).
		Return([]byte(cached), nil)

	req := newGetBlockByNumberRequest(t, network, cache)
	req.SetDirectives(&common.RequestDirectives{SkipCacheRead: conns[0].Id()})

	resp, err := cache.Get(context.Background(), req)

	require.NoError(t, err)
	require.NotNil(t, resp)
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	assert.Equal(t, cached, jrr.GetResultString())
	conns[0].AssertNotCalled(t, "Get",
		mock.Anything, mock.Anything, "evm:123:1", mock.Anything, mock.Anything)
}

// MissAsErrorIsNotEmittedAsConnectorError is the regression for the
// shadow-deployment finding: cache connectors return ErrRecordNotFound /
// ErrEndpointMissingData to signal "this connector doesn't have the key" —
// semantic misses, not transport failures. Before the fix the fan-out
// goroutine classified them as connector_error and incremented
// MetricCacheGetErrorTotal on every cache miss, polluting dashboards
// (shadow logged 36k+ "errors" per 15min that were just normal misses).
func TestEvmJsonRpcCache_FanOut_MissAsErrorIsClassifiedAsMiss(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conns, network, _, cache := createCacheTestFixtures(ctx, []upsTestCfg{
		{id: "upsA", syncing: common.EvmSyncingStateUnknown, finBn: 10, lstBn: 15},
	})
	cache.SetPolicies(fanOutPolicies(t, conns))

	for _, c := range conns {
		c.On("Get", mock.Anything, mock.Anything, "evm:123:1", mock.Anything, mock.Anything).
			Return(nil, common.NewErrRecordNotFound("evm:123:1", "k", c.Id()))
	}

	beforeErr := promUtil.CollectAndCount(telemetry.MetricCacheGetErrorTotal)

	resp, err := cache.Get(context.Background(), newGetBlockByNumberRequest(t, network, cache))

	require.NoError(t, err)
	require.Nil(t, resp, "all connectors returning ErrRecordNotFound is a miss, not an error")

	afterErr := promUtil.CollectAndCount(telemetry.MetricCacheGetErrorTotal)
	assert.Equal(t, beforeErr, afterErr,
		"ErrRecordNotFound from connectors is a semantic miss — must not increment cache_get_error_total")
}

// WrappedCancellationDoesNotInflateErrorMetric is the regression for the
// inner-failsafe wrapping issue: when a losing peer's `doGet` returns
// through an inner failsafe stack, the underlying context error can be
// wrapped in a typed error that `errors.Is(err, context.Canceled)` fails
// to unwind. The cancellation guard must trust `fanCtx.Err() != nil` as
// the authoritative signal, not the error type, so wrapped cancellation
// from a losing peer does not increment cache_get_error_total.
func TestEvmJsonRpcCache_FanOut_WrappedCancellationDoesNotInflateError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conns, network, _, cache := createCacheTestFixtures(ctx, []upsTestCfg{
		{id: "upsA", syncing: common.EvmSyncingStateUnknown, finBn: 10, lstBn: 15},
	})
	cache.SetPolicies(fanOutPolicies(t, conns))

	cached := `{"number":"0x1","hash":"0xfast"}`
	conns[0].On("Get", mock.Anything, mock.Anything, "evm:123:1", mock.Anything, mock.Anything).
		Return([]byte(cached), nil) // immediate hit

	// Loser observes cancellation but surfaces a typed, opaque error that
	// does NOT unwrap to context.Canceled — simulates an inner failsafe
	// wrapper. Pre-fix guard (errors.Is on context.Canceled) would miss
	// this and increment connector_error metric for every fan-out loser.
	conns[1].On("Get", mock.Anything, mock.Anything, "evm:123:1", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			callCtx := args.Get(0).(context.Context)
			<-callCtx.Done()
		}).
		Return(nil, errors.New("opaque-inner-failsafe-wrap")).Maybe()

	beforeErr := promUtil.CollectAndCount(telemetry.MetricCacheGetErrorTotal)

	resp, err := cache.Get(context.Background(), newGetBlockByNumberRequest(t, network, cache))

	require.NoError(t, err)
	require.NotNil(t, resp)
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	assert.Equal(t, cached, jrr.GetResultString())

	// Allow the cancelled peer goroutine a moment to drain.
	time.Sleep(50 * time.Millisecond)
	afterErr := promUtil.CollectAndCount(telemetry.MetricCacheGetErrorTotal)
	assert.Equal(t, beforeErr, afterErr,
		"wrapped cancellation from a losing peer must not increment cache_get_error_total")
}

// SlowPeerDoesNotBlockFastWinner is the regression for the consumer-drain
// bug: previously `for r := range results` waited for the channel to close,
// which only happened after wg.Wait — so any peer slow to observe
// cancellation (buffered TCP write, inner failsafe state, misbehaving
// stack) pinned the user-visible latency to MAX(connector latency). The
// fix breaks out of the drain loop on first acceptable hit; stragglers
// drain into the buffered channel on their own.
func TestEvmJsonRpcCache_FanOut_SlowPeerDoesNotBlockFastWinner(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conns, network, _, cache := createCacheTestFixtures(ctx, []upsTestCfg{
		{id: "upsA", syncing: common.EvmSyncingStateUnknown, finBn: 10, lstBn: 15},
	})
	cache.SetPolicies(fanOutPolicies(t, conns))

	cached := `{"number":"0x1","hash":"0xfast"}`
	conns[0].On("Get", mock.Anything, mock.Anything, "evm:123:1", mock.Anything, mock.Anything).
		Return([]byte(cached), nil) // immediate hit

	// Peer that intentionally ignores ctx.Done — simulates a connector with
	// buffered work or a stack that doesn't honor cancellation promptly. If
	// the consumer waited for this peer, Get() would block for the full
	// slowDelay before returning.
	const slowDelay = 800 * time.Millisecond
	conns[1].On("Get", mock.Anything, mock.Anything, "evm:123:1", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			time.Sleep(slowDelay)
		}).
		Return(nil, common.NewErrRecordNotFound("evm:123:1", "k", "slow")).Maybe()

	t0 := time.Now()
	resp, err := cache.Get(context.Background(), newGetBlockByNumberRequest(t, network, cache))
	elapsed := time.Since(t0)

	require.NoError(t, err)
	require.NotNil(t, resp)
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	assert.Equal(t, cached, jrr.GetResultString())
	assert.Less(t, elapsed, 200*time.Millisecond,
		"fast hit should not wait for unresponsive peer (took %v); slow peer would have taken %v",
		elapsed, slowDelay)
}
