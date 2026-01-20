package erpc

import (
	"context"
	"errors"
	"net/http"
	"strings"
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
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

func i64(v int64) *int64 { return &v }

// Lower bound: exactBlock, request below lower should skip (UpstreamsExhausted)
func TestNetworkAvailability_LowerExactBlock_Skip(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	upCfg := &common.UpstreamConfig{
		Id:       "rpc1",
		Type:     common.UpstreamTypeEvm,
		Endpoint: "http://rpc1.localhost",
		Evm: &common.EvmUpstreamConfig{
			ChainId:             123,
			StatePollerInterval: common.Duration(200 * time.Millisecond),
			StatePollerDebounce: common.Duration(50 * time.Millisecond),
			BlockAvailability: &common.EvmBlockAvailabilityConfig{
				Lower: &common.EvmAvailabilityBoundConfig{ExactBlock: i64(100)},
			},
		},
	}

	rlr, _ := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{}, &log.Logger)
	mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
	vr := thirdparty.NewVendorsRegistry()
	pr, _ := thirdparty.NewProvidersRegistry(&log.Logger, vr, nil, nil)
	// Create shared state config with proper timeouts for state poller
	sharedStateCfg := &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{
				MaxItems:     100_000,
				MaxTotalSize: "1GB",
			},
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
	time.Sleep(100 * time.Millisecond)

	ntwCfg := &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}}
	network, _ := NewNetwork(ctx, &log.Logger, "prjA", ntwCfg, rlr, upr, mt)
	require.NoError(t, upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123)))
	require.NoError(t, network.Bootstrap(ctx))

	// Request below lower (0x1 < 100)
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x1",false]}`))
	req.SetNetwork(network)

	resp, err := network.Forward(ctx, req)
	require.Error(t, err)
	require.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamsExhausted))
	if resp != nil {
		resp.Release()
	}
}

// Lower bound: latestBlockMinus, skip when below (latest - N)
func TestNetworkAvailability_LowerLatestMinus_Skip(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	upCfg := &common.UpstreamConfig{
		Id:       "rpc1",
		Type:     common.UpstreamTypeEvm,
		Endpoint: "http://rpc1.localhost",
		Evm: &common.EvmUpstreamConfig{
			ChainId:             123,
			StatePollerInterval: common.Duration(200 * time.Millisecond),
			StatePollerDebounce: common.Duration(50 * time.Millisecond),
			BlockAvailability: &common.EvmBlockAvailabilityConfig{
				Lower: &common.EvmAvailabilityBoundConfig{LatestBlockMinus: i64(10)},
			},
		},
	}

	rlr, _ := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{}, &log.Logger)
	mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
	vr := thirdparty.NewVendorsRegistry()
	pr, _ := thirdparty.NewProvidersRegistry(&log.Logger, vr, nil, nil)
	// Create shared state config with proper timeouts for state poller
	sharedStateCfg := &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{
				MaxItems:     100_000,
				MaxTotalSize: "1GB",
			},
		},
		LockMaxWait:     common.Duration(200 * time.Millisecond),
		UpdateMaxWait:   common.Duration(200 * time.Millisecond),
		FallbackTimeout: common.Duration(3 * time.Second),
		LockTtl:         common.Duration(4 * time.Second),
	}
	sharedStateCfg.SetDefaults("test")
	ssr, _ := data.NewSharedStateRegistry(ctx, &log.Logger, sharedStateCfg)
	upr := upstream.NewUpstreamsRegistry(ctx, &log.Logger, "prjA", []*common.UpstreamConfig{upCfg}, ssr, rlr, vr, pr, nil, mt, 1*time.Second, nil)

	ntwCfg := &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}}
	network, _ := NewNetwork(ctx, &log.Logger, "prjA", ntwCfg, rlr, upr, mt)

	// Bootstrap upstreams registry AFTER creating network
	upr.Bootstrap(ctx)

	// Prepare upstreams for network
	require.NoError(t, upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123)))

	// Bootstrap network
	require.NoError(t, network.Bootstrap(ctx))

	// Wait for state poller to initialize and get latest block
	time.Sleep(1000 * time.Millisecond)

	// Request far below latest-10 should be skipped; we don't register a method mock
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x2",false]}`))
	req.SetNetwork(network)

	resp, err := network.Forward(ctx, req)
	require.Error(t, err)
	require.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamsExhausted))
	if resp != nil {
		resp.Release()
	}
}

