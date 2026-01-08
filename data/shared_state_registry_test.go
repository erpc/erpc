package data

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

type MockLock struct {
	mock.Mock
}

func (m *MockLock) IsNil() bool {
	return m == nil
}

func (m *MockLock) Unlock(ctx context.Context) error {
	args := m.Called(ctx)
	return args.Error(0)
}

func setupTest(clusterKey string) (*sharedStateRegistry, *MockConnector, context.Context) {
	connector := &MockConnector{}
	registry := &sharedStateRegistry{
		clusterKey:      clusterKey,
		appCtx:          context.Background(),
		logger:          &log.Logger,
		connector:       connector,
		fallbackTimeout: 500 * time.Millisecond,
		lockTtl:         2 * time.Second,
		lockMaxWait:     100 * time.Millisecond,
		updateMaxWait:   50 * time.Millisecond,
		initializer:     util.NewInitializer(context.Background(), &log.Logger, nil),
	}
	return registry, connector, context.Background()
}

func TestSharedStateRegistry_UpdateCounter_Success(t *testing.T) {
	registry, connector, ctx := setupTest("my-dev")

	lock := &MockLock{}
	lock.On("Unlock", mock.Anything).Return(nil)

	connector.On("Lock", mock.Anything, "my-dev/test", mock.Anything).Return(lock, nil)
	connector.On("Get", mock.Anything, ConnectorMainIndex, "my-dev/test", "value", nil).
		Return([]byte(`{"v":5,"t":1,"b":"test"}`), nil)
	connector.On("Set", mock.Anything, "my-dev/test", "value", mock.Anything, mock.Anything).Return(nil)
	connector.On("PublishCounterInt64", mock.Anything, "my-dev/test", mock.Anything).Return(nil)

	counter := &counterInt64{
		registry:         registry,
		key:              "my-dev/test",
		ignoreRollbackOf: 1024,
	}
	counter.value.Store(5)

	result := counter.TryUpdate(ctx, 10)
	assert.Equal(t, int64(10), result)

	time.Sleep(10 * time.Millisecond)

	connector.AssertExpectations(t)
	lock.AssertExpectations(t)
}

func TestSharedStateRegistry_UpdateCounter_LockFailure(t *testing.T) {
	registry, connector, ctx := setupTest("my-dev")

	connector.On("Lock", mock.Anything, "my-dev/test", mock.Anything).
		Return(nil, errors.New("lock acquisition failed"))
	// Background push now publishes the current value even if lock acquisition fails.
	connector.On("PublishCounterInt64", mock.Anything, "my-dev/test", mock.Anything).Return(nil)

	counter := &counterInt64{
		registry:         registry,
		key:              "my-dev/test",
		ignoreRollbackOf: 1024,
	}
	counter.value.Store(5)

	result := counter.TryUpdate(ctx, 10)
	assert.Equal(t, int64(10), result) // Should fall back to local update

	// Remote push happens asynchronously.
	time.Sleep(50 * time.Millisecond)

	connector.AssertExpectations(t)
}

func TestSharedStateRegistry_UpdateCounter_GetFailure(t *testing.T) {
	registry, connector, ctx := setupTest("my-dev")

	lock := &MockLock{}
	lock.On("Unlock", mock.Anything).Return(nil)

	connector.On("Lock", mock.Anything, "my-dev/test", mock.Anything).Return(lock, nil)
	connector.On("Get", mock.Anything, ConnectorMainIndex, "my-dev/test", "value", nil).
		Return([]byte(""), errors.New("get failed"))
	connector.On("Set", mock.Anything, "my-dev/test", "value", mock.Anything, mock.Anything).Return(nil)
	connector.On("PublishCounterInt64", mock.Anything, "my-dev/test", mock.Anything).Return(nil)

	counter := &counterInt64{
		registry:         registry,
		key:              "my-dev/test",
		ignoreRollbackOf: 1024,
	}
	counter.value.Store(5)

	result := counter.TryUpdate(ctx, 10)
	assert.Equal(t, int64(10), result) // Should fall back to local update

	// Remote push happens asynchronously.
	time.Sleep(50 * time.Millisecond)

	connector.AssertExpectations(t)
	lock.AssertExpectations(t)
}

func TestSharedStateRegistry_UpdateCounter_SetFailure(t *testing.T) {
	registry, connector, ctx := setupTest("my-dev")

	lock := &MockLock{}
	lock.On("Unlock", mock.Anything).Return(nil)

	connector.On("Lock", mock.Anything, "my-dev/test", mock.Anything).Return(lock, nil)
	connector.On("Get", mock.Anything, ConnectorMainIndex, "my-dev/test", "value", nil).
		Return([]byte(`{"v":5,"t":1,"b":"test"}`), nil)
	connector.On("Set", mock.Anything, "my-dev/test", "value", mock.Anything, mock.Anything).
		Return(errors.New("set failed"))
	// Background push now publishes the current value even if remote set fails.
	connector.On("PublishCounterInt64", mock.Anything, "my-dev/test", mock.Anything).Return(nil)

	counter := &counterInt64{
		registry:         registry,
		key:              "my-dev/test",
		ignoreRollbackOf: 1024,
	}
	counter.value.Store(5)

	result := counter.TryUpdate(ctx, 10)
	assert.Equal(t, int64(10), result) // Should fall back to local update

	time.Sleep(10 * time.Millisecond)

	connector.AssertExpectations(t)
	lock.AssertExpectations(t)
}

