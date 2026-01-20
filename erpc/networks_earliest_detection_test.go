package erpc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/thirdparty"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEarliestDetection_FailOpenWhenNoEarliestConfigured tests that requests are allowed
// when no earliestBlockPlus config is set (baseline fail-open behavior).
func TestEarliestDetection_FailOpenWhenNoEarliestConfigured(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Configure upstream WITHOUT earliestBlockPlus - only exactBlock lower bound
	upCfg := &common.UpstreamConfig{
		Id:       "rpc1",
		Type:     common.UpstreamTypeEvm,
		Endpoint: "http://rpc1.localhost",
		Evm: &common.EvmUpstreamConfig{
			ChainId:             123,
			StatePollerInterval: common.Duration(100 * time.Millisecond),
			StatePollerDebounce: common.Duration(50 * time.Millisecond),
			// No BlockAvailability config - should default to unbounded (fail-open)
		},
	}

	// Mock the actual request block 5
	gock.New("http://rpc1.localhost").
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, "\"0x5\"")
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  map[string]interface{}{"number": "0x5"},
		})

	rlr, _ := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{}, &log.Logger)
	mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
	vr := thirdparty.NewVendorsRegistry()
	pr, _ := thirdparty.NewProvidersRegistry(&log.Logger, vr, nil, nil)
	sharedStateCfg := &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"},
		},
		LockMaxWait:     common.Duration(200 * time.Millisecond),
		UpdateMaxWait:   common.Duration(200 * time.Millisecond),
		FallbackTimeout: common.Duration(3 * time.Second),
		LockTtl:         common.Duration(4 * time.Second),
	}
	sharedStateCfg.SetDefaults("test")
	ssr, _ := data.NewSharedStateRegistry(ctx, &log.Logger, sharedStateCfg)
	upr := upstream.NewUpstreamsRegistry(ctx, &log.Logger, "prjA", []*common.UpstreamConfig{upCfg}, ssr, rlr, vr, pr, nil, mt, 1*time.Second, nil)
	upr.Bootstrap(ctx)
	time.Sleep(150 * time.Millisecond)

	ntwCfg := &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}}
	network, _ := NewNetwork(ctx, &log.Logger, "prjA", ntwCfg, rlr, upr, mt)
	require.NoError(t, upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123)))
	require.NoError(t, network.Bootstrap(ctx))

	// Request block 5 - should succeed (no block availability restrictions)
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x5",false]}`))
	req.SetNetwork(network)

	resp, err := network.Forward(ctx, req)
	require.NoError(t, err, "expected request to succeed when no block availability config")
	require.NotNil(t, resp)
	resp.Release()
}

