package data

import (
	"context"
	"errors"
	"fmt"
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

		updates := make(chan CounterInt64State, 10)
		cleanup := func() { close(updates) }

		// Setup mock expectations
		connector.On("WatchCounterInt64", mock.Anything, "test/counter1").
			Return(updates, cleanup, nil)

		connector.On("Get", mock.Anything, ConnectorMainIndex, "test/counter1", "value", nil).
			Return([]byte(`{"v":42,"t":1,"b":"test"}`), nil)

		// Get the counter and verify initial setup
		counter := registry.GetCounterInt64("counter1", 1024).(*counterInt64)
		time.Sleep(100 * time.Millisecond) // Allow goroutine to start

		// Verify initial value was fetched
		assert.Equal(t, int64(42), counter.GetValue())

		// Simulate remote update
		updates <- CounterInt64State{Value: 100, UpdatedAt: 2, UpdatedBy: "test"}
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

		updates := make(chan CounterInt64State, 10)
		cleanup := func() { close(updates) }

		connector.On("WatchCounterInt64", mock.Anything, "test/counter2").
			Return(updates, cleanup, nil)

		connector.On("Get", mock.Anything, ConnectorMainIndex, "test/counter2", "value", nil).
			Return([]byte(`{"v":0,"t":1,"b":"test"}`), nil)

		counter := registry.GetCounterInt64("counter2", 1024)

		// Setup callback
		callbackCalled := make(chan int64, 1)
		counter.OnValue(func(val int64) {
			callbackCalled <- val
		})

		// Simulate remote update
		updates <- CounterInt64State{Value: 50, UpdatedAt: 2, UpdatedBy: "test"}

		// Wait for callback
		select {
		case val := <-callbackCalled:
			assert.Equal(t, int64(50), val)
		case <-time.After(time.Second):
			t.Fatal("callback was not called")
		}
	})

	t.Run("remote updates respect ignoreRollbackOf (small rollbacks ignored, large applied)", func(t *testing.T) {
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

		updates := make(chan CounterInt64State, 10)
		cleanup := func() { close(updates) }

		connector.On("WatchCounterInt64", mock.Anything, "test/counter3").
			Return(updates, cleanup, nil)

		// Use realistic timestamps (current time in ms) for this test
		// The timestamp advancement fix uses time.Now().UnixMilli() so we need realistic values
		baseTs := time.Now().UnixMilli()
		connector.On("Get", mock.Anything, ConnectorMainIndex, "test/counter3", "value", nil).
			Return([]byte(fmt.Sprintf(`{"v":10000,"t":%d,"b":"test"}`, baseTs)), nil)

		// ignoreRollbackOf=1024 means ignore rollbacks <= 1024 blocks
		counter := registry.GetCounterInt64("counter3", 1024)
		time.Sleep(100 * time.Millisecond)

		// Verify initial value
		assert.Equal(t, int64(10000), counter.GetValue())

		// Small rollback (gap=500 <= 1024) should be IGNORED
		// Timestamp advances our local timestamp past this remote timestamp
		updates <- CounterInt64State{Value: 9500, UpdatedAt: baseTs + 100, UpdatedBy: "test"}
		time.Sleep(100 * time.Millisecond)
		assert.Equal(t, int64(10000), counter.GetValue(), "small rollback should be ignored")

		// Large rollback (gap=2000 > 1024) should be APPLIED (real reorg)
		// Use a timestamp that's definitely in the future to ensure it's not rejected as stale
		updates <- CounterInt64State{Value: 8000, UpdatedAt: time.Now().UnixMilli() + 1000, UpdatedBy: "test"}
		time.Sleep(100 * time.Millisecond)
		assert.Equal(t, int64(8000), counter.GetValue(), "large rollback should be applied")
	})

	t.Run("stale remote updates are rejected", func(t *testing.T) {
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

		updates := make(chan CounterInt64State, 10)
		cleanup := func() { close(updates) }

		connector.On("WatchCounterInt64", mock.Anything, "test/counter3b").
			Return(updates, cleanup, nil)

		// Initial value with timestamp 10
		connector.On("Get", mock.Anything, ConnectorMainIndex, "test/counter3b", "value", nil).
			Return([]byte(`{"v":10000,"t":10,"b":"test"}`), nil)

		counter := registry.GetCounterInt64("counter3b", 1024)
		time.Sleep(100 * time.Millisecond)

		// Verify initial value
		assert.Equal(t, int64(10000), counter.GetValue())

		// Remote update with STALE timestamp should be rejected
		updates <- CounterInt64State{Value: 8000, UpdatedAt: 5, UpdatedBy: "test"}
		time.Sleep(100 * time.Millisecond)

		// Value should remain unchanged because UpdatedAt: 5 < 10 (stale)
		assert.Equal(t, int64(10000), counter.GetValue())
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

		updates := make(chan CounterInt64State, 10)
		cleanup := func() {
			close(updates)
		}

		connector.On("WatchCounterInt64", mock.Anything, "test/counter4").
			Return(updates, cleanup, nil)

		connector.On("Get", mock.Anything, ConnectorMainIndex, "test/counter4", "value", nil).
			Return([]byte(`{"v":42,"t":1,"b":"test"}`), nil)

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

		updates := make(chan CounterInt64State, 10)
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
		updates <- CounterInt64State{Value: 100, UpdatedAt: 2, UpdatedBy: "test"}
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
		eventual       bool
		eventualValue  int64
		expectedCalls  int
		expectedRemote bool
	}{
		{
			name: "lock failure falls back to local",
			setupMocks: func(c *MockConnector, l *MockLock) {
				c.On("Lock", mock.Anything, "test", mock.Anything).
					Return(nil, errors.New("lock failed"))
				// Background push now publishes the local snapshot best-effort even when lock acquisition fails.
				c.On("PublishCounterInt64", mock.Anything, "test", mock.Anything).Return(nil)
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
				c.On("Set", mock.Anything, "test", "value", mock.Anything, mock.Anything).
					Return(errors.New("set failed"))
				// Best-effort publish happens before any remote reconciliation attempt.
				c.On("PublishCounterInt64", mock.Anything, "test", mock.Anything).Return(nil)
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
				remoteUpdatedAt := time.Now().UnixMilli() + 60_000
				c.On("Get", mock.Anything, ConnectorMainIndex, "test", "value", nil).
					Return([]byte(fmt.Sprintf(`{"v":15,"t":%d,"b":"test"}`, remoteUpdatedAt)), nil)
				// Background publish is best-effort.
				c.On("PublishCounterInt64", mock.Anything, "test", mock.Anything).Return(nil).Maybe()
			},
			initialValue: 5,
			updateValue:  10,
			// Foreground is local-only; background reconcile will eventually adopt/push 15.
			expectedValue:  10,
			eventual:       true,
			eventualValue:  15,
			expectedCalls:  1,
			expectedRemote: false,
		},
		{
			name: "successful remote update",
			setupMocks: func(c *MockConnector, l *MockLock) {
				c.On("Lock", mock.Anything, "test", mock.Anything).Return(l, nil)
				l.On("Unlock", mock.Anything).Return(nil)
				c.On("Get", mock.Anything, ConnectorMainIndex, "test", "value", nil).
					Return([]byte(`{"v":5,"t":1,"b":"test"}`), nil)
				c.On("Set", mock.Anything, "test", "value", mock.Anything, mock.Anything).Return(nil)
				c.On("PublishCounterInt64", mock.Anything, "test", mock.Anything).Return(nil)
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
			time.Sleep(50 * time.Millisecond)
			if tt.eventual {
				assert.Eventually(t, func() bool {
					return counter.GetValue() == tt.eventualValue
				}, 500*time.Millisecond, 10*time.Millisecond)
			}
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
		eventualValue int64
		eventual      bool
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
				pollLock := &MockLock{}
				c.On("Lock", mock.Anything, "test/poll", mock.Anything).Return(pollLock, nil)
				pollLock.On("Unlock", mock.Anything).Return(nil).Maybe()
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
				pollLock := &MockLock{}
				c.On("Lock", mock.Anything, "test/poll", mock.Anything).Return(pollLock, nil)
				pollLock.On("Unlock", mock.Anything).Return(nil).Maybe()
				c.On("Lock", mock.Anything, "test", mock.Anything).Return(l, nil)
				l.On("Unlock", mock.Anything).Return(nil)
				c.On("Get", mock.Anything, ConnectorMainIndex, "test", "value", nil).
					Return([]byte(`{"v":4,"t":1,"b":"test"}`), nil)
				c.On("Set", mock.Anything, "test", "value", mock.Anything, mock.Anything).Return(nil)
				c.On("PublishCounterInt64", mock.Anything, "test", mock.Anything).Return(nil)
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
				pollLock := &MockLock{}
				c.On("Lock", mock.Anything, "test/poll", mock.Anything).Return(pollLock, nil)
				pollLock.On("Unlock", mock.Anything).Return(nil).Maybe()
				c.On("Lock", mock.Anything, "test", mock.Anything).Return(l, nil)
				l.On("Unlock", mock.Anything).Return(nil)
				remoteUpdatedAt := time.Now().UnixMilli() + 60_000
				c.On("Get", mock.Anything, ConnectorMainIndex, "test", "value", nil).
					Return([]byte(fmt.Sprintf(`{"v":15,"t":%d,"b":"test"}`, remoteUpdatedAt)), nil)
				// Background push publishes local snapshot best-effort.
				c.On("PublishCounterInt64", mock.Anything, "test", mock.Anything).Return(nil)
			},
			initialValue:  5,
			staleness:     time.Second,
			updateValue:   10,
			lastProcessed: time.Now().Add(-2 * time.Second),
			// Foreground path is local-only: it returns the refresh result (10).
			// Remote reconciliation happens asynchronously and should eventually raise the local value to 15.
			expectedValue: 10,
			eventual:      true,
			eventualValue: 15,
			expectedError: nil,
			expectedCalls: 1,
		},
		{
			name: "update function returns error",
			setupMocks: func(c *MockConnector, l *MockLock) {
				pollLock := &MockLock{}
				c.On("Lock", mock.Anything, "test/poll", mock.Anything).Return(pollLock, nil)
				pollLock.On("Unlock", mock.Anything).Return(nil).Maybe()
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
			}
			counter.value.Store(tt.initialValue)
			counter.updatedAtUnixMs.Store(tt.lastProcessed.UnixMilli())

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
			// Give background goroutines (remote reconciliation/push) time to run when applicable.
			time.Sleep(100 * time.Millisecond)
			if tt.eventual {
				assert.Eventually(t, func() bool {
					return counter.GetValue() == tt.eventualValue
				}, 500*time.Millisecond, 10*time.Millisecond, "counter should eventually converge to the higher remote value")
			}
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

	// Remote reconciliation/push is best-effort and deduped; this test focuses on local correctness.
	// Allow background remote activity without asserting exact call counts (which are nondeterministic under coalescing).
	connector.On("Lock", mock.Anything, "test", mock.Anything).Return(lock, nil).Maybe()
	lock.On("Unlock", mock.Anything).Return(nil).Maybe()
	connector.On("Get", mock.Anything, ConnectorMainIndex, "test", "value", nil).
		Return([]byte(`{"v":5,"t":1,"b":"test"}`), nil).Maybe()
	connector.On("Set", mock.Anything, "test", "value", mock.Anything, mock.Anything).Return(nil).Maybe()
	connector.On("PublishCounterInt64", mock.Anything, "test", mock.Anything).Return(nil).Maybe()

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
			counter := &counterInt64{}
			counter.updatedAtUnixMs.Store(tt.lastProcessed.UnixMilli())
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
		eventual         bool
		eventualValue    int64
	}{
		{
			name: "remote lock fails, fallback local, higher update => update",
			setupMocks: func(conn *MockConnector, lock *MockLock) {
				// No Set call is expected because we fail to lock remotely
				conn.On("Lock", mock.Anything, "test", mock.Anything).
					Return(nil, errors.New("lock failed"))
				// Background push publishes best-effort even when the lock cannot be acquired.
				conn.On("PublishCounterInt64", mock.Anything, "test", mock.Anything).Return(nil)
			},
			initialValue:     5,
			updateValue:      10,
			ignoreRollbackOf: 1024,
			expectedValue:    10,
		},
		{
			name: "remote lock fails, fallback local, lower update within rollback range => ignored",
			setupMocks: func(conn *MockConnector, lock *MockLock) {
				// Small rollback within ignoreRollbackOf is silently ignored (no remote work)
			},
			initialValue:     100,
			updateValue:      95,
			ignoreRollbackOf: 10, // gap=5 <= 10, so rollback is ignored
			expectedValue:    100,
		},
		{
			name: "remote lock fails, fallback local, lower update exceeds rollback range => applied",
			setupMocks: func(conn *MockConnector, lock *MockLock) {
				// Large rollback exceeding ignoreRollbackOf is APPLIED
				conn.On("Lock", mock.Anything, "test", mock.Anything).
					Return(nil, errors.New("lock failed"))
				conn.On("PublishCounterInt64", mock.Anything, "test", mock.Anything).Return(nil)
			},
			initialValue:     100,
			updateValue:      50,
			ignoreRollbackOf: 40, // gap=50 > 40, so rollback is APPLIED
			expectedValue:    50,
		},
		{
			name: "remote lock succeeds, remote higher => override local; push reconciled value",
			setupMocks: func(conn *MockConnector, lock *MockLock) {
				conn.On("Lock", mock.Anything, "test", mock.Anything).Return(lock, nil)
				lock.On("Unlock", mock.Anything).Return(nil)
				conn.On("Get", mock.Anything, ConnectorMainIndex, "test", "value", nil).
					Return([]byte(fmt.Sprintf(`{"v":200,"t":%d,"b":"test"}`, time.Now().UnixMilli()+60_000)), nil)
				// Background publish is best-effort.
				conn.On("PublishCounterInt64", mock.Anything, "test", mock.Anything).Return(nil)
			},
			initialValue:     100,
			updateValue:      150,
			ignoreRollbackOf: 1024,
			// Foreground is local-only; background reconcile will eventually adopt/push 200.
			expectedValue: 150,
			eventual:      true,
			eventualValue: 200,
		},
		{
			name: "remote lock succeeds, remote is lower, new update is higher => triggers Set + Publish",
			setupMocks: func(conn *MockConnector, lock *MockLock) {
				conn.On("Lock", mock.Anything, "test", mock.Anything).Return(lock, nil)
				lock.On("Unlock", mock.Anything).Return(nil)
				// Remote has 100, local has 100, newValue=150 => we do an update
				conn.On("Get", mock.Anything, ConnectorMainIndex, "test", "value", nil).
					Return([]byte(`{"v":100,"t":1,"b":"test"}`), nil)
				// Because newValue (150) is higher, the code will call connector.Set
				conn.On("Set", mock.Anything, "test", "value", mock.Anything, mock.Anything).
					Return(nil)
				// And then PublishCounterInt64
				conn.On("PublishCounterInt64", mock.Anything, "test", mock.Anything).
					Return(nil)
			},
			initialValue:     100,
			updateValue:      150,
			ignoreRollbackOf: 1024,
			expectedValue:    150,
		},
		{
			name: "remote lock succeeds, remote is lower, newValue is also lower within rollback range => ignored",
			setupMocks: func(conn *MockConnector, lock *MockLock) {
				// Small rollback within ignoreRollbackOf is silently ignored (no remote work)
			},
			initialValue:     100,
			updateValue:      95,
			ignoreRollbackOf: 10, // gap=5 <= 10, so rollback is ignored
			expectedValue:    100,
		},
		{
			name: "remote lock succeeds, remote is lower, newValue exceeds rollback range => applied",
			setupMocks: func(conn *MockConnector, lock *MockLock) {
				// Large rollback exceeding ignoreRollbackOf is APPLIED
				conn.On("Lock", mock.Anything, "test", mock.Anything).Return(lock, nil)
				lock.On("Unlock", mock.Anything).Return(nil)
				conn.On("Get", mock.Anything, ConnectorMainIndex, "test", "value", nil).
					Return([]byte(`{"v":100,"t":1,"b":"test"}`), nil)
				conn.On("Set", mock.Anything, "test", "value", mock.Anything, mock.Anything).Return(nil)
				conn.On("PublishCounterInt64", mock.Anything, "test", mock.Anything).Return(nil)
			},
			initialValue:     100,
			updateValue:      50,
			ignoreRollbackOf: 40, // gap=50 > 40, so rollback is APPLIED
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
			counter.value.Store(tt.initialValue)

			actualValue := counter.TryUpdate(ctx, tt.updateValue)
			assert.Equal(t, tt.expectedValue, actualValue,
				"After TryUpdate(%d) with ignoreRollbackOf=%d, value should be %d",
				tt.updateValue, tt.ignoreRollbackOf, tt.expectedValue,
			)

			// Give async goroutines (the Set/Publish calls) time to complete
			if tt.eventual {
				assert.Eventually(t, func() bool {
					return counter.GetValue() == tt.eventualValue
				}, 500*time.Millisecond, 10*time.Millisecond)
			} else {
				time.Sleep(20 * time.Millisecond)
			}

			connector.AssertExpectations(t)
			lock.AssertExpectations(t)
		})
	}
}

