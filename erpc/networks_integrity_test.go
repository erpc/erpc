package erpc

import (
	"context"
	"fmt"
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

// Helper to convert hex string to bytes for tests (panics on error)
func mustHexToBytes(hex string) []byte {
	b, err := common.HexToBytes(hex)
	if err != nil {
		panic(err)
	}
	return b
}

// Helper to setup test network for integrity tests
func setupIntegrityTestNetwork(t *testing.T, ctx context.Context, upstreams []*common.UpstreamConfig, ntwCfg *common.NetworkConfig) (*Network, *upstream.UpstreamsRegistry) {
	rlr, _ := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{}, &log.Logger)
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

// logsBloom non-zero with zero logs should error when ValidateLogsBloomNotEmpty is enabled
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
	dirs.ValidateLogsBloomEmptiness = true
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
	dirs.ValidateLogsBloomEmptiness = false
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
	dirs.ValidateLogsBloomEmptiness = true
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
			ValidateLogsBloomEmptiness: util.BoolPtr(true),
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

// logsBloom zero with logs present should error when ValidateLogsBloomEmptiness is enabled
func TestNetworkIntegrity_EthGetBlockReceipts_LogsBloomZeroWithLogs_Error(t *testing.T) {
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
	dirs.ValidateLogsBloomEmptiness = true
	req.SetDirectives(dirs)

	// Zero logsBloom with logs present -> error (inconsistent)
	gock.New("http://rpc1.localhost").
		Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "\"eth_getBlockReceipts\"") }).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logsBloom":"0x0","logs":[{"logIndex":"0x0","address":"0x1234567890123456789012345678901234567890","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"]}]}]}`))

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

// Zero bloom with zero logs should pass (consistent empty state)
func TestNetworkIntegrity_EthGetBlockReceipts_LogsBloomZeroWithZeroLogs_NoError(t *testing.T) {
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
	dirs.ValidateLogsBloomEmptiness = true
	req.SetDirectives(dirs)

	// Zero logsBloom with zero logs -> OK (consistent)
	gock.New("http://rpc1.localhost").
		Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "\"eth_getBlockReceipts\"") }).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logsBloom":"0x0","logs":[]}]}`))

	ups := upr.GetNetworkUpstreams(ctx, util.EvmNetworkId(123))
	require.GreaterOrEqual(t, len(ups), 1)
	rawResp, fwdErr := ups[0].Forward(ctx, req, false)
	require.NoError(t, fwdErr)
	require.NotNil(t, rawResp)
	defer rawResp.Release()

	_, hookErr := evm.HandleUpstreamPostForward(ctx, network, ups[0], req, rawResp, nil, false)
	require.NoError(t, hookErr)
}

// ReceiptsCountExact mismatch should error
func TestNetworkIntegrity_EthGetBlockReceipts_ReceiptsCountExact_Mismatch_Error(t *testing.T) {
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
	// Expect exactly 3 receipts
	exactCount := int64(3)
	dirs.ReceiptsCountExact = &exactCount
	req.SetDirectives(dirs)

	// Return only 2 receipts -> mismatch error
	gock.New("http://rpc1.localhost").
		Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "\"eth_getBlockReceipts\"") }).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logs":[]},{"blockHash":"0xabc","logs":[]}]}`))

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

// ReceiptsCountExact match should pass
func TestNetworkIntegrity_EthGetBlockReceipts_ReceiptsCountExact_Match_NoError(t *testing.T) {
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
	// Expect exactly 2 receipts
	exactCount := int64(2)
	dirs.ReceiptsCountExact = &exactCount
	req.SetDirectives(dirs)

	// Return exactly 2 receipts -> OK
	gock.New("http://rpc1.localhost").
		Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "\"eth_getBlockReceipts\"") }).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logs":[]},{"blockHash":"0xabc","logs":[]}]}`))

	ups := upr.GetNetworkUpstreams(ctx, util.EvmNetworkId(123))
	require.GreaterOrEqual(t, len(ups), 1)
	rawResp, fwdErr := ups[0].Forward(ctx, req, false)
	require.NoError(t, fwdErr)
	require.NotNil(t, rawResp)
	defer rawResp.Release()

	_, hookErr := evm.HandleUpstreamPostForward(ctx, network, ups[0], req, rawResp, nil, false)
	require.NoError(t, hookErr)
}

// ReceiptsCountAtLeast should error when count is below threshold
func TestNetworkIntegrity_EthGetBlockReceipts_ReceiptsCountAtLeast_BelowThreshold_Error(t *testing.T) {
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
	// Expect at least 5 receipts
	atLeastCount := int64(5)
	dirs.ReceiptsCountAtLeast = &atLeastCount
	req.SetDirectives(dirs)

	// Return only 2 receipts -> below threshold error
	gock.New("http://rpc1.localhost").
		Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "\"eth_getBlockReceipts\"") }).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logs":[]},{"blockHash":"0xabc","logs":[]}]}`))

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

// ReceiptsCountAtLeast should pass when count meets threshold
func TestNetworkIntegrity_EthGetBlockReceipts_ReceiptsCountAtLeast_MeetsThreshold_NoError(t *testing.T) {
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
	// Expect at least 2 receipts
	atLeastCount := int64(2)
	dirs.ReceiptsCountAtLeast = &atLeastCount
	req.SetDirectives(dirs)

	// Return exactly 3 receipts -> meets threshold OK
	gock.New("http://rpc1.localhost").
		Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "\"eth_getBlockReceipts\"") }).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logs":[]},{"blockHash":"0xabc","logs":[]},{"blockHash":"0xabc","logs":[]}]}`))

	ups := upr.GetNetworkUpstreams(ctx, util.EvmNetworkId(123))
	require.GreaterOrEqual(t, len(ups), 1)
	rawResp, fwdErr := ups[0].Forward(ctx, req, false)
	require.NoError(t, fwdErr)
	require.NotNil(t, rawResp)
	defer rawResp.Release()

	_, hookErr := evm.HandleUpstreamPostForward(ctx, network, ups[0], req, rawResp, nil, false)
	require.NoError(t, hookErr)
}

// GroundTruthTransactions cross-validation: receipt tx hash mismatch should error
func TestNetworkIntegrity_EthGetBlockReceipts_GroundTruthTxHashMismatch_Error(t *testing.T) {
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
	dirs.ValidateReceiptTransactionMatch = true
	// Ground truth: expect tx hash 0xaaa... at index 0
	dirs.GroundTruthTransactions = []*common.GroundTruthTransaction{
		{Hash: mustHexToBytes("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")},
	}
	req.SetDirectives(dirs)

	// Receipt has different tx hash 0xbbb...
	gock.New("http://rpc1.localhost").
		Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "\"eth_getBlockReceipts\"") }).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","transactionHash":"0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","logs":[]}]}`))

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