// TestEarliestDetection_BlocksRequestAfterSuccessfulDetection tests that after detection
// succeeds, requests below the earliest bound are properly blocked.
func TestEarliestDetection_BlocksRequestAfterSuccessfulDetection(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	upCfg := &common.UpstreamConfig{
		Id:       "rpc1",
		Type:     common.UpstreamTypeEvm,
		Endpoint: "http://rpc1.localhost",
		Evm: &common.EvmUpstreamConfig{
			ChainId:             123,
			StatePollerInterval: common.Duration(100 * time.Millisecond),
			StatePollerDebounce: common.Duration(50 * time.Millisecond),
			BlockAvailability: &common.EvmBlockAvailabilityConfig{
				// Lower bound = earliest + 10, so if earliest=100, lower=110
				Lower: &common.EvmAvailabilityBoundConfig{EarliestBlockPlus: i64(10)},
			},
		},
	}

	// Mock earliest detection to succeed with earliest=100
	// Binary search will probe block 0, find it missing, then find 100 as earliest
	gock.New("http://rpc1.localhost").
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, "0x0")
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  nil, // Block 0 doesn't exist (pruned)
		})

	// Simulate that block 100 (0x64) exists - this is the earliest
	gock.New("http://rpc1.localhost").
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, "0x64")
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result": map[string]interface{}{
				"number": "0x64", // 100
			},
		})

	// For binary search - simulate mid-point blocks
	gock.New("http://rpc1.localhost").
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			// Match any eth_getBlockByNumber that's not 0x0 or 0x64
			return strings.Contains(body, "eth_getBlockByNumber") &&
				!strings.Contains(body, "0x0") &&
				!strings.Contains(body, "\"0x64\"") &&
				!strings.Contains(body, "latest") &&
				!strings.Contains(body, "finalized")
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  nil, // Pruned
		})

	rlr, _ := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{}, &log.Logger)
	mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
	vr := thirdparty.NewVendorsRegistry()
	pr, _ := thirdparty.NewProvidersRegistry(&log.Logger, vr, nil, nil)
	sharedStateCfg := &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"},
		},
		LockMaxWait:     common.Duration(200 * time.Millisecond),
		UpdateMaxWait:   common.Duration(200 * time.Millisecond),
		FallbackTimeout: common.Duration(3 * time.Second),
		LockTtl:         common.Duration(4 * time.Second),
	}
	sharedStateCfg.SetDefaults("test")
	ssr, _ := data.NewSharedStateRegistry(ctx, &log.Logger, sharedStateCfg)
	upr := upstream.NewUpstreamsRegistry(ctx, &log.Logger, "prjA", []*common.UpstreamConfig{upCfg}, ssr, rlr, vr, pr, nil, mt, 1*time.Second, nil)
	upr.Bootstrap(ctx)
	time.Sleep(300 * time.Millisecond) // Allow detection to complete

	ntwCfg := &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}}
	network, _ := NewNetwork(ctx, &log.Logger, "prjA", ntwCfg, rlr, upr, mt)
	require.NoError(t, upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123)))
	require.NoError(t, network.Bootstrap(ctx))

	// Request block 50 (0x32) - should be blocked (below earliest+10 = 110)
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x32",false]}`))
	req.SetNetwork(network)

	resp, err := network.Forward(ctx, req)
	require.Error(t, err, "expected request to be blocked after successful earliest detection")
	assert.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamsExhausted))
	if resp != nil {
		resp.Release()
	}
}

// TestEarliestDetection_InitialDetectionAlwaysRunsOnBootstrap tests that initial
// detection always runs on instance bootstrap, regardless of existing shared state value.
func TestEarliestDetection_InitialDetectionAlwaysRunsOnBootstrap(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Track detection calls
	var detectionCalls int32

	upCfg := &common.UpstreamConfig{
		Id:       "rpc1",
		Type:     common.UpstreamTypeEvm,
		Endpoint: "http://rpc1.localhost",
		Evm: &common.EvmUpstreamConfig{
			ChainId:             123,
			StatePollerInterval: common.Duration(100 * time.Millisecond),
			StatePollerDebounce: common.Duration(50 * time.Millisecond),
			BlockAvailability: &common.EvmBlockAvailabilityConfig{
				Lower: &common.EvmAvailabilityBoundConfig{EarliestBlockPlus: i64(0)},
			},
		},
	}

	// Mock earliest detection - track calls
	gock.New("http://rpc1.localhost").
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			if strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, "0x0") {
				atomic.AddInt32(&detectionCalls, 1)
				return true
			}
			return false
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result": map[string]interface{}{
				"number": "0x0",
			},
		})

	rlr, _ := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{}, &log.Logger)
	mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
	vr := thirdparty.NewVendorsRegistry()
	pr, _ := thirdparty.NewProvidersRegistry(&log.Logger, vr, nil, nil)

	// Create shared state with existing value (simulating another instance set it)
	sharedStateCfg := &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"},
		},
		LockMaxWait:     common.Duration(200 * time.Millisecond),
		UpdateMaxWait:   common.Duration(200 * time.Millisecond),
		FallbackTimeout: common.Duration(3 * time.Second),
		LockTtl:         common.Duration(4 * time.Second),
	}
	sharedStateCfg.SetDefaults("test")
	ssr, _ := data.NewSharedStateRegistry(ctx, &log.Logger, sharedStateCfg)

	// Pre-set a value in shared state (simulating another instance)
	// The key format matches what evm_state_poller uses
	presetKey := "earliestBlock/evm:123:rpc1/blockHeader"
	presetCounter := ssr.GetCounterInt64(presetKey, 0)
	presetCounter.TryUpdate(ctx, 50) // Pre-set to 50

	// Now bootstrap - detection should still run despite existing shared state value
	upr := upstream.NewUpstreamsRegistry(ctx, &log.Logger, "prjA", []*common.UpstreamConfig{upCfg}, ssr, rlr, vr, pr, nil, mt, 1*time.Second, nil)
	upr.Bootstrap(ctx)
	time.Sleep(300 * time.Millisecond)

	ntwCfg := &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}}
	network, _ := NewNetwork(ctx, &log.Logger, "prjA", ntwCfg, rlr, upr, mt)
	require.NoError(t, upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123)))
	require.NoError(t, network.Bootstrap(ctx))
	time.Sleep(200 * time.Millisecond)

	// Should have at least 1 detection call (initial detection on bootstrap)
	calls := atomic.LoadInt32(&detectionCalls)
	assert.GreaterOrEqual(t, calls, int32(1), "expected at least 1 detection call on bootstrap despite existing shared state value")
}

