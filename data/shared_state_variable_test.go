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

func TestSharedVariable(t *testing.T) {
	t.Run("basic remote sync updates local value", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		connector := NewMockConnector("test")
		registry := &sharedStateRegistry{
			appCtx:          ctx,
			clusterKey:      "test",
			logger:          &log.Logger,
			connector:       connector,
			fallbackTimeout: time.Second,
			initializer:     util.NewInitializer(ctx, &log.Logger, nil),
		}

		updates := make(chan int64, 10)
		cleanup := func() { close(updates) }

		// Setup mock expectations
		connector.On("WatchCounterInt64", mock.Anything, "test/counter1").
			Return(updates, cleanup, nil)

		connector.On("Get", mock.Anything, ConnectorMainIndex, "test/counter1", "value").
			Return([]byte("42"), nil)

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
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		connector := NewMockConnector("test")
		registry := &sharedStateRegistry{
			appCtx:          ctx,
			clusterKey:      "test",
			logger:          &log.Logger,
			connector:       connector,
			fallbackTimeout: time.Second,
			initializer:     util.NewInitializer(ctx, &log.Logger, nil),
		}

		updates := make(chan int64, 10)
		cleanup := func() { close(updates) }

		connector.On("WatchCounterInt64", mock.Anything, "test/counter2").
			Return(updates, cleanup, nil)

		connector.On("Get", mock.Anything, ConnectorMainIndex, "test/counter2", "value").
			Return([]byte("0"), nil)

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
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		connector := NewMockConnector("test")
		registry := &sharedStateRegistry{
			appCtx:          ctx,
			clusterKey:      "test",
			logger:          &log.Logger,
			connector:       connector,
			fallbackTimeout: time.Second,
			initializer:     util.NewInitializer(ctx, &log.Logger, nil),
		}

		updates := make(chan int64, 10)
		cleanup := func() { close(updates) }

		connector.On("WatchCounterInt64", mock.Anything, "test/counter3").
			Return(updates, cleanup, nil)

		connector.On("Get", mock.Anything, ConnectorMainIndex, "test/counter3", "value").
			Return([]byte("100"), nil)

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
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		connector := NewMockConnector("test")
		registry := &sharedStateRegistry{
			appCtx:          ctx,
			clusterKey:      "test",
			logger:          &log.Logger,
			connector:       connector,
			fallbackTimeout: time.Second,
			initializer:     util.NewInitializer(ctx, &log.Logger, nil),
		}

		updates := make(chan int64, 10)
		cleanup := func() {
			close(updates)
		}

		connector.On("WatchCounterInt64", mock.Anything, "test/counter4").
			Return(updates, cleanup, nil)

		connector.On("Get", mock.Anything, ConnectorMainIndex, "test/counter4", "value").
			Return([]byte("42"), nil)

		counter := registry.GetCounterInt64("counter4", 1024)
		time.Sleep(100 * time.Millisecond)

		// Initial value check
		assert.Equal(t, int64(42), counter.GetValue())

		time.Sleep(100 * time.Millisecond)

		// Counter should retain its last value
		assert.Equal(t, int64(42), counter.GetValue())
	})

	t.Run("handles initial fetch error gracefully", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		connector := NewMockConnector("test")
		registry := &sharedStateRegistry{
			appCtx:          ctx,
			clusterKey:      "test",
			logger:          &log.Logger,
			connector:       connector,
			fallbackTimeout: time.Second,
			initializer:     util.NewInitializer(ctx, &log.Logger, nil),
		}

		updates := make(chan int64, 10)
		cleanup := func() { close(updates) }

		connector.On("WatchCounterInt64", mock.Anything, "test/counter6").
			Return(updates, cleanup, nil)

		connector.On("Get", mock.Anything, ConnectorMainIndex, "test/counter6", "value").
			Return([]byte(""), common.NewErrRecordNotFound("test/counter6", "value", "mock"))

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
					Return([]byte(""), errors.New("get failed"))
				c.On("Set", mock.Anything, "test", "value", []byte("10"), mock.Anything).
					Return(errors.New("set failed"))
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
				c.On("Get", mock.Anything, ConnectorMainIndex, "test", "value").Return([]byte("15"), nil)
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
				c.On("Get", mock.Anything, ConnectorMainIndex, "test", "value").Return([]byte("5"), nil)
				c.On("Set", mock.Anything, "test", "value", []byte("10"), mock.Anything).Return(nil)
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
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			registry, connector, _ := setupTest("my-dev")
			lock := &MockLock{}
			tt.setupMocks(connector, lock)

			counter := &counterInt64{
				registry:         registry,
				key:              "test",
				value:            0,
				ignoreRollbackOf: 1024,
			}
			counter.value = tt.initialValue

			result := counter.TryUpdate(ctx, tt.updateValue)
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
		lastProcessed time.Time
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
			lastProcessed: time.Now(),
			expectedValue: 5,
			expectedError: nil,
			expectedCalls: 0,
		},
		{
			name: "stale local value same as remote unstales local value",
			setupMocks: func(c *MockConnector, l *MockLock) {
				c.On("Lock", mock.Anything, "test", mock.Anything).Return(l, nil)
				l.On("Unlock", mock.Anything).Return(nil)
				c.On("Get", mock.Anything, ConnectorMainIndex, "test", "value").Return([]byte("5"), nil)
			},
			initialValue:  5,
			staleness:     time.Second,
			updateValue:   5,
			lastProcessed: time.Now().Add(-2 * time.Second),
			expectedValue: 5,
			expectedError: nil,
			expectedCalls: 1,
		},
		{
			name: "stale value updates successfully",
			setupMocks: func(c *MockConnector, l *MockLock) {
				c.On("Lock", mock.Anything, "test", mock.Anything).Return(l, nil)
				l.On("Unlock", mock.Anything).Return(nil)
				c.On("Get", mock.Anything, ConnectorMainIndex, "test", "value").Return([]byte("4"), nil)
				c.On("Set", mock.Anything, "test", "value", []byte("10"), mock.Anything).Return(nil)
				c.On("PublishCounterInt64", mock.Anything, "test", int64(10)).Return(nil)
			},
			initialValue:  5,
			staleness:     time.Second,
			updateValue:   10,
			lastProcessed: time.Now().Add(-2 * time.Second),
			expectedValue: 10,
			expectedError: nil,
			expectedCalls: 1,
		},
		{
			name: "stale value with remote higher",
			setupMocks: func(c *MockConnector, l *MockLock) {
				c.On("Lock", mock.Anything, "test", mock.Anything).Return(l, nil)
				l.On("Unlock", mock.Anything).Return(nil)
				c.On("Get", mock.Anything, ConnectorMainIndex, "test", "value").Return([]byte("15"), nil)
			},
			initialValue:  5,
			staleness:     time.Second,
			updateValue:   10,
			lastProcessed: time.Now().Add(-2 * time.Second),
			expectedValue: 15,
			expectedError: nil,
			expectedCalls: 1,
		},
		{
			name: "update function returns error",
			setupMocks: func(c *MockConnector, l *MockLock) {
				c.On("Lock", mock.Anything, "test", mock.Anything).Return(l, nil)
				l.On("Unlock", mock.Anything).Return(nil)
				c.On("Get", mock.Anything, ConnectorMainIndex, "test", "value").Return([]byte("4"), nil)
			},
			initialValue:  5,
			staleness:     time.Second,
			updateValue:   0, // Will return error
			lastProcessed: time.Now().Add(-2 * time.Second),
			expectedValue: 5,
			expectedError: errors.New("update failed"),
			expectedCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			registry, connector, _ := setupTest("my-dev")
			lock := &MockLock{}
			tt.setupMocks(connector, lock)

			counter := &counterInt64{
				registry:         registry,
				key:              "test",
				value:            0,
				ignoreRollbackOf: 1024,
				baseSharedVariable: baseSharedVariable{
					lastProcessed: time.Time{},
				},
			}
			counter.value = tt.initialValue
			counter.lastProcessed = tt.lastProcessed

			getNewValue := func(ctx context.Context) (int64, error) {
				if tt.expectedError != nil {
					return 0, tt.expectedError
				}
				return tt.updateValue, nil
			}

			result, err := counter.TryUpdateIfStale(ctx, tt.staleness, getNewValue)

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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	registry, connector, _ := setupTest("my-dev")
	lock := &MockLock{}

	// Setup mocks for multiple concurrent calls
	connector.On("Lock", mock.Anything, "test", mock.Anything).Return(lock, nil).Times(10)
	lock.On("Unlock", mock.Anything).Return(nil).Times(10)
	connector.On("Get", mock.Anything, ConnectorMainIndex, "test", "value").Return([]byte("5"), nil).Times(10)
	connector.On("Set", mock.Anything, "test", "value", mock.Anything, mock.Anything).Return(nil).Times(10)
	connector.On("PublishCounterInt64", mock.Anything, "test", mock.Anything).Return(nil).Times(10)

	counter := &counterInt64{
		registry:         registry,
		key:              "test",
		value:            0,
		ignoreRollbackOf: 1024,
	}
	counter.value = 5

	// Run multiple goroutines updating the counter
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(val int64) {
			defer wg.Done()
			counter.TryUpdate(ctx, val)
		}(int64(i + 10))
	}

	wg.Wait()

	// Verify final state
	assert.Equal(t, int64(19), counter.GetValue()) // Should be highest value
}

