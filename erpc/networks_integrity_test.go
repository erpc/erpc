package erpc

import (
	"context"
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

// Helper to create a basic upstream config for tests
func createTestUpstreamConfig(id string) *common.UpstreamConfig {
	return &common.UpstreamConfig{
		Id:       id,
		Type:     common.UpstreamTypeEvm,
		Endpoint: "http://" + id + ".localhost",
		Evm: &common.EvmUpstreamConfig{
			ChainId:             123,
			StatePollerInterval: common.Duration(5 * time.Second),
			StatePollerDebounce: common.Duration(1 * time.Second),
		},
	}
}

// Helper to create properly initialized directives (numeric pointer fields are nil by default = unset)
func newTestDirectives() *common.RequestDirectives {
	return &common.RequestDirectives{}
}

// Helper to setup test network for integrity tests
func setupIntegrityTestNetwork(t *testing.T, ctx context.Context, upstreams []*common.UpstreamConfig, ntwCfg *common.NetworkConfig) (*Network, *upstream.UpstreamsRegistry) {
	rlr, _ := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{}, &log.Logger)
	mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
	vr := thirdparty.NewVendorsRegistry()
	pr, _ := thirdparty.NewProvidersRegistry(&log.Logger, vr, nil, nil)
	ssr, _ := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{Connector: &common.ConnectorConfig{Driver: common.DriverMemory, Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"}}})
	upr := upstream.NewUpstreamsRegistry(ctx, &log.Logger, "prjA", upstreams, ssr, rlr, vr, pr, nil, mt, 1*time.Second, nil)
	upr.Bootstrap(ctx)
	time.Sleep(100 * time.Millisecond)

	network, _ := NewNetwork(ctx, &log.Logger, "prjA", ntwCfg, rlr, upr, mt)
	require.NoError(t, upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123)))
	require.NoError(t, network.Bootstrap(ctx))

	return network, upr
}

// Network-level integrity test for eth_getBlockReceipts: strict logIndex increments
func TestNetworkIntegrity_EthGetBlockReceipts_LogIndexStrictIncrements(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	util.SetupMocksForEvmStatePoller()

	upCfg := createTestUpstreamConfig("rpc1")
	ntwCfg := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
	}
	network, upr := setupIntegrityTestNetwork(t, ctx, []*common.UpstreamConfig{upCfg}, ntwCfg)

	// Build request with validation directive enabled
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":[{"blockNumber":"0x1"}]}`))
	req.SetNetwork(network)
	dirs := newTestDirectives()
	dirs.EnforceLogIndexStrictIncrements = true
	req.SetDirectives(dirs)

	// Stub eth_getBlockReceipts with non-sequential global logIndex
	gock.New("http://rpc1.localhost").
		Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "\"eth_getBlockReceipts\"") }).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[
			{"blockHash":"0xabc","logs":[{"logIndex":"0x0"},{"logIndex":"0x1"}]},
			{"blockHash":"0xabc","logs":[{"logIndex":"0x1"}]}
		]}`))

	ups := upr.GetNetworkUpstreams(ctx, util.EvmNetworkId(123))
	require.GreaterOrEqual(t, len(ups), 1)
	rawResp, fwdErr := ups[0].Forward(ctx, req, false)
	require.NoError(t, fwdErr)
	require.NotNil(t, rawResp)
	defer rawResp.Release()

	_, hookErr := evm.HandleUpstreamPostForward(ctx, network, ups[0], req, rawResp, nil, false)
	require.Error(t, hookErr)
	require.True(t, common.HasErrorCode(hookErr, common.ErrCodeEndpointContentValidation), "expected ErrCodeEndpointContentValidation, got: %v", hookErr)
}

// Gap in global indices should error (contiguity enforcement)
func TestNetworkIntegrity_EthGetBlockReceipts_LogIndexGap_Error(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	util.SetupMocksForEvmStatePoller()

	upCfg := createTestUpstreamConfig("rpc1")
	ntwCfg := &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}}
	network, upr := setupIntegrityTestNetwork(t, ctx, []*common.UpstreamConfig{upCfg}, ntwCfg)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":[{"blockNumber":"0x1"}]}`))
	req.SetNetwork(network)
	dirs := newTestDirectives()
	dirs.EnforceLogIndexStrictIncrements = true
	req.SetDirectives(dirs)

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
	require.True(t, common.HasErrorCode(hookErr, common.ErrCodeEndpointContentValidation), "expected ErrCodeEndpointContentValidation, got: %v", hookErr)
}

