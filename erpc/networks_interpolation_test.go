package erpc

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
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

	rlr, _ := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{}, &log.Logger)
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupTestNetworkForInterpolation(t, ctx, nil)

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

// After tag interpolation to hex, enrichStatePoller must still enrich using the original tag intent.
// This test forces a forwarded eth_getBlockByNumber (latest) call to return a higher block number than the poller's background value,
// and expects the poller's LatestBlock to be updated to that higher number via enrichment.
// With the current bug (checking mutated params instead of original tag), enrichment won't run and the value won't update.
func TestEnrichment_AfterInterpolation_UsesOriginalLatestTagIntent(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupTestNetworkForInterpolation(t, ctx, nil)

	// Build request with tag "latest" which will be interpolated to "0x11118888"
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["latest",false]}`))
	req.SetNetwork(network)

	// Normalize and forward
	jrq, err := req.JsonRpcRequest()
	require.NoError(t, err)
	evm.NormalizeHttpJsonRpc(ctx, req, jrq)

	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	defer resp.Release()

	// Ensure original tag intent was recorded on the request
	ref := req.EvmBlockRef()
	rs, ok := ref.(string)
	require.True(t, ok, "EvmBlockRef should be a string")
	require.Equal(t, "latest", rs, "EvmBlockRef should preserve 'latest'")

	// Get the poller that served this response
	ups := resp.Upstream()
	require.NotNil(t, ups)
	eups, ok := ups.(common.EvmUpstream)
	require.True(t, ok)
	poller := eups.EvmStatePoller()
	require.NotNil(t, poller)

	// Now directly invoke enrichStatePoller with a synthetic response that has a higher block number (0x22222222).
	// If enrichment uses the original tag intent (from req.EvmBlockRef) rather than mutated params,
	// it should call SuggestLatestBlock and update the poller's latest block to 0x22222222.
	jrr := common.MustNewJsonRpcResponse(1, map[string]interface{}{"number": "0x22222222"}, nil)
	resp2 := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr).SetUpstream(ups)
	// Retry a few times to avoid racing with background poller updates that may hold the Suggest lock
	for i := 0; i < 10; i++ {
		network.enrichStatePoller(ctx, "eth_getBlockByNumber", req, resp2)
		time.Sleep(50 * time.Millisecond)
		if poller.LatestBlock() >= int64(0x22222222) {
			break
		}
	}

	latest := poller.LatestBlock()
	// Expect enrichment to have updated to 0x22222222 (572662306)
	assert.Equal(t, int64(0x22222222), latest, "LatestBlock should be enriched to the block from response when original tag was 'latest'")
}

// When using EIP-1898 object params for eth_getBlockReceipts, we must preserve the object
// and only update the nested blockNumber. Current implementation replaces the whole object
// due to conflicting refs ({0} and {0,"blockNumber"}).
func TestInterpolation_EIP1898_BlockReceipts_ObjectBlockNumber_Latest_PreservesObject(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	// Expect object param to be preserved with translated nested blockNumber, not replaced by a bare string
	gock.New("http://rpc1.localhost").
		Post("").
		Times(1).
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			return strings.Contains(body, "eth_getBlockReceipts") &&
				strings.Contains(body, "\"params\":[{\"blockNumber\":\"0x") && // object with translated hex
				!strings.Contains(body, "\"params\":[\"0x") // not a bare string param
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  []interface{}{},
		})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupTestNetworkForInterpolation(t, ctx, nil)

	// EIP-1898 object param with tag
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":[{"blockNumber":"latest"}]}`))
	req.SetNetwork(network)

	// Normalize then forward; with bug present, this will replace the object and fail to match mock
	jrq, err := req.JsonRpcRequest()
	require.NoError(t, err)
	evm.NormalizeHttpJsonRpc(ctx, req, jrq)

	// Forward to verify request shape; this should fail with current bug (no matching mock)
	_, err = network.Forward(ctx, req)
	require.NoError(t, err)
}

