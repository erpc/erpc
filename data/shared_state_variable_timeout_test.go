package data

import (
	"context"
	"errors"
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
	c.On("Set", mock.Anything, "test/my", "value", []byte("10"), mock.Anything).Return(nil).Once()
	// publish can happen multiple times (best-effort publish + publish-after-Set)
	c.On("PublishCounterInt64", mock.Anything, "test/my", int64(10)).Return(nil)

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
	c.On("Set", mock.Anything, "test/stale", "value", []byte("999"), mock.Anything).Return(nil).Once()
	// publish can happen multiple times (best-effort publish + publish-after-Set)
	c.On("PublishCounterInt64", mock.Anything, "test/stale", int64(999)).Return(nil)

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
	c.On("Get", mock.Anything, ConnectorMainIndex, "test/recon", "value", nil).Return([]byte("15"), nil).Once()
	c.On("Set", mock.Anything, "test/recon", "value", []byte("15"), mock.Anything).Return(nil).Once()
	// best-effort publish local snapshot first, then publish-after-Set for the reconciled value
	c.On("PublishCounterInt64", mock.Anything, "test/recon", int64(10)).Return(nil)
	c.On("PublishCounterInt64", mock.Anything, "test/recon", int64(15)).Return(nil)

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
	c.On("Get", mock.Anything, ConnectorMainIndex, "test/fg", "value", nil).Return([]byte("20"), nil).Once()
	c.On("Set", mock.Anything, "test/fg", "value", []byte("20"), mock.Anything).Return(nil).Once()
	// best-effort publish local snapshot first, then publish-after-Set for the reconciled value
	c.On("PublishCounterInt64", mock.Anything, "test/fg", int64(10)).Return(nil)
	c.On("PublishCounterInt64", mock.Anything, "test/fg", int64(20)).Return(nil)

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
	c.On("Set", mock.Anything, "test/slow", "value", []byte("77"), mock.Anything).Return(nil).Once()
	// publish can happen multiple times (best-effort publish + publish-after-Set)
	c.On("PublishCounterInt64", mock.Anything, "test/slow", int64(77)).Return(nil)

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
	c.On("Set", mock.Anything, "test/dedupe", "value", []byte("20"), mock.Anything).Return(nil).Once()
	// publish can happen multiple times (best-effort publish + publish-after-Set)
	c.On("PublishCounterInt64", mock.Anything, "test/dedupe", int64(20)).Return(nil)

	ctr := &counterInt64{registry: r, key: "test/dedupe", ignoreRollbackOf: 1024}
	ctr.value.Store(5)

	// Two quick updates that will schedule background push; dedupe should ensure a single remote write
	ctr.TryUpdate(context.Background(), 20)
	ctr.TryUpdate(context.Background(), 20)

	time.Sleep(200 * time.Millisecond)
	assert.Equal(t, int64(20), ctr.GetValue())
	c.AssertExpectations(t)
}