// Contiguous indices 0x0,0x1,0x2 should pass when enabled
func TestNetworkIntegrity_EthGetBlockReceipts_LogIndexContiguous_NoError(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	util.SetupMocksForEvmStatePoller()

	upCfg := createTestUpstreamConfig("rpc1")
	ntwCfg := &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}}
	network, upr := setupIntegrityTestNetwork(t, ctx, []*common.UpstreamConfig{upCfg}, ntwCfg)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":[{"blockNumber":"0x1"}]}`))
	req.SetNetwork(network)
	dirs := newTestDirectives()
	dirs.EnforceLogIndexStrictIncrements = true
	req.SetDirectives(dirs)

	// 0x0, 0x1, 0x2 -> OK
	gock.New("http://rpc1.localhost").
		Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "\"eth_getBlockReceipts\"") }).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logs":[{"logIndex":"0x0"}]},{"blockHash":"0xabc","logs":[{"logIndex":"0x1"}]},{"blockHash":"0xabc","logs":[{"logIndex":"0x2"}]}]}`))

	ups := upr.GetNetworkUpstreams(ctx, util.EvmNetworkId(123))
	require.GreaterOrEqual(t, len(ups), 1)
	rawResp, fwdErr := ups[0].Forward(ctx, req, false)
	require.NoError(t, fwdErr)
	require.NotNil(t, rawResp)
	defer rawResp.Release()

	_, hookErr := evm.HandleUpstreamPostForward(ctx, network, ups[0], req, rawResp, nil, false)
	require.NoError(t, hookErr)
}

// Inconsistent blockHash across receipts should error when ValidationExpectedBlockHash is set
func TestNetworkIntegrity_EthGetBlockReceipts_InconsistentBlockHash_Error(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	util.SetupMocksForEvmStatePoller()

	upCfg := createTestUpstreamConfig("rpc1")
	ntwCfg := &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}}
	network, upr := setupIntegrityTestNetwork(t, ctx, []*common.UpstreamConfig{upCfg}, ntwCfg)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":[{"blockNumber":"0x1"}]}`))
	req.SetNetwork(network)
	// Set expected block hash - response has different hash
	dirs := newTestDirectives()
	dirs.ValidationExpectedBlockHash = "0xabc"
	req.SetDirectives(dirs)

	// Two receipts with different blockHash - second one doesn't match expected
	gock.New("http://rpc1.localhost").Post("").Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "\"eth_getBlockReceipts\"") }).Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logs":[]},{"blockHash":"0xdef","logs":[]}]}`))

	ups := upr.GetNetworkUpstreams(ctx, util.EvmNetworkId(123))
	require.GreaterOrEqual(t, len(ups), 1)
	rawResp, fwdErr := ups[0].Forward(ctx, req, false)
	require.NoError(t, fwdErr)
	require.NotNil(t, rawResp)
	defer rawResp.Release()

	_, hookErr := evm.HandleUpstreamPostForward(ctx, network, ups[0], req, rawResp, nil, false)
	require.Error(t, hookErr)
	require.True(t, common.HasErrorCode(hookErr, common.ErrCodeEndpointContentValidation), "expected ErrCodeEndpointContentValidation, got: %v", hookErr)
}

// Result not an array should error (malformed upstream response)
func TestNetworkIntegrity_EthGetBlockReceipts_ResultNotArray_Error(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	util.SetupMocksForEvmStatePoller()

	upCfg := createTestUpstreamConfig("rpc1")
	ntwCfg := &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}}
	network, upr := setupIntegrityTestNetwork(t, ctx, []*common.UpstreamConfig{upCfg}, ntwCfg)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":[{"blockNumber":"0x1"}]}`))
	req.SetNetwork(network)
	// Enable any validation to trigger parsing
	dirs := newTestDirectives()
	dirs.EnforceLogIndexStrictIncrements = true
	req.SetDirectives(dirs)

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
	require.True(t, common.HasErrorCode(hookErr, common.ErrCodeEndpointContentValidation), "expected ErrCodeEndpointContentValidation, got: %v", hookErr)
}

// Decreasing logIndex should error
func TestNetworkIntegrity_EthGetBlockReceipts_LogIndexDecreasing_Error(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	util.SetupMocksForEvmStatePoller()

	upCfg := createTestUpstreamConfig("rpc1")
	ntwCfg := &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}}
	network, upr := setupIntegrityTestNetwork(t, ctx, []*common.UpstreamConfig{upCfg}, ntwCfg)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":[{"blockNumber":"0x1"}]}`))
	req.SetNetwork(network)
	dirs := newTestDirectives()
	dirs.EnforceLogIndexStrictIncrements = true
	req.SetDirectives(dirs)

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
	require.True(t, common.HasErrorCode(hookErr, common.ErrCodeEndpointContentValidation), "expected ErrCodeEndpointContentValidation, got: %v", hookErr)
}

