package erpc

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/architecture/evm"
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

// Network-level integrity test for eth_getBlockReceipts: strict logIndex increments
func TestNetworkIntegrity_EthGetBlockReceipts_LogIndexStrictIncrements(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Upstream config with integrity enabled
	upCfg := &common.UpstreamConfig{
		Id:       "rpc1",
		Type:     common.UpstreamTypeEvm,
		Endpoint: "http://rpc1.localhost",
		Evm: &common.EvmUpstreamConfig{
			ChainId:             123,
			StatePollerInterval: common.Duration(5 * time.Second),
			StatePollerDebounce: common.Duration(1 * time.Second),
			Integrity: &common.UpstreamIntegrityConfig{
				EthGetBlockReceipts: &common.UpstreamIntegrityEthGetBlockReceiptsConfig{
					Enabled:                       true,
					CheckLogIndexStrictIncrements: util.BoolPtr(true),
				},
			},
		},
	}

	// Minimal required mocks for poller and chainId
	util.SetupMocksForEvmStatePoller()

	// Registry plumbing
	rlr, _ := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{}, &log.Logger)
	mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
	vr := thirdparty.NewVendorsRegistry()
	pr, _ := thirdparty.NewProvidersRegistry(&log.Logger, vr, nil, nil)
	ssr, _ := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{Connector: &common.ConnectorConfig{Driver: common.DriverMemory, Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"}}})
	upr := upstream.NewUpstreamsRegistry(ctx, &log.Logger, "prjA", []*common.UpstreamConfig{upCfg}, ssr, rlr, vr, pr, nil, mt, 1*time.Second, nil)
	upr.Bootstrap(ctx)
	time.Sleep(100 * time.Millisecond)

	ntwCfg := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
	}
	network, _ := NewNetwork(ctx, &log.Logger, "prjA", ntwCfg, rlr, upr, mt)
	require.NoError(t, upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123)))
	require.NoError(t, network.Bootstrap(ctx))

	// Build request
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":[{"blockNumber":"0x1"}]}`))
	req.SetNetwork(network)

	// Stub eth_getBlockReceipts with non-sequential global logIndex
	gock.New("http://rpc1.localhost").
		Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "\"eth_getBlockReceipts\"") }).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[
			{"blockHash":"0xabc","logs":[{"logIndex":"0x0"},{"logIndex":"0x1"}]},
			{"blockHash":"0xabc","logs":[{"logIndex":"0x1"}]}
		]}`))

	// Use real upstream object from registry, forward once, then run post-forward hook
	ups := upr.GetNetworkUpstreams(ctx, util.EvmNetworkId(123))
	require.GreaterOrEqual(t, len(ups), 1)
	rawResp, fwdErr := ups[0].Forward(ctx, req, false)
	require.NoError(t, fwdErr)
	require.NotNil(t, rawResp)
	defer rawResp.Release()

	_, hookErr := evm.HandleUpstreamPostForward(ctx, network, ups[0], req, rawResp, nil, false)
	require.Error(t, hookErr)
	var mf *common.ErrUpstreamMalformedResponse
	require.True(t, errors.As(hookErr, &mf), "expected ErrUpstreamMalformedResponse, got: %v", hookErr)
}

// Gap in global indices should error (contiguity enforcement)
func TestNetworkIntegrity_EthGetBlockReceipts_LogIndexGap_Error(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	upCfg := &common.UpstreamConfig{
		Id:       "rpc1",
		Type:     common.UpstreamTypeEvm,
		Endpoint: "http://rpc1.localhost",
		Evm: &common.EvmUpstreamConfig{
			ChainId:             123,
			StatePollerInterval: common.Duration(5 * time.Second),
			StatePollerDebounce: common.Duration(1 * time.Second),
			Integrity: &common.UpstreamIntegrityConfig{
				EthGetBlockReceipts: &common.UpstreamIntegrityEthGetBlockReceiptsConfig{
					Enabled:                       true,
					CheckLogIndexStrictIncrements: util.BoolPtr(true),
				},
			},
		},
	}

	util.SetupMocksForEvmStatePoller()

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

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":[{"blockNumber":"0x1"}]}`))
	req.SetNetwork(network)

	// 0x0, 0x1, 0xC (gap) -> error
	gock.New("http://rpc1.localhost").
		Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "\"eth_getBlockReceipts\"") }).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[
            {"blockHash":"0xabc","logs":[{"logIndex":"0x0"}]},
            {"blockHash":"0xabc","logs":[{"logIndex":"0x1"}]},
            {"blockHash":"0xabc","logs":[{"logIndex":"0xc"}]}
        ]}`))

	ups := upr.GetNetworkUpstreams(ctx, util.EvmNetworkId(123))
	require.GreaterOrEqual(t, len(ups), 1)
	rawResp, fwdErr := ups[0].Forward(ctx, req, false)
	require.NoError(t, fwdErr)
	require.NotNil(t, rawResp)
	defer rawResp.Release()

	_, hookErr := evm.HandleUpstreamPostForward(ctx, network, ups[0], req, rawResp, nil, false)
	require.Error(t, hookErr)
	var mf *common.ErrUpstreamMalformedResponse
	require.True(t, errors.As(hookErr, &mf), "expected ErrUpstreamMalformedResponse, got: %v", hookErr)
}

// Contiguous indices 0x0,0x1,0x2 should pass when enabled
func TestNetworkIntegrity_EthGetBlockReceipts_LogIndexContiguous_NoError(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	upCfg := &common.UpstreamConfig{Id: "rpc1", Type: common.UpstreamTypeEvm, Endpoint: "http://rpc1.localhost", Evm: &common.EvmUpstreamConfig{ChainId: 123, StatePollerInterval: common.Duration(5 * time.Second), StatePollerDebounce: common.Duration(1 * time.Second), Integrity: &common.UpstreamIntegrityConfig{EthGetBlockReceipts: &common.UpstreamIntegrityEthGetBlockReceiptsConfig{Enabled: true, CheckLogIndexStrictIncrements: util.BoolPtr(true)}}}}

	util.SetupMocksForEvmStatePoller()

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

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":[{"blockNumber":"0x1"}]}`))
	req.SetNetwork(network)

	// 0x0, 0x1, 0x2 -> OK
	gock.New("http://rpc1.localhost").
		Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "\"eth_getBlockReceipts\"") }).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logs":[{"logIndex":"0x0"}]},{"blockHash":"0xabc","logs":[{"logIndex":"0x1"}]},{"blockHash":"0xabc","logs":[{"logIndex":"0x2"}]}]}`))

	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	if resp != nil {
		resp.Release()
	}
}