func TestCounterInt64_TryUpdateIfStale_NoThunderingHerdEqualValue(t *testing.T) {
	ctx := context.Background()
	registry, connector, _ := setupTest("my-dev")

	pollLock := &MockLock{}
	connector.On("Lock", mock.Anything, "test/poll", mock.Anything).Return(pollLock, nil).Maybe()
	pollLock.On("Unlock", mock.Anything).Return(nil).Maybe()

	counter := &counterInt64{
		registry:         registry,
		key:              "test",
		ignoreRollbackOf: 1024,
	}
	counter.value.Store(5) // Initialize with value 5
	// Force it to look stale.
	counter.updatedAtUnixMs.Store(time.Now().Add(-2 * time.Second).UnixMilli())

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

	pollLock := &MockLock{}
	connector.On("Lock", mock.Anything, "test/poll", mock.Anything).Return(pollLock, nil).Maybe()
	pollLock.On("Unlock", mock.Anything).Return(nil).Maybe()

	counter := &counterInt64{
		registry:         registry,
		key:              "test",
		ignoreRollbackOf: 1024,
	}
	counter.value.Store(5)                                                      // Initialize with value 5
	counter.updatedAtUnixMs.Store(time.Now().Add(-2 * time.Second).UnixMilli()) // force stale

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
		}
		counter.updatedAtUnixMs.Store(time.Now().Add(-2 * time.Second).UnixMilli()) // Make it stale
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

		// Background push publishes best-effort for fast propagation.
		connector.On("PublishCounterInt64", mock.Anything, "test/timeout-counter", mock.Anything).Return(nil).Maybe()

		// Setup mock for Get to return not found initially
		connector.On("Get", mock.Anything, ConnectorMainIndex, "test/timeout-counter", "value", nil).
			Return(nil, common.NewErrRecordNotFound("test/timeout-counter", "value", "mock"))

		// Setup mock for WatchCounterInt64
		updates := make(chan CounterInt64State, 10)
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

		// Background push publishes best-effort for fast propagation.
		connector.On("PublishCounterInt64", mock.Anything, "test/update-counter", mock.Anything).Return(nil).Maybe()

		// Setup other required mocks
		connector.On("Get", mock.Anything, ConnectorMainIndex, "test/update-counter", "value", nil).
			Return(nil, common.NewErrRecordNotFound("test/update-counter", "value", "mock"))

		updates := make(chan CounterInt64State, 10)
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

	t.Run("rollback callbacks are triggered when large rollback is applied", func(t *testing.T) {
		// Verifies that rollback detection triggers callbacks for monitoring/alerting
		// when a large rollback (gap > ignoreRollbackOf) is APPLIED.
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

		updates := make(chan CounterInt64State, 10)
		cleanup := func() { close(updates) }

		connector.On("WatchCounterInt64", mock.Anything, "test/callback-counter").
			Return(updates, cleanup, nil)
		connector.On("Get", mock.Anything, ConnectorMainIndex, "test/callback-counter", "value", nil).
			Return([]byte(`{"v":200,"t":1,"b":"test"}`), nil)

		// ignoreRollbackOf=10 means small rollbacks (<= 10) are ignored
		counter := registry.GetCounterInt64("callback-counter", 10).(*counterInt64)
		time.Sleep(50 * time.Millisecond)

		assert.Equal(t, int64(200), counter.GetValue())

		// Track rollback callback invocations
		var callbackMu sync.Mutex
		var callbackCalls []struct{ current, new int64 }
		counter.OnLargeRollback(func(currentVal, newVal int64) {
			callbackMu.Lock()
			callbackCalls = append(callbackCalls, struct{ current, new int64 }{currentVal, newVal})
			callbackMu.Unlock()
		})

		// Process a value with large rollback (gap=100 > ignoreRollbackOf=10)
		counter.processNewValue(UpdateSourceRemoteSync, 100)

		// Value SHOULD be rolled back (large rollback is applied)
		assert.Equal(t, int64(100), counter.GetValue())

		// And callback SHOULD have been triggered
		callbackMu.Lock()
		defer callbackMu.Unlock()
		assert.Len(t, callbackCalls, 1, "rollback callback should be triggered")
		if len(callbackCalls) > 0 {
			assert.Equal(t, int64(200), callbackCalls[0].current)
			assert.Equal(t, int64(100), callbackCalls[0].new)
		}
	})
}

