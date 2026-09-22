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

// Test that finality is "realtime" for eth_getBalance with "latest" — both
// before and after normalization. With TranslateLatestTag=false (default
// for state-reading methods), the literal tag is preserved on the wire and
// Network.GetFinality classifies the request as Realtime end-to-end.
func TestInterpolation_PreservesRealtimeFinality(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	gock.New("http://rpc1.localhost").
		Post("").
		Times(1).
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			// Request must carry the literal "latest" tag — no translation.
			return strings.Contains(body, "eth_getBalance") &&
				strings.Contains(body, "\"latest\"")
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

	finalityBefore := req.Finality(ctx)
	assert.Equal(t, common.DataFinalityStateRealtime, finalityBefore, "Request with 'latest' tag should be realtime before normalization")

	jrq, err := req.JsonRpcRequest()
	require.NoError(t, err)
	evm.NormalizeHttpJsonRpc(ctx, req, jrq)

	params := jrq.Params
	require.Len(t, params, 2)
	blockParam := params[1].(string)
	assert.Equal(t, "latest", blockParam, "eth_getBalance should preserve 'latest' tag (TranslateLatestTag default is false)")

	finalityAfter := req.Finality(ctx)
	assert.Equal(t, common.DataFinalityStateRealtime, finalityAfter, "Request should STILL be realtime after normalization")

	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	defer resp.Release()

	respFinality := resp.Finality(ctx)
	assert.Equal(t, common.DataFinalityStateRealtime, respFinality, "Response should also be realtime")
}

// Test that finality is "realtime" for eth_call with "finalized" tag,
// both before and after normalization. eth_call has TranslateFinalizedTag
// default false, so the literal tag flows through to the upstream.
func TestInterpolation_PreservesFinalizedTagFinality(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	gock.New("http://rpc1.localhost").
		Post("").
		Times(1).
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			// Request must carry the literal "finalized" tag — no translation.
			return strings.Contains(body, "eth_call") &&
				strings.Contains(body, "\"finalized\"")
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

	finalityBefore := req.Finality(ctx)
	assert.Equal(t, common.DataFinalityStateRealtime, finalityBefore, "Request with 'finalized' tag should be realtime before normalization")

	jrq, err := req.JsonRpcRequest()
	require.NoError(t, err)
	evm.NormalizeHttpJsonRpc(ctx, req, jrq)

	params := jrq.Params
	require.Len(t, params, 2)
	blockParam := params[1].(string)
	assert.Equal(t, "finalized", blockParam, "eth_call should preserve 'finalized' tag (TranslateFinalizedTag default is false)")

	finalityAfter := req.Finality(ctx)
	assert.Equal(t, common.DataFinalityStateRealtime, finalityAfter, "Request should STILL be realtime after normalization")

	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	if resp != nil {
		defer resp.Release()

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
	defer cancel()

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