// Same as above but with "finalized" tag; should still preserve object shape and translate nested field.
func TestInterpolation_EIP1898_BlockReceipts_ObjectBlockNumber_Finalized_PreservesObject(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	gock.New("http://rpc1.localhost").
		Post("").
		Times(1).
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			return strings.Contains(body, "eth_getBlockReceipts") &&
				strings.Contains(body, "\"params\":[{\"blockNumber\":\"0x") &&
				!strings.Contains(body, "\"params\":[\"0x")
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  []interface{}{},
		})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupTestNetworkForInterpolation(t, ctx, nil)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":[{"blockNumber":"finalized"}]}`))
	req.SetNetwork(network)

	jrq, err := req.JsonRpcRequest()
	require.NoError(t, err)
	evm.NormalizeHttpJsonRpc(ctx, req, jrq)

	_, err = network.Forward(ctx, req)
	require.NoError(t, err)
}

// Mixed tags should collapse EvmBlockRef to "*"
func TestInterpolation_EthGetLogs_MixedTags_EvmBlockRefCollapsed(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	// Accept any eth_getLogs call
	gock.New("http://rpc1.localhost").
		Post("").
		Times(1).
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			return strings.Contains(body, "eth_getLogs")
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  []interface{}{},
		})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupTestNetworkForInterpolation(t, ctx, nil)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{"fromBlock":"finalized","toBlock":"latest"}]}`))
	req.SetNetwork(network)

	jrq, err := req.JsonRpcRequest()
	require.NoError(t, err)
	evm.NormalizeHttpJsonRpc(ctx, req, jrq)

	ref := req.EvmBlockRef()
	require.NotNil(t, ref)
	refStr, ok := ref.(string)
	require.True(t, ok, "EvmBlockRef should be a string")
	assert.Equal(t, "*", refStr, "Mixed tags should collapse to '*'")

	// Forward to ensure end-to-end works
	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	if resp != nil {
		resp.Release()
	}
}

// Same tags should preserve that tag in EvmBlockRef
func TestInterpolation_EthGetLogs_SameTags_EvmBlockRefPreserved(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	// Accept any eth_getLogs call
	gock.New("http://rpc1.localhost").
		Post("").
		Times(1).
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			return strings.Contains(body, "eth_getLogs")
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  []interface{}{},
		})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupTestNetworkForInterpolation(t, ctx, nil)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{"fromBlock":"latest","toBlock":"latest"}]}`))
	req.SetNetwork(network)

	jrq, err := req.JsonRpcRequest()
	require.NoError(t, err)
	evm.NormalizeHttpJsonRpc(ctx, req, jrq)

	ref := req.EvmBlockRef()
	require.NotNil(t, ref)
	refStr, ok := ref.(string)
	require.True(t, ok, "EvmBlockRef should be a string")
	assert.Equal(t, "latest", refStr, "Same tag should be preserved")

	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	if resp != nil {
		resp.Release()
	}
}