// Inconsistent blockHash across receipts should error (integrity enabled)
func TestNetworkIntegrity_EthGetBlockReceipts_InconsistentBlockHash_Error(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	upCfg := &common.UpstreamConfig{Id: "rpc1", Type: common.UpstreamTypeEvm, Endpoint: "http://rpc1.localhost", Evm: &common.EvmUpstreamConfig{ChainId: 123, StatePollerInterval: common.Duration(5 * time.Second), StatePollerDebounce: common.Duration(1 * time.Second), Integrity: &common.UpstreamIntegrityConfig{EthGetBlockReceipts: &common.UpstreamIntegrityEthGetBlockReceiptsConfig{Enabled: true}}}}

	util.SetupMocksForEvmStatePoller()

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

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":[{"blockNumber":"0x1"}]}`))
	req.SetNetwork(network)

	// Two receipts with different blockHash
	gock.New("http://rpc1.localhost").Post("").Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "\"eth_getBlockReceipts\"") }).Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logs":[]},{"blockHash":"0xdef","logs":[]}]}`))

	ups := upr.GetNetworkUpstreams(ctx, util.EvmNetworkId(123))
	require.GreaterOrEqual(t, len(ups), 1)
	rawResp, fwdErr := ups[0].Forward(ctx, req, false)
	require.NoError(t, fwdErr)
	require.NotNil(t, rawResp)
	defer rawResp.Release()

	_, hookErr := evm.HandleUpstreamPostForward(ctx, network, ups[0], req, rawResp, nil, false)
	require.Error(t, hookErr)
	var mf *common.ErrUpstreamMalformedResponse
	require.True(t, errors.As(hookErr, &mf), "expected ErrUpstreamMalformedResponse, got: %v", hookErr)
}

