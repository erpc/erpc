package data

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

func TestCounterInt64_TryUpdate_LocalFallback(t *testing.T) {
	tests := []struct {
		name           string
		setupMocks     func(*MockConnector, *MockLock)
		initialValue   int64
		updateValue    int64
		expectedValue  int64
		expectedCalls  int
		expectedRemote bool
	}{
		{
			name: "lock failure falls back to local",
			setupMocks: func(c *MockConnector, l *MockLock) {
				c.On("Lock", mock.Anything, "test", mock.Anything).
					Return(nil, errors.New("lock failed"))
			},
			initialValue:   5,
			updateValue:    10,
			expectedValue:  10,
			expectedCalls:  1,
			expectedRemote: false,
		},
		{
			name: "get failure falls back to local",
			setupMocks: func(c *MockConnector, l *MockLock) {
				c.On("Lock", mock.Anything, "test", mock.Anything).Return(l, nil)
				l.On("Unlock", mock.Anything).Return(nil)
				c.On("Get", mock.Anything, ConnectorMainIndex, "test", "value").
					Return("", errors.New("get failed"))
			},
			initialValue:   5,
			updateValue:    10,
			expectedValue:  10,
			expectedCalls:  1,
			expectedRemote: false,
		},
		{
			name: "remote value higher than update",
			setupMocks: func(c *MockConnector, l *MockLock) {
				c.On("Lock", mock.Anything, "test", mock.Anything).Return(l, nil)
				l.On("Unlock", mock.Anything).Return(nil)
				c.On("Get", mock.Anything, ConnectorMainIndex, "test", "value").Return("15", nil)
			},
			initialValue:   5,
			updateValue:    10,
			expectedValue:  15,
			expectedCalls:  1,
			expectedRemote: false,
		},
		{
			name: "successful remote update",
			setupMocks: func(c *MockConnector, l *MockLock) {
				c.On("Lock", mock.Anything, "test", mock.Anything).Return(l, nil)
				l.On("Unlock", mock.Anything).Return(nil)
				c.On("Get", mock.Anything, ConnectorMainIndex, "test", "value").Return("5", nil)
				c.On("Set", mock.Anything, "test", "value", "10", mock.Anything).Return(nil)
				c.On("PublishCounterInt64", mock.Anything, "test", int64(10)).Return(nil)
			},
			initialValue:   5,
			updateValue:    10,
			expectedValue:  10,
			expectedCalls:  1,
			expectedRemote: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry, connector, _ := setupTest("my-dev")
			lock := &MockLock{}
			tt.setupMocks(connector, lock)

			counter := &counterInt64{
				registry:        registry,
				key:             "test",
				value:           atomic.Int64{},
				maxAllowedDrift: 1024,
			}
			counter.value.Store(tt.initialValue)

			result := counter.TryUpdate(context.Background(), tt.updateValue)
			assert.Equal(t, tt.expectedValue, result)
			time.Sleep(10 * time.Millisecond)
			connector.AssertExpectations(t)
			lock.AssertExpectations(t)
		})
	}
}

func TestCounterInt64_TryUpdateIfStale(t *testing.T) {
	tests := []struct {
		name          string
		setupMocks    func(*MockConnector, *MockLock)
		initialValue  int64
		staleness     time.Duration
		updateValue   int64
		lastUpdated   time.Time
		expectedValue int64
		expectedError error
		expectedCalls int
	}{
		{
			name: "not stale skips update",
			setupMocks: func(c *MockConnector, l *MockLock) {
				// No mocks needed as it should return early
			},
			initialValue:  5,
			staleness:     time.Second,
			updateValue:   10,
			lastUpdated:   time.Now(),
			expectedValue: 5,
			expectedError: nil,
			expectedCalls: 0,
		},
		{
			name: "stale value updates successfully",
			setupMocks: func(c *MockConnector, l *MockLock) {
				c.On("Lock", mock.Anything, "test", mock.Anything).Return(l, nil)
				l.On("Unlock", mock.Anything).Return(nil)
				c.On("Get", mock.Anything, ConnectorMainIndex, "test", "value").Return("5", nil)
				c.On("Set", mock.Anything, "test", "value", "10", mock.Anything).Return(nil)
				c.On("PublishCounterInt64", mock.Anything, "test", int64(10)).Return(nil)
			},
			initialValue:  5,
			staleness:     time.Second,
			updateValue:   10,
			lastUpdated:   time.Now().Add(-2 * time.Second),
			expectedValue: 10,
			expectedError: nil,
			expectedCalls: 1,
		},
		{
			name: "stale value with remote higher",
			setupMocks: func(c *MockConnector, l *MockLock) {
				c.On("Lock", mock.Anything, "test", mock.Anything).Return(l, nil)
				l.On("Unlock", mock.Anything).Return(nil)
				c.On("Get", mock.Anything, ConnectorMainIndex, "test", "value").Return("15", nil)
			},
			initialValue:  5,
			staleness:     time.Second,
			updateValue:   10,
			lastUpdated:   time.Now().Add(-2 * time.Second),
			expectedValue: 15,
			expectedError: nil,
			expectedCalls: 1,
		},
		{
			name: "update function returns error",
			setupMocks: func(c *MockConnector, l *MockLock) {
				c.On("Lock", mock.Anything, "test", mock.Anything).Return(l, nil)
				l.On("Unlock", mock.Anything).Return(nil)
				c.On("Get", mock.Anything, ConnectorMainIndex, "test", "value").Return("5", nil)
			},
			initialValue:  5,
			staleness:     time.Second,
			updateValue:   0, // Will return error
			lastUpdated:   time.Now().Add(-2 * time.Second),
			expectedValue: 5,
			expectedError: errors.New("update failed"),
			expectedCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry, connector, _ := setupTest("my-dev")
			lock := &MockLock{}
			tt.setupMocks(connector, lock)

			counter := &counterInt64{
				registry:        registry,
				key:             "test",
				value:           atomic.Int64{},
				maxAllowedDrift: 1024,
				baseSharedVariable: baseSharedVariable{
					lastUpdated: atomic.Int64{},
				},
			}
			counter.value.Store(tt.initialValue)
			counter.lastUpdated.Store(tt.lastUpdated.UnixNano())

			getNewValue := func(ctx context.Context) (int64, error) {
				if tt.expectedError != nil {
					return 0, tt.expectedError
				}
				return tt.updateValue, nil
			}

			result, err := counter.TryUpdateIfStale(context.Background(), tt.staleness, getNewValue)

			if tt.expectedError != nil {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
			assert.Equal(t, tt.expectedValue, result)
			time.Sleep(10 * time.Millisecond)
			connector.AssertExpectations(t)
			lock.AssertExpectations(t)
		})
	}
}

