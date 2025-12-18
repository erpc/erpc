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

func init() {
	util.ConfigureTestLogger()
}

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

		connector.On("Get", mock.Anything, ConnectorMainIndex, "test/counter1", "value", nil).
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

		connector.On("Get", mock.Anything, ConnectorMainIndex, "test/counter2", "value", nil).
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

		connector.On("Get", mock.Anything, ConnectorMainIndex, "test/counter3", "value", nil).
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

		connector.On("Get", mock.Anything, ConnectorMainIndex, "test/counter4", "value", nil).
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

		connector.On("Get", mock.Anything, ConnectorMainIndex, "test/counter6", "value", nil).
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
				c.On("Get", mock.Anything, ConnectorMainIndex, "test", "value", nil).
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
				c.On("Get", mock.Anything, ConnectorMainIndex, "test", "value", nil).Return([]byte("15"), nil)
				// After reconciling to 15, the code pushes the current value back to remote
				c.On("Set", mock.Anything, "test", "value", []byte("15"), mock.Anything).Return(nil)
				c.On("PublishCounterInt64", mock.Anything, "test", int64(15)).Return(nil)
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
				c.On("Get", mock.Anything, ConnectorMainIndex, "test", "value", nil).Return([]byte("5"), nil)
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
				ignoreRollbackOf: 1024,
			}
			counter.value.Store(tt.initialValue)

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
				c.On("Get", mock.Anything, ConnectorMainIndex, "test", "value", nil).Return([]byte("4"), nil)
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
				c.On("Get", mock.Anything, ConnectorMainIndex, "test", "value", nil).Return([]byte("15"), nil)
				// With lock held, reconcile will push the higher value back to remote
				c.On("Set", mock.Anything, "test", "value", []byte("15"), mock.Anything).Return(nil)
				c.On("PublishCounterInt64", mock.Anything, "test", int64(15)).Return(nil)
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
				ignoreRollbackOf: 1024,
				baseSharedVariable: baseSharedVariable{
					lastProcessed: time.Time{},
				},
			}
			counter.value.Store(tt.initialValue)
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
	connector.On("Get", mock.Anything, ConnectorMainIndex, "test", "value", nil).Return([]byte("5"), nil).Times(10)
	connector.On("Set", mock.Anything, "test", "value", mock.Anything, mock.Anything).Return(nil).Times(10)
	connector.On("PublishCounterInt64", mock.Anything, "test", mock.Anything).Return(nil).Times(10)

	counter := &counterInt64{
		registry:         registry,
		key:              "test",
		ignoreRollbackOf: 1024,
	}
	counter.value.Store(5)

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
		ignoreRollbackOf: 1024,
	}
	counter.value.Store(42)

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
	counter.processNewValue(UpdateSourceTest, 123)

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
			name: "remote lock fails, fallback local, lower update exceeds rollback range => no update (rollback not applied)",
			setupMocks: func(conn *MockConnector, lock *MockLock) {
				conn.On("Lock", mock.Anything, "test", mock.Anything).
					Return(nil, errors.New("lock failed"))
			},
			initialValue:     100,
			updateValue:      50,
			ignoreRollbackOf: 40, // difference = 50 => exceeds rollback range
			// NEW BEHAVIOR: Rollback is detected but NOT applied to preserve SetLocalValue invariant.
			// The value remains at 100 instead of rolling back to 50.
			expectedValue: 100,
		},
		{
			name: "remote lock succeeds, remote higher => override local; push reconciled value",
			setupMocks: func(conn *MockConnector, lock *MockLock) {
				conn.On("Lock", mock.Anything, "test", mock.Anything).Return(lock, nil)
				lock.On("Unlock", mock.Anything).Return(nil)
				conn.On("Get", mock.Anything, ConnectorMainIndex, "test", "value", nil).
					Return([]byte("200"), nil)
				// After reconciling to 200 while holding the lock, code pushes current back to remote
				conn.On("Set", mock.Anything, "test", "value", []byte("200"), mock.Anything).Return(nil)
				conn.On("PublishCounterInt64", mock.Anything, "test", int64(200)).Return(nil)
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
				conn.On("Get", mock.Anything, ConnectorMainIndex, "test", "value", nil).
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
				conn.On("Get", mock.Anything, ConnectorMainIndex, "test", "value", nil).
					Return([]byte("100"), nil)
				// No Set call because final local won't change (difference=5, within rollback range=10)
			},
			initialValue:     100,
			updateValue:      95,
			ignoreRollbackOf: 10, // difference = 5 => within rollback range => no update
			expectedValue:    100,
		},
		{
			name: "remote lock succeeds, remote is lower, newValue is lower, exceeds rollback range => no update (rollback not applied)",
			setupMocks: func(conn *MockConnector, lock *MockLock) {
				conn.On("Lock", mock.Anything, "test", mock.Anything).Return(lock, nil)
				lock.On("Unlock", mock.Anything).Return(nil)
				conn.On("Get", mock.Anything, ConnectorMainIndex, "test", "value", nil).
					Return([]byte("100"), nil)
				// NEW BEHAVIOR: No Set/Publish calls because rollback is not applied.
				// The value remains at 100 (both local and remote are 100).
			},
			initialValue:     100,
			updateValue:      50,
			ignoreRollbackOf: 40, // difference=50 => exceeds rollback range
			// NEW BEHAVIOR: Rollback is detected but NOT applied to preserve SetLocalValue invariant.
			expectedValue: 100,
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
			counter.value.Store(tt.initialValue)

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
		ignoreRollbackOf: 1024,
	}
	counter.value.Store(5) // Initialize with value 5
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
		ignoreRollbackOf: 1024,
	}
	counter.value.Store(5)                                   // Initialize with value 5
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