// TestCounterInt64_EarliestBlockSemantics tests that earliest block counters (ignoreRollbackOf=0)
// correctly accept decreases, which is needed when detection finds an earlier available block.
func TestCounterInt64_EarliestBlockSemantics(t *testing.T) {
	t.Run("earliest block accepts decrease via local update (processNewValue)", func(t *testing.T) {
		counter := &counterInt64{
			ignoreRollbackOf: 0, // earliest block mode: accept all rollbacks
			registry: &sharedStateRegistry{
				logger: &log.Logger,
			},
		}
		counter.value.Store(1500) // Initial earliest block

		// Simulate detection that earliest is actually lower (e.g., re-detection found earlier block)
		// This is a DECREASE: 1500 → 1000
		updated := counter.processNewValue(UpdateSourceTryUpdate, 1000)
		assert.True(t, updated, "should accept decrease with ignoreRollbackOf=0")
		assert.Equal(t, int64(1000), counter.GetValue())

		// Also accepts increases (forward progress due to pruning)
		updated = counter.processNewValue(UpdateSourceTryUpdate, 2000)
		assert.True(t, updated, "should also accept increases")
		assert.Equal(t, int64(2000), counter.GetValue())
	})

	t.Run("earliest block accepts decrease via remote update (processNewState)", func(t *testing.T) {
		counter := &counterInt64{
			ignoreRollbackOf: 0, // earliest block mode: accept all rollbacks
			registry: &sharedStateRegistry{
				logger:     &log.Logger,
				instanceId: "test",
			},
		}
		counter.value.Store(1500)
		counter.updatedAtUnixMs.Store(1)

		// Remote update with fresher timestamp and lower value
		// This is a DECREASE: 1500 → 1000
		updated := counter.processNewState(UpdateSourceRemoteSync, CounterInt64State{
			Value:     1000, // Decrease from 1500
			UpdatedAt: 2,    // Fresher timestamp
			UpdatedBy: "remote-instance",
		})
		assert.True(t, updated, "should accept decrease with ignoreRollbackOf=0 via remote")
		assert.Equal(t, int64(1000), counter.GetValue())
	})

	t.Run("earliest block remote decrease rejected if timestamp is stale", func(t *testing.T) {
		counter := &counterInt64{
			ignoreRollbackOf: 0, // earliest block mode
			registry: &sharedStateRegistry{
				logger:     &log.Logger,
				instanceId: "test",
			},
		}
		counter.value.Store(1000)
		counter.updatedAtUnixMs.Store(10) // Local has timestamp 10

		// Remote update with STALE timestamp (should be rejected regardless of value)
		updated := counter.processNewState(UpdateSourceRemoteSync, CounterInt64State{
			Value:     500, // Even though it's a valid decrease
			UpdatedAt: 5,   // STALE - older than local
			UpdatedBy: "remote-instance",
		})
		assert.False(t, updated, "should reject stale timestamp even for ignoreRollbackOf=0")
		assert.Equal(t, int64(1000), counter.GetValue())
	})
}

