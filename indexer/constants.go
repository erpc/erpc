// Package indexer is the chain- and transport-neutral core between sources
// and transports: the chain and mempool feed contracts, intake, sources and
// taps. It must not import erpc, architecture/evm, clients, or any
// transport library.
package indexer

// Supported eth_subscribe subscription types; they name topics and label
// taps.
const (
	SubTypeNewHeads               = "newHeads"
	SubTypeLogs                   = "logs"
	SubTypeNewPendingTransactions = "newPendingTransactions"
)