func TestCounterInt64_ReaderStarvation(t *testing.T) {
	t.Run("GetValue NOT blocked by long-running TryUpdateIfStale", func(t *testing.T) {
		counter := &counterInt64{
			ignoreRollbackOf: 1024,
			baseSharedVariable: baseSharedVariable{
				lastProcessed: time.Now().Add(-2 * time.Second), // Make it stale
			},
		}
		counter.value.Store(42)

		// Track timing of GetValue calls
		getValueDurations := make([]time.Duration, 0)
		getValueMu := sync.Mutex{}

		// Start multiple readers in goroutines
		var readersWg sync.WaitGroup
		for i := 0; i < 10; i++ {
			readersWg.Add(1)
			go func() {
				defer readersWg.Done()
				// Wait a bit to ensure the writer gets the lock first
				time.Sleep(10 * time.Millisecond)

				start := time.Now()
				val := counter.GetValue()
				duration := time.Since(start)

				getValueMu.Lock()
				getValueDurations = append(getValueDurations, duration)
				getValueMu.Unlock()

				// The value might be 42 or 100 depending on when GetValue runs
				assert.Contains(t, []int64{42, 100}, val)
			}()
		}

		// Start a slow update that will hold the write lock
		var updateWg sync.WaitGroup
		updateWg.Add(1)
		go func() {
			defer updateWg.Done()
			// Directly test the mutex contention without mocks
			counter.updateMu.Lock()
			defer counter.updateMu.Unlock()

			// Simulate long-running operation while holding the lock
			time.Sleep(500 * time.Millisecond)
			counter.value.Store(100)
		}()

		// Wait for all operations to complete
		updateWg.Wait()
		readersWg.Wait()

		// Check that readers were NOT blocked
		blockedReaders := 0
		totalTime := time.Duration(0)
		for _, duration := range getValueDurations {
			totalTime += duration
			if duration > 10*time.Millisecond {
				blockedReaders++
			}
		}

		avgTime := totalTime / time.Duration(len(getValueDurations))
		t.Logf("Blocked readers: %d out of 10", blockedReaders)
		t.Logf("Average GetValue time: %v", avgTime)

		// With atomic operations, GetValue should be fast and not blocked
		assert.LessOrEqual(t, blockedReaders, 1, "Expected at most 1 reader to show any delay, but %d were delayed", blockedReaders)
		assert.Less(t, avgTime, 1*time.Millisecond, "Expected average GetValue time to be under 1ms, but was %v", avgTime)
	})

	t.Run("GetValue performance remains fast under concurrent updates", func(t *testing.T) {
		counter := &counterInt64{
			ignoreRollbackOf: 1024,
		}
		counter.value.Store(42)

		// Measure baseline GetValue performance (no contention)
		start := time.Now()
		iterations := 100000
		for i := 0; i < iterations; i++ {
			counter.GetValue()
		}
		baselineDuration := time.Since(start)
		baselinePerOp := baselineDuration.Nanoseconds() / int64(iterations)

		// Now measure with concurrent writers
		stopCh := make(chan struct{})

		// Start writers that continuously update
		for i := 0; i < 5; i++ {
			go func() {
				for {
					select {
					case <-stopCh:
						return
					default:
						counter.updateMu.Lock()
						// Simulate some work while holding lock
						counter.value.Store(counter.value.Load() + 1)
						time.Sleep(1 * time.Millisecond)
						counter.updateMu.Unlock()
					}
				}
			}()
		}

		// Give writers time to start
		time.Sleep(50 * time.Millisecond)

		// Measure GetValue performance under contention
		start = time.Now()
		for i := 0; i < iterations; i++ {
			counter.GetValue()
		}
		contentionDuration := time.Since(start)
		contentionPerOp := contentionDuration.Nanoseconds() / int64(iterations)

		close(stopCh)

		// Calculate slowdown
		var slowdownFactor float64
		if baselinePerOp > 0 {
			slowdownFactor = float64(contentionPerOp) / float64(baselinePerOp)
		} else {
			// If baseline is too fast to measure, just check absolute time
			slowdownFactor = 1.0
		}

		t.Logf("Baseline per op: %dns", baselinePerOp)
		t.Logf("Under contention per op: %dns", contentionPerOp)
		t.Logf("Slowdown factor: %.2fx", slowdownFactor)

		// With atomic operations, GetValue should remain fast even under write contention
		// Allow up to 10x slowdown due to CPU cache effects, but it should be much better than the 40,000x we saw before
		assert.Less(t, slowdownFactor, 10.0, "GetValue should not be significantly slower under load: %.2fx slower", slowdownFactor)

		// Also check absolute performance - should be under 1000ns per operation even under load
		// (increased from 100ns to account for CI environment variations)
		assert.Less(t, contentionPerOp, int64(1000), "GetValue should be fast even under contention: %dns per op", contentionPerOp)
	})
}