func TestCounterInt64_GetValue(t *testing.T) {
	counter := &counterInt64{
		value:            42,
		ignoreRollbackOf: 1024,
	}

	assert.Equal(t, int64(42), counter.GetValue())
}

func TestCounterInt64_IsStale(t *testing.T) {
	tests := []struct {
		name          string
		lastProcessed time.Time
		staleness     time.Duration
		expected      bool
	}{
		{
			name:          "not stale",
			lastProcessed: time.Now(),
			staleness:     time.Second,
			expected:      false,
		},
		{
			name:          "stale",
			lastProcessed: time.Now().Add(-2 * time.Second),
			staleness:     time.Second,
			expected:      true,
		},
		{
			name:          "exactly at staleness threshold",
			lastProcessed: time.Now().Add(-time.Second),
			staleness:     time.Second,
			expected:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			counter := &counterInt64{
				baseSharedVariable: baseSharedVariable{
					lastProcessed: time.Time{},
				},
				ignoreRollbackOf: 1024,
			}
			counter.lastProcessed = tt.lastProcessed
			assert.Equal(t, tt.expected, counter.IsStale(tt.staleness))
		})
	}
}

// Test that calling setValue directly (as done by remote update watchers or initial value fetch) refreshes the
// internal lastUpdated timestamp so that subsequent IsStale checks reflect the recent update.
func TestCounterInt64_SetValueUpdatesLastUpdated(t *testing.T) {
	counter := &counterInt64{
		ignoreRollbackOf: 1024,
		registry: &sharedStateRegistry{
			logger: &log.Logger,
		},
	}

	// Simulate a write originating from the remote sync by calling setValue directly.
	counter.processNewValue(123)

	// Immediately after the write the counter must not be considered stale for any reasonable staleness window.
	assert.False(t, counter.IsStale(time.Second), "counter should not be stale right after setValue call")

	// After advancing the clock beyond the staleness window the value should be considered stale.
	time.Sleep(15 * time.Millisecond)
	assert.True(t, counter.IsStale(10*time.Millisecond), "counter should become stale once the window elapses")
}

