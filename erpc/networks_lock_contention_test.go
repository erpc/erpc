package erpc

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"
)

// This suite demonstrates the current problematic locking behavior in Network.Forward
// around cache Set:
// - A read lock is held on the response across the entire cache Set I/O path.
// - The lifetime ref (pendingOps) is decremented before the read lock is released,
//   allowing Release() to become a pending writer while a reader is still held,
//   which can block new readers due to writer preference in RWMutex.
//
// NOTE: These tests are intentionally written to "prove the issue" by asserting
// the blocking behavior. After applying the code fix, they should be inverted/updated.

// TestNetwork_CacheSetBlocksRelease shows that releasing a response is blocked
// for at least as long as a slow cache Set runs, because the response read lock
// is kept for the duration of Set.
func TestNetwork_CacheSetBlocksRelease(t *testing.T) {
	// Global gock setup per repo guidelines
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	// Configure a slow mock cache connector
	cacheCfg := &common.CacheConfig{
		Connectors: []*common.ConnectorConfig{
			{
				Id:     "mock",
				Driver: "mock",
				Mock: &common.MockConnectorConfig{
					MemoryConnectorConfig: common.MemoryConnectorConfig{
						MaxItems: 100_000, MaxTotalSize: "1GB",
					},
					SetDelay: 250 * time.Millisecond, // simulate slow I/O on Set
				},
			},
		},
		Policies: []*common.CachePolicyConfig{
			{
				Network:   "*",
				Method:    "*",
				TTL:       common.Duration(5 * time.Minute),
				Connector: "mock",
				// Match dynamic/latest data written for this request
				Finality: common.DataFinalityStateRealtime,
			},
		},
	}
	require.NoError(t, cacheCfg.SetDefaults())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network := setupTestNetworkSimple(t, ctx, nil, nil)

	// Mock a simple upstream call for eth_getBlockByNumber "latest"
	gock.New("http://rpc1.localhost").
		Post("").
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			// Match our specific request by method and id to avoid colliding with poller calls
			return strings.Contains(body, `"eth_getBlockByNumber"`) &&
				strings.Contains(body, `"id":1`)
		}).
		Times(1).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result": map[string]interface{}{
				"number": "0x1111",
				"hash":   "0xabc",
			},
		})

	// Attach slow cache to network
	slowCache, err := evm.NewEvmJsonRpcCache(ctx, &log.Logger, cacheCfg)
	require.NoError(t, err)
	network.cacheDal = slowCache.WithProjectId("prjA")

	// Make the request and ensure cache DAL is set on the request
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["latest",false],"id":1}`))
	req.SetNetwork(network)
	req.SetCacheDal(slowCache)

	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)

	// Release should not complete until the slow Set finishes, demonstrating the issue.
	start := time.Now()
	done := make(chan struct{})
	go func() {
		resp.Release()
		close(done)
	}()

	select {
	case <-done:
		// Completed; measure how long it took
		elapsed := time.Since(start)
		// Expect it to take at least ~SetDelay (prove the lock is held across I/O).
		if elapsed < 200*time.Millisecond {
			t.Fatalf("Release finished too quickly (%s). Expected to block behind slow cache set (>=200ms)", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Release did not finish in time (likely hung)")
	}
}

// TestNetwork_CacheSetHoldsReadLockAcrossIO attempts to take a writer lock while cache Set
// is in progress and verifies that it cannot be acquired quickly, proving RLock is held across I/O.
func TestNetwork_CacheSetHoldsReadLockAcrossIO(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	cacheCfg := &common.CacheConfig{
		Connectors: []*common.ConnectorConfig{
			{
				Id:     "mock",
				Driver: "mock",
				Mock: &common.MockConnectorConfig{
					MemoryConnectorConfig: common.MemoryConnectorConfig{
						MaxItems: 100_000, MaxTotalSize: "1GB",
					},
					SetDelay: 300 * time.Millisecond,
				},
			},
		},
		Policies: []*common.CachePolicyConfig{
			{
				Network:   "*",
				Method:    "*",
				TTL:       common.Duration(5 * time.Minute),
				Connector: "mock",
				Finality:  common.DataFinalityStateRealtime,
			},
		},
	}
	require.NoError(t, cacheCfg.SetDefaults())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network := setupTestNetworkSimple(t, ctx, nil, nil)

	gock.New("http://rpc1.localhost").
		Post("").
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			return strings.Contains(body, `"eth_getBlockByNumber"`) &&
				strings.Contains(body, `"id":2`)
		}).
		Times(1).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      2,
			"result": map[string]interface{}{
				"number": "0x2222",
				"hash":   "0xdef",
			},
		})

	slowCache, err := evm.NewEvmJsonRpcCache(ctx, &log.Logger, cacheCfg)
	require.NoError(t, err)
	network.cacheDal = slowCache.WithProjectId("prjA")

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["latest",false],"id":2}`))
	req.SetNetwork(network)
	req.SetCacheDal(slowCache)

	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)

	// Try to acquire writer lock while Set is in progress — should not acquire quickly.
	acquired := make(chan time.Duration, 1)
	start := time.Now()
	go func() {
		// This blocks behind the read lock kept for the duration of Set.
		resp.LockWithTrace(ctx)
		resp.Unlock()
		acquired <- time.Since(start)
	}()

	select {
	case d := <-acquired:
		// Writer lock acquired; verify it was not immediate (i.e., blocked across IO)
		if d < 250*time.Millisecond {
			t.Fatalf("writer lock acquired too quickly (%s). Expected to be blocked while cache Set holds RLock", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("writer lock attempt did not complete in time (likely hung)")
	}

	// Ensure we don't retain memory
	resp.Release()
}