// GroundTruthTransactions cross-validation: matching tx hash should pass
func TestNetworkIntegrity_EthGetBlockReceipts_GroundTruthTxHashMatch_NoError(t *testing.T) {
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
	dirs.ValidateReceiptTransactionMatch = true
	// Ground truth: expect tx hash 0xaaa... at index 0
	dirs.GroundTruthTransactions = []*common.GroundTruthTransaction{
		{Hash: mustHexToBytes("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")},
	}
	req.SetDirectives(dirs)

	// Receipt has matching tx hash 0xaaa...
	gock.New("http://rpc1.localhost").
		Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "\"eth_getBlockReceipts\"") }).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","transactionHash":"0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","logs":[]}]}`))

	ups := upr.GetNetworkUpstreams(ctx, util.EvmNetworkId(123))
	require.GreaterOrEqual(t, len(ups), 1)
	rawResp, fwdErr := ups[0].Forward(ctx, req, false)
	require.NoError(t, fwdErr)
	require.NotNil(t, rawResp)
	defer rawResp.Release()

	_, hookErr := evm.HandleUpstreamPostForward(ctx, network, ups[0], req, rawResp, nil, false)
	require.NoError(t, hookErr)
}

// Contract creation validation: tx.to is nil but receipt has no contractAddress should error
func TestNetworkIntegrity_EthGetBlockReceipts_ContractCreationMissingAddress_Error(t *testing.T) {
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
	dirs.ValidateReceiptTransactionMatch = true
	dirs.ValidateContractCreation = true
	// Ground truth: tx with nil To (contract creation)
	dirs.GroundTruthTransactions = []*common.GroundTruthTransaction{
		{Hash: mustHexToBytes("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), To: nil},
	}
	req.SetDirectives(dirs)

	// Receipt is missing contractAddress for contract creation tx
	gock.New("http://rpc1.localhost").
		Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "\"eth_getBlockReceipts\"") }).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","transactionHash":"0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","contractAddress":null,"logs":[]}]}`))

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

// Contract creation validation: tx.to is nil and receipt has contractAddress should pass
func TestNetworkIntegrity_EthGetBlockReceipts_ContractCreationWithAddress_NoError(t *testing.T) {
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
	dirs.ValidateReceiptTransactionMatch = true
	dirs.ValidateContractCreation = true
	// Ground truth: tx with nil To (contract creation)
	dirs.GroundTruthTransactions = []*common.GroundTruthTransaction{
		{Hash: mustHexToBytes("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), To: nil},
	}
	req.SetDirectives(dirs)

	// Receipt has contractAddress for contract creation tx
	gock.New("http://rpc1.localhost").
		Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "\"eth_getBlockReceipts\"") }).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","transactionHash":"0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","contractAddress":"0x1234567890123456789012345678901234567890","logs":[]}]}`))

	ups := upr.GetNetworkUpstreams(ctx, util.EvmNetworkId(123))
	require.GreaterOrEqual(t, len(ups), 1)
	rawResp, fwdErr := ups[0].Forward(ctx, req, false)
	require.NoError(t, fwdErr)
	require.NotNil(t, rawResp)
	defer rawResp.Release()

	_, hookErr := evm.HandleUpstreamPostForward(ctx, network, ups[0], req, rawResp, nil, false)
	require.NoError(t, hookErr)
}

// =============================================================================
// FAILSAFE POLICY + VALIDATION INTEGRATION TESTS
// These tests verify that validation errors properly trigger retry/hedge/fallback
// to other upstreams, ensuring we get valid data from the pool of upstreams.
// =============================================================================

// Test: Retry policy retries to next upstream when first returns invalid bloom data
func TestNetworkIntegrity_Retry_ValidationError_FallbackToGoodUpstream(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	util.SetupMocksForEvmStatePoller()

	// Setup 3 upstreams: first 2 return invalid data, third returns valid
	up1 := createTestUpstreamConfig("rpc1")
	up1.Endpoint = "http://rpc1.localhost"
	up2 := createTestUpstreamConfig("rpc2")
	up2.Endpoint = "http://rpc2.localhost"
	up3 := createTestUpstreamConfig("rpc3")
	up3.Endpoint = "http://rpc3.localhost"

	ntwCfg := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
		Failsafe: []*common.FailsafeConfig{{
			MatchMethod: "eth_getBlockReceipts",
			Retry:       &common.RetryPolicyConfig{MaxAttempts: 3},
		}},
		DirectiveDefaults: &common.DirectiveDefaultsConfig{
			ValidateLogsBloomEmptiness: util.BoolPtr(true),
		},
	}
	network, _ := setupIntegrityTestNetwork(t, ctx, []*common.UpstreamConfig{up1, up2, up3}, ntwCfg)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":["0x1"]}`))
	req.SetNetwork(network)

	// rpc1: Invalid - non-zero bloom with zero logs
	gock.New("http://rpc1.localhost").Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockReceipts") }).
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logsBloom":"0x1","logs":[]}]}`))

	// rpc2: Invalid - non-zero bloom with zero logs
	gock.New("http://rpc2.localhost").Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockReceipts") }).
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logsBloom":"0x1","logs":[]}]}`))

	// rpc3: Valid - zero bloom with zero logs (consistent)
	gock.New("http://rpc3.localhost").Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockReceipts") }).
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logsBloom":"0x0","logs":[]}]}`))

	resp, err := network.Forward(ctx, req)
	require.NoError(t, err, "Should succeed after retrying to good upstream")
	require.NotNil(t, resp)
	if resp != nil {
		// Verify we got valid data (from rpc3)
		jrr, _ := resp.JsonRpcResponse()
		require.NotNil(t, jrr)
		require.Nil(t, jrr.Error)
		resp.Release()
	}
}