// SkipInterpolation should keep params unchanged but still warm/collapse EvmBlockRef
func TestInterpolation_EthGetLogs_MixedTags_SkipInterpolation_WarmsRef(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	// Accept any eth_getLogs call (though params should remain as tags)
	gock.New("http://rpc1.localhost").
		Post("").
		Times(1).
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			return strings.Contains(body, "eth_getLogs") &&
				strings.Contains(body, `"fromBlock":"finalized"`) &&
				strings.Contains(body, `"toBlock":"latest"`)
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  []interface{}{},
		})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupTestNetworkForInterpolation(t, ctx, nil)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{"fromBlock":"finalized","toBlock":"latest"}]}`))
	req.SetNetwork(network)

	// Enable skip interpolation
	req.SetDirectives(&common.RequestDirectives{
		SkipInterpolation: true,
	})

	jrq, err := req.JsonRpcRequest()
	require.NoError(t, err)

	// Keep a copy of original params to compare
	origParams := make([]interface{}, len(jrq.Params))
	copy(origParams, jrq.Params)

	evm.NormalizeHttpJsonRpc(ctx, req, jrq)

	// Params should be unchanged
	assert.Equal(t, origParams, jrq.Params, "Params should remain unchanged when SkipInterpolation is true")

	// EvmBlockRef should be warmed and collapsed to "*"
	ref := req.EvmBlockRef()
	require.NotNil(t, ref)
	refStr, ok := ref.(string)
	require.True(t, ok, "EvmBlockRef should be a string")
	assert.Equal(t, "*", refStr, "Mixed tags should collapse to '*' even when skipping interpolation")

	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	if resp != nil {
		resp.Release()
	}
}

// Mixing with semantic tags (safe/pending) should not collapse; preserve known tag
func TestInterpolation_EthGetLogs_MixedWithSemanticTags_NoCollapse(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	// Accept any eth_getLogs call
	gock.New("http://rpc1.localhost").
		Post("").
		Times(1).
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			return strings.Contains(body, "eth_getLogs")
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  []interface{}{},
		})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupTestNetworkForInterpolation(t, ctx, nil)

	// latest + safe → should preserve "latest"
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{"fromBlock":"latest","toBlock":"safe"}]}`))
	req.SetNetwork(network)

	jrq, err := req.JsonRpcRequest()
	require.NoError(t, err)
	evm.NormalizeHttpJsonRpc(ctx, req, jrq)

	ref := req.EvmBlockRef()
	require.NotNil(t, ref)
	refStr, ok := ref.(string)
	require.True(t, ok, "EvmBlockRef should be a string")
	assert.Equal(t, "latest", refStr, "Mix with semantic tags should not collapse")

	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	if resp != nil {
		resp.Release()
	}
}

// Ensure EvmBlockRef is cleared per-request (no leak across requests)
func TestInterpolation_EvmBlockRef_IsPerRequest(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	// Accept any eth_getLogs call twice
	gock.New("http://rpc1.localhost").Post("").Times(2).Reply(200).JSON(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"result":  []interface{}{},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupTestNetworkForInterpolation(t, ctx, nil)

	// First request mixed → "*"
	req1 := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{"fromBlock":"finalized","toBlock":"latest"}]}`))
	req1.SetNetwork(network)
	jrq1, _ := req1.JsonRpcRequest()
	evm.NormalizeHttpJsonRpc(ctx, req1, jrq1)
	ref1 := req1.EvmBlockRef()
	require.NotNil(t, ref1)
	assert.Equal(t, "*", ref1.(string))
	resp1, err := network.Forward(ctx, req1)
	require.NoError(t, err)
	if resp1 != nil {
		resp1.Release()
	}

	// Second request uniform → "latest"
	req2 := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{"fromBlock":"latest","toBlock":"latest"}]}`))
	req2.SetNetwork(network)
	jrq2, _ := req2.JsonRpcRequest()
	// Ensure previous value isn't accidentally reused by initializing a dummy value
	var dummy atomic.Value
	dummy.Store("should-not-leak")
	_ = dummy // silence linter
	evm.NormalizeHttpJsonRpc(ctx, req2, jrq2)
	ref2 := req2.EvmBlockRef()
	require.NotNil(t, ref2)
	assert.Equal(t, "latest", ref2.(string))
	resp2, err := network.Forward(ctx, req2)
	require.NoError(t, err)
	if resp2 != nil {
		resp2.Release()
	}
}

// Test "finalized" tag translation to hex number
func TestInterpolation_FinalizedTag_ToHex(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupTestNetworkForInterpolation(t, ctx, nil)

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

// Test "safe" tag NOT translated (passed through)
func TestInterpolation_SafeTag_PassThrough(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	// Set up mock BEFORE network initialization
	gock.New("http://rpc1.localhost").
		Post("").
		Times(1).
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			// Should still have "safe" since we don't translate it
			return strings.Contains(body, "eth_getCode") &&
				strings.Contains(body, "\"safe\"")
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  "0x",
		})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupTestNetworkForInterpolation(t, ctx, nil)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getCode","params":["0xabc","safe"]}`))
	req.SetNetwork(network)

	jrq, err := req.JsonRpcRequest()
	require.NoError(t, err)
	evm.NormalizeHttpJsonRpc(ctx, req, jrq)

	params := jrq.Params
	require.Len(t, params, 2)
	blockParam := params[1].(string)
	assert.Equal(t, "safe", blockParam, "Should still be 'safe' - not translated")

	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	if resp != nil {
		resp.Release()
	}
}

