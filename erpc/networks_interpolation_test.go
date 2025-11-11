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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

// Helper to create a test network with mocked state poller
func setupTestNetworkForInterpolation(t *testing.T, ctx context.Context, networkConfig *common.NetworkConfig) (*Network, *upstream.UpstreamsRegistry) {
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

	rlr, _ := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{}, &log.Logger)
	mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
	vr := thirdparty.NewVendorsRegistry()
	pr, _ := thirdparty.NewProvidersRegistry(&log.Logger, vr, nil, nil)

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

	if networkConfig == nil {
		networkConfig = &common.NetworkConfig{
			Architecture: common.ArchitectureEvm,
			Evm: &common.EvmNetworkConfig{
				ChainId: 123,
			},
		}
	}

	network, _ := NewNetwork(ctx, &log.Logger, "prjA", networkConfig, rlr, upr, mt)
	require.NoError(t, upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123)))
	require.NoError(t, network.Bootstrap(ctx))

	// Wait for state poller to initialize
	time.Sleep(500 * time.Millisecond)

	return network, upr
}

// Test "latest" tag translation to hex number
func TestInterpolation_LatestTag_ToHex(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupTestNetworkForInterpolation(t, ctx, nil)

	// Mock the forwarded request to check the translated value
	gock.New("http://rpc1.localhost").
		Post("").
		Times(1).
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			// Should receive hex block number instead of "latest"
			return strings.Contains(body, "eth_getBalance") &&
				strings.Contains(body, "\"0x") &&
				!strings.Contains(body, "\"latest\"")
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  "0x1234",
		})

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xabc","latest"]}`))
	req.SetNetwork(network)

	// Normalize the request
	jrq, err := req.JsonRpcRequest()
	require.NoError(t, err)
	evm.NormalizeHttpJsonRpc(ctx, req, jrq)

	// Verify "latest" was translated to hex
	params := jrq.Params
	require.Len(t, params, 2)
	blockParam := params[1].(string)
	assert.True(t, strings.HasPrefix(blockParam, "0x"), "Block param should be hex")
	assert.NotEqual(t, "latest", blockParam, "Should not be 'latest' anymore")

	// Forward to verify mock is hit
	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	if resp != nil {
		resp.Release()
	}
}

// Test "finalized" tag translation to hex number
func TestInterpolation_FinalizedTag_ToHex(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupTestNetworkForInterpolation(t, ctx, nil)

	// Mock the forwarded request
	gock.New("http://rpc1.localhost").
		Post("").
		Times(1).
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			return strings.Contains(body, "eth_call") &&
				strings.Contains(body, "\"0x") &&
				!strings.Contains(body, "\"finalized\"")
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  "0x",
		})

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0xabc"},"finalized"]}`))
	req.SetNetwork(network)

	jrq, err := req.JsonRpcRequest()
	require.NoError(t, err)
	evm.NormalizeHttpJsonRpc(ctx, req, jrq)

	params := jrq.Params
	require.Len(t, params, 2)
	blockParam := params[1].(string)
	assert.True(t, strings.HasPrefix(blockParam, "0x"), "Block param should be hex")
	assert.NotEqual(t, "finalized", blockParam, "Should not be 'finalized' anymore")

	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	if resp != nil {
		resp.Release()
	}
}

// Test "safe" tag translation (should use finalized)
func TestInterpolation_SafeTag_ToHex(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupTestNetworkForInterpolation(t, ctx, nil)

	gock.New("http://rpc1.localhost").
		Post("").
		Times(1).
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			return strings.Contains(body, "eth_getCode") &&
				strings.Contains(body, "\"0x") &&
				!strings.Contains(body, "\"safe\"")
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  "0x",
		})

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getCode","params":["0xabc","safe"]}`))
	req.SetNetwork(network)

	jrq, err := req.JsonRpcRequest()
	require.NoError(t, err)
	evm.NormalizeHttpJsonRpc(ctx, req, jrq)

	params := jrq.Params
	require.Len(t, params, 2)
	blockParam := params[1].(string)
	assert.True(t, strings.HasPrefix(blockParam, "0x"), "Block param should be hex")
	assert.NotEqual(t, "safe", blockParam, "Should not be 'safe' anymore")

	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	if resp != nil {
		resp.Release()
	}
}

