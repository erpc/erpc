package indexer

import "encoding/json"

// EventKind discriminates the events flowing through the indexer.
type EventKind uint8

const (
	KindUnknown EventKind = iota
	KindNewHead
	KindLog
	KindPendingTx
)

// String returns the matching eth_subscribe subscription type.
func (k EventKind) String() string {
	switch k {
	case KindNewHead:
		return "newHeads"
	case KindLog:
		return "logs"
	case KindPendingTx:
		return "newPendingTransactions"
	default:
		return "unknown"
	}
}

// StreamEvent is what an ingress emits, before dedup: sources covering the
// same network may emit the same event.
type StreamEvent struct {
	Kind      EventKind
	NetworkId string
	// SourceId is the Name() of the ingress that produced the event.
	SourceId string
	// FilterHash is the BuildParamsKey of the filter the event belongs to;
	// empty for KindNewHead.
	FilterHash string
	// Payload is the upstream's notification result, verbatim; empty for
	// KindNewHead.
	Payload json.RawMessage
}

// IndexedEvent is a deduplicated StreamEvent as delivered to egresses.
type IndexedEvent struct {
	StreamEvent
}
