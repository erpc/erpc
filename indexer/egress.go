package indexer

// EventEgress receives IndexedEvents that survive dedup + lifecycle tagging.
// Adapters own their own buffering, ordering guarantees, and drop policy —
// the indexer passes events through synchronously and moves on.
//
// Today's WS-client adapter uses oldest-drop (slow client discards the
// oldest queued notification to keep up). A future Kafka producer would
// block on backpressure; a webhook adapter would queue-and-retry. Keeping
// the policy per-adapter is deliberate.
type EventEgress interface {
	// Name is a stable identifier for logs/metrics ("ws:client:<connId>",
	// "kafka:topic:<name>", …).
	Name() string
	// InterestedIn lets the indexer skip the Deliver call entirely for
	// events this egress doesn't route. Return true if the event matches
	// some active subscription on this egress; false to drop it.
	//
	// Called on the indexer's hot path — must not allocate, lock, or
	// block. Typical implementation is a sync.Map lookup on a pre-built
	// key.
	InterestedIn(kind EventKind, networkId, filterHash string) bool
	// Deliver hands the event to the egress. The indexer treats Deliver
	// as non-blocking; a slow Deliver hurts *this* egress only (via the
	// adapter's own buffer), never other egresses or the ingress.
	Deliver(ev IndexedEvent)
}
