package common

import (
	"context"
	"net/http"
	"strings"

	"github.com/rs/zerolog"
)

type Vendor interface {
	Name() string
	OwnsUpstream(upstream *UpstreamConfig) bool
	GenerateConfigs(ctx context.Context, logger *zerolog.Logger, baseConfig *UpstreamConfig, settings VendorSettings) ([]*UpstreamConfig, error)
	SupportsNetwork(ctx context.Context, logger *zerolog.Logger, settings VendorSettings, networkId string) (bool, error)
	GetVendorSpecificErrorIfAny(req *NormalizedRequest, resp *http.Response, bodyObject interface{}, details map[string]interface{}) error
}

// DeriveWebsocketEndpoint derives a WebSocket endpoint URL from an HTTP endpoint URL.
// Different vendors have different URL patterns for their WebSocket endpoints.
func DeriveWebsocketEndpoint(vendorName string, httpEndpoint string) string {
	if httpEndpoint == "" {
		return ""
	}

	// Already a websocket endpoint
	if strings.HasPrefix(httpEndpoint, "ws://") || strings.HasPrefix(httpEndpoint, "wss://") {
		return httpEndpoint
	}

	// Only handle http/https endpoints
	if !strings.HasPrefix(httpEndpoint, "http://") && !strings.HasPrefix(httpEndpoint, "https://") {
		return ""
	}

	switch strings.ToLower(vendorName) {
	case "alchemy":
		// Alchemy: https://{sub}.g.alchemy.com/v2/{key} -> wss://{sub}.g.alchemy.com/v2/{key}
		return httpToWss(httpEndpoint)

	case "quicknode":
		// QuickNode: https://{name}.quiknode.pro/{key} -> wss://{name}.quiknode.pro/{key}
		return httpToWss(httpEndpoint)

	case "infura":
		// Infura: https://{net}.infura.io/v3/{key} -> wss://{net}.infura.io/ws/v3/{key}
		wsEndpoint := httpToWss(httpEndpoint)
		// Insert /ws before /v3
		wsEndpoint = strings.Replace(wsEndpoint, ".infura.io/v3/", ".infura.io/ws/v3/", 1)
		return wsEndpoint

	case "drpc":
		// DRPC: https://lb.drpc.org/ogrpc?... -> wss://lb.drpc.org/ogws?...
		wsEndpoint := httpToWss(httpEndpoint)
		wsEndpoint = strings.Replace(wsEndpoint, "/ogrpc", "/ogws", 1)
		return wsEndpoint

	default:
		// Generic: simple scheme replacement
		return httpToWss(httpEndpoint)
	}
}

// httpToWss converts an http:// or https:// URL to ws:// or wss://
func httpToWss(httpEndpoint string) string {
	if strings.HasPrefix(httpEndpoint, "https://") {
		return "wss://" + strings.TrimPrefix(httpEndpoint, "https://")
	}
	if strings.HasPrefix(httpEndpoint, "http://") {
		return "ws://" + strings.TrimPrefix(httpEndpoint, "http://")
	}
	return httpEndpoint
}