// Result not an array should error (malformed upstream response)
func TestNetworkIntegrity_EthGetBlockReceipts_ResultNotArray_Error(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	upCfg := &common.UpstreamConfig{Id: "rpc1", Type: common.UpstreamTypeEvm, Endpoint: "http://rpc1.localhost", Evm: &common.EvmUpstreamConfig{ChainId: 123, StatePollerInterval: common.Duration(5 * time.Second), StatePollerDebounce: common.Duration(1 * time.Second), Integrity: &common.UpstreamIntegrityConfig{EthGetBlockReceipts: &common.UpstreamIntegrityEthGetBlockReceiptsConfig{Enabled: true}}}}

	util.SetupMocksForEvmStatePoller()

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

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":[{"blockNumber":"0x1"}]}`))
	req.SetNetwork(network)

	// Invalid structure: result is an object
	gock.New("http://rpc1.localhost").Post("").Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "\"eth_getBlockReceipts\"") }).Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"foo":"bar"}}`))

	ups := upr.GetNetworkUpstreams(ctx, util.EvmNetworkId(123))
	require.GreaterOrEqual(t, len(ups), 1)
	rawResp, fwdErr := ups[0].Forward(ctx, req, false)
	require.NoError(t, fwdErr)
	require.NotNil(t, rawResp)
	defer rawResp.Release()

	_, hookErr := evm.HandleUpstreamPostForward(ctx, network, ups[0], req, rawResp, nil, false)
	require.Error(t, hookErr)
	var mf *common.ErrUpstreamMalformedResponse
	require.True(t, errors.As(hookErr, &mf), "expected ErrUpstreamMalformedResponse, got: %v", hookErr)
}

// Decreasing logIndex should error
func TestNetworkIntegrity_EthGetBlockReceipts_LogIndexDecreasing_Error(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	upCfg := &common.UpstreamConfig{Id: "rpc1", Type: common.UpstreamTypeEvm, Endpoint: "http://rpc1.localhost", Evm: &common.EvmUpstreamConfig{ChainId: 123, StatePollerInterval: common.Duration(5 * time.Second), StatePollerDebounce: common.Duration(1 * time.Second), Integrity: &common.UpstreamIntegrityConfig{EthGetBlockReceipts: &common.UpstreamIntegrityEthGetBlockReceiptsConfig{Enabled: true, CheckLogIndexStrictIncrements: util.BoolPtr(true)}}}}

	util.SetupMocksForEvmStatePoller()

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

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":[{"blockNumber":"0x1"}]}`))
	req.SetNetwork(network)

	// Decreasing indices 0x2 -> 0x1
	gock.New("http://rpc1.localhost").Post("").Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "\"eth_getBlockReceipts\"") }).Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logs":[{"logIndex":"0x2"}]},{"blockHash":"0xabc","logs":[{"logIndex":"0x1"}]}]}`))

	ups := upr.GetNetworkUpstreams(ctx, util.EvmNetworkId(123))
	require.GreaterOrEqual(t, len(ups), 1)
	rawResp, fwdErr := ups[0].Forward(ctx, req, false)
	require.NoError(t, fwdErr)
	require.NotNil(t, rawResp)
	defer rawResp.Release()

	_, hookErr := evm.HandleUpstreamPostForward(ctx, network, ups[0], req, rawResp, nil, false)
	require.Error(t, hookErr)
	var mf *common.ErrUpstreamMalformedResponse
	require.True(t, errors.As(hookErr, &mf), "expected ErrUpstreamMalformedResponse, got: %v", hookErr)
}

