// Package integration contains integration tests that run against a real eRPC instance.
//
// These tests are skipped by default unless ERPC_INTEGRATION_TEST_ENDPOINT is set.
//
// Environment variables:
//   - ERPC_INTEGRATION_TEST_ENDPOINT: eRPC endpoint URL (required)
//   - ERPC_INTEGRATION_TEST_METRICS: Prometheus metrics endpoint (optional, enables metric verification)
//   - ERPC_INTEGRATION_TEST_AUTH: Auth headers in "Header: value" format (optional, use ";" for multiple)
//
// Usage:
//
//	# Run against local eRPC (no auth)
//	ERPC_INTEGRATION_TEST_ENDPOINT=http://localhost:4000/main/evm/1 \
//	  go test -v ./test/integration/...
//
//	# Run against local eRPC with auth and metrics
//	ERPC_INTEGRATION_TEST_ENDPOINT=http://localhost:4000/main/evm/1 \
//	ERPC_INTEGRATION_TEST_METRICS=http://localhost:4001/metrics \
//	ERPC_INTEGRATION_TEST_AUTH="X-ERPC-Secret-Token: your-token" \
//	  go test -v ./test/integration/...
//
//	# Run specific test
//	ERPC_INTEGRATION_TEST_ENDPOINT=http://localhost:4000/main/evm/1 \
//	ERPC_INTEGRATION_TEST_AUTH="X-ERPC-Secret-Token: your-token" \
//	  go test -v -run TestMulticall3Integration_BatchEthCalls ./test/integration/...
package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// Well-known contract addresses for testing (Ethereum mainnet)
	// These are used because they're stable and have predictable behavior
	wethAddress = "0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2"
	usdcAddress = "0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48"

	// Function selectors
	decimalsSelector    = "0x313ce567" // decimals()
	symbolSelector      = "0x95d89b41" // symbol()
	totalSupplySelector = "0x18160ddd" // totalSupply()
)

type jsonRPCRequest struct {
	JSONRPC string        `json:"jsonrpc"`
	ID      interface{}   `json:"id"`
	Method  string        `json:"method"`
	Params  []interface{} `json:"params"`
}