func TestSharedStateRegistry_UpdateCounter_PublishFailure(t *testing.T) {
	registry, connector, ctx := setupTest("my-dev")

	lock := &MockLock{}
	lock.On("Unlock", mock.Anything).Return(nil)

	connector.On("Lock", mock.Anything, "my-dev/test", mock.Anything).Return(lock, nil)
	connector.On("Get", mock.Anything, ConnectorMainIndex, "my-dev/test", "value", nil).
		Return([]byte(`{"v":5,"t":1,"b":"test"}`), nil)
	connector.On("Set", mock.Anything, "my-dev/test", "value", mock.Anything, mock.Anything).Return(nil)
	connector.On("PublishCounterInt64", mock.Anything, "my-dev/test", mock.Anything).
		Return(errors.New("publish failed"))

	counter := &counterInt64{
		registry:         registry,
		key:              "my-dev/test",
		ignoreRollbackOf: 1024,
	}
	counter.value.Store(5)

	result := counter.TryUpdate(ctx, 10)
	assert.Equal(t, int64(10), result) // Should fall back to local update

	time.Sleep(10 * time.Millisecond)

	connector.AssertExpectations(t)
	lock.AssertExpectations(t)
}

func TestSharedStateRegistry_UpdateCounter_RemoteHigherValue(t *testing.T) {
	registry, connector, ctx := setupTest("my-dev")

	lock := &MockLock{}
	lock.On("Unlock", mock.Anything).Return(nil)

	remoteUpdatedAt := time.Now().UnixMilli() + 10_000
	connector.On("Lock", mock.Anything, "my-dev/test", mock.Anything).Return(lock, nil)
	connector.On("Get", mock.Anything, ConnectorMainIndex, "my-dev/test", "value", nil).
		Return([]byte(fmt.Sprintf(`{"v":15,"t":%d,"b":"test"}`, remoteUpdatedAt)), nil)
	// Background publish is best-effort and may run multiple times.
	connector.On("PublishCounterInt64", mock.Anything, "my-dev/test", mock.Anything).Return(nil).Maybe()

	counter := &counterInt64{
		registry:         registry,
		key:              "my-dev/test",
		ignoreRollbackOf: 1024,
	}
	counter.value.Store(5)

	result := counter.TryUpdate(ctx, 10)
	// Foreground path is local-only; remote reconciliation happens in background.
	assert.Equal(t, int64(10), result)

	// Allow background reconcile/push to run and adopt the higher remote value.
	assert.Eventually(t, func() bool {
		return counter.GetValue() == int64(15)
	}, 500*time.Millisecond, 10*time.Millisecond)

	connector.AssertExpectations(t)
	lock.AssertExpectations(t)
}

func TestSharedStateRegistry_UpdateCounter_ConcurrentUpdates(t *testing.T) {
	registry, connector, ctx := setupTest("my-dev")

	// Remote push is best-effort and deduped; for this test we only care about local correctness.
	// Provide an optional Lock expectation to avoid unexpected-call panics if a background push runs.
	connector.On("Lock", mock.Anything, "my-dev/test", mock.Anything).
		Return(nil, errors.New("lock acquisition failed")).Maybe()
	// Background push also publishes best-effort for fast propagation.
	connector.On("PublishCounterInt64", mock.Anything, "my-dev/test", mock.Anything).
		Return(nil).Maybe()

	counter := &counterInt64{
		registry:         registry,
		key:              "my-dev/test",
		ignoreRollbackOf: 1024,
	}
	counter.value.Store(5)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(val int64) {
			defer wg.Done()
			counter.TryUpdate(ctx, val)
		}(int64(i + 10))
	}

	wg.Wait()

	assert.Equal(t, int64(19), counter.value.Load())
}

func TestSharedStateRegistry_GetCounterInt64_WatchSetup(t *testing.T) {
	registry, connector, _ := setupTest("my-dev")

	updates := make(chan CounterInt64State, 1)
	connector.On("WatchCounterInt64", mock.Anything, "my-dev/test").Return(updates, func() {}, nil)
	connector.On("Get", mock.Anything, ConnectorMainIndex, "my-dev/test", "value", nil).
		Return([]byte(`{"v":5,"t":1,"b":"test"}`), nil)

	counter := registry.GetCounterInt64("test", 1024)
	assert.NotNil(t, counter)

	// Simulate an update
	updates <- CounterInt64State{Value: 42, UpdatedAt: 2, UpdatedBy: "test"}
	time.Sleep(100 * time.Millisecond) // Give time for the update to process

	assert.Equal(t, int64(42), counter.GetValue())

	connector.AssertExpectations(t)
}

func TestSharedStateRegistry_GetCounterInt64_WatchFailure(t *testing.T) {
	registry, connector, _ := setupTest("my-dev")

	lock := &MockLock{}
	lock.On("Unlock", mock.Anything).Return(nil).Times(1)

	connector.On("Lock", mock.Anything, "my-dev/test", mock.Anything).Return(lock, nil)
	connector.On("Get", mock.Anything, ConnectorMainIndex, "my-dev/test", "value", nil).
		Return([]byte(`{"v":5,"t":1,"b":"test"}`), nil)
	connector.On("Set", mock.Anything, "my-dev/test", "value", mock.Anything, mock.Anything).Return(nil)
	connector.On("PublishCounterInt64", mock.Anything, "my-dev/test", mock.Anything).Return(nil)

	connector.On("WatchCounterInt64", mock.Anything, "my-dev/test").
		Return(nil, nil, errors.New("watch setup failed"))

	counter := registry.GetCounterInt64("test", 1024)
	assert.NotNil(t, counter)
	counter.TryUpdate(context.Background(), 42)
	assert.Equal(t, int64(42), counter.GetValue()) // Should still work with local value
}
