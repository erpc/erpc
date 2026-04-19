package data

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

func init() {
	util.ConfigureTestLogger()
}

// Helper to build a registry with mock connector and custom time budgets
func setupMockRegistry(t *testing.T, cfg *common.SharedStateConfig) (*sharedStateRegistry, *MockConnector) {
	t.Helper()
	connector := &MockConnector{}
	r := &sharedStateRegistry{
		appCtx:          context.Background(),
		logger:          &log.Logger,
		clusterKey:      cfg.ClusterKey,
		connector:       connector,
		fallbackTimeout: cfg.FallbackTimeout.Duration(),
		lockTtl:         cfg.LockTtl.Duration(),
		lockMaxWait:     cfg.LockMaxWait.Duration(),
		updateMaxWait:   cfg.UpdateMaxWait.Duration(),
		initializer:     util.NewInitializer(context.Background(), &log.Logger, nil),
	}
	return r, connector
}

func TestTryUpdate_LocalThenBackgroundPush(t *testing.T) {
	cfg := &common.SharedStateConfig{
		ClusterKey:      "test",
		FallbackTimeout: common.Duration(300 * time.Millisecond),
		LockTtl:         common.Duration(1 * time.Second),
		LockMaxWait:     common.Duration(50 * time.Millisecond),
		UpdateMaxWait:   common.Duration(50 * time.Millisecond),
	}
	r, c := setupMockRegistry(t, cfg)

	// Background reconcile acquires lock, reads remote (not found), sets and publishes current value 10
	lock := &MockLock{}
	lock.On("Unlock", mock.Anything).Return(nil).Once()
	c.On("Lock", mock.Anything, "test/my", mock.Anything).Return(lock, nil).Once()
	c.On("Get", mock.Anything, ConnectorMainIndex, "test/my", "value", nil).Return([]byte(""), errors.New("not found")).Once()
	c.On("Set", mock.Anything, "test/my", "value", mock.Anything, mock.Anything).Return(nil).Once()
	// publish can happen multiple times (best-effort publish + publish-after-Set)
	c.On("PublishCounterInt64", mock.Anything, "test/my", mock.Anything).Return(nil).Maybe()

	ctr := &counterInt64{registry: r, key: "test/my", ignoreRollbackOf: 1024}
	val := ctr.TryUpdate(context.Background(), 10)
	assert.Equal(t, int64(10), val)

	// Give background time to run
	time.Sleep(150 * time.Millisecond)
	c.AssertExpectations(t)
}