// Test that deep copy prevents race conditions
func TestInterpolation_DeepCopyPreventsRace(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Create a request with nested structures
	req := common.NewNormalizedRequest([]byte(`{
		"jsonrpc":"2.0",
		"id":1,
		"method":"eth_getLogs",
		"params":[{
			"fromBlock":"latest",
			"toBlock":"0x100",
			"address":"0xabc",
			"topics":["0x123","0x456"]
		}]
	}`))

	// Create a minimal network for testing
	network, _ := setupTestNetworkForInterpolation(t, ctx, nil)
	req.SetNetwork(network)

	jrq, err := req.JsonRpcRequest()
	require.NoError(t, err)

	// Capture original params structure
	originalParams := jrq.Params[0].(map[string]interface{})
	originalFromBlock := originalParams["fromBlock"].(string)

	// Run normalization in a goroutine
	done := make(chan bool)
	go func() {
		evm.NormalizeHttpJsonRpc(ctx, req, jrq)
		done <- true
	}()

	// Simulate concurrent modification of original params
	// This should NOT affect the normalization process thanks to deep copy
	go func() {
		time.Sleep(10 * time.Millisecond) // Small delay to let normalization start
		jrq.Lock()
		if params, ok := jrq.Params[0].(map[string]interface{}); ok {
			params["fromBlock"] = "corrupted"
			params["address"] = "corrupted"
			if topics, ok := params["topics"].([]interface{}); ok && len(topics) > 0 {
				topics[0] = "corrupted"
			}
		}
		jrq.Unlock()
	}()

	// Wait for normalization to complete
	<-done

	// Verify that normalization worked correctly despite concurrent modification
	// We need to lock to safely read the params after both goroutines have finished
	jrq.RLock()
	finalParams := jrq.Params[0].(map[string]interface{})

	// fromBlock should be translated from "latest" to hex (not "corrupted")
	fromBlock := finalParams["fromBlock"].(string)
	assert.True(t, strings.HasPrefix(fromBlock, "0x"), "fromBlock should be hex")
	assert.NotEqual(t, "corrupted", fromBlock, "fromBlock should not be corrupted")
	assert.NotEqual(t, originalFromBlock, fromBlock, "fromBlock should be translated")

	// toBlock should be normalized (0x100 stays as is or gets normalized)
	toBlock := finalParams["toBlock"].(string)
	assert.True(t, strings.HasPrefix(toBlock, "0x"), "toBlock should be hex")
	jrq.RUnlock()

	// The test proves that the deep copy protected the normalization process
	// from concurrent modifications during processing
}

// Test that safe and pending tags preserve their semantic meaning
func TestInterpolation_PreservesSemanticTags(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupTestNetworkForInterpolation(t, ctx, nil)

	testCases := []struct {
		name        string
		tag         string
		description string
	}{
		{
			name:        "safe_preserved",
			tag:         "safe",
			description: "safe tag represents a specific consensus state and should not be altered",
		},
		{
			name:        "pending_preserved",
			tag:         "pending",
			description: "pending tag includes mempool state and should not be altered",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			req := common.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xabc","%s"]}`, tc.tag)))
			req.SetNetwork(network)

			jrq, err := req.JsonRpcRequest()
			require.NoError(t, err)

			// Apply normalization
			evm.NormalizeHttpJsonRpc(ctx, req, jrq)

			// Verify tag is preserved
			params := jrq.Params
			require.Len(t, params, 2)
			blockParam := params[1].(string)
			assert.Equal(t, tc.tag, blockParam, tc.description)
		})
	}
}

