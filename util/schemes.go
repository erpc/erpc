package util

import (
	"net/url"
	"strings"
)

func IsNativeProtocol(endpoint string) bool {
	return strings.HasPrefix(endpoint, "http://") ||
		strings.HasPrefix(endpoint, "https://") ||
		strings.HasPrefix(endpoint, "ws://") ||
		strings.HasPrefix(endpoint, "wss://") ||
		strings.HasPrefix(endpoint, "grpc://") ||
		strings.HasPrefix(endpoint, "grpc+bds://") ||
		strings.HasPrefix(endpoint, "grpcs://")
}

// IsWsEndpoint reports whether endpoint uses the ws:// or wss:// scheme.
func IsWsEndpoint(endpoint string) bool {
	parsed, err := url.Parse(endpoint)
	return err == nil && (parsed.Scheme == "ws" || parsed.Scheme == "wss")
}