func TestNetworkIntegrity_EthGetBlockReceipts_MissingLogIndexEntries_Error(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	upCfg := &common.UpstreamConfig{
		Id:       "rpc1",
		Type:     common.UpstreamTypeEvm,
		Endpoint: "http://rpc1.localhost",
		Evm: &common.EvmUpstreamConfig{
			ChainId:             123,
			StatePollerInterval: common.Duration(5 * time.Second),
			StatePollerDebounce: common.Duration(1 * time.Second),
			Integrity: &common.UpstreamIntegrityConfig{
				EthGetBlockReceipts: &common.UpstreamIntegrityEthGetBlockReceiptsConfig{
					Enabled:                       true,
					CheckLogIndexStrictIncrements: util.BoolPtr(true),
				},
			},
		},
	}

	util.SetupMocksForEvmStatePoller()

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

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":[{"blockNumber":"0x1"}]}`))
	req.SetNetwork(network)

	// Missing logIndex must error now (contiguity requires presence)
	gock.New("http://rpc1.localhost").Post("").Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "\"eth_getBlockReceipts\"") }).Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logs":[{}, {"logIndex":"0x0"}]},{"blockHash":"0xabc","logs":[{"logIndex":"0x1"}]}]}`))

	ups := upr.GetNetworkUpstreams(ctx, util.EvmNetworkId(123))
	require.GreaterOrEqual(t, len(ups), 1)
	rawResp, fwdErr := ups[0].Forward(ctx, req, false)
	require.NoError(t, fwdErr)
	require.NotNil(t, rawResp)
	defer rawResp.Release()

	_, hookErr := evm.HandleUpstreamPostForward(ctx, network, ups[0], req, rawResp, nil, false)
	require.Error(t, hookErr)
	var mf *common.ErrUpstreamMalformedResponse
	require.True(t, errors.As(hookErr, &mf), "expected ErrUpstreamMalformedResponse, got: %v", hookErr)
}

// logsBloom non-zero with zero logs should error when CheckLogsBloom is enabled
func TestNetworkIntegrity_EthGetBlockReceipts_LogsBloomNonZeroZeroLogs_Error(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	upCfg := &common.UpstreamConfig{Id: "rpc1", Type: common.UpstreamTypeEvm, Endpoint: "http://rpc1.localhost", Evm: &common.EvmUpstreamConfig{ChainId: 123, StatePollerInterval: common.Duration(5 * time.Second), StatePollerDebounce: common.Duration(1 * time.Second), Integrity: &common.UpstreamIntegrityConfig{EthGetBlockReceipts: &common.UpstreamIntegrityEthGetBlockReceiptsConfig{Enabled: true, CheckLogsBloom: util.BoolPtr(true)}}}}

	util.SetupMocksForEvmStatePoller()

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

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":[{"blockNumber":"0x1"}]}`))
	req.SetNetwork(network)

	// Non-zero logsBloom with zero logs -> error
	gock.New("http://rpc1.localhost").
		Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "\"eth_getBlockReceipts\"") }).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logsBloom":"0x1","logs":[]} ]}`))

	ups := upr.GetNetworkUpstreams(ctx, util.EvmNetworkId(123))
	require.GreaterOrEqual(t, len(ups), 1)
	rawResp, fwdErr := ups[0].Forward(ctx, req, false)
	require.NoError(t, fwdErr)
	require.NotNil(t, rawResp)
	defer rawResp.Release()

	_, hookErr := evm.HandleUpstreamPostForward(ctx, network, ups[0], req, rawResp, nil, false)
	require.Error(t, hookErr)
	var mf *common.ErrUpstreamMalformedResponse
	require.True(t, errors.As(hookErr, &mf), "expected ErrUpstreamMalformedResponse, got: %v", hookErr)
}

// logsBloom check disabled should allow non-zero bloom with zero logs
func TestNetworkIntegrity_EthGetBlockReceipts_LogsBloomDisabled_NoError(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	upCfg := &common.UpstreamConfig{Id: "rpc1", Type: common.UpstreamTypeEvm, Endpoint: "http://rpc1.localhost", Evm: &common.EvmUpstreamConfig{ChainId: 123, StatePollerInterval: common.Duration(5 * time.Second), StatePollerDebounce: common.Duration(1 * time.Second), Integrity: &common.UpstreamIntegrityConfig{EthGetBlockReceipts: &common.UpstreamIntegrityEthGetBlockReceiptsConfig{Enabled: true, CheckLogsBloom: util.BoolPtr(false)}}}}

	util.SetupMocksForEvmStatePoller()

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

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":[{"blockNumber":"0x1"}]}`))
	req.SetNetwork(network)

	gock.New("http://rpc1.localhost").
		Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "\"eth_getBlockReceipts\"") }).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logsBloom":"0x1","logs":[]} ]}`))

	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	if resp != nil {
		resp.Release()
	}
}