func TestCounterInt64_TryUpdate_RollbackLogic(t *testing.T) {
	tests := []struct {
		name             string
		setupMocks       func(conn *MockConnector, lock *MockLock)
		initialValue     int64
		updateValue      int64
		ignoreRollbackOf int64
		expectedValue    int64
	}{
		{
			name: "remote lock fails, fallback local, higher update => update",
			setupMocks: func(conn *MockConnector, lock *MockLock) {
				// No Set call is expected because we fail to lock remotely
				conn.On("Lock", mock.Anything, "test", mock.Anything).
					Return(nil, errors.New("lock failed"))
			},
			initialValue:     5,
			updateValue:      10,
			ignoreRollbackOf: 1024,
			expectedValue:    10,
		},
		{
			name: "remote lock fails, fallback local, lower update within rollback range => no update",
			setupMocks: func(conn *MockConnector, lock *MockLock) {
				// No Set call is expected because we fail to lock remotely
				conn.On("Lock", mock.Anything, "test", mock.Anything).
					Return(nil, errors.New("lock failed"))
			},
			initialValue:     100,
			updateValue:      95,
			ignoreRollbackOf: 10,  // difference = 5 => within rollback range
			expectedValue:    100, // remains unchanged
		},
		{
			name: "remote lock fails, fallback local, lower update exceeds rollback range => update",
			setupMocks: func(conn *MockConnector, lock *MockLock) {
				conn.On("Lock", mock.Anything, "test", mock.Anything).
					Return(nil, errors.New("lock failed"))
			},
			initialValue:     100,
			updateValue:      50,
			ignoreRollbackOf: 40, // difference = 50 => exceeds rollback range
			expectedValue:    50,
		},
		{
			name: "remote lock succeeds, remote higher => override local; no Set call for newValue",
			setupMocks: func(conn *MockConnector, lock *MockLock) {
				conn.On("Lock", mock.Anything, "test", mock.Anything).Return(lock, nil)
				lock.On("Unlock", mock.Anything).Return(nil)
				conn.On("Get", mock.Anything, ConnectorMainIndex, "test", "value").
					Return([]byte("200"), nil)
				// We do NOT expect a Set call because remote is higher than newValue
				// so no new remote update is performed.
			},
			initialValue:     100,
			updateValue:      150,
			ignoreRollbackOf: 1024,
			expectedValue:    200, // remote overrides local
		},
		{
			name: "remote lock succeeds, remote is lower, new update is higher => triggers Set + Publish",
			setupMocks: func(conn *MockConnector, lock *MockLock) {
				conn.On("Lock", mock.Anything, "test", mock.Anything).Return(lock, nil)
				lock.On("Unlock", mock.Anything).Return(nil)
				// Remote has 100, local has 100, newValue=150 => we do an update
				conn.On("Get", mock.Anything, ConnectorMainIndex, "test", "value").
					Return([]byte("100"), nil)
				// Because newValue (150) is higher, the code will call connector.Set
				conn.On("Set", mock.Anything, "test", "value", []byte("150"), mock.Anything).
					Return(nil)
				// And then PublishCounterInt64
				conn.On("PublishCounterInt64", mock.Anything, "test", int64(150)).
					Return(nil)
			},
			initialValue:     100,
			updateValue:      150,
			ignoreRollbackOf: 1024,
			expectedValue:    150,
		},
		{
			name: "remote lock succeeds, remote is lower, newValue is also lower but within rollback range => no update",
			setupMocks: func(conn *MockConnector, lock *MockLock) {
				conn.On("Lock", mock.Anything, "test", mock.Anything).Return(lock, nil)
				lock.On("Unlock", mock.Anything).Return(nil)
				conn.On("Get", mock.Anything, ConnectorMainIndex, "test", "value").
					Return([]byte("100"), nil)
				// No Set call because final local won't change (difference=5, within rollback range=10)
			},
			initialValue:     100,
			updateValue:      95,
			ignoreRollbackOf: 10, // difference = 5 => within rollback range => no update
			expectedValue:    100,
		},
		{
			name: "remote lock succeeds, remote is lower, newValue is lower, exceeds rollback range => triggers Set + Publish",
			setupMocks: func(conn *MockConnector, lock *MockLock) {
				conn.On("Lock", mock.Anything, "test", mock.Anything).Return(lock, nil)
				lock.On("Unlock", mock.Anything).Return(nil)
				conn.On("Get", mock.Anything, ConnectorMainIndex, "test", "value").
					Return([]byte("100"), nil)
				conn.On("Set", mock.Anything, "test", "value", []byte("50"), mock.Anything).
					Return(nil)
				conn.On("PublishCounterInt64", mock.Anything, "test", int64(50)).
					Return(nil)
			},
			initialValue:     100,
			updateValue:      50,
			ignoreRollbackOf: 40, // difference=50 => exceeds rollback range => do update
			expectedValue:    50,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			registry, connector, _ := setupTest("my-dev")
			lock := &MockLock{}
			tt.setupMocks(connector, lock)

			counter := &counterInt64{
				registry:         registry,
				key:              "test",
				ignoreRollbackOf: tt.ignoreRollbackOf,
			}
			counter.value = tt.initialValue

			actualValue := counter.TryUpdate(ctx, tt.updateValue)
			assert.Equal(t, tt.expectedValue, actualValue,
				"After TryUpdate(%d) with ignoreRollbackOf=%d, value should be %d",
				tt.updateValue, tt.ignoreRollbackOf, tt.expectedValue,
			)

			// Give async goroutines (the Set/Publish calls) time to complete
			time.Sleep(20 * time.Millisecond)

			connector.AssertExpectations(t)
			lock.AssertExpectations(t)
		})
	}
}