// Lower bound: earliestBlockPlus, initialize earliest via binary search fast-path (block 0 present) then skip for below-bound request
func TestNetworkAvailability_LowerEarliestPlus_InitAndSkip(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 1) // Expect 1 persist mock for block 0

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	upCfg := &common.UpstreamConfig{
		Id:       "rpc1",
		Type:     common.UpstreamTypeEvm,
		Endpoint: "http://rpc1.localhost",
		Evm: &common.EvmUpstreamConfig{
			ChainId:             123,
			StatePollerInterval: common.Duration(200 * time.Millisecond),
			StatePollerDebounce: common.Duration(50 * time.Millisecond),
			BlockAvailability: &common.EvmBlockAvailabilityConfig{
				Lower: &common.EvmAvailabilityBoundConfig{EarliestBlockPlus: i64(2)},
			},
		},
	}

	rlr, _ := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{}, &log.Logger)
	mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
	vr := thirdparty.NewVendorsRegistry()
	pr, _ := thirdparty.NewProvidersRegistry(&log.Logger, vr, nil, nil)
	// Create shared state config with proper timeouts for state poller
	sharedStateCfg := &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{
				MaxItems:     100_000,
				MaxTotalSize: "1GB",
			},
		},
		LockMaxWait:     common.Duration(200 * time.Millisecond),
		UpdateMaxWait:   common.Duration(200 * time.Millisecond),
		FallbackTimeout: common.Duration(3 * time.Second),
		LockTtl:         common.Duration(4 * time.Second),
	}
	sharedStateCfg.SetDefaults("test")
	ssr, _ := data.NewSharedStateRegistry(ctx, &log.Logger, sharedStateCfg)
	upr := upstream.NewUpstreamsRegistry(ctx, &log.Logger, "prjA", []*common.UpstreamConfig{upCfg}, ssr, rlr, vr, pr, nil, mt, 1*time.Second, nil)

	// Earliest discovery fast-path: block 0 exists
	// Set up the mock AFTER creating the registry but BEFORE bootstrap
	gock.New("http://rpc1.localhost").
		Post("").
		Persist(). // Make it persistent since it might be called multiple times or not at all
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			// Match eth_getBlockByNumber with block 0x0 for earliest discovery
			return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, "0x0")
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result": map[string]interface{}{
				"number": "0x0",
			},
		})

	upr.Bootstrap(ctx)
	time.Sleep(100 * time.Millisecond)

	ntwCfg := &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}}
	network, _ := NewNetwork(ctx, &log.Logger, "prjA", ntwCfg, rlr, upr, mt)
	require.NoError(t, upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123)))
	require.NoError(t, network.Bootstrap(ctx))
	// Allow earliest discovery to run
	time.Sleep(300 * time.Millisecond)

	// Request block 1, but lower is earliest+2 => skip
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x1",false]}`))
	req.SetNetwork(network)

	resp, err := network.Forward(ctx, req)
	require.Error(t, err)
	require.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamsExhausted))
	if resp != nil {
		resp.Release()
	}
}

// Mixed invalid range (lower latest-10, upper earliest+0) should fail-open (unbounded) and allow request
func TestNetworkAvailability_InvalidRange_FailOpen_AllowsRequest(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 2) // 2 persist mocks for block 0 and block 1

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	upCfg := &common.UpstreamConfig{
		Id:       "rpc1",
		Type:     common.UpstreamTypeEvm,
		Endpoint: "http://rpc1.localhost",
		Evm: &common.EvmUpstreamConfig{
			ChainId:             123,
			StatePollerInterval: common.Duration(200 * time.Millisecond),
			StatePollerDebounce: common.Duration(50 * time.Millisecond),
			BlockAvailability: &common.EvmBlockAvailabilityConfig{
				Lower: &common.EvmAvailabilityBoundConfig{LatestBlockMinus: i64(10)},
				Upper: &common.EvmAvailabilityBoundConfig{EarliestBlockPlus: i64(0)},
			},
		},
	}

	// Earliest fast-path: block 0 exists - make it persistent
	gock.New("http://rpc1.localhost").
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			return strings.Contains(body, "\"eth_getBlockByNumber\"") && strings.Contains(body, "\"0x0\"")
		}).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"number":"0x0"}}`))

	// Forwarded method should succeed due to fail-open (specific block 0x1)
	gock.New("http://rpc1.localhost").
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			b := util.SafeReadBody(r)
			return strings.Contains(b, "\"eth_getBlockByNumber\"") && strings.Contains(b, "\"0x1\"")
		}).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"number":"0x1"}}`))

	rlr, _ := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{}, &log.Logger)
	mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
	vr := thirdparty.NewVendorsRegistry()
	pr, _ := thirdparty.NewProvidersRegistry(&log.Logger, vr, nil, nil)
	// Create shared state config with proper timeouts for state poller
	sharedStateCfg := &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{
				MaxItems:     100_000,
				MaxTotalSize: "1GB",
			},
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
	time.Sleep(100 * time.Millisecond)

	ntwCfg := &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}}
	network, _ := NewNetwork(ctx, &log.Logger, "prjA", ntwCfg, rlr, upr, mt)
	require.NoError(t, upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123)))
	require.NoError(t, network.Bootstrap(ctx))
	// Allow earliest discovery to run (upper uses earliest)
	time.Sleep(300 * time.Millisecond)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x1",false]}`))
	req.SetNetwork(network)

	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	if resp != nil {
		resp.Release()
	}
	cancel()
}