// Test "pending" tag translation (should use latest)
func TestInterpolation_PendingTag_ToHex(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupTestNetworkForInterpolation(t, ctx, nil)

	gock.New("http://rpc1.localhost").
		Post("").
		Times(1).
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			return strings.Contains(body, "eth_getTransactionCount") &&
				strings.Contains(body, "\"0x") &&
				!strings.Contains(body, "\"pending\"")
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  "0x5",
		})

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getTransactionCount","params":["0xabc","pending"]}`))
	req.SetNetwork(network)

	jrq, err := req.JsonRpcRequest()
	require.NoError(t, err)
	evm.NormalizeHttpJsonRpc(ctx, req, jrq)

	params := jrq.Params
	require.Len(t, params, 2)
	blockParam := params[1].(string)
	assert.True(t, strings.HasPrefix(blockParam, "0x"), "Block param should be hex")
	assert.NotEqual(t, "pending", blockParam, "Should not be 'pending' anymore")

	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	if resp != nil {
		resp.Release()
	}
}

// Test numeric block parameter normalization (float64 from JSON)
func TestInterpolation_NumericBlock_ToHex(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupTestNetworkForInterpolation(t, ctx, nil)

	gock.New("http://rpc1.localhost").
		Post("").
		Times(1).
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			// Should receive "0x7b" (123 in hex) instead of 123
			return strings.Contains(body, "eth_getBalance") &&
				strings.Contains(body, "\"0x7b\"")
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  "0x1234",
		})

	// Numeric block parameter (123)
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xabc",123]}`))
	req.SetNetwork(network)

	jrq, err := req.JsonRpcRequest()
	require.NoError(t, err)
	evm.NormalizeHttpJsonRpc(ctx, req, jrq)

	params := jrq.Params
	require.Len(t, params, 2)
	blockParam := params[1].(string)
	assert.Equal(t, "0x7b", blockParam, "Numeric 123 should be converted to 0x7b")

	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	if resp != nil {
		resp.Release()
	}
}

// Test eth_getLogs with fromBlock and toBlock
func TestInterpolation_EthGetLogs_MultipleParams(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupTestNetworkForInterpolation(t, ctx, nil)

	gock.New("http://rpc1.localhost").
		Post("").
		Times(1).
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			// Both fromBlock and toBlock should be translated
			return strings.Contains(body, "eth_getLogs") &&
				!strings.Contains(body, "\"latest\"") &&
				!strings.Contains(body, "\"finalized\"") &&
				strings.Contains(body, "\"fromBlock\":\"0x") &&
				strings.Contains(body, "\"toBlock\":\"0x")
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  []interface{}{},
		})

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{"fromBlock":"finalized","toBlock":"latest","address":"0xabc"}]}`))
	req.SetNetwork(network)

	jrq, err := req.JsonRpcRequest()
	require.NoError(t, err)
	evm.NormalizeHttpJsonRpc(ctx, req, jrq)

	params := jrq.Params
	require.Len(t, params, 1)
	filterParam := params[0].(map[string]interface{})

	fromBlock := filterParam["fromBlock"].(string)
	toBlock := filterParam["toBlock"].(string)

	assert.True(t, strings.HasPrefix(fromBlock, "0x"), "fromBlock should be hex")
	assert.True(t, strings.HasPrefix(toBlock, "0x"), "toBlock should be hex")
	assert.NotEqual(t, "finalized", fromBlock)
	assert.NotEqual(t, "latest", toBlock)

	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	if resp != nil {
		resp.Release()
	}
}

// Test eth_getLogs with numeric fromBlock/toBlock
func TestInterpolation_EthGetLogs_NumericBlocks(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupTestNetworkForInterpolation(t, ctx, nil)

	gock.New("http://rpc1.localhost").
		Post("").
		Times(1).
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			// Should have hex values for both blocks
			return strings.Contains(body, "eth_getLogs") &&
				strings.Contains(body, "\"fromBlock\":\"0x64\"") && // 100 in hex
				strings.Contains(body, "\"toBlock\":\"0xc8\"") // 200 in hex
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  []interface{}{},
		})

	// Numeric blocks in filter
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{"fromBlock":100,"toBlock":200,"address":"0xabc"}]}`))
	req.SetNetwork(network)

	jrq, err := req.JsonRpcRequest()
	require.NoError(t, err)
	evm.NormalizeHttpJsonRpc(ctx, req, jrq)

	params := jrq.Params
	require.Len(t, params, 1)
	filterParam := params[0].(map[string]interface{})

	assert.Equal(t, "0x64", filterParam["fromBlock"])
	assert.Equal(t, "0xc8", filterParam["toBlock"])

	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	if resp != nil {
		resp.Release()
	}
}