func TestCounterInt64_Concurrency(t *testing.T) {
	registry, connector, _ := setupTest("my-dev")
	lock := &MockLock{}

	// Setup mocks for multiple concurrent calls
	connector.On("Lock", mock.Anything, "test", mock.Anything).Return(lock, nil).Times(10)
	lock.On("Unlock", mock.Anything).Return(nil).Times(10)
	connector.On("Get", mock.Anything, ConnectorMainIndex, "test", "value").Return("5", nil).Times(10)
	connector.On("Set", mock.Anything, "test", "value", mock.Anything, mock.Anything).Return(nil).Times(10)
	connector.On("PublishCounterInt64", mock.Anything, "test", mock.Anything).Return(nil).Times(10)

	counter := &counterInt64{
		registry:        registry,
		key:             "test",
		value:           atomic.Int64{},
		maxAllowedDrift: 1024,
	}
	counter.value.Store(5)

	// Run multiple goroutines updating the counter
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(val int64) {
			defer wg.Done()
			counter.TryUpdate(context.Background(), val)
		}(int64(i + 10))
	}

	wg.Wait()

	// Verify final state
	assert.Equal(t, int64(19), counter.GetValue()) // Should be highest value
}

func TestCounterInt64_GetValue(t *testing.T) {
	counter := &counterInt64{
		value:           atomic.Int64{},
		maxAllowedDrift: 1024,
	}
	counter.value.Store(42)

	assert.Equal(t, int64(42), counter.GetValue())
}

func TestCounterInt64_IsStale(t *testing.T) {
	tests := []struct {
		name       string
		lastUpdate time.Time
		staleness  time.Duration
		expected   bool
	}{
		{
			name:       "not stale",
			lastUpdate: time.Now(),
			staleness:  time.Second,
			expected:   false,
		},
		{
			name:       "stale",
			lastUpdate: time.Now().Add(-2 * time.Second),
			staleness:  time.Second,
			expected:   true,
		},
		{
			name:       "exactly at staleness threshold",
			lastUpdate: time.Now().Add(-time.Second),
			staleness:  time.Second,
			expected:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			counter := &counterInt64{
				baseSharedVariable: baseSharedVariable{
					lastUpdated: atomic.Int64{},
				},
				maxAllowedDrift: 1024,
			}
			counter.lastUpdated.Store(tt.lastUpdate.UnixNano())
			assert.Equal(t, tt.expected, counter.IsStale(tt.staleness))
		})
	}
}

