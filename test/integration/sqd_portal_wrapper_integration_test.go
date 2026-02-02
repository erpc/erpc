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
//	  go test -v -run TestSqdPortalWrapper_EthChainId ./test/integration/...
package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

func makeWrapperRequest(t *testing.T, endpoint string, payload interface{}) sqdJsonRPCResponse {
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

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status %d: %s", resp.StatusCode, string(data))
	}

	var parsed sqdJsonRPCResponse
	require.NoError(t, json.Unmarshal(data, &parsed))
	return parsed
}

func TestSqdPortalWrapper_EthChainId(t *testing.T) {
	endpoint := getWrapperEndpoint(t)

	resp := makeWrapperRequest(t, endpoint, sqdJsonRPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "eth_chainId",
		Params:  []interface{}{},
	})

	if resp.Error != nil {
		t.Fatalf("rpc error: %s", string(*resp.Error))
	}

	var chainIDHex string
	require.NoError(t, json.Unmarshal(resp.Result, &chainIDHex))
	require.True(t, strings.HasPrefix(chainIDHex, "0x"), "expected hex chainId, got %q", chainIDHex)

	_, err := strconv.ParseInt(strings.TrimPrefix(chainIDHex, "0x"), 16, 64)
	require.NoError(t, err, fmt.Sprintf("invalid chainId hex: %q", chainIDHex))
}