// TestEarliestDetection_SchedulerHandlesPeriodicUpdates tests that the scheduler
// handles periodic updates after initial detection.
func TestEarliestDetection_SchedulerHandlesPeriodicUpdates(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Track detection calls
	var detectionCalls int32

	upCfg := &common.UpstreamConfig{
		Id:       "rpc1",
		Type:     common.UpstreamTypeEvm,
		Endpoint: "http://rpc1.localhost",
		Evm: &common.EvmUpstreamConfig{
			ChainId:             123,
			StatePollerInterval: common.Duration(100 * time.Millisecond),
			StatePollerDebounce: common.Duration(50 * time.Millisecond),
			BlockAvailability: &common.EvmBlockAvailabilityConfig{
				Lower: &common.EvmAvailabilityBoundConfig{
					EarliestBlockPlus: i64(0),
					UpdateRate:        common.Duration(200 * time.Millisecond), // Fast update for testing
				},
			},
		},
	}

	// Mock earliest detection - track calls
	gock.New("http://rpc1.localhost").
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			if strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, "0x0") {
				atomic.AddInt32(&detectionCalls, 1)
				return true
			}
			return false
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result": map[string]interface{}{
				"number": "0x0",
			},
		})

	rlr, _ := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{}, &log.Logger)
	mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
	vr := thirdparty.NewVendorsRegistry()
	pr, _ := thirdparty.NewProvidersRegistry(&log.Logger, vr, nil, nil)
	sharedStateCfg := &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"},
		},
		LockMaxWait:     common.Duration(200 * time.Millisecond),
		UpdateMaxWait:   common.Duration(200 * time.Millisecond),
		FallbackTimeout: common.Duration(3 * time.Second),
		LockTtl:         common.Duration(4 * time.Second),
	}
	sharedStateCfg.SetDefaults("test")
	ssr, _ := data.NewSharedStateRegistry(ctx, &log.Logger, sharedStateCfg)
	upr := upstream.NewUpstreamsRegistry(ctx, &log.Logger, "prjA", []*common.UpstreamConfig{upCfg}, ssr, rlr, vr, pr, nil, mt, 1*time.Second, nil)
	upr.Bootstrap(ctx)

	ntwCfg := &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}}
	network, _ := NewNetwork(ctx, &log.Logger, "prjA", ntwCfg, rlr, upr, mt)
	require.NoError(t, upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123)))
	require.NoError(t, network.Bootstrap(ctx))

	// Get initial call count after bootstrap
	time.Sleep(300 * time.Millisecond)
	initialCalls := atomic.LoadInt32(&detectionCalls)

	// Wait for scheduler to run a few times (updateRate = 200ms)
	time.Sleep(600 * time.Millisecond)

	// Should have more calls now due to scheduler
	finalCalls := atomic.LoadInt32(&detectionCalls)
	assert.Greater(t, finalCalls, initialCalls, "expected scheduler to trigger additional detection calls")
}