// TestSharedVariableTimeoutHandling verifies that when the remote backend is down,
// the shared variable operations fail fast and leave enough time for the actual operation.
func TestSharedVariableTimeoutHandling(t *testing.T) {
	t.Run("operations respect timeout when remote is down", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Create a connector that simulates a broken Redis connection
		connector := NewMockConnector("broken")
		registry := &sharedStateRegistry{
			appCtx:          ctx,
			logger:          &log.Logger,
			clusterKey:      "test",
			connector:       connector,
			fallbackTimeout: 1 * time.Second,
			lockTtl:         30 * time.Second,
			lockMaxWait:     200 * time.Millisecond,
			updateMaxWait:   200 * time.Millisecond,
			initializer:     util.NewInitializer(ctx, &log.Logger, nil),
		}

		// Setup mock to simulate timeout on Lock
		connector.On("Lock", mock.Anything, mock.Anything, mock.Anything).
			Run(func(args mock.Arguments) {
				ctx := args.Get(0).(context.Context)
				// Wait for context to timeout
				<-ctx.Done()
			}).
			Return(nil, context.DeadlineExceeded)

		// Setup mock for Get to return not found initially
		connector.On("Get", mock.Anything, ConnectorMainIndex, "test/timeout-counter", "value", nil).
			Return(nil, common.NewErrRecordNotFound("test/timeout-counter", "value", "mock"))

		// Setup mock for WatchCounterInt64
		updates := make(chan int64, 10)
		cleanup := func() { close(updates) }
		connector.On("WatchCounterInt64", mock.Anything, "test/timeout-counter").
			Return(updates, cleanup, nil)

		// Get a counter variable
		counter := registry.GetCounterInt64("timeout-counter", 100).(*counterInt64)
		time.Sleep(100 * time.Millisecond) // Allow initialization

		// Test TryUpdateIfStale with a parent context that has enough time for lock wait + operations
		operationCtx, operationCancel := context.WithTimeout(ctx, 3*time.Second)
		defer operationCancel()

		executionCount := atomic.Int32{}
		start := time.Now()

		value, err := counter.TryUpdateIfStale(operationCtx, 100*time.Millisecond, func(ctx context.Context) (int64, error) {
			executionCount.Add(1)
			// This simulates the actual operation (e.g., fetching block number)
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			case <-time.After(100 * time.Millisecond):
				return 42, nil
			}
		})

		elapsed := time.Since(start)

		// The operation should succeed quickly within UpdateMaxWait since fn finishes within budget
		assert.NoError(t, err)
		assert.Equal(t, int64(42), value)
		assert.Equal(t, int32(1), executionCount.Load(), "The fetch function should be called exactly once")
		assert.Less(t, elapsed, 500*time.Millisecond)
		assert.GreaterOrEqual(t, elapsed, 100*time.Millisecond)
	})

	t.Run("TryUpdate respects timeout when remote is down", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		connector := NewMockConnector("broken2")
		registry := &sharedStateRegistry{
			appCtx:          ctx,
			logger:          &log.Logger,
			clusterKey:      "test",
			connector:       connector,
			fallbackTimeout: 1 * time.Second,
			lockTtl:         30 * time.Second,
			initializer:     util.NewInitializer(ctx, &log.Logger, nil),
		}

		// Setup mock to simulate timeout on Lock
		connector.On("Lock", mock.Anything, mock.Anything, mock.Anything).
			Run(func(args mock.Arguments) {
				ctx := args.Get(0).(context.Context)
				// Wait for context to timeout
				<-ctx.Done()
			}).
			Return(nil, context.DeadlineExceeded)

		// Setup other required mocks
		connector.On("Get", mock.Anything, ConnectorMainIndex, "test/update-counter", "value", nil).
			Return(nil, common.NewErrRecordNotFound("test/update-counter", "value", "mock"))

		updates := make(chan int64, 10)
		cleanup := func() { close(updates) }
		connector.On("WatchCounterInt64", mock.Anything, "test/update-counter").
			Return(updates, cleanup, nil)

		counter := registry.GetCounterInt64("update-counter", 100).(*counterInt64)
		time.Sleep(100 * time.Millisecond) // Allow initialization

		operationCtx, operationCancel := context.WithTimeout(ctx, 35*time.Second)
		defer operationCancel()

		start := time.Now()
		value := counter.TryUpdate(operationCtx, 100)
		elapsed := time.Since(start)

		assert.Equal(t, int64(100), value)
		// Should wait for lock ttl when lock is held
		assert.Less(t, elapsed, 35*time.Second, "TryUpdate should complete within lock ttl")
	})

	t.Run("validates fallback timeout configuration", func(t *testing.T) {
		// Test that fallback timeout validation works
		cfg := &common.SharedStateConfig{
			ClusterKey:      "test",
			FallbackTimeout: common.Duration(10 * time.Millisecond), // Too short
			LockTtl:         common.Duration(30 * time.Second),
			Connector: &common.ConnectorConfig{
				Id:     "test-memory",
				Driver: common.DriverMemory,
				Memory: &common.MemoryConnectorConfig{MaxItems: 100},
			},
		}

		err := cfg.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "should be at least 100ms")

		// Test minimum timeout
		cfg.FallbackTimeout = common.Duration(50 * time.Millisecond)
		err = cfg.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "should be at least 100ms")

		// Test valid timeout
		cfg.FallbackTimeout = common.Duration(1 * time.Second)
		err = cfg.Validate()
		assert.NoError(t, err)
	})

	t.Run("SetLocalValue updates value synchronously without network calls", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Use a mock connector that will fail if any network call is made
		connector := NewMockConnector("sync-test")
		registry := &sharedStateRegistry{
			appCtx:          ctx,
			clusterKey:      "test",
			logger:          &log.Logger,
			connector:       connector,
			fallbackTimeout: time.Second,
			lockTtl:         30 * time.Second,
			initializer:     util.NewInitializer(ctx, &log.Logger, nil),
		}

		updates := make(chan int64, 10)
		cleanup := func() { close(updates) }

		connector.On("WatchCounterInt64", mock.Anything, "test/sync-counter").
			Return(updates, cleanup, nil)
		connector.On("Get", mock.Anything, ConnectorMainIndex, "test/sync-counter", "value", nil).
			Return([]byte("100"), nil)

		counter := registry.GetCounterInt64("sync-counter", 1024).(*counterInt64)
		time.Sleep(50 * time.Millisecond) // Allow initialization

		// Verify initial value
		assert.Equal(t, int64(100), counter.GetValue())

		// SetLocalValue should update SYNCHRONOUSLY (no network, no async)
		// This is the critical fix: the value must be immediately visible
		updated := counter.SetLocalValue(200)
		assert.True(t, updated, "SetLocalValue should return true when value increased")

		// IMMEDIATELY check the value - this is the key assertion
		// Before the fix, this would fail because update was async
		assert.Equal(t, int64(200), counter.GetValue(), "GetValue must return updated value immediately after SetLocalValue")

		// SetLocalValue with same or lower value should not update
		updated = counter.SetLocalValue(200)
		assert.False(t, updated, "SetLocalValue should return false when value is not greater")

		updated = counter.SetLocalValue(150)
		assert.False(t, updated, "SetLocalValue should return false when value is lower")

		// Value should still be 200
		assert.Equal(t, int64(200), counter.GetValue())

		// SetLocalValue with higher value should update
		updated = counter.SetLocalValue(300)
		assert.True(t, updated)
		assert.Equal(t, int64(300), counter.GetValue())
	})

	t.Run("SetLocalValue is thread-safe under concurrent access", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		connector := NewMockConnector("concurrent-sync-test")
		registry := &sharedStateRegistry{
			appCtx:          ctx,
			clusterKey:      "test",
			logger:          &log.Logger,
			connector:       connector,
			fallbackTimeout: time.Second,
			lockTtl:         30 * time.Second,
			initializer:     util.NewInitializer(ctx, &log.Logger, nil),
		}

		updates := make(chan int64, 10)
		cleanup := func() { close(updates) }

		connector.On("WatchCounterInt64", mock.Anything, "test/concurrent-counter").
			Return(updates, cleanup, nil)
		connector.On("Get", mock.Anything, ConnectorMainIndex, "test/concurrent-counter", "value", nil).
			Return([]byte("0"), nil)

		counter := registry.GetCounterInt64("concurrent-counter", 1024).(*counterInt64)
		time.Sleep(50 * time.Millisecond)

		// Spawn many goroutines all trying to SetLocalValue concurrently
		var wg sync.WaitGroup
		numGoroutines := 100
		for i := 0; i < numGoroutines; i++ {
			wg.Add(1)
			go func(val int64) {
				defer wg.Done()
				counter.SetLocalValue(val)
			}(int64(i + 1))
		}
		wg.Wait()

		// The final value should be the highest one attempted (100)
		finalValue := counter.GetValue()
		assert.Equal(t, int64(100), finalValue, "Final value should be the highest concurrent update")
	})

	t.Run("no regression when SetLocalValue and processNewValue race", func(t *testing.T) {
		// This test verifies the fix for the race condition between SetLocalValue
		// (which bypasses updateMu) and processNewValue (called from TryUpdate).
		// Before the fix, processNewValue could overwrite a higher SetLocalValue
		// with a lower value because it used Load-then-Store instead of CAS.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		connector := NewMockConnector("race-test")
		registry := &sharedStateRegistry{
			appCtx:          ctx,
			clusterKey:      "test",
			logger:          &log.Logger,
			connector:       connector,
			fallbackTimeout: time.Second,
			lockTtl:         30 * time.Second,
			lockMaxWait:     100 * time.Millisecond,
			initializer:     util.NewInitializer(ctx, &log.Logger, nil),
		}

		updates := make(chan int64, 10)
		cleanup := func() { close(updates) }

		connector.On("WatchCounterInt64", mock.Anything, "test/race-counter").
			Return(updates, cleanup, nil)
		connector.On("Get", mock.Anything, ConnectorMainIndex, "test/race-counter", "value", nil).
			Return([]byte("100"), nil)
		connector.On("Lock", mock.Anything, mock.Anything, mock.Anything).
			Return(nil, nil).Maybe()
		connector.On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
			Return(nil).Maybe()

		counter := registry.GetCounterInt64("race-counter", 1024).(*counterInt64)
		time.Sleep(50 * time.Millisecond)

		assert.Equal(t, int64(100), counter.GetValue())

		// Simulate the race: concurrent SetLocalValue (high value) and TryUpdate (lower value)
		// Run many iterations to increase chance of catching race
		for iteration := 0; iteration < 50; iteration++ {
			baseValue := int64(100 + iteration*10)
			counter.value.Store(baseValue)

			var wg sync.WaitGroup

			// Goroutine 1: SetLocalValue with higher value
			highValue := baseValue + 50
			wg.Add(1)
			go func() {
				defer wg.Done()
				counter.SetLocalValue(highValue)
			}()

			// Goroutine 2: TryUpdate with lower value (simulates upstream returning older block)
			lowerValue := baseValue + 20
			wg.Add(1)
			go func() {
				defer wg.Done()
				// Call processNewValue directly under lock (as TryUpdate does)
				counter.updateMu.Lock()
				counter.processNewValue("test", lowerValue)
				counter.updateMu.Unlock()
			}()

			wg.Wait()

			// The final value MUST be the higher one - never regress
			finalValue := counter.GetValue()
			assert.GreaterOrEqual(t, finalValue, highValue,
				"iteration %d: value regressed from %d to %d (lower TryUpdate value was %d)",
				iteration, highValue, finalValue, lowerValue)
		}
	})

	t.Run("processNewValue does not rollback SetLocalValue even from remote-sync", func(t *testing.T) {
		// This test verifies that processNewValue will NOT rollback a value set by
		// SetLocalValue, even when called with UpdateSourceRemoteSync (watch channel).
		// This is critical because SetLocalValue represents fresh RPC data that should
		// not be overwritten by potentially stale distributed state.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		connector := NewMockConnector("rollback-test")
		registry := &sharedStateRegistry{
			appCtx:          ctx,
			clusterKey:      "test",
			logger:          &log.Logger,
			connector:       connector,
			fallbackTimeout: time.Second,
			lockTtl:         30 * time.Second,
			lockMaxWait:     100 * time.Millisecond,
			initializer:     util.NewInitializer(ctx, &log.Logger, nil),
		}

		updates := make(chan int64, 10)
		cleanup := func() { close(updates) }

		connector.On("WatchCounterInt64", mock.Anything, "test/rollback-counter").
			Return(updates, cleanup, nil)
		connector.On("Get", mock.Anything, ConnectorMainIndex, "test/rollback-counter", "value", nil).
			Return([]byte("100"), nil)

		// Use small ignoreRollbackOf to ensure rollback logic is triggered
		counter := registry.GetCounterInt64("rollback-counter", 10).(*counterInt64)
		time.Sleep(50 * time.Millisecond)

		assert.Equal(t, int64(100), counter.GetValue())

		// Set a high value via SetLocalValue (simulates fresh RPC response)
		updated := counter.SetLocalValue(200)
		assert.True(t, updated)
		assert.Equal(t, int64(200), counter.GetValue())

		// Now simulate processNewValue with a lower value from various sources
		// that would normally trigger rollback (gap > ignoreRollbackOf)
		testSources := []string{
			UpdateSourceRemoteSync,       // watch channel
			UpdateSourceTryUpdate,        // async TryUpdate
			UpdateSourceTryUpdateIfStale, // polling
			UpdateSourceRemoteCheck,      // reconciliation
		}

		for _, source := range testSources {
			// Reset to 200
			counter.value.Store(200)

			// Call processNewValue with a much lower value (gap=100 > ignoreRollbackOf=10)
			counter.updateMu.Lock()
			counter.processNewValue(source, 100)
			counter.updateMu.Unlock()

			// Value should NOT have been rolled back
			assert.Equal(t, int64(200), counter.GetValue(),
				"source=%s: value should not be rolled back from 200 to 100", source)
		}
	})

	t.Run("rollback callbacks are still triggered even without applying rollback", func(t *testing.T) {
		// Verifies that rollback detection still triggers callbacks for monitoring/alerting
		// even though the actual value rollback is not applied.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		connector := NewMockConnector("callback-test")
		registry := &sharedStateRegistry{
			appCtx:          ctx,
			clusterKey:      "test",
			logger:          &log.Logger,
			connector:       connector,
			fallbackTimeout: time.Second,
			lockTtl:         30 * time.Second,
			initializer:     util.NewInitializer(ctx, &log.Logger, nil),
		}

		updates := make(chan int64, 10)
		cleanup := func() { close(updates) }

		connector.On("WatchCounterInt64", mock.Anything, "test/callback-counter").
			Return(updates, cleanup, nil)
		connector.On("Get", mock.Anything, ConnectorMainIndex, "test/callback-counter", "value", nil).
			Return([]byte("100"), nil)

		counter := registry.GetCounterInt64("callback-counter", 10).(*counterInt64)
		time.Sleep(50 * time.Millisecond)

		// Track rollback callback invocations
		var callbackMu sync.Mutex
		var callbackCalls []struct{ current, new int64 }
		counter.OnLargeRollback(func(currentVal, newVal int64) {
			callbackMu.Lock()
			callbackCalls = append(callbackCalls, struct{ current, new int64 }{currentVal, newVal})
			callbackMu.Unlock()
		})

		// Set high value
		counter.SetLocalValue(200)
		assert.Equal(t, int64(200), counter.GetValue())

		// Process a value that would trigger rollback detection
		counter.updateMu.Lock()
		counter.processNewValue(UpdateSourceRemoteSync, 100)
		counter.updateMu.Unlock()

		// Value should NOT be rolled back
		assert.Equal(t, int64(200), counter.GetValue())

		// But callback SHOULD have been triggered
		callbackMu.Lock()
		defer callbackMu.Unlock()
		assert.Len(t, callbackCalls, 1, "rollback callback should be triggered")
		if len(callbackCalls) > 0 {
			assert.Equal(t, int64(200), callbackCalls[0].current)
			assert.Equal(t, int64(100), callbackCalls[0].new)
		}
	})
}