// Test hex normalization (e.g., "0x0a" -> "0xa")
func TestInterpolation_HexNormalization(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupTestNetworkForInterpolation(t, ctx, nil)

	gock.New("http://rpc1.localhost").
		Post("").
		Times(1).
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			// Should have normalized hex (no leading zeros)
			return strings.Contains(body, "eth_getBalance") &&
				strings.Contains(body, "\"0xa\"") && // normalized from "0x0a"
				!strings.Contains(body, "\"0x0a\"")
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  "0x1234",
		})

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xabc","0x0a"]}`))
	req.SetNetwork(network)

	jrq, err := req.JsonRpcRequest()
	require.NoError(t, err)
	evm.NormalizeHttpJsonRpc(ctx, req, jrq)

	params := jrq.Params
	require.Len(t, params, 2)
	blockParam := params[1].(string)
	assert.Equal(t, "0xa", blockParam, "Hex should be normalized")

	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	if resp != nil {
		resp.Release()
	}
}

// Test with translation disabled for latest tag
func TestInterpolation_DisabledLatestTranslation(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Create network config with translation disabled for latest
	falseVal := false
	networkConfig := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm: &common.EvmNetworkConfig{
			ChainId: 123,
		},
		Methods: &common.MethodsConfig{
			Definitions: map[string]*common.CacheMethodConfig{
				"eth_getBalance": {
					ReqRefs:            [][]interface{}{{1}},
					TranslateLatestTag: &falseVal,
				},
			},
		},
	}

	network, _ := setupTestNetworkForInterpolation(t, ctx, networkConfig)

	gock.New("http://rpc1.localhost").
		Post("").
		Times(1).
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			// Should still have "latest" since translation is disabled
			return strings.Contains(body, "eth_getBalance") &&
				strings.Contains(body, "\"latest\"")
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  "0x1234",
		})

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xabc","latest"]}`))
	req.SetNetwork(network)

	jrq, err := req.JsonRpcRequest()
	require.NoError(t, err)
	evm.NormalizeHttpJsonRpc(ctx, req, jrq)

	params := jrq.Params
	require.Len(t, params, 2)
	blockParam := params[1].(string)
	assert.Equal(t, "latest", blockParam, "Should still be 'latest' when translation is disabled")

	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	if resp != nil {
		resp.Release()
	}
}