// TestCounterInt64_RemoteRollbackCallback tests that large rollback callbacks are triggered
// via remote updates (processNewState) when the rollback is APPLIED.
func TestCounterInt64_RemoteRollbackCallback(t *testing.T) {
	t.Run("large rollback applied and callback fires on remote update", func(t *testing.T) {
		counter := &counterInt64{
			ignoreRollbackOf: 100, // Ignore rollbacks <= 100
			registry: &sharedStateRegistry{
				logger:     &log.Logger,
				instanceId: "test",
			},
		}
		counter.value.Store(10000)
		counter.updatedAtUnixMs.Store(1)

		var callbackMu sync.Mutex
		var callbackCalls []struct{ current, new int64 }
		counter.OnLargeRollback(func(currentVal, newVal int64) {
			callbackMu.Lock()
			callbackCalls = append(callbackCalls, struct{ current, new int64 }{currentVal, newVal})
			callbackMu.Unlock()
		})

		// Remote update with large rollback (gap=5000 > ignoreRollbackOf=100)
		updated := counter.processNewState(UpdateSourceRemoteSync, CounterInt64State{
			Value:     5000, // 5000 blocks rollback
			UpdatedAt: 2,    // Fresher timestamp
			UpdatedBy: "remote-instance",
		})

		// Value SHOULD be rolled back (large rollback is applied)
		assert.True(t, updated)
		assert.Equal(t, int64(5000), counter.GetValue())

		// And callback SHOULD have been triggered
		callbackMu.Lock()
		defer callbackMu.Unlock()
		assert.Len(t, callbackCalls, 1, "rollback callback should fire on remote large rollback")
		if len(callbackCalls) > 0 {
			assert.Equal(t, int64(10000), callbackCalls[0].current)
			assert.Equal(t, int64(5000), callbackCalls[0].new)
		}
	})

	t.Run("small rollback ignored and callback does NOT fire", func(t *testing.T) {
		counter := &counterInt64{
			ignoreRollbackOf: 100, // Ignore rollbacks <= 100
			registry: &sharedStateRegistry{
				logger:     &log.Logger,
				instanceId: "test",
			},
		}
		counter.value.Store(10000)
		counter.updatedAtUnixMs.Store(1)

		var callbackCalls []struct{ current, new int64 }
		counter.OnLargeRollback(func(currentVal, newVal int64) {
			callbackCalls = append(callbackCalls, struct{ current, new int64 }{currentVal, newVal})
		})

		// Remote update with small rollback (gap=50 <= ignoreRollbackOf=100)
		updated := counter.processNewState(UpdateSourceRemoteSync, CounterInt64State{
			Value:     9950, // Only 50 blocks rollback
			UpdatedAt: 2,
			UpdatedBy: "remote-instance",
		})

		// Value should NOT be rolled back (small rollback ignored)
		assert.False(t, updated)
		assert.Equal(t, int64(10000), counter.GetValue())
		assert.Len(t, callbackCalls, 0, "callback should not fire for small rollback")
	})

	// This test verifies the fix for a bug where small rollback rejection would not update
	// the local timestamp correctly, causing reconciliation to fail to push back the higher local value.
	// The fix: when rejecting a small rollback from remote with newer timestamp, advance local
	// timestamp past remote's so our higher value wins reconciliation.
	t.Run("small rollback rejected advances timestamp past remote (reconciliation bug fix)", func(t *testing.T) {
		counter := &counterInt64{
			ignoreRollbackOf: 100, // Ignore rollbacks <= 100
			registry: &sharedStateRegistry{
				logger:     &log.Logger,
				instanceId: "test",
			},
		}
		// Local has value=10000 with timestamp=50
		counter.value.Store(10000)
		counter.updatedAtUnixMs.Store(50)

		// Remote sends a small rollback with newer timestamp
		// Value: 9950 (gap=50 <= 100, small rollback)
		// UpdatedAt: 100 (newer than local's 50)
		remoteState := CounterInt64State{
			Value:     9950,
			UpdatedAt: 100,
			UpdatedBy: "remote-instance",
		}

		updated := counter.processNewState(UpdateSourceRemoteSync, remoteState)

		// Value should NOT be rolled back (small rollback ignored)
		assert.False(t, updated)
		assert.Equal(t, int64(10000), counter.GetValue())

		// CRITICAL: Timestamp should be ADVANCED past remote's timestamp!
		// This ensures our higher local value wins reconciliation:
		//   local.UpdatedAt (>100) > remote.UpdatedAt (100) => TRUE
		// And the higher local value WILL be pushed back to remote.
		localTs := counter.updatedAtMs()
		assert.Greater(t, localTs, int64(100), "timestamp should be advanced past remote's when small rollback is rejected")
	})

	t.Run("large rollback accepted DOES update timestamp", func(t *testing.T) {
		counter := &counterInt64{
			ignoreRollbackOf: 100, // Ignore rollbacks <= 100
			registry: &sharedStateRegistry{
				logger:     &log.Logger,
				instanceId: "test",
			},
		}
		// Local has value=10000 with timestamp=50
		counter.value.Store(10000)
		counter.updatedAtUnixMs.Store(50)

		// Remote sends a large rollback with newer timestamp
		// Value: 5000 (gap=5000 > 100, large rollback - should be accepted)
		// UpdatedAt: 100 (newer than local's 50)
		remoteState := CounterInt64State{
			Value:     5000,
			UpdatedAt: 100,
			UpdatedBy: "remote-instance",
		}

		updated := counter.processNewState(UpdateSourceRemoteSync, remoteState)

		// Value should be rolled back (large rollback accepted)
		assert.True(t, updated)
		assert.Equal(t, int64(5000), counter.GetValue())

		// Timestamp SHOULD be updated since we accepted the value
		localTs := counter.updatedAtMs()
		assert.Equal(t, int64(100), localTs, "timestamp should be updated when large rollback is accepted")
	})

	t.Run("forward progress update DOES update timestamp", func(t *testing.T) {
		counter := &counterInt64{
			ignoreRollbackOf: 100,
			registry: &sharedStateRegistry{
				logger:     &log.Logger,
				instanceId: "test",
			},
		}
		// Local has value=10000 with timestamp=50
		counter.value.Store(10000)
		counter.updatedAtUnixMs.Store(50)

		// Remote sends a higher value with newer timestamp
		remoteState := CounterInt64State{
			Value:     11000,
			UpdatedAt: 100,
			UpdatedBy: "remote-instance",
		}

		updated := counter.processNewState(UpdateSourceRemoteSync, remoteState)

		// Value should be updated (forward progress)
		assert.True(t, updated)
		assert.Equal(t, int64(11000), counter.GetValue())

		// Timestamp SHOULD be updated since we accepted the value
		localTs := counter.updatedAtMs()
		assert.Equal(t, int64(100), localTs, "timestamp should be updated when value increases")
	})

	t.Run("same value update DOES update timestamp", func(t *testing.T) {
		counter := &counterInt64{
			ignoreRollbackOf: 100,
			registry: &sharedStateRegistry{
				logger:     &log.Logger,
				instanceId: "test",
			},
		}
		// Local has value=10000 with timestamp=50
		counter.value.Store(10000)
		counter.updatedAtUnixMs.Store(50)

		// Remote sends same value with newer timestamp
		remoteState := CounterInt64State{
			Value:     10000,
			UpdatedAt: 100,
			UpdatedBy: "remote-instance",
		}

		updated := counter.processNewState(UpdateSourceRemoteSync, remoteState)

		// Value not changed, but should return false and update timestamp
		assert.False(t, updated)
		assert.Equal(t, int64(10000), counter.GetValue())

		// Timestamp SHOULD be updated to mark as fresh since remote is newer
		localTs := counter.updatedAtMs()
		assert.Equal(t, int64(100), localTs, "timestamp should be updated when same value but newer remote timestamp")
	})
}