func TestCounterInt64_TryUpdateIfStale_NoThunderingHerdEqualValue(t *testing.T) {
	ctx := context.Background()
	registry, connector, _ := setupTest("my-dev")

	// We expect a single lock attempt during the first refresh.
	connector.On("Lock", mock.Anything, "test", mock.Anything).
		Return(nil, errors.New("lock failed")).Once()

	counter := &counterInt64{
		registry:         registry,
		key:              "test",
		value:            5,
		ignoreRollbackOf: 1024,
	}
	// Force it to look stale.
	counter.lastProcessed = time.Now().Add(-2 * time.Second)

	var calls int32
	refreshFn := func(ctx context.Context) (int64, error) {
		atomic.AddInt32(&calls, 1)
		return 5, nil // identical value
	}

	staleness := time.Second

	// First attempt – should call refreshFn.
	val, err := counter.TryUpdateIfStale(ctx, staleness, refreshFn)
	assert.NoError(t, err)
	assert.Equal(t, int64(5), val)

	// Second attempt inside debounce window – should *not* call refreshFn again.
	val, err = counter.TryUpdateIfStale(ctx, staleness, refreshFn)
	assert.NoError(t, err)
	assert.Equal(t, int64(5), val)
	assert.Equal(t, int32(1), calls, "refreshFn should be invoked exactly once")

	connector.AssertExpectations(t)
}