func TestTryUpdateIfStale_UpdateFnBudgetExceeded(t *testing.T) {
	cfg := &common.SharedStateConfig{
		ClusterKey:      "test",
		FallbackTimeout: common.Duration(300 * time.Millisecond),
		LockTtl:         common.Duration(1 * time.Second),
		LockMaxWait:     common.Duration(50 * time.Millisecond),
		UpdateMaxWait:   common.Duration(50 * time.Millisecond),
	}
	r, c := setupMockRegistry(t, cfg)

	// Background reconcile will acquire the lock once and push the final value 999
	lock := &MockLock{}
	lock.On("Unlock", mock.Anything).Return(nil).Once()
	c.On("Lock", mock.Anything, "test/stale", mock.Anything).Return(lock, nil).Once()
	c.On("Get", mock.Anything, ConnectorMainIndex, "test/stale", "value", nil).Return([]byte(""), errors.New("not found")).Once()
	c.On("Set", mock.Anything, "test/stale", "value", mock.Anything, mock.Anything).Return(nil).Once()
	// publish can happen multiple times (best-effort publish + publish-after-Set)
	c.On("PublishCounterInt64", mock.Anything, "test/stale", mock.Anything).Return(nil).Maybe()

	ctr := &counterInt64{registry: r, key: "test/stale", ignoreRollbackOf: 1024}
	ctr.value.Store(1)

	start := time.Now()
	val, err := ctr.TryUpdateIfStale(context.Background(), 1*time.Millisecond, func(ctx context.Context) (int64, error) {
		select {
		case <-time.After(200 * time.Millisecond):
			return 999, nil
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	})
	elapsed := time.Since(start)

	// Foreground returns stale value quickly; background will continue and push
	assert.NoError(t, err)
	assert.Equal(t, int64(1), val)
	assert.Less(t, elapsed, 150*time.Millisecond, "foreground should be bounded by updateMaxWait")
	time.Sleep(250 * time.Millisecond)
	assert.Equal(t, int64(999), ctr.GetValue())
	c.AssertExpectations(t)
}

func TestBackgroundReconcileUsesMax(t *testing.T) {
	cfg := &common.SharedStateConfig{
		ClusterKey:      "test",
		FallbackTimeout: common.Duration(300 * time.Millisecond),
		LockTtl:         common.Duration(1 * time.Second),
		LockMaxWait:     common.Duration(30 * time.Millisecond),
		UpdateMaxWait:   common.Duration(30 * time.Millisecond),
	}
	r, c := setupMockRegistry(t, cfg)

	// Background: acquire lock, remote higher 15, then push 15
	lock := &MockLock{}
	lock.On("Unlock", mock.Anything).Return(nil).Once()
	c.On("Lock", mock.Anything, "test/recon", mock.Anything).Return(lock, nil).Once()
	c.On("Get", mock.Anything, ConnectorMainIndex, "test/recon", "value", nil).
		Return([]byte(`{"v":15,"t":9999999999999,"b":"test"}`), nil).Once()
	// best-effort publish local snapshot
	c.On("PublishCounterInt64", mock.Anything, "test/recon", mock.Anything).Return(nil).Maybe()

	ctr := &counterInt64{registry: r, key: "test/recon", ignoreRollbackOf: 1024}
	ctr.value.Store(5)
	val := ctr.TryUpdate(context.Background(), 10)
	assert.Equal(t, int64(10), val)

	assert.Eventually(t, func() bool { return ctr.GetValue() == int64(15) }, 500*time.Millisecond, 10*time.Millisecond)
	c.AssertExpectations(t)
}

func TestTryUpdate_RemoteAhead_IsAdoptedInBackground(t *testing.T) {
	cfg := &common.SharedStateConfig{
		ClusterKey:      "test",
		FallbackTimeout: common.Duration(300 * time.Millisecond),
		LockTtl:         common.Duration(1 * time.Second),
		LockMaxWait:     common.Duration(100 * time.Millisecond),
		UpdateMaxWait:   common.Duration(50 * time.Millisecond),
	}
	r, c := setupMockRegistry(t, cfg)

	lock := &MockLock{}
	lock.On("Unlock", mock.Anything).Return(nil).Once()
	c.On("Lock", mock.Anything, "test/fg", mock.Anything).Return(lock, nil).Once()
	// remote ahead at 20
	c.On("Get", mock.Anything, ConnectorMainIndex, "test/fg", "value", nil).
		Return([]byte(`{"v":20,"t":9999999999999,"b":"test"}`), nil).Once()
	// best-effort publish local snapshot
	c.On("PublishCounterInt64", mock.Anything, "test/fg", mock.Anything).Return(nil).Maybe()

	ctr := &counterInt64{registry: r, key: "test/fg", ignoreRollbackOf: 1024}
	ctr.value.Store(5)
	val := ctr.TryUpdate(context.Background(), 10)
	assert.Equal(t, int64(10), val)
	assert.Eventually(t, func() bool { return ctr.GetValue() == int64(20) }, 500*time.Millisecond, 10*time.Millisecond)
	c.AssertExpectations(t)
}

func TestTryUpdateIfStale_SlowFn_BackgroundPush(t *testing.T) {
	cfg := &common.SharedStateConfig{
		ClusterKey:      "test",
		FallbackTimeout: common.Duration(300 * time.Millisecond),
		LockTtl:         common.Duration(1 * time.Second),
		LockMaxWait:     common.Duration(40 * time.Millisecond),
		UpdateMaxWait:   common.Duration(40 * time.Millisecond),
	}
	r, c := setupMockRegistry(t, cfg)

	// Background will acquire lock and push resulting value 77
	lock := &MockLock{}
	lock.On("Unlock", mock.Anything).Return(nil).Once()
	c.On("Lock", mock.Anything, "test/slow", mock.Anything).Return(lock, nil).Once()
	c.On("Get", mock.Anything, ConnectorMainIndex, "test/slow", "value", nil).Return([]byte(""), errors.New("not found")).Once()
	c.On("Set", mock.Anything, "test/slow", "value", mock.Anything, mock.Anything).Return(nil).Once()
	// publish can happen multiple times (best-effort publish + publish-after-Set)
	c.On("PublishCounterInt64", mock.Anything, "test/slow", mock.Anything).Return(nil).Maybe()

	ctr := &counterInt64{registry: r, key: "test/slow", ignoreRollbackOf: 1024}
	ctr.value.Store(1)

	start := time.Now()
	val, err := ctr.TryUpdateIfStale(context.Background(), 1*time.Millisecond, func(ctx context.Context) (int64, error) {
		// Runs longer than UpdateMaxWait; should continue and update in background
		time.Sleep(120 * time.Millisecond)
		return 77, nil
	})
	elapsed := time.Since(start)

	assert.NoError(t, err)
	assert.Equal(t, int64(1), val, "foreground should return stale value")
	assert.Less(t, elapsed, 120*time.Millisecond, "foreground should not wait full fn time")

	time.Sleep(200 * time.Millisecond)
	assert.Equal(t, int64(77), ctr.GetValue())
	c.AssertExpectations(t)
}

func TestBackgroundPushIsDeduped(t *testing.T) {
	cfg := &common.SharedStateConfig{
		ClusterKey:      "test",
		FallbackTimeout: common.Duration(300 * time.Millisecond),
		LockTtl:         common.Duration(1 * time.Second),
		LockMaxWait:     common.Duration(30 * time.Millisecond),
		UpdateMaxWait:   common.Duration(30 * time.Millisecond),
	}
	r, c := setupMockRegistry(t, cfg)

	// Only one background reconcile should run (deduped): acquire lock once and push once
	lock := &MockLock{}
	lock.On("Unlock", mock.Anything).Return(nil).Once()
	c.On("Lock", mock.Anything, "test/dedupe", mock.Anything).Return(lock, nil).Once()
	c.On("Get", mock.Anything, ConnectorMainIndex, "test/dedupe", "value", nil).Return([]byte(""), errors.New("not found")).Once()
	c.On("Set", mock.Anything, "test/dedupe", "value", mock.Anything, mock.Anything).Return(nil).Once()
	// publish can happen multiple times (best-effort publish + publish-after-Set)
	c.On("PublishCounterInt64", mock.Anything, "test/dedupe", mock.Anything).Return(nil).Maybe()

	ctr := &counterInt64{registry: r, key: "test/dedupe", ignoreRollbackOf: 1024}
	ctr.value.Store(5)

	// Two quick updates that will schedule background push; dedupe should ensure a single remote write
	ctr.TryUpdate(context.Background(), 20)
	ctr.TryUpdate(context.Background(), 20)

	time.Sleep(200 * time.Millisecond)
	assert.Equal(t, int64(20), ctr.GetValue())
	c.AssertExpectations(t)
}

// TestContinueAsyncRefresh_DoesNotLeakOnStuckWorker proves the helper
// goroutine exits even when the refresh function blocks past its context.
// Before the fix, the helper read from resultCh with no deadline; a worker
// that ignored its context would leak the helper for the process lifetime.
func TestContinueAsyncRefresh_DoesNotLeakOnStuckWorker(t *testing.T) {
	cfg := &common.SharedStateConfig{
		ClusterKey:      "test",
		FallbackTimeout: common.Duration(100 * time.Millisecond),
		LockTtl:         common.Duration(1 * time.Second),
		LockMaxWait:     common.Duration(10 * time.Millisecond),
		UpdateMaxWait:   common.Duration(10 * time.Millisecond),
	}
	r, c := setupMockRegistry(t, cfg)

	// Background reconcile paths may fire; allow but don't require them.
	lock := &MockLock{}
	lock.On("Unlock", mock.Anything).Return(nil).Maybe()
	c.On("Lock", mock.Anything, mock.Anything, mock.Anything).Return(lock, nil).Maybe()
	c.On("Get", mock.Anything, ConnectorMainIndex, mock.Anything, "value", nil).Return([]byte(""), errors.New("not found")).Maybe()
	c.On("Set", mock.Anything, mock.Anything, "value", mock.Anything, mock.Anything).Return(nil).Maybe()
	c.On("PublishCounterInt64", mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()

	ctr := &counterInt64{registry: r, key: "test/stuck", ignoreRollbackOf: 1024}
	ctr.value.Store(1)

	// Worker that ignores its context and only unblocks when we say so.
	release := make(chan struct{})
	defer close(release)

	before := runtime.NumGoroutine()

	// Foreground returns quickly via updateMaxWait; worker keeps running.
	val, err := ctr.TryUpdateIfStale(context.Background(), 1*time.Millisecond, func(ctx context.Context) (int64, error) {
		<-release
		return 42, nil
	})
	assert.NoError(t, err)
	assert.Equal(t, int64(1), val, "foreground should return stale value")

	// Helper is now waiting on resultCh. Wait longer than FallbackTimeout plus
	// the 1s buffer so the helper's bounded timer fires.
	time.Sleep(cfg.FallbackTimeout.Duration() + 1500*time.Millisecond)

	// Helper must have exited even though the worker is still blocked.
	// Exactly one lingering goroutine is expected (the stuck worker); the
	// helper and any transient goroutines must be gone.
	after := runtime.NumGoroutine()
	assert.LessOrEqual(t, after-before, 1,
		"expected at most the stuck worker to remain; got delta=%d", after-before)
}