func TestSharedVariable(t *testing.T) {
	t.Run("basic remote sync updates local value", func(t *testing.T) {
		connector := NewMockConnector("test")
		registry := &sharedStateRegistry{
			appCtx:          context.Background(),
			clusterKey:      "test",
			logger:          &log.Logger,
			connector:       connector,
			fallbackTimeout: time.Second,
			initializer:     util.NewInitializer(context.Background(), &log.Logger, nil),
		}

		updates := make(chan int64, 10)
		cleanup := func() { close(updates) }

		// Setup mock expectations
		connector.On("WatchCounterInt64", mock.Anything, "test/counter1").
			Return(updates, cleanup, nil)

		connector.On("Get", mock.Anything, ConnectorMainIndex, "test/counter1", "value").
			Return("42", nil)

		// Get the counter and verify initial setup
		counter := registry.GetCounterInt64("counter1", 1024).(*counterInt64)
		time.Sleep(100 * time.Millisecond) // Allow goroutine to start

		// Verify initial value was fetched
		assert.Equal(t, int64(42), counter.GetValue())

		// Simulate remote update
		updates <- 100
		time.Sleep(100 * time.Millisecond)

		// Verify value was updated
		assert.Equal(t, int64(100), counter.GetValue())
	})

	t.Run("callback is triggered on remote updates", func(t *testing.T) {
		connector := NewMockConnector("test")
		registry := &sharedStateRegistry{
			appCtx:          context.Background(),
			clusterKey:      "test",
			logger:          &log.Logger,
			connector:       connector,
			fallbackTimeout: time.Second,
			initializer:     util.NewInitializer(context.Background(), &log.Logger, nil),
		}

		updates := make(chan int64, 10)
		cleanup := func() { close(updates) }

		connector.On("WatchCounterInt64", mock.Anything, "test/counter2").
			Return(updates, cleanup, nil)

		connector.On("Get", mock.Anything, ConnectorMainIndex, "test/counter2", "value").
			Return("0", nil)

		counter := registry.GetCounterInt64("counter2", 1024)

		// Setup callback
		callbackCalled := make(chan int64, 1)
		counter.OnValue(func(val int64) {
			callbackCalled <- val
		})

		// Simulate remote update
		updates <- 50

		// Wait for callback
		select {
		case val := <-callbackCalled:
			assert.Equal(t, int64(50), val)
		case <-time.After(time.Second):
			t.Fatal("callback was not called")
		}
	})

	t.Run("lower remote values are ignored", func(t *testing.T) {
		connector := NewMockConnector("test")
		registry := &sharedStateRegistry{
			appCtx:          context.Background(),
			clusterKey:      "test",
			logger:          &log.Logger,
			connector:       connector,
			fallbackTimeout: time.Second,
			initializer:     util.NewInitializer(context.Background(), &log.Logger, nil),
		}

		updates := make(chan int64, 10)
		cleanup := func() { close(updates) }

		connector.On("WatchCounterInt64", mock.Anything, "test/counter3").
			Return(updates, cleanup, nil)

		connector.On("Get", mock.Anything, ConnectorMainIndex, "test/counter3", "value").
			Return("100", nil)

		counter := registry.GetCounterInt64("counter3", 1024)
		time.Sleep(100 * time.Millisecond)

		// Verify initial value
		assert.Equal(t, int64(100), counter.GetValue())

		// Try to update with lower value
		updates <- 50
		time.Sleep(100 * time.Millisecond)

		// Value should remain unchanged
		assert.Equal(t, int64(100), counter.GetValue())
	})

	t.Run("handles watch channel closure gracefully", func(t *testing.T) {
		connector := NewMockConnector("test")
		registry := &sharedStateRegistry{
			appCtx:          context.Background(),
			clusterKey:      "test",
			logger:          &log.Logger,
			connector:       connector,
			fallbackTimeout: time.Second,
			initializer:     util.NewInitializer(context.Background(), &log.Logger, nil),
		}

		updates := make(chan int64, 10)
		cleanup := func() {
			close(updates)
		}

		connector.On("WatchCounterInt64", mock.Anything, "test/counter4").
			Return(updates, cleanup, nil)

		connector.On("Get", mock.Anything, ConnectorMainIndex, "test/counter4", "value").
			Return("42", nil)

		counter := registry.GetCounterInt64("counter4", 1024)
		time.Sleep(100 * time.Millisecond)

		// Initial value check
		assert.Equal(t, int64(42), counter.GetValue())

		time.Sleep(100 * time.Millisecond)

		// Counter should retain its last value
		assert.Equal(t, int64(42), counter.GetValue())
	})

	t.Run("handles initial fetch error gracefully", func(t *testing.T) {
		connector := NewMockConnector("test")
		registry := &sharedStateRegistry{
			appCtx:          context.Background(),
			clusterKey:      "test",
			logger:          &log.Logger,
			connector:       connector,
			fallbackTimeout: time.Second,
			initializer:     util.NewInitializer(context.Background(), &log.Logger, nil),
		}

		updates := make(chan int64, 10)
		cleanup := func() { close(updates) }

		connector.On("WatchCounterInt64", mock.Anything, "test/counter6").
			Return(updates, cleanup, nil)

		connector.On("Get", mock.Anything, ConnectorMainIndex, "test/counter6", "value").
			Return("", common.NewErrRecordNotFound("test/counter6", "value", "mock"))

		counter := registry.GetCounterInt64("counter6", 1024)
		time.Sleep(100 * time.Millisecond)

		// Initial value should be 0
		assert.Equal(t, int64(0), counter.GetValue())

		// Should still handle updates
		updates <- 100
		time.Sleep(100 * time.Millisecond)
		assert.Equal(t, int64(100), counter.GetValue())
	})
}
