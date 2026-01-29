package data

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"
)

// TestConcurrentOperationsNoDeadlock verifies that concurrent TryUpdate operations
func TestConcurrentOperationsNoDeadlock(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cfg := &common.SharedStateConfig{
		ClusterKey: "test",
		Connector: &common.ConnectorConfig{
			Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{
				MaxItems: 100,
			},
		},
		LockTtl:       common.Duration(500 * time.Millisecond),
		LockMaxWait:   common.Duration(200 * time.Millisecond),
		UpdateMaxWait: common.Duration(200 * time.Millisecond),
	}
	cfg.SetDefaults("test")

	ssr, err := NewSharedStateRegistry(ctx, &log.Logger, cfg)
	require.NoError(t, err)

	ctr := ssr.GetCounterInt64("concurrent-test", 1024).(*counterInt64)
	ctr.value.Store(10)

	// Run many concurrent operations
	var wg sync.WaitGroup
	completed := &atomic.Int32{}

	// Start multiple TryUpdate operations
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				result := ctr.TryUpdate(ctx, int64(id*100+j))
				if result > 0 {
					completed.Add(1)
				}
				time.Sleep(5 * time.Millisecond)
			}
		}(i)
	}

	// Start multiple TryUpdateIfStale operations
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				result, _ := ctr.TryUpdateIfStale(ctx, 200*time.Millisecond, func(ctx context.Context) (int64, error) {
					current := ctr.value.Load()
					return current + int64(id+1), nil
				})
				if result > 0 {
					completed.Add(1)
				}
				time.Sleep(7 * time.Millisecond)
			}
		}(i)
	}

	// Wait for all operations with timeout
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		count := completed.Load()
		t.Logf("All operations completed successfully! Total successful updates: %d", count)
		t.Log("✓ No deadlock detected - the fix is working!")
	case <-time.After(5 * time.Second):
		t.Error("Operations timed out after 5 seconds - possible deadlock!")
		t.Log("This should not happen after the fix")
	}
}

// TestLockOrderSimulation_OldBuggyBehavior demonstrates what WOULD happen with the old buggy lock ordering
// This test simulates the OLD behavior to show it causes issues
func TestLockOrderSimulation_OldBuggyBehavior(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cfg := &common.SharedStateConfig{
		ClusterKey: "test",
		Connector: &common.ConnectorConfig{
			Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{
				MaxItems: 100,
			},
		},
		LockTtl:       common.Duration(2 * time.Second),
		LockMaxWait:   common.Duration(500 * time.Millisecond),
		UpdateMaxWait: common.Duration(500 * time.Millisecond),
	}
	cfg.SetDefaults("test")

	ssr, err := NewSharedStateRegistry(ctx, &log.Logger, cfg)
	require.NoError(t, err)

	ctr := ssr.GetCounterInt64("deadlock-test", 1024).(*counterInt64)
	ctr.value.Store(10)

	// Track completion
	var completed atomic.Int32
	var wg sync.WaitGroup
	wg.Add(2)

	// Goroutine 1: Simulates OLD scheduleBackgroundPushCurrent behavior
	// (distributed lock -> updateMu) - THIS WAS THE BUG
	go func() {
		defer wg.Done()

		// OLD BUGGY ORDER: Acquire distributed lock first
		lockCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
		defer cancel()

		lock, err := ctr.registry.connector.Lock(lockCtx, ctr.key, 2*time.Second)
		if err != nil {
			t.Logf("Goroutine 1 (OLD behavior): Failed to acquire distributed lock: %v", err)
			return
		}
		defer lock.Unlock(context.Background())

		t.Log("Goroutine 1 (OLD behavior): Acquired distributed lock")

		// Small delay to ensure other goroutine starts
		time.Sleep(50 * time.Millisecond)

		// OLD BUGGY ORDER: Then try to acquire updateMu
		t.Log("Goroutine 1 (OLD behavior): Trying to acquire updateMu...")
		ctr.updateMu.Lock()
		t.Log("Goroutine 1 (OLD behavior): Acquired updateMu")
		ctr.updateMu.Unlock()

		completed.Add(1)
		t.Log("Goroutine 1 (OLD behavior): Completed")
	}()

	// Give first goroutine time to acquire distributed lock
	time.Sleep(25 * time.Millisecond)

	// Goroutine 2: Current TryUpdate behavior (correct order)
	// (updateMu -> distributed lock)
	go func() {
		defer wg.Done()

		// CORRECT ORDER: Acquire updateMu first
		ctr.updateMu.Lock()
		t.Log("Goroutine 2 (TryUpdate): Acquired updateMu")

		// Small delay to ensure goroutine 1 is waiting for updateMu
		time.Sleep(50 * time.Millisecond)

		// CORRECT ORDER: Then try to acquire distributed lock
		t.Log("Goroutine 2 (TryUpdate): Trying to acquire distributed lock...")
		lockCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
		defer cancel()

		lock, err := ctr.registry.connector.Lock(lockCtx, ctr.key, 2*time.Second)
		if err != nil {
			t.Logf("Goroutine 2 (TryUpdate): Failed to acquire distributed lock: %v", err)
			ctr.updateMu.Unlock()
			return
		}
		t.Log("Goroutine 2 (TryUpdate): Acquired distributed lock")
		lock.Unlock(context.Background())

		ctr.updateMu.Unlock()

		completed.Add(1)
		t.Log("Goroutine 2 (TryUpdate): Completed")
	}()

	// Wait for completion or timeout
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		count := completed.Load()
		if count == 2 {
			t.Log("Both goroutines completed - deadlock not triggered in this run")
			t.Log("(Race conditions mean deadlock doesn't always occur)")
		} else {
			t.Logf("Only %d goroutine(s) completed", count)
		}
	case <-time.After(2 * time.Second):
		count := completed.Load()
		t.Logf("Timeout: %d of 2 goroutines completed", count)
		t.Log("This demonstrates the OLD buggy behavior with inconsistent lock ordering:")
		t.Log("  OLD scheduleBackgroundPushCurrent: distributed lock → updateMu")
		t.Log("  TryUpdate: updateMu → distributed lock")
		t.Log("The fix ensures both paths use: updateMu → distributed lock")
	}
}