// TestCounterInt64_TimestampCollision tests that allocateUpdatedAtMs produces unique timestamps
// even when called rapidly within the same millisecond.
func TestCounterInt64_TimestampCollision(t *testing.T) {
	t.Run("allocateUpdatedAtMs produces monotonically increasing timestamps", func(t *testing.T) {
		base := &baseSharedVariable{}

		// Rapidly allocate many timestamps
		timestamps := make([]int64, 1000)
		for i := 0; i < 1000; i++ {
			timestamps[i] = base.allocateUpdatedAtMs()
		}

		// All should be strictly increasing
		for i := 1; i < len(timestamps); i++ {
			assert.Greater(t, timestamps[i], timestamps[i-1],
				"timestamp %d should be > timestamp %d", i, i-1)
		}
	})

	t.Run("concurrent allocations produce unique timestamps", func(t *testing.T) {
		base := &baseSharedVariable{}

		var wg sync.WaitGroup
		results := make(chan int64, 100)

		// 10 goroutines each allocate 10 timestamps
		for g := 0; g < 10; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 10; i++ {
					results <- base.allocateUpdatedAtMs()
				}
			}()
		}
		wg.Wait()
		close(results)

		// Collect all timestamps
		seen := make(map[int64]bool)
		for ts := range results {
			assert.False(t, seen[ts], "duplicate timestamp detected: %d", ts)
			seen[ts] = true
		}
		assert.Equal(t, 100, len(seen), "should have 100 unique timestamps")
	})
}

