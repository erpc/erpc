package util

import (
	"net/url"
	"slices"
	"strings"
)

func IsNativeProtocol(endpoint string) bool {
	return strings.HasPrefix(endpoint, "http://") ||
		strings.HasPrefix(endpoint, "https://") ||
		strings.HasPrefix(endpoint, "ws://") ||
		strings.HasPrefix(endpoint, "wss://") ||
		IsGrpcEndpoint(endpoint)
}

// IsWsEndpoint reports whether endpoint uses the ws:// or wss:// scheme.
func IsWsEndpoint(endpoint string) bool {
	parsed, err := url.Parse(endpoint)
	return err == nil && (parsed.Scheme == "ws" || parsed.Scheme == "wss")
}

// grpcSchemes are the URL schemes the gRPC client serves.
var grpcSchemes = []string{"grpc", "grpcs", "grpc+bds"}

// IsGrpcScheme reports whether scheme is one the gRPC client serves.
func IsGrpcScheme(scheme string) bool {
	return slices.Contains(grpcSchemes, scheme)
}

// IsGrpcEndpoint reports whether endpoint starts with a gRPC scheme
// (grpc://, grpcs:// or grpc+bds://).
func IsGrpcEndpoint(endpoint string) bool {
	for _, s := range grpcSchemes {
		if strings.HasPrefix(endpoint, s+"://") {
			return true
		}
	}
	return false
}