func TestNetworkIntegrity_EthGetBlockReceipts_MissingLogIndexEntries_Error(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	util.SetupMocksForEvmStatePoller()

	upCfg := createTestUpstreamConfig("rpc1")
	ntwCfg := &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}}
	network, upr := setupIntegrityTestNetwork(t, ctx, []*common.UpstreamConfig{upCfg}, ntwCfg)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":[{"blockNumber":"0x1"}]}`))
	req.SetNetwork(network)
	dirs := newTestDirectives()
	dirs.EnforceLogIndexStrictIncrements = true
	req.SetDirectives(dirs)

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
	require.True(t, common.HasErrorCode(hookErr, common.ErrCodeEndpointContentValidation), "expected ErrCodeEndpointContentValidation, got: %v", hookErr)
}

// logsBloom non-zero with zero logs should error when EnforceNonEmptyLogsBloom is enabled
func TestNetworkIntegrity_EthGetBlockReceipts_LogsBloomNonZeroZeroLogs_Error(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	util.SetupMocksForEvmStatePoller()

	upCfg := createTestUpstreamConfig("rpc1")
	ntwCfg := &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}}
	network, upr := setupIntegrityTestNetwork(t, ctx, []*common.UpstreamConfig{upCfg}, ntwCfg)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":[{"blockNumber":"0x1"}]}`))
	req.SetNetwork(network)
	dirs := newTestDirectives()
	dirs.EnforceNonEmptyLogsBloom = true
	req.SetDirectives(dirs)

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
	require.True(t, common.HasErrorCode(hookErr, common.ErrCodeEndpointContentValidation), "expected ErrCodeEndpointContentValidation, got: %v", hookErr)
}

// logsBloom check disabled should allow non-zero bloom with zero logs
func TestNetworkIntegrity_EthGetBlockReceipts_LogsBloomDisabled_NoError(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	util.SetupMocksForEvmStatePoller()

	gock.New("http://rpc1.localhost").
		Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "\"eth_getBlockReceipts\"") }).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logsBloom":"0x1","logs":[]} ]}`))

	upCfg := createTestUpstreamConfig("rpc1")
	ntwCfg := &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}}
	network, upr := setupIntegrityTestNetwork(t, ctx, []*common.UpstreamConfig{upCfg}, ntwCfg)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":[{"blockNumber":"0x1"}]}`))
	req.SetNetwork(network)
	// No validation directives set - should pass
	dirs := newTestDirectives()
	dirs.EnforceNonEmptyLogsBloom = false
	req.SetDirectives(dirs)

	ups := upr.GetNetworkUpstreams(ctx, util.EvmNetworkId(123))
	require.GreaterOrEqual(t, len(ups), 1)
	rawResp, fwdErr := ups[0].Forward(ctx, req, false)
	require.NoError(t, fwdErr)
	require.NotNil(t, rawResp)
	defer rawResp.Release()

	_, hookErr := evm.HandleUpstreamPostForward(ctx, network, ups[0], req, rawResp, nil, false)
	require.NoError(t, hookErr)
}

// Empty receipts should not trigger integrity errors
func TestNetworkIntegrity_EthGetBlockReceipts_EmptyReceipts_NoError(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	util.SetupMocksForEvmStatePoller()

	upCfg := createTestUpstreamConfig("rpc1")
	ntwCfg := &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}}
	network, upr := setupIntegrityTestNetwork(t, ctx, []*common.UpstreamConfig{upCfg}, ntwCfg)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":[{"blockNumber":"0x1"}]}`))
	req.SetNetwork(network)
	dirs := newTestDirectives()
	dirs.EnforceNonEmptyLogsBloom = true
	dirs.EnforceLogIndexStrictIncrements = true
	dirs.RetryEmpty = false // Don't retry empty - we want to test empty response handling
	req.SetDirectives(dirs)

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

	util.SetupMocksForEvmStatePoller()

	upCfg := createTestUpstreamConfig("rpc1")
	ntwCfg := &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}}
	network, upr := setupIntegrityTestNetwork(t, ctx, []*common.UpstreamConfig{upCfg}, ntwCfg)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":[{"blockNumber":"0x1"}]}`))
	req.SetNetwork(network)
	// Directive disabled - non-sequential should pass
	dirs := newTestDirectives()
	dirs.EnforceLogIndexStrictIncrements = false
	req.SetDirectives(dirs)

	gock.New("http://rpc1.localhost").
		Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "\"eth_getBlockReceipts\"") }).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logs":[{"logIndex":"0x0"},{"logIndex":"0x2"}]}]}`))

	ups := upr.GetNetworkUpstreams(ctx, util.EvmNetworkId(123))
	require.GreaterOrEqual(t, len(ups), 1)
	rawResp, fwdErr := ups[0].Forward(ctx, req, false)
	require.NoError(t, fwdErr)
	require.NotNil(t, rawResp)
	defer rawResp.Release()

	_, hookErr := evm.HandleUpstreamPostForward(ctx, network, ups[0], req, rawResp, nil, false)
	require.NoError(t, hookErr)
}