// Exact window with both bounds: below and above should skip, inside should pass
func TestNetworkAvailability_Window_ExactLowerUpper(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	upCfg := &common.UpstreamConfig{
		Id:       "rpc1",
		Type:     common.UpstreamTypeEvm,
		Endpoint: "http://rpc1.localhost",
		Evm: &common.EvmUpstreamConfig{
			ChainId:             123,
			StatePollerInterval: common.Duration(200 * time.Millisecond),
			StatePollerDebounce: common.Duration(50 * time.Millisecond),
			BlockAvailability: &common.EvmBlockAvailabilityConfig{
				Lower: &common.EvmAvailabilityBoundConfig{ExactBlock: i64(100)},
				Upper: &common.EvmAvailabilityBoundConfig{ExactBlock: i64(200)},
			},
		},
	}

	// In-window method mock at 0x64 and 0xc8
	gock.New("http://rpc1.localhost").
		Post("").
		Times(1).
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "\"eth_getBlockByNumber\"") && strings.Contains(util.SafeReadBody(r), "\"0x64\"")
		}).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"number":"0x64"}}`))

	rlr, _ := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{}, &log.Logger)
	mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
	vr := thirdparty.NewVendorsRegistry()
	pr, _ := thirdparty.NewProvidersRegistry(&log.Logger, vr, nil, nil)
	// Create shared state config with proper timeouts for state poller
	sharedStateCfg := &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{
				MaxItems:     100_000,
				MaxTotalSize: "1GB",
			},
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
	time.Sleep(100 * time.Millisecond)

	ntwCfg := &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}}
	network, _ := NewNetwork(ctx, &log.Logger, "prjA", ntwCfg, rlr, upr, mt)
	require.NoError(t, upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123)))
	require.NoError(t, network.Bootstrap(ctx))
	// Allow earliest discovery to run
	time.Sleep(300 * time.Millisecond)

	// Below lower (0x1) -> skip/exhausted
	reqBelow := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x1",false]}`))
	reqBelow.SetNetwork(network)
	resp, err := network.Forward(ctx, reqBelow)
	require.Error(t, err)
	require.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamsExhausted))
	if resp != nil {
		resp.Release()
	}

	// Inside window at 0x64 -> OK
	reqInside := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x64",false]}`))
	reqInside.SetNetwork(network)
	resp, err = network.Forward(ctx, reqInside)
	require.NoError(t, err)
	if resp != nil {
		resp.Release()
	}

	// Above upper (0x201) -> skip/exhausted
	reqAbove := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x201",false]}`))
	reqAbove.SetNetwork(network)
	resp, err = network.Forward(ctx, reqAbove)
	require.Error(t, err)
	require.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamsExhausted))
	if resp != nil {
		resp.Release()
	}
}

// earliestBlockPlus with updateRate=0 (freeze): discover earliest=1 (0 missing, 1 present) and do not advance later
func TestNetworkAvailability_EarliestPlus_Freeze_NoAdvance(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 2) // 2 persist mocks for block discovery

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	upCfg := &common.UpstreamConfig{
		Id:       "rpc1",
		Type:     common.UpstreamTypeEvm,
		Endpoint: "http://rpc1.localhost",
		Evm: &common.EvmUpstreamConfig{
			ChainId:             123,
			StatePollerInterval: common.Duration(200 * time.Millisecond),
			StatePollerDebounce: common.Duration(50 * time.Millisecond),
			BlockAvailability: &common.EvmBlockAvailabilityConfig{
				Lower: &common.EvmAvailabilityBoundConfig{EarliestBlockPlus: i64(2)},
			},
		},
	}

	// Earliest discovery via binary search: 0 -> empty, 1 -> present - make them persistent
	gock.New("http://rpc1.localhost").Post("").Persist().Filter(func(r *http.Request) bool {
		b := util.SafeReadBody(r)
		return strings.Contains(b, "\"eth_getBlockByNumber\"") && strings.Contains(b, "\"0x0\"")
	}).Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":null}`))
	gock.New("http://rpc1.localhost").Post("").Persist().Filter(func(r *http.Request) bool {
		b := util.SafeReadBody(r)
		return strings.Contains(b, "\"eth_getBlockByNumber\"") && strings.Contains(b, "\"0x1\"")
	}).Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"number":"0x1"}}`))

	// Method mock for block 3 - keep as Times(1) since it's expected to be consumed
	gock.New("http://rpc1.localhost").Post("").Times(1).Filter(func(r *http.Request) bool {
		b := util.SafeReadBody(r)
		return strings.Contains(b, "\"eth_getBlockByNumber\"") && strings.Contains(b, "\"0x3\"")
	}).Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"number":"0x3"}}`))

	rlr, _ := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{}, &log.Logger)
	mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
	vr := thirdparty.NewVendorsRegistry()
	pr, _ := thirdparty.NewProvidersRegistry(&log.Logger, vr, nil, nil)
	// Create shared state config with proper timeouts for state poller
	sharedStateCfg := &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{
				MaxItems:     100_000,
				MaxTotalSize: "1GB",
			},
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
	time.Sleep(100 * time.Millisecond)

	ntwCfg := &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}}
	network, _ := NewNetwork(ctx, &log.Logger, "prjA", ntwCfg, rlr, upr, mt)
	require.NoError(t, upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123)))
	require.NoError(t, network.Bootstrap(ctx))
	// Allow earliest discovery to run
	time.Sleep(300 * time.Millisecond)

	// Request block 1 -> below earliest+2 (3) => skipped
	req0 := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x1",false]}`))
	req0.SetNetwork(network)
	resp, err := network.Forward(ctx, req0)
	require.Error(t, err)
	require.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamsExhausted))
	if resp != nil {
		resp.Release()
	}

	// Request block 3 -> allowed
	req1 := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x3",false]}`))
	req1.SetNetwork(network)
	resp, err = network.Forward(ctx, req1)
	require.NoError(t, err)
	if resp != nil {
		resp.Release()
	}
}