// Test "pending" tag NOT translated (passed through)
func TestInterpolation_PendingTag_PassThrough(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	// Set up mock BEFORE network initialization
	gock.New("http://rpc1.localhost").
		Post("").
		Times(1).
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			// Should still have "pending" since we don't translate it
			return strings.Contains(body, "eth_getTransactionCount") &&
				strings.Contains(body, "\"pending\"")
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  "0x5",
		})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupTestNetworkForInterpolation(t, ctx, nil)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getTransactionCount","params":["0xabc","pending"]}`))
	req.SetNetwork(network)

	jrq, err := req.JsonRpcRequest()
	require.NoError(t, err)
	evm.NormalizeHttpJsonRpc(ctx, req, jrq)

	params := jrq.Params
	require.Len(t, params, 2)
	blockParam := params[1].(string)
	assert.Equal(t, "pending", blockParam, "Should still be 'pending' - not translated")

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

	// Set up mock BEFORE network initialization
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupTestNetworkForInterpolation(t, ctx, nil)

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

	// Set up mock BEFORE network initialization
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupTestNetworkForInterpolation(t, ctx, nil)

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

	// Set up mock BEFORE network initialization
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupTestNetworkForInterpolation(t, ctx, nil)

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

	// Set up mock BEFORE network initialization
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupTestNetworkForInterpolation(t, ctx, nil)

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

	// Set up mock BEFORE network initialization
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

	// Set up mock BEFORE network initialization
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

// Test that eth_getBlockByNumber does NOT interpolate "latest" by default.
// This method should fetch actual latest from upstream, not use the state poller's
// potentially stale value. This is critical for indexers that poll for latest blocks.
func TestInterpolation_EthGetBlockByNumber_Latest_NotInterpolated(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupTestNetworkForInterpolation(t, ctx, nil)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["latest",false]}`))
	req.SetNetwork(network)

	// Normalize the request - this is where interpolation would happen
	jrq, err := req.JsonRpcRequest()
	require.NoError(t, err)
	evm.NormalizeHttpJsonRpc(ctx, req, jrq)

	// Verify "latest" was NOT translated (should still be "latest")
	// This is the key assertion - eth_getBlockByNumber should NOT interpolate the tag
	params := jrq.Params
	require.Len(t, params, 2)
	blockParam := params[0].(string)
	assert.Equal(t, "latest", blockParam, "eth_getBlockByNumber should NOT interpolate 'latest' - it should fetch actual latest from upstream")
}

// Test that eth_getBlockByNumber does NOT interpolate "finalized" by default.
func TestInterpolation_EthGetBlockByNumber_Finalized_NotInterpolated(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupTestNetworkForInterpolation(t, ctx, nil)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["finalized",false]}`))
	req.SetNetwork(network)

	// Normalize the request - this is where interpolation would happen
	jrq, err := req.JsonRpcRequest()
	require.NoError(t, err)
	evm.NormalizeHttpJsonRpc(ctx, req, jrq)

	// Verify "finalized" was NOT translated (should still be "finalized")
	params := jrq.Params
	require.Len(t, params, 2)
	blockParam := params[0].(string)
	assert.Equal(t, "finalized", blockParam, "eth_getBlockByNumber should NOT interpolate 'finalized'")
}