// Retry fallback on logsBloom error: bad upstream then good upstream
func TestNetworkIntegrity_EthGetBlockReceipts_RetryFallbackGoodUpstream_Bloom(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	util.SetupMocksForEvmStatePoller()

	upBad := createTestUpstreamConfig("rpc-bad")
	upBad.Endpoint = "http://rpc1.localhost"
	upGood := createTestUpstreamConfig("rpc-good")
	upGood.Endpoint = "http://rpc2.localhost"

	ntwCfg := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
		Failsafe:     []*common.FailsafeConfig{{MatchMethod: "eth_getBlockReceipts", Retry: &common.RetryPolicyConfig{MaxAttempts: 2}}},
		// Enable validation via directive defaults
		DirectiveDefaults: &common.DirectiveDefaultsConfig{
			EnforceNonEmptyLogsBloom: util.BoolPtr(true),
		},
	}
	network, _ := setupIntegrityTestNetwork(t, ctx, []*common.UpstreamConfig{upBad, upGood}, ntwCfg)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":[{"blockNumber":"0x1"}]}`))
	req.SetNetwork(network)

	// Bad upstream -> non-zero bloom with zero logs
	gock.New("http://rpc1.localhost").Post("").Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "\"eth_getBlockReceipts\"") }).Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logsBloom":"0x1","logs":[]} ]}`))
	// Good upstream -> zero bloom with zero logs
	gock.New("http://rpc2.localhost").Post("").Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "\"eth_getBlockReceipts\"") }).Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logsBloom":"0x0","logs":[]} ]}`))

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

	util.SetupMocksForEvmStatePoller()

	upCfg := createTestUpstreamConfig("rpc1")
	ntwCfg := &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}}
	network, upr := setupIntegrityTestNetwork(t, ctx, []*common.UpstreamConfig{upCfg}, ntwCfg)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":[{"blockNumber":"0x1"}]}`))
	req.SetNetwork(network)
	dirs := newTestDirectives()
	dirs.EnforceLogIndexStrictIncrements = true
	req.SetDirectives(dirs)

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
	require.True(t, common.HasErrorCode(hookErr, common.ErrCodeEndpointContentValidation), "expected ErrCodeEndpointContentValidation, got: %v", hookErr)
}

// Retry fallback test for non-sequential logIndex bad->good
func TestNetworkIntegrity_EthGetBlockReceipts_RetryFallbackGoodUpstream(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	util.SetupMocksForEvmStatePoller()
	gock.New("http://rpc1.localhost").
		Post("").
		Persist().
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "\"eth_chainId\"") }).
		Reply(200).
		JSON([]byte(`{"result":"0x7b"}`))
	gock.New("http://rpc2.localhost").
		Post("").
		Persist().
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "\"eth_chainId\"") }).
		Reply(200).
		JSON([]byte(`{"result":"0x7b"}`))

	upBad := createTestUpstreamConfig("rpc-bad")
	upBad.Endpoint = "http://rpc1.localhost"
	upGood := createTestUpstreamConfig("rpc-good")
	upGood.Endpoint = "http://rpc2.localhost"

	// Network with a retry policy and directive defaults
	ntwCfg := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
		Failsafe: []*common.FailsafeConfig{
			{
				MatchMethod: "eth_getBlockReceipts",
				Retry:       &common.RetryPolicyConfig{MaxAttempts: 2},
			},
		},
		DirectiveDefaults: &common.DirectiveDefaultsConfig{
			EnforceLogIndexStrictIncrements: util.BoolPtr(true),
		},
	}
	network, _ := setupIntegrityTestNetwork(t, ctx, []*common.UpstreamConfig{upBad, upGood}, ntwCfg)

	// Request
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":[{"blockNumber":"0x1"}]}`))
	req.SetNetwork(network)

	// Bad upstream returns non-sequential indices
	gock.New("http://rpc1.localhost").
		Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "\"eth_getBlockReceipts\"") }).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logs":[{"logIndex":"0x0"},{"logIndex":"0x1"}]},{"blockHash":"0xabc","logs":[{"logIndex":"0x1"}]}]}`))

	// Good upstream returns sequential indices
	gock.New("http://rpc2.localhost").
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