// TestCounterInt64_BackgroundPushCoalescing tests that multiple rapid TryUpdate calls
// are coalesced into fewer remote pushes.
func TestCounterInt64_BackgroundPushCoalescing(t *testing.T) {
	t.Run("rapid updates are coalesced to fewer pushes", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		connector := NewMockConnector("coalesce-test")
		lock := &MockLock{}
		registry := &sharedStateRegistry{
			appCtx:          ctx,
			clusterKey:      "test",
			instanceId:      "test-instance",
			logger:          &log.Logger,
			connector:       connector,
			fallbackTimeout: time.Second,
			lockTtl:         30 * time.Second,
			lockMaxWait:     100 * time.Millisecond,
			initializer:     util.NewInitializer(ctx, &log.Logger, nil),
		}

		// Allow any number of publishes (coalesced)
		var publishCount atomic.Int32
		connector.On("PublishCounterInt64", mock.Anything, "test/coalesce-counter", mock.Anything).
			Run(func(args mock.Arguments) {
				publishCount.Add(1)
			}).Return(nil).Maybe()

		// Allow lock acquisition
		connector.On("Lock", mock.Anything, "test/coalesce-counter", mock.Anything).
			Return(lock, nil).Maybe()
		lock.On("Unlock", mock.Anything).Return(nil).Maybe()
		connector.On("Get", mock.Anything, ConnectorMainIndex, "test/coalesce-counter", "value", nil).
			Return([]byte(`{"v":0,"t":1,"b":"test"}`), nil).Maybe()
		connector.On("Set", mock.Anything, "test/coalesce-counter", "value", mock.Anything, mock.Anything).
			Return(nil).Maybe()

		counter := &counterInt64{
			registry:         registry,
			key:              "test/coalesce-counter",
			ignoreRollbackOf: 1024,
		}

		// Rapidly fire 100 updates
		for i := 1; i <= 100; i++ {
			counter.TryUpdate(ctx, int64(i))
		}

		// Wait for background pushes to complete
		time.Sleep(300 * time.Millisecond)

		// Final value should be 100
		assert.Equal(t, int64(100), counter.GetValue())

		// Publishes should be far fewer than 100 due to coalescing
		// The exact number varies but should definitely be < 50
		publishes := publishCount.Load()
		t.Logf("Total publishes for 100 updates: %d", publishes)
		assert.Less(t, publishes, int32(50), "publishes should be coalesced, got %d", publishes)
	})
}