// earliestBlockPlus with updateRate>0: start with earliest=0 then prune to 1 and verify request at 0 is skipped after scheduler
func TestNetworkAvailability_EarliestPlus_UpdateRate_Advance(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 2) // 2 persist mocks remain after Times(1) is consumed

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	upCfg := &common.UpstreamConfig{
		Id:       "rpc1",
		Type:     common.UpstreamTypeEvm,
		Endpoint: "http://rpc1.localhost",
		Evm: &common.EvmUpstreamConfig{
			ChainId:             123,
			StatePollerInterval: common.Duration(200 * time.Millisecond),
			StatePollerDebounce: common.Duration(50 * time.Millisecond),
			BlockAvailability: &common.EvmBlockAvailabilityConfig{
				Lower: &common.EvmAvailabilityBoundConfig{EarliestBlockPlus: i64(2), UpdateRate: common.Duration(200 * time.Millisecond)},
			},
		},
	}

	// Initial earliest: block 0 present (first call ok), then pruned (subsequent calls empty). Block 1 present.
	gock.New("http://rpc1.localhost").Post("").Times(1).Filter(func(r *http.Request) bool {
		b := util.SafeReadBody(r)
		return strings.Contains(b, "\"eth_getBlockByNumber\"") && strings.Contains(b, "\"0x0\"")
	}).Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"number":"0x0"}}`))
	gock.New("http://rpc1.localhost").Post("").Persist().Filter(func(r *http.Request) bool {
		b := util.SafeReadBody(r)
		return strings.Contains(b, "\"eth_getBlockByNumber\"") && strings.Contains(b, "\"0x0\"")
	}).Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":null}`))
	gock.New("http://rpc1.localhost").Post("").Persist().Filter(func(r *http.Request) bool {
		b := util.SafeReadBody(r)
		return strings.Contains(b, "\"eth_getBlockByNumber\"") && strings.Contains(b, "\"0x1\"")
	}).Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"number":"0x1"}}`))

	rlr, _ := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{}, &log.Logger)
	mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
	vr := thirdparty.NewVendorsRegistry()
	pr, _ := thirdparty.NewProvidersRegistry(&log.Logger, vr, nil, nil)
	// Create shared state config with proper timeouts for state poller
	sharedStateCfg := &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{
				MaxItems:     100_000,
				MaxTotalSize: "1GB",
			},
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
	time.Sleep(100 * time.Millisecond)

	ntwCfg := &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}}
	network, _ := NewNetwork(ctx, &log.Logger, "prjA", ntwCfg, rlr, upr, mt)
	require.NoError(t, upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123)))
	require.NoError(t, network.Bootstrap(ctx))

	// Wait longer than updateRate for scheduler to advance earliest from 0 to 1
	time.Sleep(500 * time.Millisecond)

	// Block 1 should now be below lower (earliest moved to 1; lower=earliest+2 => 3)
	req0 := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x1",false]}`))
	req0.SetNetwork(network)
	resp, err := network.Forward(ctx, req0)
	require.Error(t, err)
	require.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamsExhausted))
	if resp != nil {
		resp.Release()
	}
}

// Unsupported probe (eventLogs) should fail-open (earliest unknown => unbounded) and allow requests
func TestNetworkAvailability_UnsupportedProbe_FailOpen(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 1) // 1 persist mock for block 0

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	upCfg := &common.UpstreamConfig{
		Id:       "rpc1",
		Type:     common.UpstreamTypeEvm,
		Endpoint: "http://rpc1.localhost",
		Evm: &common.EvmUpstreamConfig{
			ChainId:             123,
			StatePollerInterval: common.Duration(200 * time.Millisecond),
			StatePollerDebounce: common.Duration(50 * time.Millisecond),
			BlockAvailability: &common.EvmBlockAvailabilityConfig{
				Lower: &common.EvmAvailabilityBoundConfig{EarliestBlockPlus: i64(1), Probe: common.EvmProbeEventLogs},
			},
		},
	}

	// Forwarded method at 0x0 should succeed due to fail-open - make it persistent
	gock.New("http://rpc1.localhost").Post("").Persist().Filter(func(r *http.Request) bool {
		b := util.SafeReadBody(r)
		return strings.Contains(b, "\"eth_getBlockByNumber\"") && strings.Contains(b, "\"0x0\"")
	}).Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"number":"0x0"}}`))

	rlr, _ := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{}, &log.Logger)
	mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
	vr := thirdparty.NewVendorsRegistry()
	pr, _ := thirdparty.NewProvidersRegistry(&log.Logger, vr, nil, nil)
	// Create shared state config with proper timeouts for state poller
	sharedStateCfg := &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{
				MaxItems:     100_000,
				MaxTotalSize: "1GB",
			},
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
	time.Sleep(100 * time.Millisecond)

	ntwCfg := &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}}
	network, _ := NewNetwork(ctx, &log.Logger, "prjA", ntwCfg, rlr, upr, mt)
	require.NoError(t, upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123)))
	require.NoError(t, network.Bootstrap(ctx))

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x0",false]}`))
	req.SetNetwork(network)
	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	if resp != nil {
		resp.Release()
	}
}

