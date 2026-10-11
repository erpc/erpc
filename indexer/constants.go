// Package indexer is the transport-neutral event-stream core: dedup,
// per-filter subscription bookkeeping, and fan-out to egresses. It must not
// import erpc, clients, or any transport library, so ingress and egress
// implementations stay swappable.
package indexer

// Supported eth_subscribe subscription types.
const (
	SubTypeNewHeads               = "newHeads"
	SubTypeLogs                   = "logs"
	SubTypeNewPendingTransactions = "newPendingTransactions"
)

// DefaultDedupWindowSize bounds the per-filter set of recently delivered
// notification keys.
const DefaultDedupWindowSize = 8192