// Test: All upstreams return invalid data - should fail with exhausted error
func TestNetworkIntegrity_Retry_AllUpstreamsInvalid_ExhaustedError(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	util.SetupMocksForEvmStatePoller()

	// Setup 2 upstreams: both return invalid data
	up1 := createTestUpstreamConfig("rpc1")
	up1.Endpoint = "http://rpc1.localhost"
	up2 := createTestUpstreamConfig("rpc2")
	up2.Endpoint = "http://rpc2.localhost"

	ntwCfg := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
		Failsafe: []*common.FailsafeConfig{{
			MatchMethod: "eth_getBlockReceipts",
			Retry:       &common.RetryPolicyConfig{MaxAttempts: 2},
		}},
		DirectiveDefaults: &common.DirectiveDefaultsConfig{
			ValidateLogsBloomEmptiness: util.BoolPtr(true),
		},
	}
	network, _ := setupIntegrityTestNetwork(t, ctx, []*common.UpstreamConfig{up1, up2}, ntwCfg)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":["0x1"]}`))
	req.SetNetwork(network)

	// Both upstreams return invalid data
	gock.New("http://rpc1.localhost").Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockReceipts") }).
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logsBloom":"0x1","logs":[]}]}`))

	gock.New("http://rpc2.localhost").Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockReceipts") }).
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logsBloom":"0x1","logs":[]}]}`))

	resp, err := network.Forward(ctx, req)

	// Should fail with exhausted or retry exceeded error since all upstreams returned invalid data
	require.Error(t, err, "Should fail when all upstreams return invalid data")
	require.True(t,
		common.HasErrorCode(err, common.ErrCodeUpstreamsExhausted) ||
			common.HasErrorCode(err, common.ErrCodeFailsafeRetryExceeded),
		"Expected upstreams exhausted or retry exceeded error, got: %v", err)
	if resp != nil {
		resp.Release()
	}
}

// Test: ReceiptsCountExact mismatch triggers retry to upstream with correct count
func TestNetworkIntegrity_Retry_ReceiptsCountMismatch_FallbackToCorrectUpstream(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	util.SetupMocksForEvmStatePoller()

	up1 := createTestUpstreamConfig("rpc1")
	up1.Endpoint = "http://rpc1.localhost"
	up2 := createTestUpstreamConfig("rpc2")
	up2.Endpoint = "http://rpc2.localhost"

	ntwCfg := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
		Failsafe: []*common.FailsafeConfig{{
			MatchMethod: "eth_getBlockReceipts",
			Retry:       &common.RetryPolicyConfig{MaxAttempts: 2},
		}},
	}
	network, _ := setupIntegrityTestNetwork(t, ctx, []*common.UpstreamConfig{up1, up2}, ntwCfg)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":["0x1"]}`))
	req.SetNetwork(network)

	// Set directive to expect exactly 2 receipts
	dirs := newTestDirectives()
	exactCount := int64(2)
	dirs.ReceiptsCountExact = &exactCount
	req.SetDirectives(dirs)

	// rpc1: Returns only 1 receipt (mismatch)
	gock.New("http://rpc1.localhost").Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockReceipts") }).
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logs":[]}]}`))

	// rpc2: Returns 2 receipts (correct)
	gock.New("http://rpc2.localhost").Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockReceipts") }).
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logs":[]},{"blockHash":"0xabc","logs":[]}]}`))

	resp, err := network.Forward(ctx, req)
	require.NoError(t, err, "Should succeed after retrying to upstream with correct receipt count")
	require.NotNil(t, resp)
	if resp != nil {
		resp.Release()
	}
}

// Test: LogIndex strict increment violation triggers retry
func TestNetworkIntegrity_Retry_LogIndexViolation_FallbackToValidUpstream(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	util.SetupMocksForEvmStatePoller()

	up1 := createTestUpstreamConfig("rpc1")
	up1.Endpoint = "http://rpc1.localhost"
	up2 := createTestUpstreamConfig("rpc2")
	up2.Endpoint = "http://rpc2.localhost"

	ntwCfg := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
		Failsafe: []*common.FailsafeConfig{{
			MatchMethod: "eth_getBlockReceipts",
			Retry:       &common.RetryPolicyConfig{MaxAttempts: 2},
		}},
		DirectiveDefaults: &common.DirectiveDefaultsConfig{
			EnforceLogIndexStrictIncrements: util.BoolPtr(true),
		},
	}
	network, _ := setupIntegrityTestNetwork(t, ctx, []*common.UpstreamConfig{up1, up2}, ntwCfg)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":["0x1"]}`))
	req.SetNetwork(network)

	// rpc1: Non-sequential log indices (0, 5 - gap)
	gock.New("http://rpc1.localhost").Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockReceipts") }).
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logs":[{"logIndex":"0x0"},{"logIndex":"0x5"}]}]}`))

	// rpc2: Sequential log indices (0, 1)
	gock.New("http://rpc2.localhost").Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockReceipts") }).
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logs":[{"logIndex":"0x0"},{"logIndex":"0x1"}]}]}`))

	resp, err := network.Forward(ctx, req)
	require.NoError(t, err, "Should succeed after retrying to upstream with valid log indices")
	require.NotNil(t, resp)
	if resp != nil {
		resp.Release()
	}
}

// Test: Logs exist but bloom is zero - triggers retry to upstream with consistent data
func TestNetworkIntegrity_Retry_LogsWithZeroBloom_FallbackToConsistentUpstream(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	util.SetupMocksForEvmStatePoller()

	up1 := createTestUpstreamConfig("rpc1")
	up1.Endpoint = "http://rpc1.localhost"
	up2 := createTestUpstreamConfig("rpc2")
	up2.Endpoint = "http://rpc2.localhost"

	ntwCfg := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
		Failsafe: []*common.FailsafeConfig{{
			MatchMethod: "eth_getBlockReceipts",
			Retry:       &common.RetryPolicyConfig{MaxAttempts: 2},
		}},
		DirectiveDefaults: &common.DirectiveDefaultsConfig{
			ValidateLogsBloomEmptiness: util.BoolPtr(true),
		},
	}
	network, _ := setupIntegrityTestNetwork(t, ctx, []*common.UpstreamConfig{up1, up2}, ntwCfg)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":["0x1"]}`))
	req.SetNetwork(network)

	// rpc1: Logs exist but bloom is zero (inconsistent)
	gock.New("http://rpc1.localhost").Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockReceipts") }).
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logsBloom":"0x0","logs":[{"logIndex":"0x0","address":"0x1234567890123456789012345678901234567890","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"]}]}]}`))

	// rpc2: Non-zero bloom with logs (consistent)
	gock.New("http://rpc2.localhost").Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockReceipts") }).
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logsBloom":"0x1","logs":[{"logIndex":"0x0","address":"0x1234567890123456789012345678901234567890","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"]}]}]}`))

	resp, err := network.Forward(ctx, req)
	require.NoError(t, err, "Should succeed after retrying to upstream with consistent bloom/logs")
	require.NotNil(t, resp)
	if resp != nil {
		resp.Release()
	}
}