// Upper earliest+0: above upper skipped, at earliest OK
func TestNetworkAvailability_UpperEarliestPlus_Enforced(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 3) // 3 persist mocks for blocks

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	upCfg := &common.UpstreamConfig{
		Id:       "rpc1",
		Type:     common.UpstreamTypeEvm,
		Endpoint: "http://rpc1.localhost",
		Evm: &common.EvmUpstreamConfig{
			ChainId:             123,
			StatePollerInterval: common.Duration(200 * time.Millisecond),
			StatePollerDebounce: common.Duration(50 * time.Millisecond),
			BlockAvailability: &common.EvmBlockAvailabilityConfig{
				Upper: &common.EvmAvailabilityBoundConfig{EarliestBlockPlus: i64(0)},
			},
		},
	}

	// Earliest fast-path and method mocks - make them all persistent to avoid flakiness
	// Block 0 mock (for earliest discovery and actual request)
	gock.New("http://rpc1.localhost").Post("").Persist().Filter(func(r *http.Request) bool {
		b := util.SafeReadBody(r)
		return strings.Contains(b, "\"eth_getBlockByNumber\"") && strings.Contains(b, "\"0x0\"")
	}).Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"number":"0x0"}}`))

	// Block 1 mock
	gock.New("http://rpc1.localhost").Post("").Persist().Filter(func(r *http.Request) bool {
		b := util.SafeReadBody(r)
		return strings.Contains(b, "\"eth_getBlockByNumber\"") && strings.Contains(b, "\"0x1\"")
	}).Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"number":"0x1"}}`))

	// General fallback for any other eth_getBlockByNumber
	gock.New("http://rpc1.localhost").Post("").Persist().Filter(func(r *http.Request) bool {
		b := util.SafeReadBody(r)
		return strings.Contains(b, "\"eth_getBlockByNumber\"") && !strings.Contains(b, "\"0x0\"") && !strings.Contains(b, "\"0x1\"")
	}).Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"number":"0x1"}}`))

	rlr, _ := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{}, &log.Logger)
	mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
	vr := thirdparty.NewVendorsRegistry()
	pr, _ := thirdparty.NewProvidersRegistry(&log.Logger, vr, nil, nil)
	// Create shared state config with proper timeouts for state poller
	sharedStateCfg := &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{
				MaxItems:     100_000,
				MaxTotalSize: "1GB",
			},
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
	time.Sleep(100 * time.Millisecond)

	ntwCfg := &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}}
	network, _ := NewNetwork(ctx, &log.Logger, "prjA", ntwCfg, rlr, upr, mt)
	require.NoError(t, upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123)))
	require.NoError(t, network.Bootstrap(ctx))
	// Allow earliest discovery to run
	time.Sleep(300 * time.Millisecond)

	// Above upper: 0x1 should be skipped
	req1 := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x1",false]}`))
	req1.SetNetwork(network)
	resp, err := network.Forward(ctx, req1)
	require.Error(t, err)
	require.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamsExhausted))
	if resp != nil {
		resp.Release()
	}

	// At earliest: 0x0 OK
	req0 := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x0",false]}`))
	req0.SetNetwork(network)
	resp, err = network.Forward(ctx, req0)
	require.NoError(t, err)
	if resp != nil {
		resp.Release()
	}
}

// bool pointer helper
func b(v bool) *bool { return &v }

// Default should NOT override method-level true (method > defaults)
func TestNetworkAvailability_Enforce_Precedence_DefaultDoesNotOverrideMethod(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Temporarily force default for eth_getBalance to disable enforcement
	orig := common.DefaultWithBlockCacheMethods["eth_getBalance"].EnforceBlockAvailability
	common.DefaultWithBlockCacheMethods["eth_getBalance"].EnforceBlockAvailability = b(false)
	defer func() {
		common.DefaultWithBlockCacheMethods["eth_getBalance"].EnforceBlockAvailability = orig
	}()

	upCfg := &common.UpstreamConfig{
		Id:       "rpc1",
		Type:     common.UpstreamTypeEvm,
		Endpoint: "http://rpc1.localhost",
		Evm: &common.EvmUpstreamConfig{
			ChainId:             123,
			StatePollerInterval: common.Duration(200 * time.Millisecond),
			StatePollerDebounce: common.Duration(50 * time.Millisecond),
			BlockAvailability: &common.EvmBlockAvailabilityConfig{
				Lower: &common.EvmAvailabilityBoundConfig{ExactBlock: i64(100)},
			},
		},
	}

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
	time.Sleep(100 * time.Millisecond)

	ntwCfg := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
		Methods: &common.MethodsConfig{Definitions: map[string]*common.CacheMethodConfig{
			// method-level true; carry over ReqRefs/RespRefs from defaults so normalization works
			"eth_getBalance": {
				ReqRefs:                  common.DefaultWithBlockCacheMethods["eth_getBalance"].ReqRefs,
				RespRefs:                 common.DefaultWithBlockCacheMethods["eth_getBalance"].RespRefs,
				EnforceBlockAvailability: b(true),
			},
		}},
	}
	network, _ := NewNetwork(ctx, &log.Logger, "prjA", ntwCfg, rlr, upr, mt)
	require.NoError(t, upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123)))
	require.NoError(t, network.Bootstrap(ctx))

	// Request below lower bound; with enforcement=true we should skip (UpstreamsExhausted)
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x0000000000000000000000000000000000000000","0x1"]}`))
	req.SetNetwork(network)
	resp, err := network.Forward(ctx, req)
	require.Error(t, err)
	require.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamsExhausted))
	if resp != nil {
		resp.Release()
	}
}