// TestCounterInt64_SmallRollbackReconciliation tests that when a small rollback is rejected,
// the higher local value is still pushed to remote during background reconciliation.
// This is the integration test for the timestamp advancement fix.
func TestCounterInt64_SmallRollbackReconciliation(t *testing.T) {
	t.Run("rejected small rollback triggers push of higher local value to remote", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		connector := NewMockConnector("small-rollback-reconcile-test")
		lock := &MockLock{}
		registry := &sharedStateRegistry{
			appCtx:          ctx,
			clusterKey:      "test",
			instanceId:      "test-instance",
			logger:          &log.Logger,
			connector:       connector,
			fallbackTimeout: time.Second,
			lockTtl:         30 * time.Second,
			lockMaxWait:     100 * time.Millisecond,
			initializer:     util.NewInitializer(ctx, &log.Logger, nil),
		}

		// Track values pushed to remote
		var pushedValues []int64
		var pushedMu sync.Mutex

		connector.On("PublishCounterInt64", mock.Anything, "test/rollback-counter", mock.Anything).
			Run(func(args mock.Arguments) {
				st := args.Get(2).(CounterInt64State)
				pushedMu.Lock()
				pushedValues = append(pushedValues, st.Value)
				pushedMu.Unlock()
			}).Return(nil).Maybe()

		connector.On("Lock", mock.Anything, "test/rollback-counter", mock.Anything).
			Return(lock, nil)
		lock.On("Unlock", mock.Anything).Return(nil)

		// Remote has lower value with NEWER timestamp (simulates another instance reporting lower value)
		// This is the scenario: remote has 9950 (t=100), local has 10000 (t=50)
		// Small rollback gap=50 <= ignoreRollbackOf=100, so rollback is rejected
		// But we should still push our higher value to remote
		connector.On("Get", mock.Anything, ConnectorMainIndex, "test/rollback-counter", "value", nil).
			Return([]byte(`{"v":9950,"t":100,"b":"other-instance"}`), nil)

		// Track if Set was called with our higher value
		var setCallValue int64
		var setCallCount int
		var setCallMu sync.Mutex

		connector.On("Set", mock.Anything, "test/rollback-counter", "value", mock.Anything, mock.Anything).
			Run(func(args mock.Arguments) {
				payload := args.Get(3).([]byte)
				var st CounterInt64State
				if err := common.SonicCfg.Unmarshal(payload, &st); err == nil {
					setCallMu.Lock()
					setCallValue = st.Value
					setCallCount++
					setCallMu.Unlock()
				}
			}).Return(nil)

		counter := &counterInt64{
			registry:         registry,
			key:              "test/rollback-counter",
			ignoreRollbackOf: 100, // Ignore rollbacks <= 100
		}
		// Local has higher value but older timestamp
		counter.value.Store(10000)
		counter.updatedAtUnixMs.Store(50) // Older than remote's 100

		// Trigger background push - this should:
		// 1. See remote.UpdatedAt (100) > local.UpdatedAt (50)
		// 2. Try to adopt remote's lower value (9950)
		// 3. Reject it as small rollback (gap=50 <= 100)
		// 4. Advance local timestamp past 100
		// 5. Push local's higher value (10000) to remote
		counter.scheduleBackgroundPushCurrent()

		// Wait for push to complete
		time.Sleep(300 * time.Millisecond)

		// Verify local value is unchanged (rollback rejected)
		assert.Equal(t, int64(10000), counter.GetValue())

		// Verify local timestamp was advanced past remote's
		localTs := counter.updatedAtMs()
		assert.Greater(t, localTs, int64(100), "local timestamp should be advanced past remote's")

		// CRITICAL: Verify Set was called with our higher local value
		setCallMu.Lock()
		assert.Equal(t, int64(10000), setCallValue, "should push higher local value to remote")
		assert.Greater(t, setCallCount, 0, "Set should have been called")
		setCallMu.Unlock()
	})
}