// Test with translation disabled for finalized tag
func TestInterpolation_DisabledFinalizedTranslation(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Create network config with translation disabled for finalized
	falseVal := false
	networkConfig := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm: &common.EvmNetworkConfig{
			ChainId: 123,
		},
		Methods: &common.MethodsConfig{
			Definitions: map[string]*common.CacheMethodConfig{
				"eth_call": {
					ReqRefs:               [][]interface{}{{1}},
					TranslateFinalizedTag: &falseVal,
				},
			},
		},
	}

	network, _ := setupTestNetworkForInterpolation(t, ctx, networkConfig)

	gock.New("http://rpc1.localhost").
		Post("").
		Times(1).
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			// Should still have "finalized" since translation is disabled
			return strings.Contains(body, "eth_call") &&
				strings.Contains(body, "\"finalized\"")
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  "0x",
		})

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0xabc"},"finalized"]}`))
	req.SetNetwork(network)

	jrq, err := req.JsonRpcRequest()
	require.NoError(t, err)
	evm.NormalizeHttpJsonRpc(ctx, req, jrq)

	params := jrq.Params
	require.Len(t, params, 2)
	blockParam := params[1].(string)
	assert.Equal(t, "finalized", blockParam, "Should still be 'finalized' when translation is disabled")

	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	if resp != nil {
		resp.Release()
	}
}

// Test methods without block parameters (should not be affected)
func TestInterpolation_MethodsWithoutBlockParams(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupTestNetworkForInterpolation(t, ctx, nil)

	// Test eth_blockNumber (no params)
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`))
	req.SetNetwork(network)

	jrq, err := req.JsonRpcRequest()
	require.NoError(t, err)
	originalParams := make([]interface{}, len(jrq.Params))
	copy(originalParams, jrq.Params)

	evm.NormalizeHttpJsonRpc(ctx, req, jrq)

	// Params should remain unchanged
	assert.Equal(t, originalParams, jrq.Params)

	// Test eth_accounts (no params)
	req2 := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_accounts","params":[]}`))
	req2.SetNetwork(network)

	jrq2, err := req2.JsonRpcRequest()
	require.NoError(t, err)
	originalParams2 := make([]interface{}, len(jrq2.Params))
	copy(originalParams2, jrq2.Params)

	evm.NormalizeHttpJsonRpc(ctx, req2, jrq2)

	// Params should remain unchanged
	assert.Equal(t, originalParams2, jrq2.Params)
}

// Test eth_getStorageAt with third parameter
func TestInterpolation_EthGetStorageAt_ThirdParam(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupTestNetworkForInterpolation(t, ctx, nil)

	gock.New("http://rpc1.localhost").
		Post("").
		Times(1).
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			// Third param should be translated
			return strings.Contains(body, "eth_getStorageAt") &&
				!strings.Contains(body, "\"latest\"") &&
				strings.Contains(body, "\"0x")
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  "0x0",
		})

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getStorageAt","params":["0xabc","0x0","latest"]}`))
	req.SetNetwork(network)

	jrq, err := req.JsonRpcRequest()
	require.NoError(t, err)
	evm.NormalizeHttpJsonRpc(ctx, req, jrq)

	params := jrq.Params
	require.Len(t, params, 3)
	blockParam := params[2].(string)
	assert.True(t, strings.HasPrefix(blockParam, "0x"), "Third param should be hex")
	assert.NotEqual(t, "latest", blockParam)

	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	if resp != nil {
		resp.Release()
	}
}

