package data

import (
	"context"
	"errors"
	"strconv"
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
				registry: registry,
				key:      "test",
				value:    atomic.Int64{},
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
				registry: registry,
				key:      "test",
				value:    atomic.Int64{},
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
		registry: registry,
		key:      "test",
		value:    atomic.Int64{},
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
		value: atomic.Int64{},
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

func TestCounterInt64_TryUpdate_DriftLogic(t *testing.T) {
	type testCase struct {
		name            string
		lockShouldFail  bool
		remoteValue     string // empty string => treat as not found or skip
		initialValue    int64
		newValue        int64
		maxAllowedDrift int64
		expectedValue   int64
	}

	tests := []testCase{
		{
			name:            "local fallback: newValue is bigger => adopt newValue",
			lockShouldFail:  true, // force local fallback
			remoteValue:     "",   // won't be used, as lock fails
			initialValue:    100,
			newValue:        120,
			maxAllowedDrift: 10,
			expectedValue:   120,
		},
		{
			name:            "local fallback: newValue is smaller but difference <= maxAllowedDrift => keep current",
			lockShouldFail:  true,
			remoteValue:     "",
			initialValue:    100,
			newValue:        95,
			maxAllowedDrift: 10,  // difference = 5 <= 10
			expectedValue:   100, // keeps the old value
		},
		{
			name:            "local fallback: newValue is smaller and difference > maxAllowedDrift => adopt newValue",
			lockShouldFail:  true,
			remoteValue:     "",
			initialValue:    100,
			newValue:        80,
			maxAllowedDrift: 10, // difference = 20 > 10
			expectedValue:   80,
		},
		{
			name:            "remote lock: newValue is bigger => adopt newValue",
			lockShouldFail:  false,
			remoteValue:     "", // remote read returns nothing or not found
			initialValue:    50,
			newValue:        60,
			maxAllowedDrift: 10,
			expectedValue:   60,
		},
		{
			name:            "remote lock: newValue is smaller but difference <= maxAllowedDrift => keep current",
			lockShouldFail:  false,
			remoteValue:     "", // remote read returns nothing or not found
			initialValue:    50,
			newValue:        45, // difference = 5
			maxAllowedDrift: 10,
			expectedValue:   50,
		},
		{
			name:            "remote lock: newValue is smaller and difference > maxAllowedDrift => adopt newValue",
			lockShouldFail:  false,
			remoteValue:     "", // remote read returns nothing or not found
			initialValue:    50,
			newValue:        35, // difference = 15 > 10
			maxAllowedDrift: 10,
			expectedValue:   35,
		},
		{
			name:            "remote lock: also compare remoteValue if it's higher than both",
			lockShouldFail:  false,
			remoteValue:     "70", // simulate a remote read
			initialValue:    50,
			newValue:        60, // newValue < remoteValue
			maxAllowedDrift: 10,
			expectedValue:   70, // remote is highest
		},
		{
			name:            "remote lock: remoteValue is lower, newValue triggers drift downward",
			lockShouldFail:  false,
			remoteValue:     "20", // remote is lower than current
			initialValue:    50,
			newValue:        30, // difference=20 > 10 => adopt
			maxAllowedDrift: 10,
			// Among local(50), remote(20), new(30):
			//   - newValue is not bigger than local
			//   - difference between local(50) and newValue(30)=20 > 10 => adopt newValue
			// So final = 30.
			expectedValue: 30,
		},
		{
			name:            "remote lock: remoteValue is lower, newValue difference < maxAllowedDrift => keep current",
			lockShouldFail:  false,
			remoteValue:     "20", // remote is lower
			initialValue:    50,
			newValue:        45, // difference=5 <= 10 => keep old
			maxAllowedDrift: 10,
			expectedValue:   50,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry, connector, _ := setupTest("my-dev")
			lock := &MockLock{}

			// If lockShouldFail is true, simulate a lock error => local fallback path
			// Otherwise, simulate a successful lock => remote path
			if tt.lockShouldFail {
				connector.On("Lock", mock.Anything, "test-drift", mock.Anything).
					Return(nil, errors.New("lock failed"))
			} else {
				connector.On("Lock", mock.Anything, "test-drift", mock.Anything).
					Return(lock, nil)
				lock.On("Unlock", mock.Anything).Return(nil)

				// If we have a remoteValue, mock the Get call
				if tt.remoteValue != "" {
					connector.On("Get", mock.Anything, ConnectorMainIndex, "test-drift", "value").
						Return(tt.remoteValue, nil)
				} else {
					// Pretend either record not found or empty string
					connector.On("Get", mock.Anything, ConnectorMainIndex, "test-drift", "value").
						Return("", common.NewErrRecordNotFound("test-drift", "value", "mock")).
						Maybe()
				}
			}

			// If we expect to adopt newValue (or any new highestValue),
			// the code tries an async Set/Publish if the updated value changes.
			// If the final value equals the old value, no remote update is triggered.
			// So we only expect a Set call if the final value != initialValue.
			// This is just to be consistent with typical usage.
			if tt.expectedValue != tt.initialValue ||
				(tt.lockShouldFail == false && tt.remoteValue != "" && tt.expectedValue != mustAtoi64(tt.remoteValue)) {
				// Because after we decide the final value, we'll do an async "Set" + "PublishCounterInt64"
				// The exact calls can vary; you can refine if you want to confirm the exact arguments.
				connector.On("Set", mock.Anything, "test-drift", "value", mock.Anything, mock.Anything).
					Return(nil).Maybe()
				connector.On("PublishCounterInt64", mock.Anything, "test-drift", mock.Anything).
					Return(nil).Maybe()
			}

			counter := &counterInt64{
				registry:        registry,
				key:             "test-drift",
				value:           atomic.Int64{},
				maxAllowedDrift: tt.maxAllowedDrift,
			}
			counter.value.Store(tt.initialValue)

			result := counter.TryUpdate(context.Background(), tt.newValue)
			assert.Equal(t, tt.expectedValue, result)

			// Short wait to let any async goroutines finish the "Set" / "Publish" calls
			time.Sleep(100 * time.Millisecond)

			// Validate all expected calls
			connector.AssertExpectations(t)
			lock.AssertExpectations(t)
		})
	}
}

func mustAtoi64(s string) int64 {
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		panic(err)
	}
	return v
}
