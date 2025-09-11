package erpc

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

func TestConsensusSelectsLargestResponseWithinGroup(t *testing.T) {
	// Test that when multiple responses are in the same consensus group,
	// the largest response is selected (not just the first one)

	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		time.Sleep(50 * time.Millisecond)
	}()

	// Create upstream configs
	upstreams := []*common.UpstreamConfig{
		{
			Id:       "upstream1",
			Endpoint: "http://rpc1.localhost",
			Type:     common.UpstreamTypeEvm,
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		},
		{
			Id:       "upstream2",
			Endpoint: "http://rpc2.localhost",
			Type:     common.UpstreamTypeEvm,
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		},
		{
			Id:       "upstream3",
			Endpoint: "http://rpc3.localhost",
			Type:     common.UpstreamTypeEvm,
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		},
	}

	// Create network config with consensus that ignores certain fields
	// This allows responses with different sizes to be in the same consensus group
	consensusConfig := &common.ConsensusPolicyConfig{
		MaxParticipants:    3,
		AgreementThreshold: 3,
		DisputeBehavior:    common.ConsensusDisputeBehaviorReturnError,
		IgnoreFields: map[string][]string{
			// For eth_getTransactionReceipt, ignore blockTimestamp and other fields that might vary
			"eth_getTransactionReceipt": {"blockTimestamp", "gasUsedRatio"},
		},
	}

	// Setup network
	ntw, _ := setupNetworkForConsensusTest(t, ctx, consensusTestCase{
		upstreams:       upstreams,
		consensusConfig: consensusConfig,
	})

	// Create receipt responses with same core data but different additional fields
	// The ignored fields (blockTimestamp) allow them to be in the same consensus group

	// All receipts MUST have the same fields (except ignored ones) to be in same consensus group
	// Only the ignored fields (blockTimestamp, gasUsedRatio) can differ in value
	// Response size difference comes from different values of ignored fields

	// Small receipt - short values for ignored fields
	smallReceipt := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"result": map[string]interface{}{
			"transactionHash":   "0xabc123",
			"blockHash":         "0xdef456",
			"blockNumber":       "0x1234",
			"status":            "0x1",
			"gasUsed":           "0x5208",
			"cumulativeGasUsed": "0x5208",
			"from":              "0x1111111111111111111111111111111111111111",
			"to":                "0x2222222222222222222222222222222222222222",
			"transactionIndex":  "0x0",
			"type":              "0x2",
			"effectiveGasPrice": "0x3b9aca00",
			"logs":              []interface{}{},
			"logsBloom":         "0x00000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000",
			"contractAddress":   nil,
			"blockTimestamp":    "0x1", // Short value (ignored field)
		},
	}

	// Medium receipt - medium values for ignored fields
	mediumReceipt := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"result": map[string]interface{}{
			"transactionHash":   "0xabc123",
			"blockHash":         "0xdef456",
			"blockNumber":       "0x1234",
			"status":            "0x1",
			"gasUsed":           "0x5208",
			"cumulativeGasUsed": "0x5208",
			"from":              "0x1111111111111111111111111111111111111111",
			"to":                "0x2222222222222222222222222222222222222222",
			"transactionIndex":  "0x0",
			"type":              "0x2",
			"effectiveGasPrice": "0x3b9aca00",
			"logs":              []interface{}{},
			"logsBloom":         "0x00000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000",
			"contractAddress":   nil,
			"blockTimestamp":    "0x650000001234", // Medium value (ignored field)
			"gasUsedRatio":      "0.333333333333", // Medium value (ignored field)
		},
	}

	// Large receipt - large values for ignored fields
	largeReceipt := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"result": map[string]interface{}{
			"transactionHash":   "0xabc123",
			"blockHash":         "0xdef456",
			"blockNumber":       "0x1234",
			"status":            "0x1",
			"gasUsed":           "0x5208",
			"cumulativeGasUsed": "0x5208",
			"from":              "0x1111111111111111111111111111111111111111",
			"to":                "0x2222222222222222222222222222222222222222",
			"transactionIndex":  "0x0",
			"type":              "0x2",
			"effectiveGasPrice": "0x3b9aca00",
			"logs":              []interface{}{},
			"logsBloom":         "0x00000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000",
			"contractAddress":   nil,
			"blockTimestamp":    "0x6500000012345678901234567890abcdef", // Large value (ignored field)
			"gasUsedRatio":      "0.555555555555555555555555555555555",  // Large value (ignored field)
		},
	}

	// Setup mocks - upstream1 returns small, upstream2 returns medium, upstream3 returns large
	// All should be in the same consensus group since blockTimestamp is ignored
	gock.New("http://rpc1.localhost").
		Post("").
		Filter(func(request *http.Request) bool {
			body := util.SafeReadBody(request)
			return strings.Contains(body, "eth_getTransactionReceipt")
		}).
		Times(1).
		Reply(200).
		JSON(smallReceipt)

	gock.New("http://rpc2.localhost").
		Post("").
		Filter(func(request *http.Request) bool {
			body := util.SafeReadBody(request)
			return strings.Contains(body, "eth_getTransactionReceipt")
		}).
		Times(1).
		Reply(200).
		JSON(mediumReceipt)

	gock.New("http://rpc3.localhost").
		Post("").
		Filter(func(request *http.Request) bool {
			body := util.SafeReadBody(request)
			return strings.Contains(body, "eth_getTransactionReceipt")
		}).
		Times(1).
		Reply(200).
		JSON(largeReceipt)

	// Make request
	reqBytes, err := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "eth_getTransactionReceipt",
		"params":  []interface{}{"0xabc123"},
	})
	require.NoError(t, err)

	fakeReq := common.NewNormalizedRequest(reqBytes)
	fakeReq.SetNetwork(ntw)

	// Execute request
	resp, err := ntw.Forward(ctx, fakeReq)
	require.NoError(t, err)
	require.NotNil(t, resp)

	// Get the JSON-RPC response
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)

	// Verify that the largest response was selected
	resultStr := jrr.GetResultString()

	// The largest response should contain all the extra fields
	assert.Contains(t, resultStr, "effectiveGasPrice", "Expected the largest response with effectiveGasPrice to be selected")
	assert.Contains(t, resultStr, "logsBloom", "Expected the largest response with logsBloom to be selected")
	assert.Contains(t, resultStr, "transactionIndex", "Expected the largest response with transactionIndex to be selected")

	// Verify the response size is the largest
	resultLen := jrr.ResultLength()
	assert.True(t, resultLen > 500, "Expected large result length from the largest response")
}