// Test that other methods like eth_getBalance STILL interpolate "latest".
// This ensures our change to eth_getBlockByNumber doesn't affect other methods.
func TestInterpolation_OtherMethods_StillInterpolateLatest(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Set up mock BEFORE network initialization
	// eth_getBalance should have "latest" interpolated to hex
	gock.New("http://rpc1.localhost").
		Post("").
		Times(1).
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			// Should have hex block number, NOT "latest"
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

	network, _ := setupTestNetworkForInterpolation(t, ctx, nil)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xabc","latest"]}`))
	req.SetNetwork(network)

	// Normalize the request
	jrq, err := req.JsonRpcRequest()
	require.NoError(t, err)
	evm.NormalizeHttpJsonRpc(ctx, req, jrq)

	// Verify "latest" WAS translated to hex for eth_getBalance
	params := jrq.Params
	require.Len(t, params, 2)
	blockParam := params[1].(string)
	assert.True(t, strings.HasPrefix(blockParam, "0x"), "eth_getBalance should interpolate 'latest' to hex")
	assert.NotEqual(t, "latest", blockParam, "eth_getBalance should NOT have 'latest' anymore")

	// Forward to verify mock is hit
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

	// Set up mock BEFORE network initialization
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupTestNetworkForInterpolation(t, ctx, nil)

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

	// Set up mock BEFORE network initialization
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupTestNetworkForInterpolation(t, ctx, nil)

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

	// Set up mock BEFORE network initialization
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupTestNetworkForInterpolation(t, ctx, nil)

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

	// Set up mock BEFORE network initialization
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupTestNetworkForInterpolation(t, ctx, nil)

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

	// Set up mock BEFORE network initialization
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupTestNetworkForInterpolation(t, ctx, nil)

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

	// Set up mock BEFORE network initialization
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupTestNetworkForInterpolation(t, ctx, nil)

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

// Benchmark tag translation performance with deep copy
func BenchmarkInterpolation_TagTranslationWithDeepCopy(b *testing.B) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx := context.Background()
	network, _ := setupTestNetworkForInterpolation(nil, ctx, nil)

	// Use a complex nested structure to test deep copy performance
	req := common.NewNormalizedRequest([]byte(`{
		"jsonrpc":"2.0",
		"id":1,
		"method":"eth_getLogs",
		"params":[{
			"fromBlock":"finalized",
			"toBlock":"latest",
			"address":"0xabc",
			"topics":["0x123","0x456","0x789"],
			"nested":{
				"field1":"value1",
				"field2":"value2"
			}
		}]
	}`))
	req.SetNetwork(network)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		jrq, _ := req.JsonRpcRequest()
		evm.NormalizeHttpJsonRpc(ctx, req, jrq)
	}
}

// Benchmark tag translation performance (simple params)
func BenchmarkInterpolation_TagTranslation(b *testing.B) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx := context.Background()
	network, _ := setupTestNetworkForInterpolation(nil, ctx, nil)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xabc","latest"]}`))
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
			params: `["0xabc","latest"]`,
			expectedFilter: func(body string) bool {
				return strings.Contains(body, "eth_getTransactionCount") && !strings.Contains(body, "\"latest\"")
			},
		},
		{
			name:   "eth_getCode",
			method: "eth_getCode",
			params: `["0xabc","finalized"]`,
			expectedFilter: func(body string) bool {
				return strings.Contains(body, "eth_getCode") && !strings.Contains(body, "\"finalized\"")
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

			// Mock for the specific method - BEFORE network initialization
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

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			network, _ := setupTestNetworkForInterpolation(t, ctx, nil)

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

// When interpolating "latest", only upstreams whose latest >= interpolated block should be used.
func TestInterpolation_UpstreamSkipping_OnInterpolatedLatest(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Create 3 upstreams with different latest blocks via util.SetupMocksForEvmStatePoller
	upCfgs := []*common.UpstreamConfig{
		{
			Id:       "rpc1",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId:             123,
				StatePollerInterval: common.Duration(200 * time.Millisecond),
				StatePollerDebounce: common.Duration(50 * time.Millisecond),
			},
		},
		{
			Id:       "rpc2",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "http://rpc2.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId:             123,
				StatePollerInterval: common.Duration(200 * time.Millisecond),
				StatePollerDebounce: common.Duration(50 * time.Millisecond),
			},
		},
		{
			Id:       "rpc3",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "http://rpc3.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId:             123,
				StatePollerInterval: common.Duration(200 * time.Millisecond),
				StatePollerDebounce: common.Duration(50 * time.Millisecond),
			},
		},
	}

	// Only set user mock for rpc3 (highest latest) - this should be used
	gock.New("http://rpc3.localhost").
		Post("").
		Times(1).
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			return strings.Contains(body, "eth_getBalance") && strings.Contains(body, "\"0x") && !strings.Contains(body, "\"latest\"")
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  "0x1234",
		})

	rlr, _ := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{}, &log.Logger)
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
	upr := upstream.NewUpstreamsRegistry(ctx, &log.Logger, "prjA", upCfgs, ssr, rlr, vr, pr, nil, mt, 1*time.Second, nil)
	upr.Bootstrap(ctx)
	time.Sleep(200 * time.Millisecond)

	networkConfig := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm: &common.EvmNetworkConfig{
			ChainId: 123,
		},
	}
	network, _ := NewNetwork(ctx, &log.Logger, "prjA", networkConfig, rlr, upr, mt)
	require.NoError(t, upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123)))
	require.NoError(t, network.Bootstrap(ctx))

	// Build request with 'latest' which will be interpolated to highest latest block (from rpc3)
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xabc","latest"]}`))
	req.SetNetwork(network)

	jrq, err := req.JsonRpcRequest()
	require.NoError(t, err)
	evm.NormalizeHttpJsonRpc(ctx, req, jrq)

	// Ensure the cached EvmBlockRef is preserved as the original tag despite param mutation
	ref := req.EvmBlockRef()
	require.NotNil(t, ref)
	refStr, ok := ref.(string)
	require.True(t, ok)
	assert.Equal(t, "latest", refStr, "Normalization must preserve the original tag in EvmBlockRef")
	// And ensure params changed to hex (no literal 'latest' remains)
	jrq.RLock()
	bodyHex := false
	if len(jrq.Params) > 1 {
		if s, ok := jrq.Params[1].(string); ok && strings.HasPrefix(s, "0x") {
			bodyHex = true
		}
	}
	jrq.RUnlock()
	assert.True(t, bodyHex, "Param should be translated to hex while EvmBlockRef keeps the tag")

	// Forward and ensure rpc3 handled it
	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	defer resp.Release()
	require.NotNil(t, resp.Upstream())
	assert.Equal(t, "rpc3", resp.Upstream().Id(), "request should be routed only to the upstream with highest latest block")
}

