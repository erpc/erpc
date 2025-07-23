package util

import "strings"

func IsNativeProtocol(endpoint string) bool {
	return strings.HasPrefix(endpoint, "http://") ||
		strings.HasPrefix(endpoint, "https://") ||
		strings.HasPrefix(endpoint, "ws://") ||
		strings.HasPrefix(endpoint, "wss://") ||
		strings.HasPrefix(endpoint, "grpc://") ||
		strings.HasPrefix(endpoint, "grpc+bds://")
}