// Test: Multiple validation errors across upstreams - eventually finds valid one
func TestNetworkIntegrity_Retry_MultipleValidationTypes_EventuallySucceeds(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	util.SetupMocksForEvmStatePoller()

	// 4 upstreams with different validation issues
	up1 := createTestUpstreamConfig("rpc1")
	up1.Endpoint = "http://rpc1.localhost"
	up2 := createTestUpstreamConfig("rpc2")
	up2.Endpoint = "http://rpc2.localhost"
	up3 := createTestUpstreamConfig("rpc3")
	up3.Endpoint = "http://rpc3.localhost"
	up4 := createTestUpstreamConfig("rpc4")
	up4.Endpoint = "http://rpc4.localhost"

	ntwCfg := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
		Failsafe: []*common.FailsafeConfig{{
			MatchMethod: "eth_getBlockReceipts",
			Retry:       &common.RetryPolicyConfig{MaxAttempts: 4},
		}},
		DirectiveDefaults: &common.DirectiveDefaultsConfig{
			ValidateLogsBloomEmptiness:      util.BoolPtr(true),
			EnforceLogIndexStrictIncrements: util.BoolPtr(true),
		},
	}
	network, _ := setupIntegrityTestNetwork(t, ctx, []*common.UpstreamConfig{up1, up2, up3, up4}, ntwCfg)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":["0x1"]}`))
	req.SetNetwork(network)

	// rpc1: Bloom inconsistency (non-zero bloom, no logs)
	gock.New("http://rpc1.localhost").Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockReceipts") }).
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logsBloom":"0x1","logs":[]}]}`))

	// rpc2: LogIndex gap (0, 5)
	gock.New("http://rpc2.localhost").Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockReceipts") }).
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logsBloom":"0x0","logs":[{"logIndex":"0x0"},{"logIndex":"0x5"}]}]}`))

	// rpc3: Logs with zero bloom (inconsistent)
	gock.New("http://rpc3.localhost").Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockReceipts") }).
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logsBloom":"0x0","logs":[{"logIndex":"0x0","address":"0x1234567890123456789012345678901234567890","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"]}]}]}`))

	// rpc4: All valid (consistent bloom, sequential logs)
	gock.New("http://rpc4.localhost").Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockReceipts") }).
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logsBloom":"0x1","logs":[{"logIndex":"0x0","address":"0x1234567890123456789012345678901234567890","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"]},{"logIndex":"0x1","address":"0x1234567890123456789012345678901234567890","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"]}]}]}`))

	resp, err := network.Forward(ctx, req)
	require.NoError(t, err, "Should eventually succeed after trying multiple upstreams")
	require.NotNil(t, resp)
	if resp != nil {
		resp.Release()
	}
}

// Test: Validation disabled - accepts invalid data without retry
func TestNetworkIntegrity_ValidationDisabled_AcceptsInvalidData(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 1) // rpc2 should NOT be called

	up1 := createTestUpstreamConfig("rpc1")
	up1.Endpoint = "http://rpc1.localhost"
	up2 := createTestUpstreamConfig("rpc2")
	up2.Endpoint = "http://rpc2.localhost"

	ntwCfg := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
		Failsafe: []*common.FailsafeConfig{{
			MatchMethod: "eth_getBlockReceipts",
			Retry:       &common.RetryPolicyConfig{MaxAttempts: 2},
		}},
		// Explicitly disable validation
		DirectiveDefaults: &common.DirectiveDefaultsConfig{
			ValidateLogsBloomEmptiness:      util.BoolPtr(false),
			EnforceLogIndexStrictIncrements: util.BoolPtr(false),
		},
	}
	network, _ := setupIntegrityTestNetwork(t, ctx, []*common.UpstreamConfig{up1, up2}, ntwCfg)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":["0x1"]}`))
	req.SetNetwork(network)

	// rpc1: Would be invalid if validation was enabled (non-zero bloom with no logs)
	gock.New("http://rpc1.localhost").Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockReceipts") }).
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logsBloom":"0x1","logs":[]}]}`))

	// rpc2: Valid data (should NOT be called since validation is disabled)
	gock.New("http://rpc2.localhost").Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockReceipts") }).
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logsBloom":"0x0","logs":[]}]}`))

	resp, err := network.Forward(ctx, req)
	require.NoError(t, err, "Should accept data without validation when disabled")
	require.NotNil(t, resp)
	if resp != nil {
		// Should get the "invalid" data from rpc1 (but accepted because validation is off)
		require.Equal(t, 0, resp.Retries(), "Should have no retries when validation is disabled")
		resp.Release()
	}
}

// =============================================================================
// HEDGE + CONSENSUS + RETRY COMBO TESTS
// These tests mimic production configurations where multiple failsafe policies
// are combined to ensure data integrity across a pool of upstreams.
// =============================================================================

// Test: Hedge spawns multiple requests, validation fails on some, consensus picks valid one
func TestNetworkIntegrity_HedgeConsensus_ValidationFiltersInvalidUpstreams(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	util.SetupMocksForEvmStatePoller()

	// 4 upstreams to simulate hedge + consensus scenario
	up1 := createTestUpstreamConfig("rpc1")
	up1.Endpoint = "http://rpc1.localhost"
	up2 := createTestUpstreamConfig("rpc2")
	up2.Endpoint = "http://rpc2.localhost"
	up3 := createTestUpstreamConfig("rpc3")
	up3.Endpoint = "http://rpc3.localhost"
	up4 := createTestUpstreamConfig("rpc4")
	up4.Endpoint = "http://rpc4.localhost"

	// Production-like config: hedge + consensus + retry
	ntwCfg := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
		Failsafe: []*common.FailsafeConfig{{
			MatchMethod: "eth_getBlockReceipts",
			Hedge: &common.HedgePolicyConfig{
				MaxCount: 4,
				Delay:    common.Duration(10 * time.Millisecond),
			},
			Consensus: &common.ConsensusPolicyConfig{
				MaxParticipants:    4,
				AgreementThreshold: 2,
				PreferNonEmpty:     util.BoolPtr(true),
			},
			Retry: &common.RetryPolicyConfig{
				MaxAttempts: 4,
				Delay:       common.Duration(5 * time.Millisecond),
			},
		}},
		DirectiveDefaults: &common.DirectiveDefaultsConfig{
			ValidateLogsBloomEmptiness: util.BoolPtr(true),
		},
	}
	network, _ := setupIntegrityTestNetwork(t, ctx, []*common.UpstreamConfig{up1, up2, up3, up4}, ntwCfg)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":["0x1"]}`))
	req.SetNetwork(network)

	// rpc1 & rpc2: Invalid - non-zero bloom with zero logs (will fail validation)
	gock.New("http://rpc1.localhost").Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockReceipts") }).
		Persist().
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logsBloom":"0x1","logs":[]}]}`))

	gock.New("http://rpc2.localhost").Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockReceipts") }).
		Persist().
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logsBloom":"0x1","logs":[]}]}`))

	// rpc3 & rpc4: Valid - zero bloom with zero logs (consistent)
	gock.New("http://rpc3.localhost").Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockReceipts") }).
		Persist().
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logsBloom":"0x0","logs":[]}]}`))

	gock.New("http://rpc4.localhost").Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockReceipts") }).
		Persist().
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logsBloom":"0x0","logs":[]}]}`))

	resp, err := network.Forward(ctx, req)
	require.NoError(t, err, "Should succeed - consensus should pick from valid upstreams")
	require.NotNil(t, resp)
	if resp != nil {
		jrr, _ := resp.JsonRpcResponse()
		require.NotNil(t, jrr)
		require.Nil(t, jrr.Error)
		resp.Release()
	}
}

