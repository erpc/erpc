package common

import (
	"net/http"
)

// JsonRpcErrorExtractor allows callers to inject architecture-specific
// JSON-RPC error normalization logic into HTTP clients without creating
// package import cycles.
type JsonRpcErrorExtractor interface {
	Extract(resp *http.Response, nr *NormalizedResponse, jr *JsonRpcResponse, upstream Upstream) error
}

// JsonRpcErrorExtractorFunc is an adapter to allow normal functions to be used
// as JsonRpcErrorExtractor implementations.
// Similar to http.HandlerFunc style adapters.
type JsonRpcErrorExtractorFunc func(resp *http.Response, nr *NormalizedResponse, jr *JsonRpcResponse, upstream Upstream) error

func (f JsonRpcErrorExtractorFunc) Extract(resp *http.Response, nr *NormalizedResponse, jr *JsonRpcResponse, upstream Upstream) error {
	return f(resp, nr, jr, upstream)
}