// Test trace_call with second parameter
func TestInterpolation_TraceCall_SecondParam(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupTestNetworkForInterpolation(t, ctx, nil)

	gock.New("http://rpc1.localhost").
		Post("").
		Times(1).
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			return strings.Contains(body, "trace_call") &&
				!strings.Contains(body, "\"latest\"") &&
				strings.Contains(body, "\"0x")
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  map[string]interface{}{},
		})

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"trace_call","params":[{"to":"0xabc"},"latest",["trace"]]}`))
	req.SetNetwork(network)

	jrq, err := req.JsonRpcRequest()
	require.NoError(t, err)
	evm.NormalizeHttpJsonRpc(ctx, req, jrq)

	params := jrq.Params
	require.Len(t, params, 3)
	blockParam := params[1].(string)
	assert.True(t, strings.HasPrefix(blockParam, "0x"), "Second param should be hex")
	assert.NotEqual(t, "latest", blockParam)

	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	if resp != nil {
		resp.Release()
	}
}

// Test debug_traceCall with second parameter
func TestInterpolation_DebugTraceCall_SecondParam(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupTestNetworkForInterpolation(t, ctx, nil)

	gock.New("http://rpc1.localhost").
		Post("").
		Times(1).
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			return strings.Contains(body, "debug_traceCall") &&
				!strings.Contains(body, "\"finalized\"") &&
				strings.Contains(body, "\"0x")
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  map[string]interface{}{},
		})

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"debug_traceCall","params":[{"to":"0xabc"},"finalized"]}`))
	req.SetNetwork(network)

	jrq, err := req.JsonRpcRequest()
	require.NoError(t, err)
	evm.NormalizeHttpJsonRpc(ctx, req, jrq)

	params := jrq.Params
	require.Len(t, params, 2)
	blockParam := params[1].(string)
	assert.True(t, strings.HasPrefix(blockParam, "0x"), "Second param should be hex")
	assert.NotEqual(t, "finalized", blockParam)

	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	if resp != nil {
		resp.Release()
	}
}

// Test eth_estimateGas with second parameter
func TestInterpolation_EthEstimateGas_SecondParam(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupTestNetworkForInterpolation(t, ctx, nil)

	gock.New("http://rpc1.localhost").
		Post("").
		Times(1).
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			return strings.Contains(body, "eth_estimateGas") &&
				!strings.Contains(body, "\"latest\"") &&
				strings.Contains(body, "\"0x")
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  "0x5208",
		})

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_estimateGas","params":[{"to":"0xabc","value":"0x1"},"latest"]}`))
	req.SetNetwork(network)

	jrq, err := req.JsonRpcRequest()
	require.NoError(t, err)
	evm.NormalizeHttpJsonRpc(ctx, req, jrq)

	params := jrq.Params
	require.Len(t, params, 2)
	blockParam := params[1].(string)
	assert.True(t, strings.HasPrefix(blockParam, "0x"), "Second param should be hex")
	assert.NotEqual(t, "latest", blockParam)

	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	if resp != nil {
		resp.Release()
	}
}

// Test edge case: nil network
func TestInterpolation_NilNetwork(t *testing.T) {
	ctx := context.Background()

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xabc","latest"]}`))
	// Don't set network (nil)

	jrq, err := req.JsonRpcRequest()
	require.NoError(t, err)

	originalParams := make([]interface{}, len(jrq.Params))
	copy(originalParams, jrq.Params)

	// Should not panic and params should remain unchanged
	evm.NormalizeHttpJsonRpc(ctx, req, jrq)

	assert.Equal(t, originalParams, jrq.Params, "Params should remain unchanged with nil network")
}

// Test edge case: network without state poller data (no latest/finalized blocks)
func TestInterpolation_NoStatePollerData(t *testing.T) {
	ctx := context.Background()

	// Create a request without setting a network (simulates no state poller data)
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xabc","latest"]}`))
	// Don't set network - this simulates no state poller data available

	jrq, err := req.JsonRpcRequest()
	require.NoError(t, err)

	// Store original params
	originalParams := make([]interface{}, len(jrq.Params))
	copy(originalParams, jrq.Params)

	// Normalize - should not change since no network/state poller data
	evm.NormalizeHttpJsonRpc(ctx, req, jrq)

	params := jrq.Params
	require.Len(t, params, 2)
	blockParam := params[1].(string)
	assert.Equal(t, "latest", blockParam, "Should remain 'latest' when no state poller data")
}

