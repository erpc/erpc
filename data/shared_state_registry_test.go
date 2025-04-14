package data

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
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
		initializer:     util.NewInitializer(context.Background(), &log.Logger, nil),
	}
	return registry, connector, context.Background()
}

func TestSharedStateRegistry_UpdateCounter_Success(t *testing.T) {
	registry, connector, ctx := setupTest("my-dev")

	lock := &MockLock{}
	lock.On("Unlock", mock.Anything).Return(nil)

	connector.On("Lock", mock.Anything, "my-dev/test", mock.Anything).Return(lock, nil)
	connector.On("Get", mock.Anything, ConnectorMainIndex, "my-dev/test", "value").Return("5", nil)
	connector.On("Set", mock.Anything, "my-dev/test", "value", "10", mock.Anything).Return(nil)
	connector.On("PublishCounterInt64", mock.Anything, "my-dev/test", int64(10)).Return(nil)

	counter := &counterInt64{
		registry:          registry,
		key:               "my-dev/test",
		value:             atomic.Int64{},
		ignoreDownDriftOf: 1024,
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

	counter := &counterInt64{
		registry:          registry,
		key:               "my-dev/test",
		value:             atomic.Int64{},
		ignoreDownDriftOf: 1024,
	}
	counter.value.Store(5)

	result := counter.TryUpdate(ctx, 10)
	assert.Equal(t, int64(10), result) // Should fall back to local update

	connector.AssertExpectations(t)
}

func TestSharedStateRegistry_UpdateCounter_GetFailure(t *testing.T) {
	registry, connector, ctx := setupTest("my-dev")

	lock := &MockLock{}
	lock.On("Unlock", mock.Anything).Return(nil)

	connector.On("Lock", mock.Anything, "my-dev/test", mock.Anything).Return(lock, nil)
	connector.On("Get", mock.Anything, ConnectorMainIndex, "my-dev/test", "value").
		Return("", errors.New("get failed"))

	counter := &counterInt64{
		registry:          registry,
		key:               "my-dev/test",
		value:             atomic.Int64{},
		ignoreDownDriftOf: 1024,
	}
	counter.value.Store(5)

	result := counter.TryUpdate(ctx, 10)
	assert.Equal(t, int64(10), result) // Should fall back to local update

	connector.AssertExpectations(t)
	lock.AssertExpectations(t)
}

func TestSharedStateRegistry_UpdateCounter_SetFailure(t *testing.T) {
	registry, connector, ctx := setupTest("my-dev")

	lock := &MockLock{}
	lock.On("Unlock", mock.Anything).Return(nil)

	connector.On("Lock", mock.Anything, "my-dev/test", mock.Anything).Return(lock, nil)
	connector.On("Get", mock.Anything, ConnectorMainIndex, "my-dev/test", "value").Return("5", nil)
	connector.On("Set", mock.Anything, "my-dev/test", "value", "10", mock.Anything).
		Return(errors.New("set failed"))

	counter := &counterInt64{
		registry:          registry,
		key:               "my-dev/test",
		value:             atomic.Int64{},
		ignoreDownDriftOf: 1024,
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
	connector.On("Get", mock.Anything, ConnectorMainIndex, "my-dev/test", "value").Return("5", nil)
	connector.On("Set", mock.Anything, "my-dev/test", "value", "10", mock.Anything).Return(nil)
	connector.On("PublishCounterInt64", mock.Anything, "my-dev/test", int64(10)).
		Return(errors.New("publish failed"))

	counter := &counterInt64{
		registry:          registry,
		key:               "my-dev/test",
		value:             atomic.Int64{},
		ignoreDownDriftOf: 1024,
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

	connector.On("Lock", mock.Anything, "my-dev/test", mock.Anything).Return(lock, nil)
	connector.On("Get", mock.Anything, ConnectorMainIndex, "my-dev/test", "value").Return("15", nil)

	counter := &counterInt64{
		registry:          registry,
		key:               "my-dev/test",
		value:             atomic.Int64{},
		ignoreDownDriftOf: 1024,
	}
	counter.value.Store(5)

	result := counter.TryUpdate(ctx, 10)
	assert.Equal(t, int64(15), result) // Should use higher remote value

	connector.AssertExpectations(t)
	lock.AssertExpectations(t)
}

func TestSharedStateRegistry_UpdateCounter_ConcurrentUpdates(t *testing.T) {
	registry, connector, ctx := setupTest("my-dev")

	lock := &MockLock{}
	lock.On("Unlock", mock.Anything).Return(nil).Times(10)

	connector.On("Lock", mock.Anything, "my-dev/test", mock.Anything).Return(lock, nil).Times(10)
	connector.On("Get", mock.Anything, ConnectorMainIndex, "my-dev/test", "value").Return("5", nil).Times(10)
	connector.On("Set", mock.Anything, "my-dev/test", "value", mock.Anything, mock.Anything).Return(nil).Times(10)
	connector.On("PublishCounterInt64", mock.Anything, "my-dev/test", mock.Anything).Return(nil).Times(10)

	counter := &counterInt64{
		registry:          registry,
		key:               "my-dev/test",
		value:             atomic.Int64{},
		ignoreDownDriftOf: 1024,
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

	updates := make(chan int64, 1)
	connector.On("WatchCounterInt64", mock.Anything, "my-dev/test").Return(updates, func() {}, nil)
	connector.On("Get", mock.Anything, ConnectorMainIndex, "my-dev/test", "value").Return("5", nil)

	counter := registry.GetCounterInt64("test", 1024)
	assert.NotNil(t, counter)

	// Simulate an update
	updates <- 42
	time.Sleep(100 * time.Millisecond) // Give time for the update to process

	assert.Equal(t, int64(42), counter.GetValue())

	connector.AssertExpectations(t)
}

func TestSharedStateRegistry_GetCounterInt64_WatchFailure(t *testing.T) {
	registry, connector, _ := setupTest("my-dev")

	lock := &MockLock{}
	lock.On("Unlock", mock.Anything).Return(nil).Times(1)

	connector.On("Lock", mock.Anything, "my-dev/test", mock.Anything).Return(lock, nil)
	connector.On("Get", mock.Anything, ConnectorMainIndex, "my-dev/test", "value").Return("5", nil)
	connector.On("Set", mock.Anything, "my-dev/test", "value", mock.Anything, mock.Anything).Return(nil)
	connector.On("PublishCounterInt64", mock.Anything, "my-dev/test", mock.Anything).Return(nil)

	connector.On("WatchCounterInt64", mock.Anything, "my-dev/test").
		Return(nil, nil, errors.New("watch setup failed"))

	counter := registry.GetCounterInt64("test", 1024)
	assert.NotNil(t, counter)
	counter.TryUpdate(context.Background(), 42)
	assert.Equal(t, int64(42), counter.GetValue()) // Should still work with local value
}
