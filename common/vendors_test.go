package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDeriveWebsocketEndpoint(t *testing.T) {
	t.Run("EmptyEndpoint", func(t *testing.T) {
		result := DeriveWebsocketEndpoint("alchemy", "")
		assert.Equal(t, "", result)
	})

	t.Run("AlreadyWebsocket_WS", func(t *testing.T) {
		result := DeriveWebsocketEndpoint("alchemy", "ws://example.com")
		assert.Equal(t, "ws://example.com", result)
	})

	t.Run("AlreadyWebsocket_WSS", func(t *testing.T) {
		result := DeriveWebsocketEndpoint("alchemy", "wss://example.com")
		assert.Equal(t, "wss://example.com", result)
	})

	t.Run("NonHTTPEndpoint", func(t *testing.T) {
		result := DeriveWebsocketEndpoint("alchemy", "alchemy://api-key")
		assert.Equal(t, "", result)
	})

	t.Run("Alchemy_HTTPS", func(t *testing.T) {
		result := DeriveWebsocketEndpoint("alchemy", "https://eth-mainnet.g.alchemy.com/v2/my-api-key")
		assert.Equal(t, "wss://eth-mainnet.g.alchemy.com/v2/my-api-key", result)
	})

	t.Run("Alchemy_CaseInsensitive", func(t *testing.T) {
		result := DeriveWebsocketEndpoint("ALCHEMY", "https://eth-mainnet.g.alchemy.com/v2/my-api-key")
		assert.Equal(t, "wss://eth-mainnet.g.alchemy.com/v2/my-api-key", result)
	})

	t.Run("QuickNode_HTTPS", func(t *testing.T) {
		result := DeriveWebsocketEndpoint("quicknode", "https://example-endpoint.quiknode.pro/abc123")
		assert.Equal(t, "wss://example-endpoint.quiknode.pro/abc123", result)
	})

	t.Run("Infura_HTTPS", func(t *testing.T) {
		result := DeriveWebsocketEndpoint("infura", "https://mainnet.infura.io/v3/my-project-id")
		assert.Equal(t, "wss://mainnet.infura.io/ws/v3/my-project-id", result)
	})

	t.Run("Infura_Polygon", func(t *testing.T) {
		result := DeriveWebsocketEndpoint("infura", "https://polygon-mainnet.infura.io/v3/my-project-id")
		assert.Equal(t, "wss://polygon-mainnet.infura.io/ws/v3/my-project-id", result)
	})

	t.Run("DRPC_HTTPS", func(t *testing.T) {
		result := DeriveWebsocketEndpoint("drpc", "https://lb.drpc.org/ogrpc?network=ethereum&dkey=my-api-key")
		assert.Equal(t, "wss://lb.drpc.org/ogws?network=ethereum&dkey=my-api-key", result)
	})

	t.Run("Generic_HTTPS", func(t *testing.T) {
		result := DeriveWebsocketEndpoint("unknown-vendor", "https://rpc.example.com/v1")
		assert.Equal(t, "wss://rpc.example.com/v1", result)
	})

	t.Run("Generic_HTTP", func(t *testing.T) {
		result := DeriveWebsocketEndpoint("unknown-vendor", "http://localhost:8545")
		assert.Equal(t, "ws://localhost:8545", result)
	})

	t.Run("EmptyVendor_FallsBackToGeneric", func(t *testing.T) {
		result := DeriveWebsocketEndpoint("", "https://rpc.example.com")
		assert.Equal(t, "wss://rpc.example.com", result)
	})
}

func TestHttpToWss(t *testing.T) {
	t.Run("HTTPS_to_WSS", func(t *testing.T) {
		result := httpToWss("https://example.com/path")
		assert.Equal(t, "wss://example.com/path", result)
	})

	t.Run("HTTP_to_WS", func(t *testing.T) {
		result := httpToWss("http://localhost:8545")
		assert.Equal(t, "ws://localhost:8545", result)
	})

	t.Run("NonHTTP_Unchanged", func(t *testing.T) {
		result := httpToWss("ftp://example.com")
		assert.Equal(t, "ftp://example.com", result)
	})

	t.Run("WSS_Unchanged", func(t *testing.T) {
		result := httpToWss("wss://example.com")
		assert.Equal(t, "wss://example.com", result)
	})
}
