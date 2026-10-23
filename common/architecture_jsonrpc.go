package common

const (
	// UpstreamTypeJsonRpc marks an upstream that serves a `jsonrpc:<slug>`
	// network: plain JSON-RPC 2.0 over HTTP with no chain-specific probing.
	UpstreamTypeJsonRpc UpstreamType = "jsonrpc"
)