// TestEarliestDetection_InvalidRangeTriggersFailOpen verifies that when earliest=0 is used
// with an upper bound (earliestBlockPlus) and a lower bound (latestBlockMinus), an invalid
// range (min > max) is created which triggers fail-open behavior, allowing any request.
func TestEarliestDetection_InvalidRangeTriggersFailOpen(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Configure with BOTH bounds - this creates an invalid range when earliest=0
	// Lower: latestMinus=10 → ~286M (from SetupMocksForEvmStatePoller)
	// Upper: earliestPlus=0 → 0 (when earliest=0)
	// Range: [286M, 0] is invalid → triggers fail-open
	upCfg := &common.UpstreamConfig{
		Id:       "rpc1",
		Type:     common.UpstreamTypeEvm,
		Endpoint: "http://rpc1.localhost",
		Evm: &common.EvmUpstreamConfig{
			ChainId:             123,
			StatePollerInterval: common.Duration(5 * time.Second), // Slow so detection doesn't complete
			StatePollerDebounce: common.Duration(50 * time.Millisecond),
			BlockAvailability: &common.EvmBlockAvailabilityConfig{
				Lower: &common.EvmAvailabilityBoundConfig{LatestBlockMinus: i64(10)},
				Upper: &common.EvmAvailabilityBoundConfig{EarliestBlockPlus: i64(0)},
			},
		},
	}

	// Mock for actual request (block 1)
	gock.New("http://rpc1.localhost").
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, "\"0x1\"")
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result": map[string]interface{}{
				"number": "0x1",
			},
		})

	// Mock for earliest detection - make it take forever (essentially skip detection)
	gock.New("http://rpc1.localhost").
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, "0x0")
		}).
		Reply(200).
		Delay(10 * time.Second). // Very slow response
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  nil,
		})

	rlr, _ := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{}, &log.Logger)
	mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
	vr := thirdparty.NewVendorsRegistry()
	pr, _ := thirdparty.NewProvidersRegistry(&log.Logger, vr, nil, nil)
	sharedStateCfg := &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"},
		},
		LockMaxWait:     common.Duration(100 * time.Millisecond),
		UpdateMaxWait:   common.Duration(100 * time.Millisecond),
		FallbackTimeout: common.Duration(500 * time.Millisecond),
		LockTtl:         common.Duration(4 * time.Second),
	}
	sharedStateCfg.SetDefaults("test")
	ssr, _ := data.NewSharedStateRegistry(ctx, &log.Logger, sharedStateCfg)
	upr := upstream.NewUpstreamsRegistry(ctx, &log.Logger, "prjA", []*common.UpstreamConfig{upCfg}, ssr, rlr, vr, pr, nil, mt, 1*time.Second, nil)
	upr.Bootstrap(ctx)
	time.Sleep(200 * time.Millisecond)

	ntwCfg := &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}}
	network, _ := NewNetwork(ctx, &log.Logger, "prjA", ntwCfg, rlr, upr, mt)
	require.NoError(t, upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123)))
	require.NoError(t, network.Bootstrap(ctx))

	// Request block 1 - should succeed due to fail-open (invalid range: min > max)
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x1",false]}`))
	req.SetNetwork(network)

	resp, err := network.Forward(ctx, req)
	require.NoError(t, err, "expected request to succeed due to invalid range fail-open")
	require.NotNil(t, resp)
	resp.Release()
}