// Empty receipts should not trigger integrity errors
func TestNetworkIntegrity_EthGetBlockReceipts_EmptyReceipts_NoError(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	upCfg := &common.UpstreamConfig{Id: "rpc1", Type: common.UpstreamTypeEvm, Endpoint: "http://rpc1.localhost", Evm: &common.EvmUpstreamConfig{ChainId: 123, StatePollerInterval: common.Duration(5 * time.Second), StatePollerDebounce: common.Duration(1 * time.Second), Integrity: &common.UpstreamIntegrityConfig{EthGetBlockReceipts: &common.UpstreamIntegrityEthGetBlockReceiptsConfig{Enabled: true, CheckLogsBloom: util.BoolPtr(true), CheckLogIndexStrictIncrements: util.BoolPtr(true)}}}}

	util.SetupMocksForEvmStatePoller()

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

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":[{"blockNumber":"0x1"}]}`))
	req.SetNetwork(network)

	gock.New("http://rpc1.localhost").
		Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "\"eth_getBlockReceipts\"") }).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[]}`))

	ups := upr.GetNetworkUpstreams(ctx, util.EvmNetworkId(123))
	require.GreaterOrEqual(t, len(ups), 1)
	rawResp, fwdErr := ups[0].Forward(ctx, req, false)
	require.NoError(t, fwdErr)
	require.NotNil(t, rawResp)
	defer rawResp.Release()

	_, hookErr := evm.HandleUpstreamPostForward(ctx, network, ups[0], req, rawResp, nil, false)
	require.NoError(t, hookErr)
}

// Non-sequential logIndex should pass when strict increments check is disabled
func TestNetworkIntegrity_EthGetBlockReceipts_LogIndexCheckDisabled_NoError(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	upCfg := &common.UpstreamConfig{Id: "rpc1", Type: common.UpstreamTypeEvm, Endpoint: "http://rpc1.localhost", Evm: &common.EvmUpstreamConfig{ChainId: 123, StatePollerInterval: common.Duration(5 * time.Second), StatePollerDebounce: common.Duration(1 * time.Second), Integrity: &common.UpstreamIntegrityConfig{EthGetBlockReceipts: &common.UpstreamIntegrityEthGetBlockReceiptsConfig{Enabled: true, CheckLogIndexStrictIncrements: util.BoolPtr(false)}}}}

	util.SetupMocksForEvmStatePoller()

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

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":[{"blockNumber":"0x1"}]}`))
	req.SetNetwork(network)

	gock.New("http://rpc1.localhost").
		Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "\"eth_getBlockReceipts\"") }).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logs":[{"logIndex":"0x0"},{"logIndex":"0x2"}]}]}`))

	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	if resp != nil {
		resp.Release()
	}
}

