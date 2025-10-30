package erpc

import (
	"context"
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

	rlr, _ := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{}, &log.Logger)
	mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
	vr := thirdparty.NewVendorsRegistry()
	pr, _ := thirdparty.NewProvidersRegistry(&log.Logger, vr, nil, nil)
	ssr, _ := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{Connector: &common.ConnectorConfig{Driver: common.DriverMemory, Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"}}})
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

	rlr, _ := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{}, &log.Logger)
	mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
	vr := thirdparty.NewVendorsRegistry()
	pr, _ := thirdparty.NewProvidersRegistry(&log.Logger, vr, nil, nil)
	ssr, _ := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{Connector: &common.ConnectorConfig{Driver: common.DriverMemory, Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"}}})
	upr := upstream.NewUpstreamsRegistry(ctx, &log.Logger, "prjA", []*common.UpstreamConfig{upCfg}, ssr, rlr, vr, pr, nil, mt, 1*time.Second, nil)
	upr.Bootstrap(ctx)
	time.Sleep(100 * time.Millisecond)

	ntwCfg := &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}}
	network, _ := NewNetwork(ctx, &log.Logger, "prjA", ntwCfg, rlr, upr, mt)
	require.NoError(t, upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123)))
	require.NoError(t, network.Bootstrap(ctx))

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
				Lower: &common.EvmAvailabilityBoundConfig{EarliestBlockPlus: i64(2)},
			},
		},
	}

	// Earliest discovery fast-path: block 0 exists
	gock.New("http://rpc1.localhost").
		Post("").
		Times(1).
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			return strings.Contains(body, "\"eth_getBlockByNumber\"") && strings.Contains(body, "\"0x0\"")
		}).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"number":"0x0"}}`))

	rlr, _ := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{}, &log.Logger)
	mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
	vr := thirdparty.NewVendorsRegistry()
	pr, _ := thirdparty.NewProvidersRegistry(&log.Logger, vr, nil, nil)
	ssr, _ := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{Connector: &common.ConnectorConfig{Driver: common.DriverMemory, Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"}}})
	upr := upstream.NewUpstreamsRegistry(ctx, &log.Logger, "prjA", []*common.UpstreamConfig{upCfg}, ssr, rlr, vr, pr, nil, mt, 1*time.Second, nil)
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
	defer util.AssertNoPendingMocks(t, 1)

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

	// Earliest fast-path: block 0 exists
	gock.New("http://rpc1.localhost").
		Post("").
		Times(1).
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

	rlr, _ := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{}, &log.Logger)
	mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
	vr := thirdparty.NewVendorsRegistry()
	pr, _ := thirdparty.NewProvidersRegistry(&log.Logger, vr, nil, nil)
	ssr, _ := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{Connector: &common.ConnectorConfig{Driver: common.DriverMemory, Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"}}})
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

	rlr, _ := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{}, &log.Logger)
	mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
	vr := thirdparty.NewVendorsRegistry()
	pr, _ := thirdparty.NewProvidersRegistry(&log.Logger, vr, nil, nil)
	ssr, _ := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{Connector: &common.ConnectorConfig{Driver: common.DriverMemory, Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"}}})
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
				Lower: &common.EvmAvailabilityBoundConfig{EarliestBlockPlus: i64(2)},
			},
		},
	}

	// Earliest discovery via binary search: 0 -> empty, 1 -> present
	gock.New("http://rpc1.localhost").Post("").Times(1).Filter(func(r *http.Request) bool {
		b := util.SafeReadBody(r)
		return strings.Contains(b, "\"eth_getBlockByNumber\"") && strings.Contains(b, "\"0x0\"")
	}).Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":null}`))
	gock.New("http://rpc1.localhost").Post("").Times(1).Filter(func(r *http.Request) bool {
		b := util.SafeReadBody(r)
		return strings.Contains(b, "\"eth_getBlockByNumber\"") && strings.Contains(b, "\"0x1\"")
	}).Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"number":"0x1"}}`))

	// Method mock for block 3
	gock.New("http://rpc1.localhost").Post("").Times(1).Filter(func(r *http.Request) bool {
		b := util.SafeReadBody(r)
		return strings.Contains(b, "\"eth_getBlockByNumber\"") && strings.Contains(b, "\"0x3\"")
	}).Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"number":"0x3"}}`))

	rlr, _ := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{}, &log.Logger)
	mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
	vr := thirdparty.NewVendorsRegistry()
	pr, _ := thirdparty.NewProvidersRegistry(&log.Logger, vr, nil, nil)
	ssr, _ := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{Connector: &common.ConnectorConfig{Driver: common.DriverMemory, Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"}}})
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
	defer util.AssertNoPendingMocks(t, 2)

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

	rlr, _ := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{}, &log.Logger)
	mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
	vr := thirdparty.NewVendorsRegistry()
	pr, _ := thirdparty.NewProvidersRegistry(&log.Logger, vr, nil, nil)
	ssr, _ := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{Connector: &common.ConnectorConfig{Driver: common.DriverMemory, Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"}}})
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
				Lower: &common.EvmAvailabilityBoundConfig{EarliestBlockPlus: i64(1), Probe: common.EvmProbeEventLogs},
			},
		},
	}

	// Forwarded method at 0x0 should succeed due to fail-open
	gock.New("http://rpc1.localhost").Post("").Times(2).Filter(func(r *http.Request) bool {
		b := util.SafeReadBody(r)
		return strings.Contains(b, "\"eth_getBlockByNumber\"") && strings.Contains(b, "\"0x0\"")
	}).Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"number":"0x0"}}`))

	rlr, _ := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{}, &log.Logger)
	mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
	vr := thirdparty.NewVendorsRegistry()
	pr, _ := thirdparty.NewProvidersRegistry(&log.Logger, vr, nil, nil)
	ssr, _ := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{Connector: &common.ConnectorConfig{Driver: common.DriverMemory, Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"}}})
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
	defer util.AssertNoPendingMocks(t, 2)

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

	// Earliest fast-path: block 0 exists
	gock.New("http://rpc1.localhost").Post("").Times(1).Filter(func(r *http.Request) bool {
		b := util.SafeReadBody(r)
		return strings.Contains(b, "\"eth_getBlockByNumber\"") && strings.Contains(b, "\"0x0\"")
	}).Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"number":"0x0"}}`))

	// Method for 0x0 OK
	gock.New("http://rpc1.localhost").Post("").Times(1).Filter(func(r *http.Request) bool {
		b := util.SafeReadBody(r)
		return strings.Contains(b, "\"eth_getBlockByNumber\"") && strings.Contains(b, "\"0x0\"")
	}).Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"number":"0x0"}}`))
	// Method for 0x1 OK (and any eth_getBlockByNumber as fallback)
	gock.New("http://rpc1.localhost").Post("").Times(1).Filter(func(r *http.Request) bool {
		b := util.SafeReadBody(r)
		return strings.Contains(b, "\"eth_getBlockByNumber\"") && strings.Contains(b, "\"0x1\"")
	}).Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"number":"0x1"}}`))
	gock.New("http://rpc1.localhost").Post("").Persist().Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "\"eth_getBlockByNumber\"") }).Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"number":"0x1"}}`))

	rlr, _ := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{}, &log.Logger)
	mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
	vr := thirdparty.NewVendorsRegistry()
	pr, _ := thirdparty.NewProvidersRegistry(&log.Logger, vr, nil, nil)
	ssr, _ := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{Connector: &common.ConnectorConfig{Driver: common.DriverMemory, Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"}}})
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