// Test: All hedged requests return invalid data, retry kicks in and eventually finds valid
func TestNetworkIntegrity_HedgeRetry_AllHedgesInvalid_RetryFindsValid(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	util.SetupMocksForEvmStatePoller()

	// 3 upstreams
	up1 := createTestUpstreamConfig("rpc1")
	up1.Endpoint = "http://rpc1.localhost"
	up2 := createTestUpstreamConfig("rpc2")
	up2.Endpoint = "http://rpc2.localhost"
	up3 := createTestUpstreamConfig("rpc3")
	up3.Endpoint = "http://rpc3.localhost"

	// Hedge with retry
	ntwCfg := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
		Failsafe: []*common.FailsafeConfig{{
			MatchMethod: "eth_getBlockReceipts",
			Hedge: &common.HedgePolicyConfig{
				MaxCount: 2,
				Delay:    common.Duration(10 * time.Millisecond),
			},
			Retry: &common.RetryPolicyConfig{
				MaxAttempts: 3,
				Delay:       common.Duration(5 * time.Millisecond),
			},
		}},
		DirectiveDefaults: &common.DirectiveDefaultsConfig{
			ValidateLogsBloomEmptiness: util.BoolPtr(true),
		},
	}
	network, _ := setupIntegrityTestNetwork(t, ctx, []*common.UpstreamConfig{up1, up2, up3}, ntwCfg)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":["0x1"]}`))
	req.SetNetwork(network)

	// rpc1 & rpc2: Invalid (will be tried by hedge)
	gock.New("http://rpc1.localhost").Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockReceipts") }).
		Persist().
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logsBloom":"0x1","logs":[]}]}`))

	gock.New("http://rpc2.localhost").Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockReceipts") }).
		Persist().
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logsBloom":"0x1","logs":[]}]}`))

	// rpc3: Valid (retry should eventually reach this)
	gock.New("http://rpc3.localhost").Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockReceipts") }).
		Persist().
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logsBloom":"0x0","logs":[]}]}`))

	resp, err := network.Forward(ctx, req)
	require.NoError(t, err, "Should succeed after retrying past invalid hedged responses")
	require.NotNil(t, resp)
	if resp != nil {
		resp.Release()
	}
}

// Test: Consensus with different validation errors - should pick the valid one
func TestNetworkIntegrity_Consensus_DifferentValidationErrors_PicksValid(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	util.SetupMocksForEvmStatePoller()

	// 4 upstreams for consensus
	up1 := createTestUpstreamConfig("rpc1")
	up1.Endpoint = "http://rpc1.localhost"
	up2 := createTestUpstreamConfig("rpc2")
	up2.Endpoint = "http://rpc2.localhost"
	up3 := createTestUpstreamConfig("rpc3")
	up3.Endpoint = "http://rpc3.localhost"
	up4 := createTestUpstreamConfig("rpc4")
	up4.Endpoint = "http://rpc4.localhost"

	// Consensus-heavy config
	ntwCfg := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
		Failsafe: []*common.FailsafeConfig{{
			MatchMethod: "eth_getBlockReceipts",
			Consensus: &common.ConsensusPolicyConfig{
				MaxParticipants:    4,
				AgreementThreshold: 2,
				PreferNonEmpty:     util.BoolPtr(true),
			},
			Retry: &common.RetryPolicyConfig{
				MaxAttempts: 4,
			},
		}},
		DirectiveDefaults: &common.DirectiveDefaultsConfig{
			ValidateLogsBloomEmptiness:      util.BoolPtr(true),
			EnforceLogIndexStrictIncrements: util.BoolPtr(true),
		},
	}
	network, _ := setupIntegrityTestNetwork(t, ctx, []*common.UpstreamConfig{up1, up2, up3, up4}, ntwCfg)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":["0x1"]}`))
	req.SetNetwork(network)

	// rpc1: Bloom inconsistency
	gock.New("http://rpc1.localhost").Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockReceipts") }).
		Persist().
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logsBloom":"0x1","logs":[]}]}`))

	// rpc2: LogIndex gap
	gock.New("http://rpc2.localhost").Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockReceipts") }).
		Persist().
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logsBloom":"0x0","logs":[{"logIndex":"0x0"},{"logIndex":"0x5"}]}]}`))

	// rpc3 & rpc4: Valid (same response - will form consensus)
	validResponse := `{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logsBloom":"0x0","logs":[]}]}`
	gock.New("http://rpc3.localhost").Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockReceipts") }).
		Persist().
		Reply(200).JSON([]byte(validResponse))

	gock.New("http://rpc4.localhost").Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockReceipts") }).
		Persist().
		Reply(200).JSON([]byte(validResponse))

	resp, err := network.Forward(ctx, req)
	require.NoError(t, err, "Should succeed - consensus should pick from valid upstreams")
	require.NotNil(t, resp)
	if resp != nil {
		resp.Release()
	}
}