// Default should NOT override network-level true (network > defaults)
func TestNetworkAvailability_Enforce_Precedence_DefaultDoesNotOverrideNetwork(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Temporarily force default for eth_getBalance to disable enforcement
	orig := common.DefaultWithBlockCacheMethods["eth_getBalance"].EnforceBlockAvailability
	common.DefaultWithBlockCacheMethods["eth_getBalance"].EnforceBlockAvailability = b(false)
	defer func() {
		common.DefaultWithBlockCacheMethods["eth_getBalance"].EnforceBlockAvailability = orig
	}()

	upCfg := &common.UpstreamConfig{
		Id:       "rpc1",
		Type:     common.UpstreamTypeEvm,
		Endpoint: "http://rpc1.localhost",
		Evm: &common.EvmUpstreamConfig{
			ChainId:             123,
			StatePollerInterval: common.Duration(200 * time.Millisecond),
			StatePollerDebounce: common.Duration(50 * time.Millisecond),
			BlockAvailability: &common.EvmBlockAvailabilityConfig{
				Lower: &common.EvmAvailabilityBoundConfig{ExactBlock: i64(100)},
			},
		},
	}

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
	time.Sleep(100 * time.Millisecond)

	ntwCfg := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123, EnforceBlockAvailability: b(true)}, // network-level true
	}
	network, _ := NewNetwork(ctx, &log.Logger, "prjA", ntwCfg, rlr, upr, mt)
	require.NoError(t, upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123)))
	require.NoError(t, network.Bootstrap(ctx))

	// Request below lower bound; with enforcement=true we should skip (UpstreamsExhausted)
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x0000000000000000000000000000000000000000","0x1"]}`))
	req.SetNetwork(network)
	resp, err := network.Forward(ctx, req)
	require.Error(t, err)
	require.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamsExhausted))
	if resp != nil {
		resp.Release()
	}
}

// When nothing is set, default (false for eth_getBalance via override) should disable enforcement and allow forward
func TestNetworkAvailability_Enforce_DefaultFalse_Disables_WhenNoExplicitConfig(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Temporarily force default for eth_getBalance to disable enforcement
	orig := common.DefaultWithBlockCacheMethods["eth_getBalance"].EnforceBlockAvailability
	common.DefaultWithBlockCacheMethods["eth_getBalance"].EnforceBlockAvailability = b(false)
	defer func() {
		common.DefaultWithBlockCacheMethods["eth_getBalance"].EnforceBlockAvailability = orig
	}()

	upCfg := &common.UpstreamConfig{
		Id:       "rpc1",
		Type:     common.UpstreamTypeEvm,
		Endpoint: "http://rpc1.localhost",
		Evm: &common.EvmUpstreamConfig{
			ChainId:             123,
			StatePollerInterval: common.Duration(200 * time.Millisecond),
			StatePollerDebounce: common.Duration(50 * time.Millisecond),
			BlockAvailability: &common.EvmBlockAvailabilityConfig{
				Lower: &common.EvmAvailabilityBoundConfig{ExactBlock: i64(100)},
			},
		},
	}

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
	time.Sleep(100 * time.Millisecond)

	ntwCfg := &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}}
	network, _ := NewNetwork(ctx, &log.Logger, "prjA", ntwCfg, rlr, upr, mt)
	require.NoError(t, upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123)))
	require.NoError(t, network.Bootstrap(ctx))

	// Build request and verify network-level gating returns nil (allowed, no enforcement)
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x0000000000000000000000000000000000000000","0x1"]}`))
	req.SetNetwork(network)
	ups := upr.GetNetworkUpstreams(ctx, util.EvmNetworkId(123))[0]
	skipErr, _ := network.checkUpstreamBlockAvailability(ctx, ups, req, "eth_getBalance")
	require.NoError(t, skipErr, "expected no skip error (allowed)")
}

// Network-level false should disable enforcement regardless of defaults
func TestNetworkAvailability_Enforce_NetworkFalse_Disables(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Regardless of defaults, network-level false should disable enforcement
	// Ensure default for eth_getBalance does not counteract (set to true to prove network false wins)
	orig := common.DefaultWithBlockCacheMethods["eth_getBalance"].EnforceBlockAvailability
	common.DefaultWithBlockCacheMethods["eth_getBalance"].EnforceBlockAvailability = b(true)
	defer func() {
		common.DefaultWithBlockCacheMethods["eth_getBalance"].EnforceBlockAvailability = orig
	}()

	upCfg := &common.UpstreamConfig{
		Id:       "rpc1",
		Type:     common.UpstreamTypeEvm,
		Endpoint: "http://rpc1.localhost",
		Evm: &common.EvmUpstreamConfig{
			ChainId:             123,
			StatePollerInterval: common.Duration(200 * time.Millisecond),
			StatePollerDebounce: common.Duration(50 * time.Millisecond),
			BlockAvailability: &common.EvmBlockAvailabilityConfig{
				Lower: &common.EvmAvailabilityBoundConfig{ExactBlock: i64(100)},
			},
		},
	}

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
	time.Sleep(100 * time.Millisecond)

	ntwCfg := &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123, EnforceBlockAvailability: b(false)}}
	network, _ := NewNetwork(ctx, &log.Logger, "prjA", ntwCfg, rlr, upr, mt)
	require.NoError(t, upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123)))
	require.NoError(t, network.Bootstrap(ctx))

	// Build request and verify network-level gating returns nil (allowed, no enforcement)
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x0000000000000000000000000000000000000000","0x1"]}`))
	req.SetNetwork(network)
	ups := upr.GetNetworkUpstreams(ctx, util.EvmNetworkId(123))[0]
	skipErr, _ := network.checkUpstreamBlockAvailability(ctx, ups, req, "eth_getBalance")
	require.NoError(t, skipErr, "expected no skip error (allowed)")
}