// Test mixed parameters in eth_getLogs
func TestInterpolation_EthGetLogs_MixedParams(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Create a mock network with state poller data
	network, _ := setupTestNetworkForInterpolation(t, ctx, nil)

	// Mixed: numeric fromBlock, tag toBlock
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{"fromBlock":100,"toBlock":"latest","address":"0xabc"}]}`))
	req.SetNetwork(network)

	jrq, err := req.JsonRpcRequest()
	require.NoError(t, err)

	// Capture original values
	originalParams := jrq.Params[0].(map[string]interface{})
	originalFromBlock := originalParams["fromBlock"]
	originalToBlock := originalParams["toBlock"]

	// Apply normalization
	evm.NormalizeHttpJsonRpc(ctx, req, jrq)

	params := jrq.Params
	require.Len(t, params, 1)
	filterParam := params[0].(map[string]interface{})

	fromBlock := filterParam["fromBlock"].(string)
	toBlock := filterParam["toBlock"].(string)

	// Verify transformations
	assert.Equal(t, "0x64", fromBlock, "Numeric 100 should be 0x64")
	assert.True(t, strings.HasPrefix(toBlock, "0x"), "toBlock should be hex")
	assert.NotEqual(t, "latest", toBlock, "Should not be 'latest' anymore")
	assert.NotEqual(t, originalFromBlock, fromBlock, "fromBlock should have changed")
	assert.NotEqual(t, originalToBlock, toBlock, "toBlock should have changed")
}

// Test large numeric values
func TestInterpolation_LargeNumericValues(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupTestNetworkForInterpolation(t, ctx, nil)

	gock.New("http://rpc1.localhost").
		Post("").
		Times(1).
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			// Should handle large numbers correctly
			return strings.Contains(body, "eth_getBalance") &&
				strings.Contains(body, "\"0xf4240\"") // 1000000 in hex
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  "0x0",
		})

	// Large numeric block parameter
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xabc",1000000]}`))
	req.SetNetwork(network)

	jrq, err := req.JsonRpcRequest()
	require.NoError(t, err)
	evm.NormalizeHttpJsonRpc(ctx, req, jrq)

	params := jrq.Params
	require.Len(t, params, 2)
	blockParam := params[1].(string)
	assert.Equal(t, "0xf4240", blockParam, "Large number should be converted correctly")

	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	if resp != nil {
		resp.Release()
	}
}

// Test eth_getBlockReceipts with object parameter
func TestInterpolation_EthGetBlockReceipts_ObjectParam(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupTestNetworkForInterpolation(t, ctx, nil)

	gock.New("http://rpc1.localhost").
		Post("").
		Times(1).
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			// Object parameter with blockNumber should be translated
			return strings.Contains(body, "eth_getBlockReceipts") &&
				strings.Contains(body, "\"blockNumber\":\"0x") &&
				!strings.Contains(body, "\"latest\"")
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  []interface{}{},
		})

	// Object parameter with blockNumber
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":[{"blockNumber":"latest"}]}`))
	req.SetNetwork(network)

	jrq, err := req.JsonRpcRequest()
	require.NoError(t, err)
	evm.NormalizeHttpJsonRpc(ctx, req, jrq)

	params := jrq.Params
	require.Len(t, params, 1)
	objParam := params[0].(map[string]interface{})
	blockNumber := objParam["blockNumber"].(string)

	assert.True(t, strings.HasPrefix(blockNumber, "0x"), "blockNumber in object should be hex")
	assert.NotEqual(t, "latest", blockNumber)

	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	if resp != nil {
		resp.Release()
	}
}

// Benchmark tag translation performance
func BenchmarkInterpolation_TagTranslation(b *testing.B) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx := context.Background()
	network, _ := setupTestNetworkForInterpolation(nil, ctx, nil)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{"fromBlock":"finalized","toBlock":"latest","address":"0xabc"}]}`))
	req.SetNetwork(network)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		jrq, _ := req.JsonRpcRequest()
		evm.NormalizeHttpJsonRpc(ctx, req, jrq)
	}
}

// Benchmark numeric normalization performance
func BenchmarkInterpolation_NumericNormalization(b *testing.B) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx := context.Background()
	network, _ := setupTestNetworkForInterpolation(nil, ctx, nil)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{"fromBlock":100,"toBlock":200,"address":"0xabc"}]}`))
	req.SetNetwork(network)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		jrq, _ := req.JsonRpcRequest()
		evm.NormalizeHttpJsonRpc(ctx, req, jrq)
	}
}