// Test: ReceiptsCountExact with consensus - only valid counts should participate
func TestNetworkIntegrity_Consensus_ReceiptsCountExact_OnlyValidParticipate(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	util.SetupMocksForEvmStatePoller()

	up1 := createTestUpstreamConfig("rpc1")
	up1.Endpoint = "http://rpc1.localhost"
	up2 := createTestUpstreamConfig("rpc2")
	up2.Endpoint = "http://rpc2.localhost"
	up3 := createTestUpstreamConfig("rpc3")
	up3.Endpoint = "http://rpc3.localhost"

	ntwCfg := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
		Failsafe: []*common.FailsafeConfig{{
			MatchMethod: "eth_getBlockReceipts",
			Consensus: &common.ConsensusPolicyConfig{
				MaxParticipants:    3,
				AgreementThreshold: 2,
				PreferNonEmpty:     util.BoolPtr(true),
			},
			Retry: &common.RetryPolicyConfig{
				MaxAttempts: 3,
			},
		}},
	}
	network, _ := setupIntegrityTestNetwork(t, ctx, []*common.UpstreamConfig{up1, up2, up3}, ntwCfg)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":["0x1"]}`))
	req.SetNetwork(network)

	// Set directive to expect exactly 2 receipts
	dirs := newTestDirectives()
	exactCount := int64(2)
	dirs.ReceiptsCountExact = &exactCount
	req.SetDirectives(dirs)

	// rpc1: Returns 1 receipt (wrong count)
	gock.New("http://rpc1.localhost").Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockReceipts") }).
		Persist().
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logs":[]}]}`))

	// rpc2 & rpc3: Return 2 receipts (correct count - will form consensus)
	correctResponse := `{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logs":[]},{"blockHash":"0xabc","logs":[]}]}`
	gock.New("http://rpc2.localhost").Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockReceipts") }).
		Persist().
		Reply(200).JSON([]byte(correctResponse))

	gock.New("http://rpc3.localhost").Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockReceipts") }).
		Persist().
		Reply(200).JSON([]byte(correctResponse))

	resp, err := network.Forward(ctx, req)
	require.NoError(t, err, "Should succeed - consensus should form from upstreams with correct receipt count")
	require.NotNil(t, resp)
	if resp != nil {
		resp.Release()
	}
}

// Test: Production-like config with all policies combined
func TestNetworkIntegrity_ProductionConfig_HedgeConsensusRetry_ValidationIntegrity(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	util.SetupMocksForEvmStatePoller()

	// 6 upstreams like production
	upstreams := make([]*common.UpstreamConfig, 6)
	for i := 0; i < 6; i++ {
		up := createTestUpstreamConfig(fmt.Sprintf("rpc%d", i+1))
		up.Endpoint = fmt.Sprintf("http://rpc%d.localhost", i+1)
		upstreams[i] = up
	}

	// Production-like config from polygon/config.yml
	ntwCfg := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
		Failsafe: []*common.FailsafeConfig{{
			MatchMethod: "eth_getBlockReceipts",
			Hedge: &common.HedgePolicyConfig{
				MaxCount: 4,
				Delay:    common.Duration(10 * time.Millisecond),
			},
			Consensus: &common.ConsensusPolicyConfig{
				MaxParticipants:    8,
				AgreementThreshold: 1,
				PreferNonEmpty:     util.BoolPtr(true),
			},
			Retry: &common.RetryPolicyConfig{
				MaxAttempts: 8,
				Delay:       common.Duration(5 * time.Millisecond),
			},
			Timeout: &common.TimeoutPolicyConfig{
				Duration: common.Duration(10 * time.Second),
			},
		}},
		DirectiveDefaults: &common.DirectiveDefaultsConfig{
			ValidateLogsBloomEmptiness:      util.BoolPtr(true),
			EnforceLogIndexStrictIncrements: util.BoolPtr(true),
		},
	}
	network, _ := setupIntegrityTestNetwork(t, ctx, upstreams, ntwCfg)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":["0x1"]}`))
	req.SetNetwork(network)

	// Mix of valid and invalid responses
	// rpc1, rpc2, rpc3: Invalid (various issues)
	gock.New("http://rpc1.localhost").Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockReceipts") }).
		Persist().
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logsBloom":"0x1","logs":[]}]}`))

	gock.New("http://rpc2.localhost").Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockReceipts") }).
		Persist().
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logsBloom":"0x0","logs":[{"logIndex":"0x0"},{"logIndex":"0x5"}]}]}`))

	gock.New("http://rpc3.localhost").Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockReceipts") }).
		Persist().
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logsBloom":"0x0","logs":[{"logIndex":"0x0","address":"0x1234567890123456789012345678901234567890","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"]}]}]}`))

	// rpc4, rpc5, rpc6: Valid (same response)
	validResponse := `{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logsBloom":"0x0","logs":[]}]}`
	gock.New("http://rpc4.localhost").Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockReceipts") }).
		Persist().
		Reply(200).JSON([]byte(validResponse))

	gock.New("http://rpc5.localhost").Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockReceipts") }).
		Persist().
		Reply(200).JSON([]byte(validResponse))

	gock.New("http://rpc6.localhost").Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockReceipts") }).
		Persist().
		Reply(200).JSON([]byte(validResponse))

	resp, err := network.Forward(ctx, req)
	require.NoError(t, err, "Production-like config should succeed with mix of valid/invalid upstreams")
	require.NotNil(t, resp)
	if resp != nil {
		jrr, _ := resp.JsonRpcResponse()
		require.NotNil(t, jrr)
		require.Nil(t, jrr.Error)
		resp.Release()
	}
}

// =============================================================================
// PREFER LARGER RESPONSES + VALIDATION TESTS
// These tests verify that PreferLargerResponses respects validation -
// invalid responses should be filtered before size comparison.
// =============================================================================

