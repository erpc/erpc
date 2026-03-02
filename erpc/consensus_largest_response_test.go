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

func TestConsensusSelectsLargestResponseBelowThreshold(t *testing.T) {
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

	// Create network config with consensus preference for larger responses.
	// With accept-most-common behavior, the policy can pick the largest non-empty
	// response even when all participants are below threshold.
	consensusConfig := &common.ConsensusPolicyConfig{
		MaxParticipants:    3,
		AgreementThreshold: 2,
		DisputeBehavior:    common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
	}

	// Setup network
	ntw, _ := setupNetworkForConsensusTest(t, ctx, consensusTestCase{
		upstreams:       upstreams,
		consensusConfig: consensusConfig,
	})

	// Use struct-encoded JSON to keep field order deterministic across responses.
	type receiptResult struct {
		TransactionHash string `json:"transactionHash"`
		BlockHash       string `json:"blockHash"`
		BlockNumber     string `json:"blockNumber"`
		Status          string `json:"status"`
		GasUsed         string `json:"gasUsed"`
		From            string `json:"from"`
		To              string `json:"to"`
		BlockTimestamp  string `json:"blockTimestamp"`
		GasUsedRatio    string `json:"gasUsedRatio"`
		Padding         string `json:"padding"`
	}
	type receiptResponse struct {
		Jsonrpc string        `json:"jsonrpc"`
		ID      int           `json:"id"`
		Result  receiptResult `json:"result"`
	}
	base := receiptResult{
		TransactionHash: "0xabc123",
		BlockHash:       "0xdef456",
		BlockNumber:     "0x1234",
		Status:          "0x1",
		GasUsed:         "0x5208",
		From:            "0x1111111111111111111111111111111111111111",
		To:              "0x2222222222222222222222222222222222222222",
	}
	smallReceipt := receiptResponse{
		Jsonrpc: "2.0",
		ID:      1,
		Result: receiptResult{
			TransactionHash: base.TransactionHash,
			BlockHash:       base.BlockHash,
			BlockNumber:     base.BlockNumber,
			Status:          base.Status,
			GasUsed:         base.GasUsed,
			From:            base.From,
			To:              base.To,
			BlockTimestamp:  "0x650000001234",
			GasUsedRatio:    "0.333333333333",
			Padding:         "0x0",
		},
	}
	mediumReceipt := receiptResponse{
		Jsonrpc: "2.0",
		ID:      1,
		Result: receiptResult{
			TransactionHash: base.TransactionHash,
			BlockHash:       base.BlockHash,
			BlockNumber:     base.BlockNumber,
			Status:          base.Status,
			GasUsed:         base.GasUsed,
			From:            base.From,
			To:              base.To,
			BlockTimestamp:  "0x650000001234",
			GasUsedRatio:    "0.333333333333",
			Padding:         "0x0000000000000000",
		},
	}
	largePadding := "0x0000000000000000000000000000000000000000000000000000000000000000"
	largeReceipt := receiptResponse{
		Jsonrpc: "2.0",
		ID:      1,
		Result: receiptResult{
			TransactionHash: base.TransactionHash,
			BlockHash:       base.BlockHash,
			BlockNumber:     base.BlockNumber,
			Status:          base.Status,
			GasUsed:         base.GasUsed,
			From:            base.From,
			To:              base.To,
			BlockTimestamp:  "0x650000001234",
			GasUsedRatio:    "0.333333333333",
			Padding:         largePadding,
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

	assert.Contains(t, resultStr, largePadding, "expected largest response to be selected")
}