// TestEarliestDetection_StaleHighValueInSharedState tests that when shared state
// has a stale/wrong HIGH value (e.g., latest block mistakenly stored as earliest),
// fresh detection with a LOWER correct value will override it.
//
// This test would FAIL with old code where ignoreRollbackOf=0 didn't allow decreases.
// Bug scenario:
// 1. Redis has 46M (wrong - this was the latest block, not earliest)
// 2. Detection finds 13M (correct earliest block)
// 3. Old code: 13M < 46M, so update rejected → requests to block 30M fail with "below lower bound: 30M < 46M"
// 4. New code: 13M < 46M, but with ignoreRollbackOf=0 decreases are allowed → block 30M succeeds
func TestEarliestDetection_StaleHighValueInSharedState(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	upCfg := &common.UpstreamConfig{
		Id:       "rpc1",
		Type:     common.UpstreamTypeEvm,
		Endpoint: "http://rpc1.localhost",
		Evm: &common.EvmUpstreamConfig{
			ChainId:             123,
			StatePollerInterval: common.Duration(100 * time.Millisecond),
			StatePollerDebounce: common.Duration(50 * time.Millisecond),
			BlockAvailability: &common.EvmBlockAvailabilityConfig{
				Lower: &common.EvmAvailabilityBoundConfig{
					EarliestBlockPlus: i64(0), // Use detected earliest as lower bound
				},
			},
		},
	}

	// Mock latest block = 47M
	gock.New("http://rpc1.localhost").
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			return strings.Contains(body, "eth_blockNumber")
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  "0x2CD29C0", // 47M
		})

	// Mock earliest detection: block 0 doesn't exist (pruned)
	gock.New("http://rpc1.localhost").
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, "0x0")
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  nil, // Block 0 doesn't exist (pruned)
		})

	// Any block >= 13M exists - binary search will find 13M as earliest
	gock.New("http://rpc1.localhost").
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			if !strings.Contains(body, "eth_getBlockByNumber") {
				return false
			}
			if strings.Contains(body, "0x0") || strings.Contains(body, "0x1C9C380") {
				return false
			}
			return true
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result": map[string]interface{}{
				"number": "0xC65D40", // 13M
			},
		})

	// Mock for actual request to block 30M (0x1C9C380)
	gock.New("http://rpc1.localhost").
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, "0x1C9C380")
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result": map[string]interface{}{
				"number": "0x1C9C380", // 30M
			},
		})

	rlr, _ := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{}, &log.Logger)
	mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
	vr := thirdparty.NewVendorsRegistry()
	pr, _ := thirdparty.NewProvidersRegistry(&log.Logger, vr, nil, nil)

	// Create shared state
	sharedStateCfg := &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"},
		},
		LockMaxWait:     common.Duration(500 * time.Millisecond),
		UpdateMaxWait:   common.Duration(500 * time.Millisecond),
		FallbackTimeout: common.Duration(3 * time.Second),
		LockTtl:         common.Duration(4 * time.Second),
	}
	sharedStateCfg.SetDefaults("test")
	ssr, _ := data.NewSharedStateRegistry(ctx, &log.Logger, sharedStateCfg)

	// Pre-compute the key that will be used for earliest block storage
	// This matches what UniqueUpstreamKey computes: id + "/" + sha256(id + endpoint + networkId)
	sha := sha256.New()
	sha.Write([]byte("rpc1"))                  // upCfg.Id
	sha.Write([]byte("http://rpc1.localhost")) // upCfg.Endpoint
	sha.Write([]byte("evm:123"))               // networkId
	uniqueKey := "rpc1/" + hex.EncodeToString(sha.Sum(nil))
	presetKey := fmt.Sprintf("earliestBlock/%s/blockHeader", uniqueKey)
	t.Logf("Pre-computed key: %s", presetKey)

	// PRE-SET STALE/WRONG VALUE BEFORE BOOTSTRAP:
	// Simulate Redis having 46M (latest block mistakenly stored as earliest)
	// This is the exact bug scenario from production
	staleCounter := ssr.GetCounterInt64(presetKey, 0)
	staleCounter.TryUpdate(ctx, 46_000_000) // WRONG: 46M is the latest, not earliest!
	require.Equal(t, int64(46_000_000), staleCounter.GetValue(), "stale value should be set before bootstrap")
	t.Logf("Set stale value BEFORE bootstrap: %d", staleCounter.GetValue())

	// Wait for the value to become "stale" (detection uses 1ms staleness on initial bootstrap)
	time.Sleep(10 * time.Millisecond)

	// NOW bootstrap upstream - detection should find ~13M and OVERRIDE the stale 46M value
	upr := upstream.NewUpstreamsRegistry(ctx, &log.Logger, "prjA", []*common.UpstreamConfig{upCfg}, ssr, rlr, vr, pr, nil, mt, 1*time.Second, nil)
	upr.Bootstrap(ctx)
	time.Sleep(500 * time.Millisecond)

	// Create network and bootstrap
	ntwCfg := &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}}
	network, _ := NewNetwork(ctx, &log.Logger, "prjA", ntwCfg, rlr, upr, mt)
	require.NoError(t, upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123)))
	require.NoError(t, network.Bootstrap(ctx))
	time.Sleep(300 * time.Millisecond)

	// The counter should now have been updated to the detected value (around 13M)
	currentValue := staleCounter.GetValue()
	t.Logf("Counter value after detection: %d", currentValue)

	// Key assertion: the value should have DECREASED from 46M
	// With the old bug, this would still be 46M because decreases weren't applied
	assert.Less(t, currentValue, int64(46_000_000),
		"shared state should have been updated to lower correct value; old bug would keep 46M")

	// Now request block 30M - this should SUCCEED
	// With the bug: 30M < 46M → "block below lower availability bound" error
	// Fixed: 30M > 13M → request succeeds
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x1C9C380",false]}`))
	req.SetNetwork(network)

	resp, err := network.Forward(ctx, req)
	require.NoError(t, err, "request to block 30M should succeed (30M > detected earliest); bug would fail with 'below lower bound: 30M < 46M'")
	require.NotNil(t, resp)
	resp.Release()
}