// Test: PreferLargerResponses should NOT pick a larger but invalid response
func TestNetworkIntegrity_Consensus_PreferLargerResponses_ValidationFiltersLargerInvalid(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	util.SetupMocksForEvmStatePoller()

	// 3 upstreams
	up1 := createTestUpstreamConfig("rpc1")
	up1.Endpoint = "http://rpc1.localhost"
	up2 := createTestUpstreamConfig("rpc2")
	up2.Endpoint = "http://rpc2.localhost"
	up3 := createTestUpstreamConfig("rpc3")
	up3.Endpoint = "http://rpc3.localhost"

	// Consensus with PreferLargerResponses enabled
	ntwCfg := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
		Failsafe: []*common.FailsafeConfig{{
			MatchMethod: "eth_getBlockReceipts",
			Consensus: &common.ConsensusPolicyConfig{
				MaxParticipants:         3,
				AgreementThreshold:      1,
				PreferLargerResponses:   util.BoolPtr(true),
				PreferNonEmpty:          util.BoolPtr(true),
				DisputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
			},
			Retry: &common.RetryPolicyConfig{
				MaxAttempts: 3,
			},
		}},
		DirectiveDefaults: &common.DirectiveDefaultsConfig{
			ValidateLogsBloomEmptiness: util.BoolPtr(true),
		},
	}
	network, _ := setupIntegrityTestNetwork(t, ctx, []*common.UpstreamConfig{up1, up2, up3}, ntwCfg)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":["0x1"]}`))
	req.SetNetwork(network)

	// rpc1: LARGER response but INVALID (non-zero bloom with zero logs)
	// This is larger due to more fields/data
	largerInvalidResponse := `{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logsBloom":"0x1","logs":[],"transactionHash":"0x123","transactionIndex":"0x0","blockNumber":"0x1","cumulativeGasUsed":"0x5208","gasUsed":"0x5208","status":"0x1","from":"0x1234567890123456789012345678901234567890","to":"0x1234567890123456789012345678901234567891"}]}`
	gock.New("http://rpc1.localhost").Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockReceipts") }).
		Persist().
		Reply(200).JSON([]byte(largerInvalidResponse))

	// rpc2 & rpc3: SMALLER response but VALID (zero bloom with zero logs)
	smallerValidResponse := `{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logsBloom":"0x0","logs":[]}]}`
	gock.New("http://rpc2.localhost").Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockReceipts") }).
		Persist().
		Reply(200).JSON([]byte(smallerValidResponse))

	gock.New("http://rpc3.localhost").Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockReceipts") }).
		Persist().
		Reply(200).JSON([]byte(smallerValidResponse))

	resp, err := network.Forward(ctx, req)
	require.NoError(t, err, "Should succeed - validation should filter out larger invalid response")
	require.NotNil(t, resp)
	if resp != nil {
		// Should get the smaller but valid response, not the larger invalid one
		jrr, jrrErr := resp.JsonRpcResponse(ctx)
		require.NoError(t, jrrErr)
		require.NotNil(t, jrr)
		require.Nil(t, jrr.Error)
		// Verify we got the valid response (has logsBloom "0x0")
		result, peekErr := jrr.PeekStringByPath(ctx, 0, "logsBloom")
		require.NoError(t, peekErr, "Should be able to peek logsBloom")
		require.Equal(t, "0x0", result, "Should have picked the valid response with zero bloom, not the invalid larger one")
		resp.Release()
	}
}

// Test: PreferLargerResponses picks largest VALID response when multiple valid exist
func TestNetworkIntegrity_Consensus_PreferLargerResponses_PicksLargestValid(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	util.SetupMocksForEvmStatePoller()

	// 3 upstreams
	up1 := createTestUpstreamConfig("rpc1")
	up1.Endpoint = "http://rpc1.localhost"
	up2 := createTestUpstreamConfig("rpc2")
	up2.Endpoint = "http://rpc2.localhost"
	up3 := createTestUpstreamConfig("rpc3")
	up3.Endpoint = "http://rpc3.localhost"

	ntwCfg := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
		Failsafe: []*common.FailsafeConfig{{
			MatchMethod: "eth_getBlockReceipts",
			Consensus: &common.ConsensusPolicyConfig{
				MaxParticipants:         3,
				AgreementThreshold:      1,
				PreferLargerResponses:   util.BoolPtr(true),
				PreferNonEmpty:          util.BoolPtr(true),
				DisputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
			},
			Retry: &common.RetryPolicyConfig{
				MaxAttempts: 3,
			},
		}},
		DirectiveDefaults: &common.DirectiveDefaultsConfig{
			ValidateLogsBloomEmptiness: util.BoolPtr(true),
		},
	}
	network, _ := setupIntegrityTestNetwork(t, ctx, []*common.UpstreamConfig{up1, up2, up3}, ntwCfg)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":["0x1"]}`))
	req.SetNetwork(network)

	// All valid responses but different sizes
	// rpc1: Smallest valid (1 receipt, no logs)
	smallValid := `{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logsBloom":"0x0","logs":[]}]}`
	gock.New("http://rpc1.localhost").Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockReceipts") }).
		Persist().
		Reply(200).JSON([]byte(smallValid))

	// rpc2: Medium valid (1 receipt with extra fields)
	mediumValid := `{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logsBloom":"0x0","logs":[],"transactionHash":"0x123","status":"0x1"}]}`
	gock.New("http://rpc2.localhost").Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockReceipts") }).
		Persist().
		Reply(200).JSON([]byte(mediumValid))

	// rpc3: Largest valid (2 receipts)
	largestValid := `{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logsBloom":"0x0","logs":[]},{"blockHash":"0xabc","logsBloom":"0x0","logs":[]}]}`
	gock.New("http://rpc3.localhost").Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockReceipts") }).
		Persist().
		Reply(200).JSON([]byte(largestValid))

	resp, err := network.Forward(ctx, req)
	require.NoError(t, err, "Should succeed")
	require.NotNil(t, resp)
	if resp != nil {
		jrr, jrrErr := resp.JsonRpcResponse(ctx)
		require.NoError(t, jrrErr)
		require.NotNil(t, jrr)
		require.Nil(t, jrr.Error)
		// Should have picked the largest valid response (2 receipts)
		// Check that the second receipt exists (index 1)
		_, peekErr := jrr.PeekStringByPath(ctx, 1, "blockHash")
		require.NoError(t, peekErr, "Should have picked the largest valid response with 2 receipts")
		resp.Release()
	}
}

