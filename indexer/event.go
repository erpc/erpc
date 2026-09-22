package indexer

import (
	"encoding/json"
	"time"
)

// EventKind is a tagged-union discriminant for events flowing through the
// indexer pipeline. Separate types for each kind would force sum-type
// dispatch at every stage (dedup, lifecycle tagging, fan-out) — the
// discriminant lets every stage operate on a uniform value, which is
// worth the modest loss of compile-time guarantees on per-kind fields.
type EventKind uint8

const (
	KindUnknown EventKind = iota
	// KindNewHead: a canonical block header. One-shot per new block.
	KindNewHead
	// KindLog: an event log delivered by a filter subscription.
	KindLog
	// KindPendingTx: a newly-seen pending transaction hash or object.
	KindPendingTx
	// KindReorg: a synthetic event emitted by the indexer when the
	// canonical chain diverges. The payload describes the evicted segment.
	KindReorg
)

// String returns the lowercase name matching Ethereum's eth_subscribe
// surface (or "reorg"/"unknown" for synthetic/unknown kinds).
func (k EventKind) String() string {
	switch k {
	case KindNewHead:
		return "newHeads"
	case KindLog:
		return "logs"
	case KindPendingTx:
		return "newPendingTransactions"
	case KindReorg:
		return "reorg"
	default:
		return "unknown"
	}
}

// Lifecycle records whether an event sits above or below the network's
// finality boundary at the moment it's emitted. A consumer that only
// cares about finalized data can filter on `Lifecycle == LifeFinalized`.
type Lifecycle uint8

const (
	// LifeSoft: event has not yet passed the network's finality depth.
	// May be reorged out. This is the default for head-tracking.
	LifeSoft Lifecycle = iota
	// LifeFinalized: event is at or below the finalized block. Treated
	// as immutable by consumers.
	LifeFinalized
)

// String renders Lifecycle for log/metric labels.
func (l Lifecycle) String() string {
	switch l {
	case LifeFinalized:
		return "finalized"
	default:
		return "soft"
	}
}

// BlockRef identifies a block on a network. Zero-valued for KindPendingTx
// (pending txs don't carry a block reference until mined).
type BlockRef struct {
	Number     int64
	Hash       string
	ParentHash string
}

// Zero reports whether the BlockRef is the zero value — i.e. no block
// reference is attached (pending tx).
func (b BlockRef) Zero() bool {
	return b.Number == 0 && b.Hash == "" && b.ParentHash == ""
}

// StreamEvent is what an ingress emits. It is pre-dedup, pre-lifecycle,
// and may duplicate events delivered by sibling sources covering the
// same network. The Indexer converts StreamEvents → IndexedEvents.
type StreamEvent struct {
	Kind      EventKind
	NetworkId string
	// SourceId names the ingress that produced this event ("ws:<upstreamId>",
	// "kafka:<topic>", …). Used for per-source bookkeeping inside the
	// indexer (e.g. state-poller updates are per upstream) but never
	// surfaced to egresses.
	SourceId string
	Block    BlockRef
	// FilterHash identifies the filter subscription this event belongs to
	// for Kind{Log,PendingTx}. Empty for KindNewHead. Matches the hash
	// returned by BuildParamsKey when clients subscribe.
	FilterHash string
	// Payload is the upstream-provided notification result, verbatim JSON.
	// Adapters must not re-marshal — both to preserve upstream formatting
	// quirks and to let non-JSON egresses (protobuf, flatbuf) decode once
	// and cache beside the event.
	Payload    json.RawMessage
	ObservedAt time.Time
}

// IndexedEvent is what the Indexer emits to every registered egress after
// dedup, sequencing, and lifecycle tagging. One IndexedEvent per observed
// StreamEvent (minus duplicates), with at most one synthetic KindReorg
// injected between the old and new canonical heads.
type IndexedEvent struct {
	StreamEvent

	// Seq is a monotonically-increasing per (NetworkId, Kind, FilterHash)
	// counter, stable for the life of an indexer instance. Egresses that
	// need resumption (Kafka producer, webhooks with retry) key off Seq.
	// It is NOT globally unique across indexer restarts.
	Seq uint64
	// Lifecycle is computed on the way out from the NetworkHandle's
	// finality depth. See LifeSoft / LifeFinalized.
	Lifecycle Lifecycle
	// Removed is true when the upstream signalled the event was reorged
	// out (for logs) or when the indexer's canonical chain tracker
	// evicted a previously-emitted block.
	Removed bool
}