// Retry fallback on logsBloom error: bad upstream then good upstream
func TestNetworkIntegrity_EthGetBlockReceipts_RetryFallbackGoodUpstream_Bloom(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	upBad := &common.UpstreamConfig{Id: "rpc-bad", Type: common.UpstreamTypeEvm, Endpoint: "http://rpc-bad.localhost", Evm: &common.EvmUpstreamConfig{ChainId: 123, StatePollerInterval: common.Duration(5 * time.Second), StatePollerDebounce: common.Duration(1 * time.Second), Integrity: &common.UpstreamIntegrityConfig{EthGetBlockReceipts: &common.UpstreamIntegrityEthGetBlockReceiptsConfig{Enabled: true, CheckLogsBloom: util.BoolPtr(true)}}}}
	upGood := &common.UpstreamConfig{Id: "rpc-good", Type: common.UpstreamTypeEvm, Endpoint: "http://rpc-good.localhost", Evm: &common.EvmUpstreamConfig{ChainId: 123, StatePollerInterval: common.Duration(5 * time.Second), StatePollerDebounce: common.Duration(1 * time.Second), Integrity: &common.UpstreamIntegrityConfig{EthGetBlockReceipts: &common.UpstreamIntegrityEthGetBlockReceiptsConfig{Enabled: true, CheckLogsBloom: util.BoolPtr(true)}}}}

	util.SetupMocksForEvmStatePoller()
	gock.New("http://rpc-bad.localhost").Post("").Persist().Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "\"eth_chainId\"") }).Reply(200).JSON([]byte(`{"result":"0x7b"}`))
	gock.New("http://rpc-good.localhost").Post("").Persist().Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "\"eth_chainId\"") }).Reply(200).JSON([]byte(`{"result":"0x7b"}`))

	rlr, _ := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{}, &log.Logger)
	mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
	vr := thirdparty.NewVendorsRegistry()
	pr, _ := thirdparty.NewProvidersRegistry(&log.Logger, vr, nil, nil)
	ssr, _ := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{Connector: &common.ConnectorConfig{Driver: common.DriverMemory, Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"}}})
	upr := upstream.NewUpstreamsRegistry(ctx, &log.Logger, "prjA", []*common.UpstreamConfig{upBad, upGood}, ssr, rlr, vr, pr, nil, mt, 1*time.Second, nil)
	upr.Bootstrap(ctx)
	time.Sleep(100 * time.Millisecond)

	ntwCfg := &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}, Failsafe: []*common.FailsafeConfig{{MatchMethod: "eth_getBlockReceipts", Retry: &common.RetryPolicyConfig{MaxAttempts: 2}}}}
	network, _ := NewNetwork(ctx, &log.Logger, "prjA", ntwCfg, rlr, upr, mt)
	require.NoError(t, upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123)))
	require.NoError(t, network.Bootstrap(ctx))

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":[{"blockNumber":"0x1"}]}`))
	req.SetNetwork(network)

	// Bad upstream -> non-zero bloom with zero logs
	gock.New("http://rpc-bad.localhost").Post("").Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "\"eth_getBlockReceipts\"") }).Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logsBloom":"0x1","logs":[]} ]}`))
	// Good upstream -> zero bloom with zero logs
	gock.New("http://rpc-good.localhost").Post("").Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "\"eth_getBlockReceipts\"") }).Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logsBloom":"0x0","logs":[]} ]}`))

	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	if resp != nil {
		resp.Release()
	}
}

// Invalid hex in logIndex should error when the check is enabled
func TestNetworkIntegrity_EthGetBlockReceipts_InvalidLogIndexHex_Error(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	upCfg := &common.UpstreamConfig{Id: "rpc1", Type: common.UpstreamTypeEvm, Endpoint: "http://rpc1.localhost", Evm: &common.EvmUpstreamConfig{ChainId: 123, StatePollerInterval: common.Duration(5 * time.Second), StatePollerDebounce: common.Duration(1 * time.Second), Integrity: &common.UpstreamIntegrityConfig{EthGetBlockReceipts: &common.UpstreamIntegrityEthGetBlockReceiptsConfig{Enabled: true, CheckLogIndexStrictIncrements: util.BoolPtr(true)}}}}

	util.SetupMocksForEvmStatePoller()

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

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":[{"blockNumber":"0x1"}]}`))
	req.SetNetwork(network)

	// Invalid hex in logIndex
	gock.New("http://rpc1.localhost").Post("").Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "\"eth_getBlockReceipts\"") }).Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logs":[{"logIndex":"0xzz"}]}]}`))

	ups := upr.GetNetworkUpstreams(ctx, util.EvmNetworkId(123))
	require.GreaterOrEqual(t, len(ups), 1)
	rawResp, fwdErr := ups[0].Forward(ctx, req, false)
	require.NoError(t, fwdErr)
	require.NotNil(t, rawResp)
	defer rawResp.Release()

	_, hookErr := evm.HandleUpstreamPostForward(ctx, network, ups[0], req, rawResp, nil, false)
	require.Error(t, hookErr)
	var mf *common.ErrUpstreamMalformedResponse
	require.True(t, errors.As(hookErr, &mf), "expected ErrUpstreamMalformedResponse, got: %v", hookErr)
}