// Test that ErrUpstreamBlockUnavailable is returned when block is beyond upstream's latest
func TestCheckUpstreamBlockAvailability_BlockBeyondLatest_ReturnsRetryableError(t *testing.T) {
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
			StatePollerInterval: common.Duration(200 * time.Millisecond),
			StatePollerDebounce: common.Duration(50 * time.Millisecond),
		},
	}

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
	time.Sleep(100 * time.Millisecond)

	// Enable enforcement
	ntwCfg := &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123, EnforceBlockAvailability: b(true)}}
	network, _ := NewNetwork(ctx, &log.Logger, "prjA", ntwCfg, rlr, upr, mt)
	require.NoError(t, upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123)))
	require.NoError(t, network.Bootstrap(ctx))

	// Request block 0x99999999 (2576980377 decimal, beyond the mock's latest block of 0x11118888=286397576)
	// The mock state poller returns 0x11118888 as latest block.
	// Distance is 2576980377 - 286397576 = 2290582801, way beyond default 128 threshold
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x0000000000000000000000000000000000000000","0x99999999"]}`))
	req.SetNetwork(network)
	// Set the block number on the request so it's available for gating
	req.SetEvmBlockNumber(int64(0x99999999))

	ups := upr.GetNetworkUpstreams(ctx, util.EvmNetworkId(123))[0]
	skipErr, isRetryable := network.checkUpstreamBlockAvailability(ctx, ups, req, "eth_getBalance")

	// Should return ErrUpstreamBlockUnavailable
	require.Error(t, skipErr)
	require.True(t, common.HasErrorCode(skipErr, common.ErrCodeUpstreamBlockUnavailable),
		"expected ErrUpstreamBlockUnavailable, got: %v", skipErr)

	// Should NOT be retryable because distance is too large (> 128 default)
	require.False(t, isRetryable, "expected not retryable for large block distance")
}

// Test that small block distance is retryable
func TestCheckUpstreamBlockAvailability_SmallDistance_IsRetryable(t *testing.T) {
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
			StatePollerInterval: common.Duration(200 * time.Millisecond),
			StatePollerDebounce: common.Duration(50 * time.Millisecond),
		},
	}

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
	time.Sleep(100 * time.Millisecond)

	// Enable enforcement
	ntwCfg := &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123, EnforceBlockAvailability: b(true)}}
	network, _ := NewNetwork(ctx, &log.Logger, "prjA", ntwCfg, rlr, upr, mt)
	require.NoError(t, upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123)))
	require.NoError(t, network.Bootstrap(ctx))

	// Request block just 50 blocks beyond latest (0x11118888 + 50 = 0x111188BA)
	// Distance is 50, within default 128 threshold - should be retryable
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x0000000000000000000000000000000000000000","0x111188BA"]}`))
	req.SetNetwork(network)
	req.SetEvmBlockNumber(int64(0x111188BA))

	ups := upr.GetNetworkUpstreams(ctx, util.EvmNetworkId(123))[0]
	skipErr, isRetryable := network.checkUpstreamBlockAvailability(ctx, ups, req, "eth_getBalance")

	// Should return error (block not available)
	require.Error(t, skipErr)
	require.True(t, common.HasErrorCode(skipErr, common.ErrCodeUpstreamBlockUnavailable))

	// Should be retryable because distance (50) is within threshold (128)
	require.True(t, isRetryable, "expected retryable for small block distance")
}

// Test that ErrUpstreamRequestSkipped is NOT retryable (for comparison)
func TestErrUpstreamRequestSkipped_IsNotRetryable(t *testing.T) {
	err := common.NewErrUpstreamRequestSkipped(nil, "test-upstream")
	require.False(t, common.IsRetryableTowardsUpstream(err),
		"ErrUpstreamRequestSkipped should NOT be retryable")
}

// Test that ErrUpstreamBlockUnavailable IS retryable
func TestErrUpstreamBlockUnavailable_IsRetryable(t *testing.T) {
	err := common.NewErrUpstreamBlockUnavailable("test-upstream", 1000, 500, 400)
	require.True(t, common.IsRetryableTowardsUpstream(err),
		"ErrUpstreamBlockUnavailable should be retryable")
}

// Test that ErrUpstreamBlockUnavailable contains proper details
func TestErrUpstreamBlockUnavailable_ContainsDetails(t *testing.T) {
	err := common.NewErrUpstreamBlockUnavailable("test-upstream", 1000, 500, 400)

	var baseErr *common.ErrUpstreamBlockUnavailable
	require.ErrorAs(t, err, &baseErr)
	require.Equal(t, common.ErrCodeUpstreamBlockUnavailable, baseErr.Code)
	require.Equal(t, "test-upstream", baseErr.Details["upstreamId"])
	require.Equal(t, int64(1000), baseErr.Details["blockNumber"])
	require.Equal(t, int64(500), baseErr.Details["latestBlock"])
	require.Equal(t, int64(400), baseErr.Details["finalizedBlock"])
}

