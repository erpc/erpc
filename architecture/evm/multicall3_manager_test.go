package evm

import (
	"sync"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/require"
)

func TestBatcherManagerGetOrCreate(t *testing.T) {
	mgr := NewBatcherManager()

	cfg := &common.Multicall3AggregationConfig{
		Enabled:                true,
		WindowMs:               25,
		MinWaitMs:              2,
		MaxCalls:               20,
		MaxCalldataBytes:       64000,
		MaxQueueSize:           100,
		MaxPendingBatches:      20,
		AllowCrossUserBatching: util.BoolPtr(true),
	}
	cfg.SetDefaults()

	forwarder := &mockForwarder{}

	// Get batcher for project+network
	batcher1 := mgr.GetOrCreate("project1", "evm:1", cfg, forwarder, nil)
	require.NotNil(t, batcher1)

	// Same project+network should return same batcher
	batcher2 := mgr.GetOrCreate("project1", "evm:1", cfg, forwarder, nil)
	require.Same(t, batcher1, batcher2)

	// Different network (same project) should return different batcher
	batcher3 := mgr.GetOrCreate("project1", "evm:137", cfg, forwarder, nil)
	require.NotSame(t, batcher1, batcher3)

	// Different project (same network) should return different batcher
	batcher4 := mgr.GetOrCreate("project2", "evm:1", cfg, forwarder, nil)
	require.NotSame(t, batcher1, batcher4)

	mgr.Shutdown()
}

func TestBatcherManagerConcurrency(t *testing.T) {
	mgr := NewBatcherManager()

	cfg := &common.Multicall3AggregationConfig{
		Enabled:   true,
		WindowMs:  25,
		MinWaitMs: 2,
		MaxCalls:  20,
	}
	cfg.SetDefaults()

	forwarder := &mockForwarder{}

	var wg sync.WaitGroup
	batchers := make([]*Batcher, 100)

	// Concurrent access
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			batchers[idx] = mgr.GetOrCreate("project1", "evm:1", cfg, forwarder, nil)
		}(i)
	}
	wg.Wait()

	// All should be the same batcher
	for i := 1; i < 100; i++ {
		require.Same(t, batchers[0], batchers[i])
	}

	mgr.Shutdown()
}

func TestBatcherManagerGet(t *testing.T) {
	mgr := NewBatcherManager()

	cfg := &common.Multicall3AggregationConfig{
		Enabled:   true,
		WindowMs:  25,
		MinWaitMs: 2,
		MaxCalls:  20,
	}
	cfg.SetDefaults()

	forwarder := &mockForwarder{}

	// Get before create should return nil
	batcher := mgr.Get("project1", "evm:1")
	require.Nil(t, batcher)

	// Create batcher
	created := mgr.GetOrCreate("project1", "evm:1", cfg, forwarder, nil)
	require.NotNil(t, created)

	// Get after create should return the same batcher
	retrieved := mgr.Get("project1", "evm:1")
	require.Same(t, created, retrieved)

	// Get for different network should return nil
	other := mgr.Get("project1", "evm:137")
	require.Nil(t, other)

	// Get for different project should return nil
	otherProject := mgr.Get("project2", "evm:1")
	require.Nil(t, otherProject)

	mgr.Shutdown()
}

func TestBatcherManagerShutdown(t *testing.T) {
	mgr := NewBatcherManager()

	cfg := &common.Multicall3AggregationConfig{
		Enabled:   true,
		WindowMs:  25,
		MinWaitMs: 2,
		MaxCalls:  20,
	}
	cfg.SetDefaults()

	forwarder := &mockForwarder{}

	// Create multiple batchers across projects and networks
	mgr.GetOrCreate("project1", "evm:1", cfg, forwarder, nil)
	mgr.GetOrCreate("project1", "evm:137", cfg, forwarder, nil)
	mgr.GetOrCreate("project2", "evm:1", cfg, forwarder, nil)

	// Shutdown should clean up all batchers
	mgr.Shutdown()

	// After shutdown, batchers map should be empty
	require.Nil(t, mgr.Get("project1", "evm:1"))
	require.Nil(t, mgr.Get("project1", "evm:137"))
	require.Nil(t, mgr.Get("project2", "evm:1"))
}