// Test: PreferLargerResponses with validation - all larger responses invalid, falls back to smaller valid
func TestNetworkIntegrity_Consensus_PreferLargerResponses_AllLargerInvalid_FallsBackToSmaller(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	util.SetupMocksForEvmStatePoller()

	// 4 upstreams
	up1 := createTestUpstreamConfig("rpc1")
	up1.Endpoint = "http://rpc1.localhost"
	up2 := createTestUpstreamConfig("rpc2")
	up2.Endpoint = "http://rpc2.localhost"
	up3 := createTestUpstreamConfig("rpc3")
	up3.Endpoint = "http://rpc3.localhost"
	up4 := createTestUpstreamConfig("rpc4")
	up4.Endpoint = "http://rpc4.localhost"

	ntwCfg := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
		Failsafe: []*common.FailsafeConfig{{
			MatchMethod: "eth_getBlockReceipts",
			Consensus: &common.ConsensusPolicyConfig{
				MaxParticipants:         4,
				AgreementThreshold:      1,
				PreferLargerResponses:   util.BoolPtr(true),
				PreferNonEmpty:          util.BoolPtr(true),
				DisputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
			},
			Retry: &common.RetryPolicyConfig{
				MaxAttempts: 4,
			},
		}},
		DirectiveDefaults: &common.DirectiveDefaultsConfig{
			ValidateLogsBloomEmptiness:      util.BoolPtr(true),
			EnforceLogIndexStrictIncrements: util.BoolPtr(true),
		},
	}
	network, _ := setupIntegrityTestNetwork(t, ctx, []*common.UpstreamConfig{up1, up2, up3, up4}, ntwCfg)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":["0x1"]}`))
	req.SetNetwork(network)

	// rpc1 & rpc2: Large but INVALID (bloom inconsistency)
	largeInvalid := `{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logsBloom":"0x1","logs":[],"transactionHash":"0x123","status":"0x1"},{"blockHash":"0xabc","logsBloom":"0x1","logs":[],"transactionHash":"0x456","status":"0x1"}]}`
	gock.New("http://rpc1.localhost").Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockReceipts") }).
		Persist().
		Reply(200).JSON([]byte(largeInvalid))

	gock.New("http://rpc2.localhost").Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockReceipts") }).
		Persist().
		Reply(200).JSON([]byte(largeInvalid))

	// rpc3: Medium but INVALID (log index gap)
	mediumInvalid := `{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logsBloom":"0x0","logs":[{"logIndex":"0x0"},{"logIndex":"0x5"}]}]}`
	gock.New("http://rpc3.localhost").Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockReceipts") }).
		Persist().
		Reply(200).JSON([]byte(mediumInvalid))

	// rpc4: Smallest but VALID
	smallValid := `{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logsBloom":"0x0","logs":[]}]}`
	gock.New("http://rpc4.localhost").Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockReceipts") }).
		Persist().
		Reply(200).JSON([]byte(smallValid))

	resp, err := network.Forward(ctx, req)
	require.NoError(t, err, "Should succeed - should fall back to smallest valid response")
	require.NotNil(t, resp)
	if resp != nil {
		jrr, jrrErr := resp.JsonRpcResponse(ctx)
		require.NoError(t, jrrErr)
		require.NotNil(t, jrr)
		require.Nil(t, jrr.Error)
		// Should have the smallest valid response
		result, peekErr := jrr.PeekStringByPath(ctx, 0, "logsBloom")
		require.NoError(t, peekErr, "Should be able to peek logsBloom")
		require.Equal(t, "0x0", result, "Should have picked the valid response")
		resp.Release()
	}
}

// Test: Hedge + PreferLargerResponses + Validation combo
func TestNetworkIntegrity_HedgeConsensus_PreferLargerResponses_ValidationIntegrity(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	util.SetupMocksForEvmStatePoller()

	// 4 upstreams
	up1 := createTestUpstreamConfig("rpc1")
	up1.Endpoint = "http://rpc1.localhost"
	up2 := createTestUpstreamConfig("rpc2")
	up2.Endpoint = "http://rpc2.localhost"
	up3 := createTestUpstreamConfig("rpc3")
	up3.Endpoint = "http://rpc3.localhost"
	up4 := createTestUpstreamConfig("rpc4")
	up4.Endpoint = "http://rpc4.localhost"

	// Production-like config with hedge + consensus + preferLargerResponses
	ntwCfg := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
		Failsafe: []*common.FailsafeConfig{{
			MatchMethod: "eth_getBlockReceipts",
			Hedge: &common.HedgePolicyConfig{
				MaxCount: 4,
				Delay:    common.Duration(10 * time.Millisecond),
			},
			Consensus: &common.ConsensusPolicyConfig{
				MaxParticipants:         4,
				AgreementThreshold:      2,
				PreferLargerResponses:   util.BoolPtr(true),
				PreferNonEmpty:          util.BoolPtr(true),
				DisputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
			},
			Retry: &common.RetryPolicyConfig{
				MaxAttempts: 4,
				Delay:       common.Duration(5 * time.Millisecond),
			},
		}},
		DirectiveDefaults: &common.DirectiveDefaultsConfig{
			ValidateLogsBloomEmptiness: util.BoolPtr(true),
		},
	}
	network, _ := setupIntegrityTestNetwork(t, ctx, []*common.UpstreamConfig{up1, up2, up3, up4}, ntwCfg)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":["0x1"]}`))
	req.SetNetwork(network)

	// rpc1: Largest but INVALID
	largestInvalid := `{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logsBloom":"0x1","logs":[]},{"blockHash":"0xabc","logsBloom":"0x1","logs":[]},{"blockHash":"0xabc","logsBloom":"0x1","logs":[]}]}`
	gock.New("http://rpc1.localhost").Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockReceipts") }).
		Persist().
		Reply(200).JSON([]byte(largestInvalid))

	// rpc2: Medium INVALID
	mediumInvalid := `{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logsBloom":"0x1","logs":[]},{"blockHash":"0xabc","logsBloom":"0x1","logs":[]}]}`
	gock.New("http://rpc2.localhost").Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockReceipts") }).
		Persist().
		Reply(200).JSON([]byte(mediumInvalid))

	// rpc3 & rpc4: Valid (same response - will form consensus)
	validResponse := `{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0xabc","logsBloom":"0x0","logs":[]}]}`
	gock.New("http://rpc3.localhost").Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockReceipts") }).
		Persist().
		Reply(200).JSON([]byte(validResponse))

	gock.New("http://rpc4.localhost").Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockReceipts") }).
		Persist().
		Reply(200).JSON([]byte(validResponse))

	resp, err := network.Forward(ctx, req)
	require.NoError(t, err, "Should succeed - consensus should form from valid upstreams despite larger invalid ones")
	require.NotNil(t, resp)
	if resp != nil {
		jrr, jrrErr := resp.JsonRpcResponse(ctx)
		require.NoError(t, jrrErr)
		require.NotNil(t, jrr)
		require.Nil(t, jrr.Error)
		// Should have the valid response
		result, peekErr := jrr.PeekStringByPath(ctx, 0, "logsBloom")
		require.NoError(t, peekErr, "Should be able to peek logsBloom")
		require.Equal(t, "0x0", result, "Should have picked valid response, not larger invalid ones")
		resp.Release()
	}
}