func TestCounterInt64_TryUpdateIfStale_NoThunderingHerdOnError(t *testing.T) {
	ctx := context.Background()
	registry, connector, _ := setupTest("my-dev")

	connector.On("Lock", mock.Anything, "test", mock.Anything).
		Return(nil, errors.New("lock failed")).Once()

	counter := &counterInt64{
		registry:         registry,
		key:              "test",
		value:            5,
		ignoreRollbackOf: 1024,
	}
	counter.lastProcessed = time.Now().Add(-2 * time.Second) // force stale

	var calls int32
	refreshErr := errors.New("upstream failure")
	refreshFn := func(ctx context.Context) (int64, error) {
		atomic.AddInt32(&calls, 1)
		return 0, refreshErr
	}

	staleness := time.Second

	// First call should execute refreshFn and propagate the error.
	val, err := counter.TryUpdateIfStale(ctx, staleness, refreshFn)
	assert.Equal(t, refreshErr, err)
	assert.Equal(t, int64(5), val)

	// Second call within debounce window – no second refresh, therefore no error.
	val, err = counter.TryUpdateIfStale(ctx, staleness, refreshFn)
	assert.NoError(t, err)
	assert.Equal(t, int64(5), val)

	// Only one refresh attempt overall.
	assert.Equal(t, int32(1), calls, "refreshFn should be invoked exactly once despite the error")

	connector.AssertExpectations(t)
}
