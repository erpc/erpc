package erpc

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

// Test that finality is preserved as "realtime" when translating tags
func TestInterpolation_PreservesRealtimeFinality(t *testing.T) {
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
			// Request will have translated hex value instead of "latest"
			// The actual value depends on what the state poller returns
			return strings.Contains(body, "eth_getBalance") &&
				!strings.Contains(body, "\"latest\"") && // Should NOT have "latest"
				strings.Contains(body, "\"0x") // Should have hex value
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  "0x1234",
		})

	ctx, cancel := context.WithCancel(context.Background())
	defer util.CancelAndWait(cancel)

	network, _ := setupTestNetworkForInterpolation(t, ctx, nil)

	// Create request with "latest" tag
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xabc","latest"]}`))
	req.SetNetwork(network)

	// Check finality BEFORE normalization
	finalityBefore := req.Finality(ctx)
	assert.Equal(t, common.DataFinalityStateRealtime, finalityBefore, "Request with 'latest' tag should be realtime before translation")

	// Normalize the request (which will translate "latest" to hex)
	jrq, err := req.JsonRpcRequest()
	require.NoError(t, err)
	evm.NormalizeHttpJsonRpc(ctx, req, jrq)

	// Verify the tag was translated
	params := jrq.Params
	require.Len(t, params, 2)
	blockParam := params[1].(string)
	assert.True(t, strings.HasPrefix(blockParam, "0x"), "latest should be translated to hex")
	assert.NotEqual(t, "latest", blockParam, "Should not be 'latest' anymore")

	// Check finality AFTER normalization - this is the critical test
	finalityAfter := req.Finality(ctx)
	assert.Equal(t, common.DataFinalityStateRealtime, finalityAfter, "Request should STILL be realtime after tag translation")

	// Forward the request and check response finality
	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	defer resp.Release()

	// Check response finality
	respFinality := resp.Finality(ctx)
	assert.Equal(t, common.DataFinalityStateRealtime, respFinality, "Response should also be realtime")
}

// Test that finality is preserved for finalized tag
func TestInterpolation_PreservesFinalizedTagFinality(t *testing.T) {
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
			// Should not have the original tag and should contain a hex block ref
			return strings.Contains(body, "eth_call") &&
				!strings.Contains(body, "\"finalized\"") &&
				strings.Contains(body, "\"0x")
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  "0x",
		})

	ctx, cancel := context.WithCancel(context.Background())
	defer util.CancelAndWait(cancel)

	network, _ := setupTestNetworkForInterpolation(t, ctx, nil)

	// Create request with "finalized" tag
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0xabc"},"finalized"]}`))
	req.SetNetwork(network)

	// Check finality BEFORE normalization
	finalityBefore := req.Finality(ctx)
	assert.Equal(t, common.DataFinalityStateRealtime, finalityBefore, "Request with 'finalized' tag should be realtime before translation")

	// Normalize the request
	jrq, err := req.JsonRpcRequest()
	require.NoError(t, err)
	evm.NormalizeHttpJsonRpc(ctx, req, jrq)

	// Check finality AFTER normalization
	finalityAfter := req.Finality(ctx)
	assert.Equal(t, common.DataFinalityStateRealtime, finalityAfter, "Request should STILL be realtime after finalized tag translation")

	// Forward the request
	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	if resp != nil {
		defer resp.Release()

		// Check response finality
		respFinality := resp.Finality(ctx)
		assert.Equal(t, common.DataFinalityStateRealtime, respFinality, "Response should also be realtime")
	}
}

// Test that numeric block parameters are correctly identified as unfinalized
func TestInterpolation_NumericBlockFinality(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	// We don't forward any requests in this test, so no user mocks are needed
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer util.CancelAndWait(cancel)

	network, _ := setupTestNetworkForInterpolation(t, ctx, nil)

	// Create request with numeric block (100)
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xabc",100]}`))
	req.SetNetwork(network)

	// Check finality for numeric block
	finality := req.Finality(ctx)
	// Numeric blocks should typically be unfinalized (unless we can determine they're finalized)
	assert.NotEqual(t, common.DataFinalityStateRealtime, finality, "Numeric block should NOT be realtime")

	// Normalize the request
	jrq, err := req.JsonRpcRequest()
	require.NoError(t, err)
	evm.NormalizeHttpJsonRpc(ctx, req, jrq)

	// Check finality after normalization
	finalityAfter := req.Finality(ctx)
	assert.NotEqual(t, common.DataFinalityStateRealtime, finalityAfter, "Numeric block should still NOT be realtime after normalization")
}