// Retry fallback test for non-sequential logIndex bad->good
func TestNetworkIntegrity_EthGetBlockReceipts_RetryFallbackGoodUpstream(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	upBad := &common.UpstreamConfig{Id: "rpc-bad", Type: common.UpstreamTypeEvm, Endpoint: "http://rpc-bad.localhost", Evm: &common.EvmUpstreamConfig{ChainId: 123, StatePollerInterval: common.Duration(5 * time.Second), StatePollerDebounce: common.Duration(1 * time.Second), Integrity: &common.UpstreamIntegrityConfig{EthGetBlockReceipts: &common.UpstreamIntegrityEthGetBlockReceiptsConfig{Enabled: true, CheckLogIndexStrictIncrements: util.BoolPtr(true)}}}}
	upGood := &common.UpstreamConfig{Id: "rpc-good", Type: common.UpstreamTypeEvm, Endpoint: "http://rpc-good.localhost", Evm: &common.EvmUpstreamConfig{ChainId: 123, StatePollerInterval: common.Duration(5 * time.Second), StatePollerDebounce: common.Duration(1 * time.Second), Integrity: &common.UpstreamIntegrityConfig{EthGetBlockReceipts: &common.UpstreamIntegrityEthGetBlockReceiptsConfig{Enabled: true, CheckLogIndexStrictIncrements: util.BoolPtr(true)}}}}

	// Required poller mocks + chainId mocks for both upstreams
	util.SetupMocksForEvmStatePoller()
	gock.New("http://rpc-bad.localhost").
		Post("").
		Persist().
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "\"eth_chainId\"") }).
		Reply(200).
		JSON([]byte(`{"result":"0x7b"}`))
	gock.New("http://rpc-good.localhost").
		Post("").
		Persist().
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "\"eth_chainId\"") }).
		Reply(200).
		JSON([]byte(`{"result":"0x7b"}`))

	rlr, _ := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{}, &log.Logger)
	mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
	vr := thirdparty.NewVendorsRegistry()
	pr, _ := thirdparty.NewProvidersRegistry(&log.Logger, vr, nil, nil)
	ssr, _ := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{Connector: &common.ConnectorConfig{Driver: common.DriverMemory, Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"}}})
	upr := upstream.NewUpstreamsRegistry(ctx, &log.Logger, "prjA", []*common.UpstreamConfig{upBad, upGood}, ssr, rlr, vr, pr, nil, mt, 1*time.Second, nil)
	upr.Bootstrap(ctx)
	time.Sleep(100 * time.Millisecond)

	// Network with a retry policy
	ntwCfg := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
		Failsafe: []*common.FailsafeConfig{
			{
				MatchMethod: "eth_getBlockReceipts",
				Retry:       &common.RetryPolicyConfig{MaxAttempts: 2},
			},
		},
	}
	network, _ := NewNetwork(ctx, &log.Logger, "prjA", ntwCfg, rlr, upr, mt)
	require.NoError(t, upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123)))
	require.NoError(t, network.Bootstrap(ctx))

	// Request
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":[{"blockNumber":"0x1"}]}`))
	req.SetNetwork(network)

	// Bad upstream returns non-sequential indices
	gock.New("http://rpc-bad.localhost").
		Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "\"eth_getBlockReceipts\"") }).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logs":[{"logIndex":"0x0"},{"logIndex":"0x1"}]},{"blockHash":"0xabc","logs":[{"logIndex":"0x1"}]}]}`))

	// Good upstream returns sequential indices
	gock.New("http://rpc-good.localhost").
		Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "\"eth_getBlockReceipts\"") }).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logs":[{"logIndex":"0x0"}]},{"blockHash":"0xabc","logs":[{"logIndex":"0x1"},{"logIndex":"0x2"}]}]}`))

	// Forward via network. Expect final success due to retry to good upstream
	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	if resp != nil {
		defer resp.Release()
	}
}