// Test all methods with various parameters
func TestInterpolation_AllMethodsCoverage(t *testing.T) {
	testCases := []struct {
		name           string
		method         string
		params         string
		expectedFilter func(body string) bool
	}{
		{
			name:   "eth_getBalance",
			method: "eth_getBalance",
			params: `["0xabc","latest"]`,
			expectedFilter: func(body string) bool {
				return strings.Contains(body, "eth_getBalance") && !strings.Contains(body, "\"latest\"")
			},
		},
		{
			name:   "eth_getStorageAt",
			method: "eth_getStorageAt",
			params: `["0xabc","0x0","finalized"]`,
			expectedFilter: func(body string) bool {
				return strings.Contains(body, "eth_getStorageAt") && !strings.Contains(body, "\"finalized\"")
			},
		},
		{
			name:   "eth_getTransactionCount",
			method: "eth_getTransactionCount",
			params: `["0xabc","safe"]`,
			expectedFilter: func(body string) bool {
				return strings.Contains(body, "eth_getTransactionCount") && !strings.Contains(body, "\"safe\"")
			},
		},
		{
			name:   "eth_getCode",
			method: "eth_getCode",
			params: `["0xabc","pending"]`,
			expectedFilter: func(body string) bool {
				return strings.Contains(body, "eth_getCode") && !strings.Contains(body, "\"pending\"")
			},
		},
		{
			name:   "eth_call",
			method: "eth_call",
			params: `[{"to":"0xabc"},"latest"]`,
			expectedFilter: func(body string) bool {
				return strings.Contains(body, "eth_call") && !strings.Contains(body, "\"latest\"")
			},
		},
		{
			name:   "eth_estimateGas",
			method: "eth_estimateGas",
			params: `[{"to":"0xabc"},"latest"]`,
			expectedFilter: func(body string) bool {
				return strings.Contains(body, "eth_estimateGas") && !strings.Contains(body, "\"latest\"")
			},
		},
		{
			name:   "trace_call",
			method: "trace_call",
			params: `[{"to":"0xabc"},"latest",["trace"]]`,
			expectedFilter: func(body string) bool {
				return strings.Contains(body, "trace_call") && !strings.Contains(body, "\"latest\"")
			},
		},
		{
			name:   "debug_traceCall",
			method: "debug_traceCall",
			params: `[{"to":"0xabc"},"latest"]`,
			expectedFilter: func(body string) bool {
				return strings.Contains(body, "debug_traceCall") && !strings.Contains(body, "\"latest\"")
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			util.ResetGock()
			defer util.ResetGock()
			util.SetupMocksForEvmStatePoller()
			defer util.AssertNoPendingMocks(t, 0)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			network, _ := setupTestNetworkForInterpolation(t, ctx, nil)

			// Mock for the specific method
			gock.New("http://rpc1.localhost").
				Post("").
				Times(1).
				Filter(func(r *http.Request) bool {
					body := util.SafeReadBody(r)
					return tc.expectedFilter(body)
				}).
				Reply(200).
				JSON(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x0",
				})

			reqBody := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"%s","params":%s}`, tc.method, tc.params)
			req := common.NewNormalizedRequest([]byte(reqBody))
			req.SetNetwork(network)

			jrq, err := req.JsonRpcRequest()
			require.NoError(t, err)
			evm.NormalizeHttpJsonRpc(ctx, req, jrq)

			// Verify tag was translated
			resp, err := network.Forward(ctx, req)
			require.NoError(t, err, "Method %s should succeed", tc.method)
			if resp != nil {
				resp.Release()
			}
		})
	}
}