// TestEarliestDetection_DecreaseNotAllowedWithHighRollbackThreshold tests that
// counters with ignoreRollbackOf > 0 still reject decreases (existing behavior).
// This ensures our fix only affects earliestBlock (ignoreRollbackOf=0).
func TestEarliestDetection_DecreaseNotAllowedWithHighRollbackThreshold(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sharedStateCfg := &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"},
		},
	}
	sharedStateCfg.SetDefaults("test")
	ssr, _ := data.NewSharedStateRegistry(ctx, &log.Logger, sharedStateCfg)

	// Counter with ignoreRollbackOf = 1024 (like latestBlock/finalizedBlock)
	counter := ssr.GetCounterInt64("test-counter", 1024)
	counter.TryUpdate(ctx, 100)

	// Try to decrease - should be REJECTED (existing behavior preserved)
	counter.TryUpdate(ctx, 50)

	assert.Equal(t, int64(100), counter.GetValue(),
		"counters with ignoreRollbackOf > 0 should still reject decreases")
}

// TestEarliestDetection_DecreaseAllowedWithZeroRollbackThreshold tests that
// counters with ignoreRollbackOf = 0 accept decreases (the fix).
func TestEarliestDetection_DecreaseAllowedWithZeroRollbackThreshold(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sharedStateCfg := &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"},
		},
	}
	sharedStateCfg.SetDefaults("test")
	ssr, _ := data.NewSharedStateRegistry(ctx, &log.Logger, sharedStateCfg)

	// Counter with ignoreRollbackOf = 0 (like earliestBlock)
	counter := ssr.GetCounterInt64("test-counter", 0)
	counter.TryUpdate(ctx, 100)

	// Try to decrease - should be ACCEPTED with ignoreRollbackOf = 0
	counter.TryUpdate(ctx, 50)

	assert.Equal(t, int64(50), counter.GetValue(),
		"counters with ignoreRollbackOf = 0 should accept decreases")
}