// TestCounterInt64_FresherLocalPushesToStaleRemote tests that when local state is fresher
// than remote, it gets pushed to remote during background reconciliation.
func TestCounterInt64_FresherLocalPushesToStaleRemote(t *testing.T) {
	t.Run("fresher local state is pushed to remote", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		connector := NewMockConnector("fresher-local-test")
		lock := &MockLock{}
		registry := &sharedStateRegistry{
			appCtx:          ctx,
			clusterKey:      "test",
			instanceId:      "test-instance",
			logger:          &log.Logger,
			connector:       connector,
			fallbackTimeout: time.Second,
			lockTtl:         30 * time.Second,
			lockMaxWait:     100 * time.Millisecond,
			initializer:     util.NewInitializer(ctx, &log.Logger, nil),
		}

		// Track if Set was called with our fresher value
		var setCallValue int64
		var setCallMu sync.Mutex

		connector.On("PublishCounterInt64", mock.Anything, "test/fresher-counter", mock.Anything).
			Return(nil).Maybe()
		connector.On("Lock", mock.Anything, "test/fresher-counter", mock.Anything).
			Return(lock, nil)
		lock.On("Unlock", mock.Anything).Return(nil)

		// Remote has stale state (updatedAt=1)
		connector.On("Get", mock.Anything, ConnectorMainIndex, "test/fresher-counter", "value", nil).
			Return([]byte(`{"v":50,"t":1,"b":"stale-instance"}`), nil)

		connector.On("Set", mock.Anything, "test/fresher-counter", "value", mock.Anything, mock.Anything).
			Run(func(args mock.Arguments) {
				payload := args.Get(3).([]byte)
				var st CounterInt64State
				if err := common.SonicCfg.Unmarshal(payload, &st); err == nil {
					setCallMu.Lock()
					setCallValue = st.Value
					setCallMu.Unlock()
				}
			}).Return(nil)

		counter := &counterInt64{
			registry:         registry,
			key:              "test/fresher-counter",
			ignoreRollbackOf: 1024,
		}
		// Local has fresher state
		counter.value.Store(100)
		counter.updatedAtUnixMs.Store(time.Now().UnixMilli()) // Much fresher than remote's t=1

		// Trigger background push
		counter.scheduleBackgroundPushCurrent()

		// Wait for push to complete
		time.Sleep(200 * time.Millisecond)

		// Verify Set was called with our fresher local value
		setCallMu.Lock()
		assert.Equal(t, int64(100), setCallValue, "should push fresher local value to remote")
		setCallMu.Unlock()
	})
}
