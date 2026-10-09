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

// BlockRef identifies a block. It is zero for events without one.
type BlockRef struct {
	Number int64
	Hash   string
}

func (b BlockRef) Zero() bool {
	return b.Number == 0 && b.Hash == ""
}

// StreamEvent is what an ingress emits, before dedup: sources covering the
// same network may emit the same event.
type StreamEvent struct {
	Kind      EventKind
	NetworkId string
	// SourceId is the Name() of the ingress that produced the event.
	SourceId string
	Block    BlockRef
	// FilterHash is the BuildParamsKey of the filter the event belongs to;
	// empty for KindNewHead.
	FilterHash string
	// Payload is the upstream's notification result, verbatim.
	Payload json.RawMessage
}

// IndexedEvent is a deduplicated StreamEvent as delivered to egresses. The
// indexer never synthesizes events: reorged-out logs reach clients only as
// the upstream's own removed:true notifications.
type IndexedEvent struct {
	StreamEvent

	// Removed is the log payload's "removed" flag; false for other kinds.
	Removed bool
}