// When method-level enforcement is disabled, upstreams are not skipped even if their head is behind interpolated block.
func TestInterpolation_UpstreamSkipping_DisabledByMethodConfig(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Two upstreams; rpc2 has higher latest (due to poller mocks), but we'll only mock rpc1 user call.
	upCfgs := []*common.UpstreamConfig{
		{
			Id:       "rpc1",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId:             123,
				StatePollerInterval: common.Duration(200 * time.Millisecond),
				StatePollerDebounce: common.Duration(50 * time.Millisecond),
			},
		},
		{
			Id:       "rpc2",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "http://rpc2.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId:             123,
				StatePollerInterval: common.Duration(200 * time.Millisecond),
				StatePollerDebounce: common.Duration(50 * time.Millisecond),
			},
		},
	}

	// Mock only rpc1 user call; if enforcement honored the head, this would be skipped,
	// but with enforcement disabled method-level, it should still be used.
	gock.New("http://rpc1.localhost").
		Post("").
		Times(1).
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			return strings.Contains(body, "eth_getBalance") && strings.Contains(body, "\"0x") && !strings.Contains(body, "\"latest\"")
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  "0x99",
		})

	rlr, _ := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{}, &log.Logger)
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
	upr := upstream.NewUpstreamsRegistry(ctx, &log.Logger, "prjA", upCfgs, ssr, rlr, vr, pr, nil, mt, 1*time.Second, nil)
	upr.Bootstrap(ctx)
	time.Sleep(200 * time.Millisecond)

	// Network config disables enforcement for eth_getBalance
	falseVal := false
	networkConfig := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm: &common.EvmNetworkConfig{
			ChainId: 123,
		},
		Methods: &common.MethodsConfig{
			Definitions: map[string]*common.CacheMethodConfig{
				"eth_getBalance": {
					ReqRefs:                  [][]interface{}{{1}},
					EnforceBlockAvailability: &falseVal,
				},
			},
		},
	}
	network, _ := NewNetwork(ctx, &log.Logger, "prjA", networkConfig, rlr, upr, mt)
	require.NoError(t, upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123)))
	require.NoError(t, network.Bootstrap(ctx))

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xabc","latest"]}`))
	req.SetNetwork(network)
	jrq, err := req.JsonRpcRequest()
	require.NoError(t, err)
	evm.NormalizeHttpJsonRpc(ctx, req, jrq)

	upstream.ReorderUpstreams(upr, "rpc1", "rpc2")

	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	defer resp.Release()
	require.NotNil(t, resp.Upstream())
	assert.Equal(t, "rpc1", resp.Upstream().Id(), "with enforcement disabled for method, upstream should not be skipped")
}