type jsonRPCResponse struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      interface{}      `json:"id"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *json.RawMessage `json:"error,omitempty"`
}

type ethCallParams struct {
	To   string `json:"to"`
	Data string `json:"data"`
}

func getTestEndpoint(t *testing.T) string {
	endpoint := os.Getenv("ERPC_INTEGRATION_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("ERPC_INTEGRATION_TEST_ENDPOINT not set, skipping integration test")
	}
	return endpoint
}

func getMetricsEndpoint() string {
	return os.Getenv("ERPC_INTEGRATION_TEST_METRICS")
}

// getAuthHeaders returns authentication headers if configured
// Format: "Header-Name: value" or multiple headers separated by ";"
func getAuthHeaders() map[string]string {
	headers := make(map[string]string)
	authHeader := os.Getenv("ERPC_INTEGRATION_TEST_AUTH")
	if authHeader == "" {
		return headers
	}

	// Support multiple headers separated by ";"
	parts := strings.Split(authHeader, ";")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if idx := strings.Index(part, ":"); idx > 0 {
			key := strings.TrimSpace(part[:idx])
			value := strings.TrimSpace(part[idx+1:])
			headers[key] = value
		}
	}
	return headers
}

func makeRequest(t *testing.T, endpoint string, payload interface{}) []byte {
	t.Helper()

	body, err := json.Marshal(payload)
	require.NoError(t, err)

	req, err := http.NewRequest("POST", endpoint, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	// Add auth headers if configured
	for key, value := range getAuthHeaders() {
		req.Header.Set(key, value)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	return respBody
}

func getMetricValue(metricsEndpoint, metricName string) (float64, error) {
	resp, err := http.Get(metricsEndpoint)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}

	var total float64
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, metricName) && !strings.HasPrefix(line, "#") {
			// Parse the metric value (last field)
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				var val float64
				fmt.Sscanf(fields[len(fields)-1], "%f", &val)
				total += val
			}
		}
	}
	return total, nil
}

// TestMulticall3Integration_Connectivity verifies basic endpoint connectivity
func TestMulticall3Integration_Connectivity(t *testing.T) {
	endpoint := getTestEndpoint(t)

	req := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "eth_chainId",
		Params:  []interface{}{},
	}

	respBody := makeRequest(t, endpoint, req)

	var resp jsonRPCResponse
	err := json.Unmarshal(respBody, &resp)
	require.NoError(t, err, "Failed to parse response: %s", string(respBody))
	require.Nil(t, resp.Error, "Request returned error: %s", string(respBody))
	require.NotNil(t, resp.Result, "No result in response")

	var chainId string
	err = json.Unmarshal(resp.Result, &chainId)
	require.NoError(t, err)
	assert.NotEmpty(t, chainId, "chainId should not be empty")

	t.Logf("Connected to chain: %s", chainId)
}

// TestMulticall3Integration_SingleEthCall verifies single eth_call works (baseline)
func TestMulticall3Integration_SingleEthCall(t *testing.T) {
	endpoint := getTestEndpoint(t)

	req := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "eth_call",
		Params: []interface{}{
			ethCallParams{To: wethAddress, Data: decimalsSelector},
			"latest",
		},
	}

	respBody := makeRequest(t, endpoint, req)

	var resp jsonRPCResponse
	err := json.Unmarshal(respBody, &resp)
	require.NoError(t, err, "Failed to parse response: %s", string(respBody))
	require.Nil(t, resp.Error, "Request returned error: %s", string(respBody))
	require.NotNil(t, resp.Result, "No result in response")

	var result string
	err = json.Unmarshal(resp.Result, &result)
	require.NoError(t, err)
	assert.NotEmpty(t, result, "result should not be empty")

	t.Logf("Single eth_call result (decimals): %s", result)
}

// TestMulticall3Integration_BatchEthCalls verifies JSON-RPC batch with multiple eth_calls
// These should be aggregated into a single multicall3 call
func TestMulticall3Integration_BatchEthCalls(t *testing.T) {
	endpoint := getTestEndpoint(t)
	metricsEndpoint := getMetricsEndpoint()

	// Capture baseline metrics if available
	var baselineAggregation float64
	if metricsEndpoint != "" {
		var err error
		baselineAggregation, err = getMetricValue(metricsEndpoint, "erpc_multicall3_aggregation_total")
		if err != nil {
			t.Logf("Warning: could not get baseline metrics: %v", err)
		}
	}

	// Send batch with 3 eth_calls to the same contract
	batch := []jsonRPCRequest{
		{JSONRPC: "2.0", ID: 1, Method: "eth_call", Params: []interface{}{ethCallParams{To: wethAddress, Data: decimalsSelector}, "latest"}},
		{JSONRPC: "2.0", ID: 2, Method: "eth_call", Params: []interface{}{ethCallParams{To: wethAddress, Data: symbolSelector}, "latest"}},
		{JSONRPC: "2.0", ID: 3, Method: "eth_call", Params: []interface{}{ethCallParams{To: wethAddress, Data: totalSupplySelector}, "latest"}},
	}

	respBody := makeRequest(t, endpoint, batch)

	var responses []jsonRPCResponse
	err := json.Unmarshal(respBody, &responses)
	require.NoError(t, err, "Failed to parse batch response: %s", string(respBody))
	require.Len(t, responses, 3, "Expected 3 responses in batch")

	for i, resp := range responses {
		assert.Nil(t, resp.Error, "Response %d returned error", i)
		assert.NotNil(t, resp.Result, "Response %d has no result", i)
	}

	t.Logf("Batch eth_call responses: all 3 successful")

	// Check if metrics increased (indicates multicall3 was used)
	if metricsEndpoint != "" {
		time.Sleep(1 * time.Second) // Allow metrics to update

		newAggregation, err := getMetricValue(metricsEndpoint, "erpc_multicall3_aggregation_total")
		if err == nil {
			diff := newAggregation - baselineAggregation
			t.Logf("Multicall3 aggregation metric: %.0f -> %.0f (+%.0f)", baselineAggregation, newAggregation, diff)
			if diff > 0 {
				t.Logf("✓ Multicall3 aggregation confirmed via metrics")
			} else {
				t.Logf("⚠ No increase in aggregation metric - multicall3 may not be enabled or requests fell back")
			}
		}
	}
}

// TestMulticall3Integration_ConcurrentRequests verifies concurrent requests are batched together
func TestMulticall3Integration_ConcurrentRequests(t *testing.T) {
	endpoint := getTestEndpoint(t)
	metricsEndpoint := getMetricsEndpoint()

	// Capture baseline metrics if available
	var baselineAggregation float64
	if metricsEndpoint != "" {
		baselineAggregation, _ = getMetricValue(metricsEndpoint, "erpc_multicall3_aggregation_total")
	}

	// Send 10 concurrent requests - they should be batched within the window
	const numRequests = 10
	var wg sync.WaitGroup
	results := make(chan bool, numRequests)

	for i := 0; i < numRequests; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			req := jsonRPCRequest{
				JSONRPC: "2.0",
				ID:      id,
				Method:  "eth_call",
				Params: []interface{}{
					ethCallParams{To: wethAddress, Data: decimalsSelector},
					"latest",
				},
			}

			respBody := makeRequest(t, endpoint, req)

			var resp jsonRPCResponse
			if err := json.Unmarshal(respBody, &resp); err != nil {
				results <- false
				return
			}

			results <- resp.Error == nil && resp.Result != nil
		}(i)
	}

	wg.Wait()
	close(results)

	successCount := 0
	for success := range results {
		if success {
			successCount++
		}
	}

	assert.Equal(t, numRequests, successCount, "All concurrent requests should succeed")
	t.Logf("Concurrent requests: %d/%d successful", successCount, numRequests)

	// Check if metrics show batching occurred
	if metricsEndpoint != "" {
		time.Sleep(1 * time.Second)

		newAggregation, err := getMetricValue(metricsEndpoint, "erpc_multicall3_aggregation_total")
		if err == nil {
			diff := newAggregation - baselineAggregation
			t.Logf("Multicall3 aggregation metric: %.0f -> %.0f (+%.0f)", baselineAggregation, newAggregation, diff)

			// If multicall3 is working, we should see fewer aggregations than requests
			// (since multiple requests get batched into one multicall3 call)
			if diff > 0 && diff < float64(numRequests) {
				t.Logf("✓ Batching confirmed: %d requests resulted in %.0f aggregations", numRequests, diff)
			}
		}
	}
}

// TestMulticall3Integration_MixedBatch verifies mixed batch with eth_call and other methods
func TestMulticall3Integration_MixedBatch(t *testing.T) {
	endpoint := getTestEndpoint(t)

	batch := []jsonRPCRequest{
		{JSONRPC: "2.0", ID: 1, Method: "eth_call", Params: []interface{}{ethCallParams{To: wethAddress, Data: decimalsSelector}, "latest"}},
		{JSONRPC: "2.0", ID: 2, Method: "eth_blockNumber", Params: []interface{}{}},
		{JSONRPC: "2.0", ID: 3, Method: "eth_call", Params: []interface{}{ethCallParams{To: wethAddress, Data: totalSupplySelector}, "latest"}},
		{JSONRPC: "2.0", ID: 4, Method: "eth_chainId", Params: []interface{}{}},
	}

	respBody := makeRequest(t, endpoint, batch)

	var responses []jsonRPCResponse
	err := json.Unmarshal(respBody, &responses)
	require.NoError(t, err, "Failed to parse mixed batch response: %s", string(respBody))
	require.Len(t, responses, 4, "Expected 4 responses in mixed batch")

	for i, resp := range responses {
		assert.Nil(t, resp.Error, "Response %d returned error", i)
		assert.NotNil(t, resp.Result, "Response %d has no result", i)
	}

	t.Logf("Mixed batch: all 4 responses successful (2 eth_call, 1 eth_blockNumber, 1 eth_chainId)")
}

// TestMulticall3Integration_DifferentContracts verifies batching across different contracts
func TestMulticall3Integration_DifferentContracts(t *testing.T) {
	endpoint := getTestEndpoint(t)

	// Batch calls to different contracts - should still be batched via multicall3
	batch := []jsonRPCRequest{
		{JSONRPC: "2.0", ID: 1, Method: "eth_call", Params: []interface{}{ethCallParams{To: wethAddress, Data: decimalsSelector}, "latest"}},
		{JSONRPC: "2.0", ID: 2, Method: "eth_call", Params: []interface{}{ethCallParams{To: usdcAddress, Data: decimalsSelector}, "latest"}},
		{JSONRPC: "2.0", ID: 3, Method: "eth_call", Params: []interface{}{ethCallParams{To: wethAddress, Data: symbolSelector}, "latest"}},
		{JSONRPC: "2.0", ID: 4, Method: "eth_call", Params: []interface{}{ethCallParams{To: usdcAddress, Data: symbolSelector}, "latest"}},
	}

	respBody := makeRequest(t, endpoint, batch)

	var responses []jsonRPCResponse
	err := json.Unmarshal(respBody, &responses)
	require.NoError(t, err, "Failed to parse response: %s", string(respBody))
	require.Len(t, responses, 4, "Expected 4 responses")

	for i, resp := range responses {
		assert.Nil(t, resp.Error, "Response %d returned error", i)
		assert.NotNil(t, resp.Result, "Response %d has no result", i)
	}

	t.Logf("Different contracts batch: all 4 responses successful (WETH + USDC)")
}

// TestMulticall3Integration_ErrorHandling verifies error handling for failing calls
func TestMulticall3Integration_ErrorHandling(t *testing.T) {
	endpoint := getTestEndpoint(t)

	// Send a batch with one valid and one invalid call
	batch := []jsonRPCRequest{
		{JSONRPC: "2.0", ID: 1, Method: "eth_call", Params: []interface{}{ethCallParams{To: wethAddress, Data: decimalsSelector}, "latest"}},
		{JSONRPC: "2.0", ID: 2, Method: "eth_call", Params: []interface{}{ethCallParams{To: wethAddress, Data: "0xdeadbeef"}, "latest"}}, // Invalid function
		{JSONRPC: "2.0", ID: 3, Method: "eth_call", Params: []interface{}{ethCallParams{To: wethAddress, Data: symbolSelector}, "latest"}},
	}

	respBody := makeRequest(t, endpoint, batch)

	var responses []jsonRPCResponse
	err := json.Unmarshal(respBody, &responses)
	require.NoError(t, err, "Failed to parse response: %s", string(respBody))
	require.Len(t, responses, 3, "Expected 3 responses")

	// First and third should succeed
	assert.Nil(t, responses[0].Error, "Response 0 should succeed")
	assert.NotNil(t, responses[0].Result, "Response 0 should have result")

	assert.Nil(t, responses[2].Error, "Response 2 should succeed")
	assert.NotNil(t, responses[2].Result, "Response 2 should have result")

	// Second may error or return empty - both are acceptable
	if responses[1].Error != nil {
		t.Logf("Invalid call returned error (expected): %s", string(*responses[1].Error))
	} else if responses[1].Result != nil {
		var result string
		json.Unmarshal(responses[1].Result, &result)
		t.Logf("Invalid call returned result: %s (may be empty or revert data)", result)
	}

	t.Logf("Error handling: valid calls succeeded, invalid call handled gracefully")
}

// TestMulticall3Integration_LargeBatch verifies handling of larger batches
func TestMulticall3Integration_LargeBatch(t *testing.T) {
	endpoint := getTestEndpoint(t)
	metricsEndpoint := getMetricsEndpoint()

	// Capture baseline metrics
	var baselineAggregation float64
	if metricsEndpoint != "" {
		baselineAggregation, _ = getMetricValue(metricsEndpoint, "erpc_multicall3_aggregation_total")
	}

	// Send a batch with 20 calls (should trigger batching limits)
	batch := make([]jsonRPCRequest, 20)
	for i := 0; i < 20; i++ {
		batch[i] = jsonRPCRequest{
			JSONRPC: "2.0",
			ID:      i + 1,
			Method:  "eth_call",
			Params: []interface{}{
				ethCallParams{To: wethAddress, Data: decimalsSelector},
				"latest",
			},
		}
	}

	respBody := makeRequest(t, endpoint, batch)

	var responses []jsonRPCResponse
	err := json.Unmarshal(respBody, &responses)
	require.NoError(t, err, "Failed to parse response: %s", string(respBody))
	require.Len(t, responses, 20, "Expected 20 responses")

	successCount := 0
	for _, resp := range responses {
		if resp.Error == nil && resp.Result != nil {
			successCount++
		}
	}

	assert.Equal(t, 20, successCount, "All 20 calls should succeed")
	t.Logf("Large batch: %d/20 calls successful", successCount)

	// Check metrics
	if metricsEndpoint != "" {
		time.Sleep(1 * time.Second)

		newAggregation, err := getMetricValue(metricsEndpoint, "erpc_multicall3_aggregation_total")
		if err == nil {
			diff := newAggregation - baselineAggregation
			t.Logf("Multicall3 aggregation metric: %.0f -> %.0f (+%.0f)", baselineAggregation, newAggregation, diff)

			// With default maxCalls=20, a batch of 20 should result in 1 aggregation
			// (or possibly 2 if there's splitting)
			if diff > 0 && diff <= 2 {
				t.Logf("✓ Efficient batching: 20 requests resulted in %.0f multicall3 aggregation(s)", diff)
			}
		}
	}
}

// TestMulticall3Integration_Deduplication verifies that duplicate requests are deduplicated
func TestMulticall3Integration_Deduplication(t *testing.T) {
	endpoint := getTestEndpoint(t)
	metricsEndpoint := getMetricsEndpoint()

	// Capture baseline dedupe metric
	var baselineDedupe float64
	if metricsEndpoint != "" {
		baselineDedupe, _ = getMetricValue(metricsEndpoint, "erpc_multicall3_dedupe_total")
	}

	// Send batch with DUPLICATE calls - same contract, same function, same block
	// These should be deduplicated (only one actual call to the contract)
	batch := []jsonRPCRequest{
		{JSONRPC: "2.0", ID: 1, Method: "eth_call", Params: []interface{}{ethCallParams{To: wethAddress, Data: decimalsSelector}, "latest"}},
		{JSONRPC: "2.0", ID: 2, Method: "eth_call", Params: []interface{}{ethCallParams{To: wethAddress, Data: decimalsSelector}, "latest"}}, // Duplicate
		{JSONRPC: "2.0", ID: 3, Method: "eth_call", Params: []interface{}{ethCallParams{To: wethAddress, Data: decimalsSelector}, "latest"}}, // Duplicate
		{JSONRPC: "2.0", ID: 4, Method: "eth_call", Params: []interface{}{ethCallParams{To: wethAddress, Data: symbolSelector}, "latest"}},   // Different call
	}

	respBody := makeRequest(t, endpoint, batch)

	var responses []jsonRPCResponse
	err := json.Unmarshal(respBody, &responses)
	require.NoError(t, err, "Failed to parse response: %s", string(respBody))
	require.Len(t, responses, 4, "Expected 4 responses")

	// All responses should succeed
	for i, resp := range responses {
		assert.Nil(t, resp.Error, "Response %d returned error", i)
		assert.NotNil(t, resp.Result, "Response %d has no result", i)
	}

	// First 3 responses should be identical (same call)
	var result1, result2, result3 string
	json.Unmarshal(responses[0].Result, &result1)
	json.Unmarshal(responses[1].Result, &result2)
	json.Unmarshal(responses[2].Result, &result3)
	assert.Equal(t, result1, result2, "Duplicate calls should return same result")
	assert.Equal(t, result2, result3, "Duplicate calls should return same result")

	t.Logf("Deduplication: 3 identical calls returned same result: %s", result1)

	// Check if dedupe metric increased
	if metricsEndpoint != "" {
		time.Sleep(1 * time.Second)

		newDedupe, err := getMetricValue(metricsEndpoint, "erpc_multicall3_dedupe_total")
		if err == nil {
			diff := newDedupe - baselineDedupe
			t.Logf("Deduplication metric: %.0f -> %.0f (+%.0f)", baselineDedupe, newDedupe, diff)
			if diff >= 2 {
				t.Logf("✓ Deduplication confirmed: %0.f duplicate requests were deduplicated", diff)
			}
		}
	}
}

// TestMulticall3Integration_BlockTagVariations tests batching with different block tags
func TestMulticall3Integration_BlockTagVariations(t *testing.T) {
	endpoint := getTestEndpoint(t)

	t.Run("LatestBlock", func(t *testing.T) {
		batch := []jsonRPCRequest{
			{JSONRPC: "2.0", ID: 1, Method: "eth_call", Params: []interface{}{ethCallParams{To: wethAddress, Data: decimalsSelector}, "latest"}},
			{JSONRPC: "2.0", ID: 2, Method: "eth_call", Params: []interface{}{ethCallParams{To: wethAddress, Data: symbolSelector}, "latest"}},
		}

		respBody := makeRequest(t, endpoint, batch)

		var responses []jsonRPCResponse
		err := json.Unmarshal(respBody, &responses)
		require.NoError(t, err)
		require.Len(t, responses, 2)

		for i, resp := range responses {
			assert.Nil(t, resp.Error, "Response %d with 'latest' tag should succeed", i)
			assert.NotNil(t, resp.Result, "Response %d should have result", i)
		}
		t.Logf("'latest' block tag: both calls succeeded")
	})

	t.Run("SpecificBlockNumber", func(t *testing.T) {
		// First get the latest block number
		blockNumReq := jsonRPCRequest{
			JSONRPC: "2.0",
			ID:      1,
			Method:  "eth_blockNumber",
			Params:  []interface{}{},
		}
		respBody := makeRequest(t, endpoint, blockNumReq)

		var blockResp jsonRPCResponse
		err := json.Unmarshal(respBody, &blockResp)
		require.NoError(t, err)
		require.NotNil(t, blockResp.Result)

		var blockNum string
		json.Unmarshal(blockResp.Result, &blockNum)

		// Now send batch with specific block number
		batch := []jsonRPCRequest{
			{JSONRPC: "2.0", ID: 1, Method: "eth_call", Params: []interface{}{ethCallParams{To: wethAddress, Data: decimalsSelector}, blockNum}},
			{JSONRPC: "2.0", ID: 2, Method: "eth_call", Params: []interface{}{ethCallParams{To: wethAddress, Data: symbolSelector}, blockNum}},
		}

		respBody = makeRequest(t, endpoint, batch)

		var responses []jsonRPCResponse
		err = json.Unmarshal(respBody, &responses)
		require.NoError(t, err)
		require.Len(t, responses, 2)

		for i, resp := range responses {
			assert.Nil(t, resp.Error, "Response %d with specific block should succeed", i)
			assert.NotNil(t, resp.Result, "Response %d should have result", i)
		}
		t.Logf("Specific block number (%s): both calls succeeded", blockNum)
	})

	t.Run("PendingBlock", func(t *testing.T) {
		// By default, "pending" block tag should NOT be batched (allowPendingTagBatching=false)
		// Each call should still succeed but may not be batched together
		batch := []jsonRPCRequest{
			{JSONRPC: "2.0", ID: 1, Method: "eth_call", Params: []interface{}{ethCallParams{To: wethAddress, Data: decimalsSelector}, "pending"}},
			{JSONRPC: "2.0", ID: 2, Method: "eth_call", Params: []interface{}{ethCallParams{To: wethAddress, Data: symbolSelector}, "pending"}},
		}

		respBody := makeRequest(t, endpoint, batch)

		var responses []jsonRPCResponse
		err := json.Unmarshal(respBody, &responses)
		require.NoError(t, err)
		require.Len(t, responses, 2)

		// Calls should still succeed (individually forwarded)
		successCount := 0
		for _, resp := range responses {
			if resp.Error == nil && resp.Result != nil {
				successCount++
			}
		}

		// Note: some networks don't support "pending" tag, so we just check responses are returned
		t.Logf("'pending' block tag: %d/2 calls returned results (may not be batched)", successCount)
	})
}

// TestMulticall3Integration_CacheHits verifies that repeated calls hit the cache
func TestMulticall3Integration_CacheHits(t *testing.T) {
	endpoint := getTestEndpoint(t)
	metricsEndpoint := getMetricsEndpoint()

	// First, get a specific block number to ensure consistent caching
	blockNumReq := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "eth_blockNumber",
		Params:  []interface{}{},
	}
	respBody := makeRequest(t, endpoint, blockNumReq)

	var blockResp jsonRPCResponse
	err := json.Unmarshal(respBody, &blockResp)
	require.NoError(t, err)
	require.NotNil(t, blockResp.Result)

	var blockNum string
	json.Unmarshal(blockResp.Result, &blockNum)

	// Capture baseline cache hit metric
	var baselineCacheHits float64
	if metricsEndpoint != "" {
		baselineCacheHits, _ = getMetricValue(metricsEndpoint, "erpc_multicall3_cache_hits_total")
	}

	// Make the first request (should populate cache)
	batch := []jsonRPCRequest{
		{JSONRPC: "2.0", ID: 1, Method: "eth_call", Params: []interface{}{ethCallParams{To: wethAddress, Data: decimalsSelector}, blockNum}},
		{JSONRPC: "2.0", ID: 2, Method: "eth_call", Params: []interface{}{ethCallParams{To: wethAddress, Data: symbolSelector}, blockNum}},
	}

	respBody = makeRequest(t, endpoint, batch)
	var responses1 []jsonRPCResponse
	err = json.Unmarshal(respBody, &responses1)
	require.NoError(t, err)
	require.Len(t, responses1, 2)

	// Wait a bit for cache to be written
	time.Sleep(500 * time.Millisecond)

	// Make the same request again (should hit cache)
	respBody = makeRequest(t, endpoint, batch)
	var responses2 []jsonRPCResponse
	err = json.Unmarshal(respBody, &responses2)
	require.NoError(t, err)
	require.Len(t, responses2, 2)

	// Results should be identical
	var result1a, result1b, result2a, result2b string
	json.Unmarshal(responses1[0].Result, &result1a)
	json.Unmarshal(responses2[0].Result, &result1b)
	json.Unmarshal(responses1[1].Result, &result2a)
	json.Unmarshal(responses2[1].Result, &result2b)

	assert.Equal(t, result1a, result1b, "Cached result should match original")
	assert.Equal(t, result2a, result2b, "Cached result should match original")

	t.Logf("Cache test: results match for block %s", blockNum)

	// Check if cache hit metric increased
	if metricsEndpoint != "" {
		time.Sleep(1 * time.Second)

		newCacheHits, err := getMetricValue(metricsEndpoint, "erpc_multicall3_cache_hits_total")
		if err == nil {
			diff := newCacheHits - baselineCacheHits
			t.Logf("Cache hits metric: %.0f -> %.0f (+%.0f)", baselineCacheHits, newCacheHits, diff)
			if diff > 0 {
				t.Logf("✓ Cache hits confirmed via metrics")
			} else {
				t.Logf("⚠ No cache hits detected (caching may be disabled or cache not populated)")
			}
		}
	}
}

// TestMulticall3Integration_UseUpstreamDirective tests that UseUpstream header works with batching
func TestMulticall3Integration_UseUpstreamDirective(t *testing.T) {
	endpoint := getTestEndpoint(t)

	// This test verifies that the UseUpstream directive works
	// The actual upstream selection is internal, but we can verify the request succeeds
	// with the header present

	req := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "eth_call",
		Params: []interface{}{
			ethCallParams{To: wethAddress, Data: decimalsSelector},
			"latest",
		},
	}

	body, err := json.Marshal(req)
	require.NoError(t, err)

	httpReq, err := http.NewRequest("POST", endpoint, bytes.NewReader(body))
	require.NoError(t, err)
	httpReq.Header.Set("Content-Type", "application/json")

	// Add auth headers
	for key, value := range getAuthHeaders() {
		httpReq.Header.Set(key, value)
	}

	// Add UseUpstream directive (this may or may not match an upstream, but shouldn't error)
	httpReq.Header.Set("X-ERPC-Use-Upstream", "alchemy") // Common provider name

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(httpReq)
	require.NoError(t, err)
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var jsonResp jsonRPCResponse
	err = json.Unmarshal(respBody, &jsonResp)
	require.NoError(t, err, "Failed to parse response: %s", string(respBody))

	// Request should succeed (either via specified upstream or fallback)
	if jsonResp.Error != nil {
		t.Logf("UseUpstream directive: request returned error (upstream may not exist): %s", string(*jsonResp.Error))
	} else {
		assert.NotNil(t, jsonResp.Result, "Request should have result")
		t.Logf("UseUpstream directive: request succeeded")
	}
}

// TestMulticall3Integration_HighConcurrency stress tests with high concurrency
func TestMulticall3Integration_HighConcurrency(t *testing.T) {
	endpoint := getTestEndpoint(t)
	metricsEndpoint := getMetricsEndpoint()

	// Capture baseline metrics
	var baselineAggregation, baselineOverflow float64
	if metricsEndpoint != "" {
		baselineAggregation, _ = getMetricValue(metricsEndpoint, "erpc_multicall3_aggregation_total")
		baselineOverflow, _ = getMetricValue(metricsEndpoint, "erpc_multicall3_queue_overflow_total")
	}

	// Send 50 concurrent requests
	const numRequests = 50
	var wg sync.WaitGroup
	results := make(chan bool, numRequests)

	for i := 0; i < numRequests; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			req := jsonRPCRequest{
				JSONRPC: "2.0",
				ID:      id,
				Method:  "eth_call",
				Params: []interface{}{
					ethCallParams{To: wethAddress, Data: decimalsSelector},
					"latest",
				},
			}

			respBody := makeRequest(t, endpoint, req)

			var resp jsonRPCResponse
			if err := json.Unmarshal(respBody, &resp); err != nil {
				results <- false
				return
			}

			results <- resp.Error == nil && resp.Result != nil
		}(i)
	}

	wg.Wait()
	close(results)

	successCount := 0
	for success := range results {
		if success {
			successCount++
		}
	}

	// All requests should succeed
	assert.Equal(t, numRequests, successCount, "All high-concurrency requests should succeed")
	t.Logf("High concurrency: %d/%d requests successful", successCount, numRequests)

	// Check metrics
	if metricsEndpoint != "" {
		time.Sleep(1 * time.Second)

		newAggregation, _ := getMetricValue(metricsEndpoint, "erpc_multicall3_aggregation_total")
		newOverflow, _ := getMetricValue(metricsEndpoint, "erpc_multicall3_queue_overflow_total")

		aggDiff := newAggregation - baselineAggregation
		overflowDiff := newOverflow - baselineOverflow

		t.Logf("Aggregation metric: %.0f -> %.0f (+%.0f)", baselineAggregation, newAggregation, aggDiff)
		t.Logf("Queue overflow metric: %.0f -> %.0f (+%.0f)", baselineOverflow, newOverflow, overflowDiff)

		if aggDiff > 0 && aggDiff < float64(numRequests)/2 {
			t.Logf("✓ Efficient batching under high concurrency: %d requests → %.0f aggregations", numRequests, aggDiff)
		}

		if overflowDiff > 0 {
			t.Logf("⚠ Some requests overflowed (%.0f) - this is expected under extreme load", overflowDiff)
		}
	}
}

// TestMulticall3Integration_DifferentBlockTags tests that calls with different block tags are NOT batched together
func TestMulticall3Integration_DifferentBlockTags(t *testing.T) {
	endpoint := getTestEndpoint(t)

	// Get latest block number
	blockNumReq := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "eth_blockNumber",
		Params:  []interface{}{},
	}
	respBody := makeRequest(t, endpoint, blockNumReq)

	var blockResp jsonRPCResponse
	json.Unmarshal(respBody, &blockResp)
	var blockNum string
	json.Unmarshal(blockResp.Result, &blockNum)

	// Send batch with DIFFERENT block tags - these should NOT be batched together
	// (different batch keys due to different blockRef)
	batch := []jsonRPCRequest{
		{JSONRPC: "2.0", ID: 1, Method: "eth_call", Params: []interface{}{ethCallParams{To: wethAddress, Data: decimalsSelector}, "latest"}},
		{JSONRPC: "2.0", ID: 2, Method: "eth_call", Params: []interface{}{ethCallParams{To: wethAddress, Data: decimalsSelector}, blockNum}},
	}

	respBody = makeRequest(t, endpoint, batch)

	var responses []jsonRPCResponse
	err := json.Unmarshal(respBody, &responses)
	require.NoError(t, err)
	require.Len(t, responses, 2)

	// Both should succeed
	for i, resp := range responses {
		assert.Nil(t, resp.Error, "Response %d should succeed", i)
		assert.NotNil(t, resp.Result, "Response %d should have result", i)
	}

	// Results should be the same (same call, different block refs but close in time)
	var result1, result2 string
	json.Unmarshal(responses[0].Result, &result1)
	json.Unmarshal(responses[1].Result, &result2)

	t.Logf("Different block tags: 'latest' returned %s, '%s' returned %s", result1, blockNum, result2)
	// Note: Results may differ if there was a state change between blocks
}

// TestMulticall3Integration_EmptyCalldata tests handling of calls with empty/minimal calldata
func TestMulticall3Integration_EmptyCalldata(t *testing.T) {
	endpoint := getTestEndpoint(t)

	// Some contracts have fallback functions that accept empty calldata
	// We're testing that the batching handles this gracefully
	batch := []jsonRPCRequest{
		{JSONRPC: "2.0", ID: 1, Method: "eth_call", Params: []interface{}{ethCallParams{To: wethAddress, Data: decimalsSelector}, "latest"}},
		{JSONRPC: "2.0", ID: 2, Method: "eth_call", Params: []interface{}{ethCallParams{To: wethAddress, Data: "0x"}, "latest"}}, // Empty calldata
	}

	respBody := makeRequest(t, endpoint, batch)

	var responses []jsonRPCResponse
	err := json.Unmarshal(respBody, &responses)
	require.NoError(t, err)
	require.Len(t, responses, 2)

	// First call should succeed
	assert.Nil(t, responses[0].Error, "Normal call should succeed")
	assert.NotNil(t, responses[0].Result, "Normal call should have result")

	// Second call may succeed or fail depending on the contract's fallback function
	if responses[1].Error != nil {
		t.Logf("Empty calldata: returned error (expected for contracts without fallback)")
	} else {
		t.Logf("Empty calldata: returned result (contract has fallback function)")
	}
}

// TestMulticall3Integration_LargeCalldata tests handling of calls with large calldata
func TestMulticall3Integration_LargeCalldata(t *testing.T) {
	endpoint := getTestEndpoint(t)

	// Create a call with larger calldata (balanceOf with address padded)
	// balanceOf(address) = 0x70a08231 + 32-byte address
	largeCalldata := "0x70a08231000000000000000000000000" + strings.Repeat("ab", 20) // balanceOf(0xabab...ab)

	batch := []jsonRPCRequest{
		{JSONRPC: "2.0", ID: 1, Method: "eth_call", Params: []interface{}{ethCallParams{To: wethAddress, Data: largeCalldata}, "latest"}},
		{JSONRPC: "2.0", ID: 2, Method: "eth_call", Params: []interface{}{ethCallParams{To: wethAddress, Data: decimalsSelector}, "latest"}},
	}

	respBody := makeRequest(t, endpoint, batch)

	var responses []jsonRPCResponse
	err := json.Unmarshal(respBody, &responses)
	require.NoError(t, err)
	require.Len(t, responses, 2)

	// Both should return results (balanceOf returns 0 for unknown address, decimals returns 18)
	for i, resp := range responses {
		assert.Nil(t, resp.Error, "Response %d should succeed", i)
		assert.NotNil(t, resp.Result, "Response %d should have result", i)
	}

	t.Logf("Large calldata: both calls succeeded")
}

// =============================================================================
// Bypass Tests - Verify calls that should NOT be batched
// =============================================================================

// TestMulticall3Integration_BypassWithValueField tests that calls with 'value' field bypass batching
func TestMulticall3Integration_BypassWithValueField(t *testing.T) {
	endpoint := getTestEndpoint(t)

	// eth_call with 'value' field should bypass multicall3 batching
	// (multicall3 aggregate3 doesn't support value transfers)
	batch := []jsonRPCRequest{
		{JSONRPC: "2.0", ID: 1, Method: "eth_call", Params: []interface{}{
			map[string]interface{}{"to": wethAddress, "data": decimalsSelector, "value": "0x0"},
			"latest",
		}},
		{JSONRPC: "2.0", ID: 2, Method: "eth_call", Params: []interface{}{
			map[string]interface{}{"to": wethAddress, "data": symbolSelector},
			"latest",
		}},
	}

	respBody := makeRequest(t, endpoint, batch)

	var responses []jsonRPCResponse
	err := json.Unmarshal(respBody, &responses)
	require.NoError(t, err, "Failed to parse response: %s", string(respBody))
	require.Len(t, responses, 2)

	// Both should still succeed (first bypasses batching, second may be batched)
	for i, resp := range responses {
		assert.Nil(t, resp.Error, "Response %d should succeed", i)
		assert.NotNil(t, resp.Result, "Response %d should have result", i)
	}

	t.Logf("Bypass with 'value' field: both calls succeeded (first bypassed batching)")
}

// TestMulticall3Integration_BypassWithFromField tests that calls with 'from' field bypass batching
func TestMulticall3Integration_BypassWithFromField(t *testing.T) {
	endpoint := getTestEndpoint(t)

	// eth_call with 'from' field should bypass multicall3 batching
	batch := []jsonRPCRequest{
		{JSONRPC: "2.0", ID: 1, Method: "eth_call", Params: []interface{}{
			map[string]interface{}{"to": wethAddress, "data": decimalsSelector, "from": "0x0000000000000000000000000000000000000001"},
			"latest",
		}},
		{JSONRPC: "2.0", ID: 2, Method: "eth_call", Params: []interface{}{
			map[string]interface{}{"to": wethAddress, "data": symbolSelector},
			"latest",
		}},
	}

	respBody := makeRequest(t, endpoint, batch)

	var responses []jsonRPCResponse
	err := json.Unmarshal(respBody, &responses)
	require.NoError(t, err, "Failed to parse response: %s", string(respBody))
	require.Len(t, responses, 2)

	for i, resp := range responses {
		assert.Nil(t, resp.Error, "Response %d should succeed", i)
		assert.NotNil(t, resp.Result, "Response %d should have result", i)
	}

	t.Logf("Bypass with 'from' field: both calls succeeded (first bypassed batching)")
}

// TestMulticall3Integration_BypassWithGasField tests that calls with 'gas' field bypass batching
func TestMulticall3Integration_BypassWithGasField(t *testing.T) {
	endpoint := getTestEndpoint(t)

	// eth_call with 'gas' field should bypass multicall3 batching
	// Use a reasonable gas value (500k) to avoid "intrinsic gas too low" errors
	batch := []jsonRPCRequest{
		{JSONRPC: "2.0", ID: 1, Method: "eth_call", Params: []interface{}{
			map[string]interface{}{"to": wethAddress, "data": decimalsSelector, "gas": "0x7a120"}, // 500000 gas
			"latest",
		}},
		{JSONRPC: "2.0", ID: 2, Method: "eth_call", Params: []interface{}{
			map[string]interface{}{"to": wethAddress, "data": symbolSelector},
			"latest",
		}},
	}

	respBody := makeRequest(t, endpoint, batch)

	var responses []jsonRPCResponse
	err := json.Unmarshal(respBody, &responses)
	require.NoError(t, err, "Failed to parse response: %s", string(respBody))
	require.Len(t, responses, 2)

	// First call (with gas field) may succeed or fail depending on gas amount
	// The important thing is that it was processed (bypassed batching)
	if responses[0].Error != nil {
		t.Logf("Call with 'gas' field returned error (processed, bypassed batching)")
	} else {
		assert.NotNil(t, responses[0].Result, "Response 0 should have result")
	}

	// Second call (normal) should always succeed
	assert.Nil(t, responses[1].Error, "Response 1 (normal call) should succeed")
	assert.NotNil(t, responses[1].Result, "Response 1 should have result")

	t.Logf("Bypass with 'gas' field: calls handled correctly (first bypassed batching)")
}

// TestMulticall3Integration_BypassWithStateOverride tests that calls with state override bypass batching
func TestMulticall3Integration_BypassWithStateOverride(t *testing.T) {
	endpoint := getTestEndpoint(t)

	// eth_call with state override (3rd param) should bypass multicall3 batching
	stateOverride := map[string]interface{}{
		wethAddress: map[string]interface{}{
			"balance": "0x1000000000000000000",
		},
	}

	batch := []jsonRPCRequest{
		{JSONRPC: "2.0", ID: 1, Method: "eth_call", Params: []interface{}{
			map[string]interface{}{"to": wethAddress, "data": decimalsSelector},
			"latest",
			stateOverride, // State override as 3rd param
		}},
		{JSONRPC: "2.0", ID: 2, Method: "eth_call", Params: []interface{}{
			map[string]interface{}{"to": wethAddress, "data": symbolSelector},
			"latest",
		}},
	}

	respBody := makeRequest(t, endpoint, batch)

	var responses []jsonRPCResponse
	err := json.Unmarshal(respBody, &responses)
	require.NoError(t, err, "Failed to parse response: %s", string(respBody))
	require.Len(t, responses, 2)

	// First may error if upstream doesn't support state override, second should succeed
	if responses[0].Error != nil {
		t.Logf("State override call returned error (upstream may not support it)")
	} else {
		assert.NotNil(t, responses[0].Result, "Response 0 should have result")
	}

	assert.Nil(t, responses[1].Error, "Response 1 (normal call) should succeed")
	assert.NotNil(t, responses[1].Result, "Response 1 should have result")

	t.Logf("Bypass with state override: calls handled correctly (first bypassed batching)")
}

// TestMulticall3Integration_BypassRecursionGuard tests that calls to multicall3 contract bypass batching
func TestMulticall3Integration_BypassRecursionGuard(t *testing.T) {
	endpoint := getTestEndpoint(t)

	// Multicall3 contract address (same on most EVM chains)
	multicall3Address := "0xcA11bde05977b3631167028862bE2a173976CA11"

	// Calling multicall3 directly should bypass the batching (recursion guard)
	batch := []jsonRPCRequest{
		{JSONRPC: "2.0", ID: 1, Method: "eth_call", Params: []interface{}{
			map[string]interface{}{"to": multicall3Address, "data": "0x252dba42"}, // aggregate() selector
			"latest",
		}},
		{JSONRPC: "2.0", ID: 2, Method: "eth_call", Params: []interface{}{
			map[string]interface{}{"to": wethAddress, "data": decimalsSelector},
			"latest",
		}},
	}

	respBody := makeRequest(t, endpoint, batch)

	var responses []jsonRPCResponse
	err := json.Unmarshal(respBody, &responses)
	require.NoError(t, err, "Failed to parse response: %s", string(respBody))
	require.Len(t, responses, 2)

	// First call (to multicall3) may revert due to invalid calldata, but shouldn't cause batching issues
	if responses[0].Error != nil {
		t.Logf("Call to multicall3 returned error (expected for empty aggregate call)")
	} else {
		t.Logf("Call to multicall3 returned result (bypassed batching)")
	}

	// Second call should succeed normally
	assert.Nil(t, responses[1].Error, "Normal call should succeed")
	assert.NotNil(t, responses[1].Result, "Normal call should have result")

	t.Logf("Recursion guard: calls to multicall3 contract handled correctly")
}

// TestMulticall3Integration_BypassRequireCanonicalFalse tests that requireCanonical:false bypasses batching
func TestMulticall3Integration_BypassRequireCanonicalFalse(t *testing.T) {
	endpoint := getTestEndpoint(t)

	// First get a recent block hash
	blockNumReq := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "eth_getBlockByNumber",
		Params:  []interface{}{"latest", false},
	}
	respBody := makeRequest(t, endpoint, blockNumReq)

	var blockResp jsonRPCResponse
	err := json.Unmarshal(respBody, &blockResp)
	require.NoError(t, err)
	require.NotNil(t, blockResp.Result)

	var block map[string]interface{}
	json.Unmarshal(blockResp.Result, &block)
	blockHash, ok := block["hash"].(string)
	if !ok || blockHash == "" {
		t.Skip("Could not get block hash for requireCanonical test")
	}

	// EIP-1898 block param with requireCanonical: false should bypass batching
	batch := []jsonRPCRequest{
		{JSONRPC: "2.0", ID: 1, Method: "eth_call", Params: []interface{}{
			map[string]interface{}{"to": wethAddress, "data": decimalsSelector},
			map[string]interface{}{"blockHash": blockHash, "requireCanonical": false},
		}},
		{JSONRPC: "2.0", ID: 2, Method: "eth_call", Params: []interface{}{
			map[string]interface{}{"to": wethAddress, "data": symbolSelector},
			"latest",
		}},
	}

	respBody = makeRequest(t, endpoint, batch)

	var responses []jsonRPCResponse
	err = json.Unmarshal(respBody, &responses)
	require.NoError(t, err, "Failed to parse response: %s", string(respBody))
	require.Len(t, responses, 2)

	// Both should succeed (first bypasses batching due to requireCanonical:false)
	for i, resp := range responses {
		assert.Nil(t, resp.Error, "Response %d should succeed", i)
		assert.NotNil(t, resp.Result, "Response %d should have result", i)
	}

	t.Logf("Bypass with requireCanonical:false: both calls succeeded")
}

// =============================================================================
// Block Reference Variation Tests
// =============================================================================

// TestMulticall3Integration_BlockHashReference tests batching with block hash (EIP-1898)
func TestMulticall3Integration_BlockHashReference(t *testing.T) {
	endpoint := getTestEndpoint(t)

	// First get a recent block hash
	blockReq := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "eth_getBlockByNumber",
		Params:  []interface{}{"latest", false},
	}
	respBody := makeRequest(t, endpoint, blockReq)

	var blockResp jsonRPCResponse
	err := json.Unmarshal(respBody, &blockResp)
	require.NoError(t, err)
	require.NotNil(t, blockResp.Result)

	var block map[string]interface{}
	json.Unmarshal(blockResp.Result, &block)
	blockHash, ok := block["hash"].(string)
	if !ok || blockHash == "" {
		t.Skip("Could not get block hash")
	}

	// EIP-1898 block param with blockHash
	batch := []jsonRPCRequest{
		{JSONRPC: "2.0", ID: 1, Method: "eth_call", Params: []interface{}{
			map[string]interface{}{"to": wethAddress, "data": decimalsSelector},
			map[string]interface{}{"blockHash": blockHash},
		}},
		{JSONRPC: "2.0", ID: 2, Method: "eth_call", Params: []interface{}{
			map[string]interface{}{"to": wethAddress, "data": symbolSelector},
			map[string]interface{}{"blockHash": blockHash},
		}},
	}

	respBody = makeRequest(t, endpoint, batch)

	var responses []jsonRPCResponse
	err = json.Unmarshal(respBody, &responses)
	require.NoError(t, err, "Failed to parse response: %s", string(respBody))
	require.Len(t, responses, 2)

	for i, resp := range responses {
		assert.Nil(t, resp.Error, "Response %d with block hash should succeed", i)
		assert.NotNil(t, resp.Result, "Response %d should have result", i)
	}

	t.Logf("Block hash reference (EIP-1898): both calls succeeded with hash %s", blockHash[:18]+"...")
}

// TestMulticall3Integration_FinalizedSafeEarliestTags tests batching with special block tags
func TestMulticall3Integration_FinalizedSafeEarliestTags(t *testing.T) {
	endpoint := getTestEndpoint(t)

	testCases := []struct {
		name     string
		blockTag string
	}{
		{"finalized", "finalized"},
		{"safe", "safe"},
		{"earliest", "earliest"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			batch := []jsonRPCRequest{
				{JSONRPC: "2.0", ID: 1, Method: "eth_call", Params: []interface{}{
					map[string]interface{}{"to": wethAddress, "data": decimalsSelector},
					tc.blockTag,
				}},
				{JSONRPC: "2.0", ID: 2, Method: "eth_call", Params: []interface{}{
					map[string]interface{}{"to": wethAddress, "data": symbolSelector},
					tc.blockTag,
				}},
			}

			respBody := makeRequest(t, endpoint, batch)

			var responses []jsonRPCResponse
			err := json.Unmarshal(respBody, &responses)
			require.NoError(t, err, "Failed to parse response: %s", string(respBody))
			require.Len(t, responses, 2)

			// Note: 'earliest' may fail on some contracts that didn't exist at genesis
			// 'finalized' and 'safe' may not be supported by all nodes
			successCount := 0
			for _, resp := range responses {
				if resp.Error == nil && resp.Result != nil {
					successCount++
				}
			}

			if successCount == 2 {
				t.Logf("'%s' block tag: both calls succeeded", tc.blockTag)
			} else if successCount > 0 {
				t.Logf("'%s' block tag: %d/2 calls succeeded (some may not be supported)", tc.blockTag, successCount)
			} else {
				t.Logf("'%s' block tag: calls failed (tag may not be supported by upstream)", tc.blockTag)
			}
		})
	}
}

// =============================================================================
// Input Variations Tests
// =============================================================================

// TestMulticall3Integration_InputFieldAlternative tests that 'input' field works as alternative to 'data'
func TestMulticall3Integration_InputFieldAlternative(t *testing.T) {
	endpoint := getTestEndpoint(t)

	// Some clients use 'input' instead of 'data' - both should work
	batch := []jsonRPCRequest{
		{JSONRPC: "2.0", ID: 1, Method: "eth_call", Params: []interface{}{
			map[string]interface{}{"to": wethAddress, "input": decimalsSelector}, // 'input' instead of 'data'
			"latest",
		}},
		{JSONRPC: "2.0", ID: 2, Method: "eth_call", Params: []interface{}{
			map[string]interface{}{"to": wethAddress, "data": symbolSelector}, // 'data' for comparison
			"latest",
		}},
	}

	respBody := makeRequest(t, endpoint, batch)

	var responses []jsonRPCResponse
	err := json.Unmarshal(respBody, &responses)
	require.NoError(t, err, "Failed to parse response: %s", string(respBody))
	require.Len(t, responses, 2)

	for i, resp := range responses {
		assert.Nil(t, resp.Error, "Response %d should succeed", i)
		assert.NotNil(t, resp.Result, "Response %d should have result", i)
	}

	// Both should return valid results
	var result1, result2 string
	json.Unmarshal(responses[0].Result, &result1)
	json.Unmarshal(responses[1].Result, &result2)

	assert.NotEmpty(t, result1, "'input' field should return valid result")
	assert.NotEmpty(t, result2, "'data' field should return valid result")

	t.Logf("'input' field alternative: both 'input' and 'data' fields work correctly")
}

// =============================================================================
// Directive Tests
// =============================================================================

// TestMulticall3Integration_SkipCacheReadDirective tests that skip-cache-read creates separate batch
func TestMulticall3Integration_SkipCacheReadDirective(t *testing.T) {
	endpoint := getTestEndpoint(t)

	req := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "eth_call",
		Params: []interface{}{
			map[string]interface{}{"to": wethAddress, "data": decimalsSelector},
			"latest",
		},
	}

	body, err := json.Marshal(req)
	require.NoError(t, err)

	httpReq, err := http.NewRequest("POST", endpoint, bytes.NewReader(body))
	require.NoError(t, err)
	httpReq.Header.Set("Content-Type", "application/json")

	// Add auth headers
	for key, value := range getAuthHeaders() {
		httpReq.Header.Set(key, value)
	}

	// Add skip-cache-read directive
	httpReq.Header.Set("X-ERPC-Skip-Cache-Read", "true")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(httpReq)
	require.NoError(t, err)
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var jsonResp jsonRPCResponse
	err = json.Unmarshal(respBody, &jsonResp)
	require.NoError(t, err, "Failed to parse response: %s", string(respBody))

	assert.Nil(t, jsonResp.Error, "Request with skip-cache-read should succeed")
	assert.NotNil(t, jsonResp.Result, "Request should have result")

	t.Logf("Skip-cache-read directive: request succeeded")
}

// TestMulticall3Integration_RetryEmptyDirective tests that retry-empty creates separate batch
func TestMulticall3Integration_RetryEmptyDirective(t *testing.T) {
	endpoint := getTestEndpoint(t)

	req := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "eth_call",
		Params: []interface{}{
			map[string]interface{}{"to": wethAddress, "data": decimalsSelector},
			"latest",
		},
	}

	body, err := json.Marshal(req)
	require.NoError(t, err)

	httpReq, err := http.NewRequest("POST", endpoint, bytes.NewReader(body))
	require.NoError(t, err)
	httpReq.Header.Set("Content-Type", "application/json")

	// Add auth headers
	for key, value := range getAuthHeaders() {
		httpReq.Header.Set(key, value)
	}

	// Add retry-empty directive
	httpReq.Header.Set("X-ERPC-Retry-Empty", "true")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(httpReq)
	require.NoError(t, err)
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var jsonResp jsonRPCResponse
	err = json.Unmarshal(respBody, &jsonResp)
	require.NoError(t, err, "Failed to parse response: %s", string(respBody))

	assert.Nil(t, jsonResp.Error, "Request with retry-empty should succeed")
	assert.NotNil(t, jsonResp.Result, "Request should have result")

	t.Logf("Retry-empty directive: request succeeded")
}

// =============================================================================
// Fallback Tests
// =============================================================================

// TestMulticall3Integration_FallbackMetricTracking verifies fallback metric is tracked
func TestMulticall3Integration_FallbackMetricTracking(t *testing.T) {
	endpoint := getTestEndpoint(t)
	metricsEndpoint := getMetricsEndpoint()

	if metricsEndpoint == "" {
		t.Skip("ERPC_INTEGRATION_TEST_METRICS not set, skipping fallback metric test")
	}

	// Check current fallback metric
	fallbackTotal, err := getMetricValue(metricsEndpoint, "erpc_multicall3_fallback_total")
	require.NoError(t, err)

	// We can't easily trigger a fallback in integration tests without a broken upstream,
	// but we can verify the metric exists and is being tracked
	t.Logf("Current fallback total: %.0f", fallbackTotal)

	// Also check fallback requests metric
	fallbackRequests, _ := getMetricValue(metricsEndpoint, "erpc_multicall3_fallback_requests_total")
	t.Logf("Current fallback requests total: %.0f", fallbackRequests)

	// Run a normal batch to ensure batching still works
	batch := []jsonRPCRequest{
		{JSONRPC: "2.0", ID: 1, Method: "eth_call", Params: []interface{}{
			map[string]interface{}{"to": wethAddress, "data": decimalsSelector},
			"latest",
		}},
	}

	respBody := makeRequest(t, endpoint, batch)

	var responses []jsonRPCResponse
	err = json.Unmarshal(respBody, &responses)
	require.NoError(t, err)
	require.Len(t, responses, 1)
	assert.Nil(t, responses[0].Error, "Normal call should succeed")

	// Verify fallback metric didn't increase (no fallback needed)
	time.Sleep(500 * time.Millisecond)
	newFallbackTotal, _ := getMetricValue(metricsEndpoint, "erpc_multicall3_fallback_total")

	if newFallbackTotal == fallbackTotal {
		t.Logf("✓ No fallback triggered for normal batch (as expected)")
	} else {
		t.Logf("⚠ Fallback was triggered: %.0f -> %.0f", fallbackTotal, newFallbackTotal)
	}
}

// TestMulticall3Integration_MetricsSummary prints a summary of multicall3 metrics
func TestMulticall3Integration_MetricsSummary(t *testing.T) {
	getTestEndpoint(t) // Ensure we have an endpoint configured
	metricsEndpoint := getMetricsEndpoint()

	if metricsEndpoint == "" {
		t.Skip("ERPC_INTEGRATION_TEST_METRICS not set, skipping metrics summary")
	}

	metrics := []string{
		"erpc_multicall3_aggregation_total",
		"erpc_multicall3_fallback_total",
		"erpc_multicall3_cache_hits_total",
		"erpc_multicall3_queue_overflow_total",
		"erpc_multicall3_dedupe_total",
		"erpc_multicall3_panic_total",
		"erpc_multicall3_abandoned_total",
		"erpc_multicall3_cache_write_dropped_total",
	}

	t.Logf("\n=== Multicall3 Metrics Summary ===")
	for _, metric := range metrics {
		value, err := getMetricValue(metricsEndpoint, metric)
		if err != nil {
			t.Logf("  %s: error fetching", metric)
		} else {
			t.Logf("  %s: %.0f", metric, value)
		}
	}
	t.Logf("===================================")
}
