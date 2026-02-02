// Package integration contains integration tests that run against live services.
//
// SQD wrapper tests are skipped unless SQD_WRAPPER_E2E_ENDPOINT is set.
//
// Environment variables:
//   - SQD_WRAPPER_E2E_ENDPOINT: wrapper JSON-RPC endpoint (required)
//   - SQD_WRAPPER_E2E_CHAIN_ID: chain id for X-Chain-Id header (optional, default 1)
//   - SQD_WRAPPER_E2E_AUTH: auth headers "Header: value" format, ";" separated (optional)
//
// Usage:
//
//	SQD_WRAPPER_E2E_ENDPOINT=http://localhost:8080 \
//	  SQD_WRAPPER_E2E_CHAIN_ID=1 \
//	  go test -v -run TestSqdPortalWrapper_Methods ./test/integration/...
package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type sqdJsonRPCRequest struct {
	JSONRPC string        `json:"jsonrpc"`
	ID      interface{}   `json:"id"`
	Method  string        `json:"method"`
	Params  []interface{} `json:"params"`
}

type sqdJsonRPCResponse struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      interface{}      `json:"id"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *json.RawMessage `json:"error,omitempty"`
}

type sqdRpcResponse struct {
	Status int
	Body   []byte
	Parsed sqdJsonRPCResponse
}

type sqdRpcError struct {
	Code    int                    `json:"code"`
	Message string                 `json:"message"`
	Data    map[string]interface{} `json:"data"`
}

type sqdCapabilitiesResponse struct {
	Methods []string `json:"methods"`
}

func getWrapperEndpoint(t *testing.T) string {
	endpoint := strings.TrimSpace(os.Getenv("SQD_WRAPPER_E2E_ENDPOINT"))
	if endpoint == "" {
		t.Skip("SQD_WRAPPER_E2E_ENDPOINT not set, skipping SQD wrapper integration test")
	}
	return endpoint
}

func getWrapperChainID() string {
	chainID := strings.TrimSpace(os.Getenv("SQD_WRAPPER_E2E_CHAIN_ID"))
	if chainID == "" {
		return "1"
	}
	return chainID
}

func getWrapperAuthHeaders() map[string]string {
	headers := make(map[string]string)
	raw := os.Getenv("SQD_WRAPPER_E2E_AUTH")
	if raw == "" {
		return headers
	}
	parts := strings.Split(raw, ";")
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

func wrapperBaseURL(endpoint string) string {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme == "" {
		return "http://" + strings.TrimRight(endpoint, "/")
	}
	if parsed.Host == "" {
		return strings.TrimRight(endpoint, "/")
	}
	return fmt.Sprintf("%s://%s", parsed.Scheme, parsed.Host)
}

func fetchWrapperCapabilities(t *testing.T, endpoint string) map[string]bool {
	t.Helper()

	base := wrapperBaseURL(endpoint)
	req, err := http.NewRequest("GET", base+"/capabilities", nil)
	require.NoError(t, err)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, "capabilities status %d: %s", resp.StatusCode, string(data))

	var parsed sqdCapabilitiesResponse
	require.NoError(t, json.Unmarshal(data, &parsed))

	methods := make(map[string]bool, len(parsed.Methods))
	for _, method := range parsed.Methods {
		methods[method] = true
	}
	return methods
}

func makeWrapperRequest(t *testing.T, endpoint string, payload interface{}) sqdRpcResponse {
	t.Helper()

	body, err := json.Marshal(payload)
	require.NoError(t, err)

	req, err := http.NewRequest("POST", endpoint, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Chain-Id", getWrapperChainID())

	for key, value := range getWrapperAuthHeaders() {
		req.Header.Set(key, value)
	}

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var parsed sqdJsonRPCResponse
	require.NoError(t, json.Unmarshal(data, &parsed))
	return sqdRpcResponse{
		Status: resp.StatusCode,
		Body:   data,
		Parsed: parsed,
	}
}

func parseHexInt64(t *testing.T, value string) int64 {
	t.Helper()
	require.True(t, strings.HasPrefix(value, "0x"), "expected hex value, got %q", value)
	parsed, err := strconv.ParseInt(strings.TrimPrefix(value, "0x"), 16, 64)
	require.NoError(t, err, fmt.Sprintf("invalid hex value: %q", value))
	return parsed
}

func skipIfPortalMissingField(t *testing.T, method string, resp sqdRpcResponse) bool {
	t.Helper()

	if resp.Status == http.StatusOK {
		return false
	}
	if resp.Parsed.Error == nil {
		return false
	}
	var rpcErr sqdRpcError
	if err := json.Unmarshal(*resp.Parsed.Error, &rpcErr); err != nil {
		return false
	}
	if strings.Contains(rpcErr.Message, "portal does not support required field") {
		t.Skipf("%s skipped: %s (configure upstream or update portal)", method, rpcErr.Message)
		return true
	}
	return false
}

func TestSqdPortalWrapper_Methods(t *testing.T) {
	endpoint := getWrapperEndpoint(t)
	capabilities := fetchWrapperCapabilities(t, endpoint)

	baseMethods := []string{
		"eth_chainId",
		"eth_blockNumber",
		"eth_getBlockByNumber",
		"eth_getTransactionByBlockNumberAndIndex",
		"eth_getLogs",
		"trace_block",
	}
	upstreamMethods := []string{
		"eth_getBlockByHash",
		"eth_getTransactionByHash",
		"eth_getTransactionReceipt",
		"trace_transaction",
	}

	for _, method := range baseMethods {
		if !capabilities[method] {
			t.Fatalf("capabilities missing base method %s", method)
		}
	}

	t.Run("eth_chainId", func(t *testing.T) {
		resp := makeWrapperRequest(t, endpoint, sqdJsonRPCRequest{
			JSONRPC: "2.0",
			ID:      1,
			Method:  "eth_chainId",
			Params:  []interface{}{},
		})
		if resp.Status != http.StatusOK {
			t.Fatalf("unexpected status %d: %s", resp.Status, string(resp.Body))
		}
		if resp.Parsed.Error != nil {
			t.Fatalf("rpc error: %s", string(*resp.Parsed.Error))
		}
		var chainIDHex string
		require.NoError(t, json.Unmarshal(resp.Parsed.Result, &chainIDHex))
		parseHexInt64(t, chainIDHex)
	})

	t.Run("eth_blockNumber", func(t *testing.T) {
		resp := makeWrapperRequest(t, endpoint, sqdJsonRPCRequest{
			JSONRPC: "2.0",
			ID:      2,
			Method:  "eth_blockNumber",
			Params:  []interface{}{},
		})
		if resp.Status != http.StatusOK {
			t.Fatalf("unexpected status %d: %s", resp.Status, string(resp.Body))
		}
		if resp.Parsed.Error != nil {
			t.Fatalf("rpc error: %s", string(*resp.Parsed.Error))
		}
		var blockNumberHex string
		require.NoError(t, json.Unmarshal(resp.Parsed.Result, &blockNumberHex))
		parseHexInt64(t, blockNumberHex)
	})

	var blockHash string
	var txHash string
	t.Run("eth_getBlockByNumber", func(t *testing.T) {
		resp := makeWrapperRequest(t, endpoint, sqdJsonRPCRequest{
			JSONRPC: "2.0",
			ID:      3,
			Method:  "eth_getBlockByNumber",
			Params:  []interface{}{"latest", false},
		})
		if skipIfPortalMissingField(t, "eth_getBlockByNumber", resp) {
			return
		}
		if resp.Status != http.StatusOK {
			t.Fatalf("unexpected status %d: %s", resp.Status, string(resp.Body))
		}
		if resp.Parsed.Error != nil {
			t.Fatalf("rpc error: %s", string(*resp.Parsed.Error))
		}
		if string(resp.Parsed.Result) == "null" {
			t.Skip("no block returned for latest")
		}
		var block map[string]interface{}
		require.NoError(t, json.Unmarshal(resp.Parsed.Result, &block))
		if hash, ok := block["hash"].(string); ok {
			blockHash = hash
		}
		if txs, ok := block["transactions"].([]interface{}); ok && len(txs) > 0 {
			if first, ok := txs[0].(string); ok {
				txHash = first
			}
		}
	})

	t.Run("eth_getTransactionByBlockNumberAndIndex", func(t *testing.T) {
		resp := makeWrapperRequest(t, endpoint, sqdJsonRPCRequest{
			JSONRPC: "2.0",
			ID:      4,
			Method:  "eth_getTransactionByBlockNumberAndIndex",
			Params:  []interface{}{"latest", "0x0"},
		})
		if skipIfPortalMissingField(t, "eth_getTransactionByBlockNumberAndIndex", resp) {
			return
		}
		if resp.Status != http.StatusOK {
			t.Fatalf("unexpected status %d: %s", resp.Status, string(resp.Body))
		}
		if resp.Parsed.Error != nil {
			t.Fatalf("rpc error: %s", string(*resp.Parsed.Error))
		}
		if string(resp.Parsed.Result) != "null" {
			var tx map[string]interface{}
			require.NoError(t, json.Unmarshal(resp.Parsed.Result, &tx))
		}
	})

	t.Run("eth_getLogs", func(t *testing.T) {
		resp := makeWrapperRequest(t, endpoint, sqdJsonRPCRequest{
			JSONRPC: "2.0",
			ID:      5,
			Method:  "eth_getLogs",
			Params:  []interface{}{map[string]interface{}{}},
		})
		if resp.Status != http.StatusOK {
			t.Fatalf("unexpected status %d: %s", resp.Status, string(resp.Body))
		}
		if resp.Parsed.Error != nil {
			t.Fatalf("rpc error: %s", string(*resp.Parsed.Error))
		}
		var logs []interface{}
		require.NoError(t, json.Unmarshal(resp.Parsed.Result, &logs))
	})

	t.Run("trace_block", func(t *testing.T) {
		resp := makeWrapperRequest(t, endpoint, sqdJsonRPCRequest{
			JSONRPC: "2.0",
			ID:      6,
			Method:  "trace_block",
			Params:  []interface{}{"latest"},
		})
		if skipIfPortalMissingField(t, "trace_block", resp) {
			return
		}
		if resp.Status != http.StatusOK {
			t.Fatalf("unexpected status %d: %s", resp.Status, string(resp.Body))
		}
		if resp.Parsed.Error != nil {
			t.Fatalf("rpc error: %s", string(*resp.Parsed.Error))
		}
		var traces []interface{}
		require.NoError(t, json.Unmarshal(resp.Parsed.Result, &traces))
	})

	upstreamEnabled := false
	for _, method := range upstreamMethods {
		if capabilities[method] {
			upstreamEnabled = true
			break
		}
	}
	if !upstreamEnabled {
		return
	}

	if blockHash == "" || txHash == "" {
		t.Skip("upstream methods enabled, but missing block/tx hash from latest block")
	}

	t.Run("eth_getBlockByHash", func(t *testing.T) {
		resp := makeWrapperRequest(t, endpoint, sqdJsonRPCRequest{
			JSONRPC: "2.0",
			ID:      7,
			Method:  "eth_getBlockByHash",
			Params:  []interface{}{blockHash, false},
		})
		if resp.Status != http.StatusOK {
			t.Fatalf("unexpected status %d: %s", resp.Status, string(resp.Body))
		}
		if resp.Parsed.Error != nil {
			t.Fatalf("rpc error: %s", string(*resp.Parsed.Error))
		}
		if string(resp.Parsed.Result) != "null" {
			var block map[string]interface{}
			require.NoError(t, json.Unmarshal(resp.Parsed.Result, &block))
		}
	})

	t.Run("eth_getTransactionByHash", func(t *testing.T) {
		resp := makeWrapperRequest(t, endpoint, sqdJsonRPCRequest{
			JSONRPC: "2.0",
			ID:      8,
			Method:  "eth_getTransactionByHash",
			Params:  []interface{}{txHash},
		})
		if resp.Status != http.StatusOK {
			t.Fatalf("unexpected status %d: %s", resp.Status, string(resp.Body))
		}
		if resp.Parsed.Error != nil {
			t.Fatalf("rpc error: %s", string(*resp.Parsed.Error))
		}
		if string(resp.Parsed.Result) != "null" {
			var tx map[string]interface{}
			require.NoError(t, json.Unmarshal(resp.Parsed.Result, &tx))
		}
	})

	t.Run("eth_getTransactionReceipt", func(t *testing.T) {
		resp := makeWrapperRequest(t, endpoint, sqdJsonRPCRequest{
			JSONRPC: "2.0",
			ID:      9,
			Method:  "eth_getTransactionReceipt",
			Params:  []interface{}{txHash},
		})
		if resp.Status != http.StatusOK {
			t.Fatalf("unexpected status %d: %s", resp.Status, string(resp.Body))
		}
		if resp.Parsed.Error != nil {
			t.Fatalf("rpc error: %s", string(*resp.Parsed.Error))
		}
		if string(resp.Parsed.Result) != "null" {
			var receipt map[string]interface{}
			require.NoError(t, json.Unmarshal(resp.Parsed.Result, &receipt))
		}
	})

	t.Run("trace_transaction", func(t *testing.T) {
		resp := makeWrapperRequest(t, endpoint, sqdJsonRPCRequest{
			JSONRPC: "2.0",
			ID:      10,
			Method:  "trace_transaction",
			Params:  []interface{}{txHash},
		})
		if resp.Status != http.StatusOK {
			t.Fatalf("unexpected status %d: %s", resp.Status, string(resp.Body))
		}
		if resp.Parsed.Error != nil {
			t.Fatalf("rpc error: %s", string(*resp.Parsed.Error))
		}
		var traces []interface{}
		require.NoError(t, json.Unmarshal(resp.Parsed.Result, &traces))
	})

	t.Run("eth_getLogs blockHash", func(t *testing.T) {
		resp := makeWrapperRequest(t, endpoint, sqdJsonRPCRequest{
			JSONRPC: "2.0",
			ID:      11,
			Method:  "eth_getLogs",
			Params:  []interface{}{map[string]interface{}{"blockHash": blockHash}},
		})
		if resp.Status != http.StatusOK {
			t.Fatalf("unexpected status %d: %s", resp.Status, string(resp.Body))
		}
		if resp.Parsed.Error != nil {
			t.Fatalf("rpc error: %s", string(*resp.Parsed.Error))
		}
		var logs []interface{}
		require.NoError(t, json.Unmarshal(resp.Parsed.Result, &logs))
	})
}