// Test that ErrUpstreamBlockUnavailable error contains correct details
func TestCheckUpstreamBlockAvailability_ErrorHasCorrectDetails(t *testing.T) {
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
			StatePollerInterval: common.Duration(200 * time.Millisecond),
			StatePollerDebounce: common.Duration(50 * time.Millisecond),
		},
	}

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
	time.Sleep(100 * time.Millisecond)

	// Enable enforcement so block availability is checked
	ntwCfg := &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123, EnforceBlockAvailability: b(true)}}
	network, _ := NewNetwork(ctx, &log.Logger, "prjA", ntwCfg, rlr, upr, mt)
	require.NoError(t, upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123)))
	require.NoError(t, network.Bootstrap(ctx))

	ups := upr.GetNetworkUpstreams(ctx, util.EvmNetworkId(123))[0]

	// Create a request for a block beyond the upstream's latest (0x11118888=286397576)
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x0000000000000000000000000000000000000000","0x99999999"]}`))
	req.SetNetwork(network)
	req.SetEvmBlockNumber(int64(0x99999999))

	// Call checkUpstreamBlockAvailability - should return error
	skipErr, _ := network.checkUpstreamBlockAvailability(ctx, ups, req, "eth_getBalance")
	require.Error(t, skipErr)
	require.True(t, common.HasErrorCode(skipErr, common.ErrCodeUpstreamBlockUnavailable))

	// Verify the error contains the expected details
	var blockErr *common.ErrUpstreamBlockUnavailable
	require.ErrorAs(t, skipErr, &blockErr)
	require.Equal(t, "rpc1", blockErr.Details["upstreamId"])
	require.Equal(t, int64(0x99999999), blockErr.Details["blockNumber"])
	// The mock returns 0x11118888 as latest
	require.Equal(t, int64(0x11118888), blockErr.Details["latestBlock"])
}

// TestRetryableBlockUnavailability_NoInfiniteLoop tests that when an upstream returns a
// retryable block unavailability error (block slightly ahead), the upstream selection loop
// does NOT spin infinitely re-selecting the same upstream.
//
// BUG: When checkUpstreamBlockAvailability returns a retryable error, calling
// MarkUpstreamCompleted with a nil response sets canReUse=true (because !hasResponse=true),
// which removes the upstream from ConsumedUpstreams. Since ErrUpstreamBlockUnavailable
// is retryable per IsRetryableTowardsUpstream, NextUpstream doesn't skip it, causing
// the same upstream to be selected repeatedly until context timeout.
//
// EXPECTED: The upstream should be selected once, fail block availability check, and
// the error should be recorded. The loop should then exhaust upstreams and return
// ErrUpstreamsExhausted, NOT spin until context timeout.
func TestRetryableBlockUnavailability_NoInfiniteLoop(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Single upstream - if it gets re-selected infinitely, the test will timeout
	upCfg := &common.UpstreamConfig{
		Id:       "rpc1",
		Type:     common.UpstreamTypeEvm,
		Endpoint: "http://rpc1.localhost",
		Evm: &common.EvmUpstreamConfig{
			ChainId:             123,
			StatePollerInterval: common.Duration(200 * time.Millisecond),
			StatePollerDebounce: common.Duration(50 * time.Millisecond),
		},
	}

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
	time.Sleep(100 * time.Millisecond)

	// Enable block availability enforcement
	ntwCfg := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm: &common.EvmNetworkConfig{
			ChainId:                  123,
			EnforceBlockAvailability: b(true),
		},
	}
	network, err := NewNetwork(ctx, &log.Logger, "prjA", ntwCfg, rlr, upr, mt)
	require.NoError(t, err)
	require.NoError(t, upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123)))
	require.NoError(t, network.Bootstrap(ctx))

	// Request block 0x111188BA = latestBlock(0x11118888) + 50
	// Distance of 50 is within default MaxRetryableBlockDistance of 128, so this is a RETRYABLE error
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x0000000000000000000000000000000000000000","0x111188BA"]}`))
	req.SetNetwork(network)

	// Use a short timeout - if there's an infinite loop, we'll hit this
	// If fixed, the call should complete almost instantly with ErrUpstreamsExhausted
	forwardCtx, forwardCancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer forwardCancel()

	startTime := time.Now()
	_, err = network.Forward(forwardCtx, req)
	elapsed := time.Since(startTime)

	// The request MUST fail (no upstream has the block)
	require.Error(t, err, "expected error since no upstream has the requested block")

	// Critical assertion: If the bug exists, we'll hit the 500ms timeout and get a context error.
	// If fixed, we should get ErrUpstreamsExhausted almost immediately (< 100ms).
	isTimeout := common.HasErrorCode(err, common.ErrCodeNetworkRequestTimeout, common.ErrCodeEndpointRequestTimeout, common.ErrCodeFailsafeTimeoutExceeded) ||
		errors.Is(err, context.DeadlineExceeded)
	require.False(t, isTimeout,
		"BUG DETECTED: Request timed out instead of exhausting upstreams. "+
			"This indicates an infinite loop where the same upstream is being re-selected repeatedly. "+
			"elapsed=%v, error=%v", elapsed, err)

	// Should be ErrUpstreamsExhausted - all upstreams tried and failed
	require.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamsExhausted),
		"expected ErrUpstreamsExhausted, got: %v (elapsed=%v)", err, elapsed)

	// Should complete quickly - not waiting for timeout
	require.Less(t, elapsed, 200*time.Millisecond,
		"Forward took too long (%v), possible spin loop before exhausting", elapsed)

	// The underlying error should be ErrUpstreamBlockUnavailable
	require.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamBlockUnavailable),
		"expected underlying ErrUpstreamBlockUnavailable, got: %v", err)
}
