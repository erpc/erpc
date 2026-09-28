package indexer

// EventEgress receives the IndexedEvents that survive dedup. Each egress
// owns its buffering and drop policy; the indexer delivers synchronously.
type EventEgress interface {
	// Name identifies the egress; it must be unique among attached egresses.
	Name() string
	// InterestedIn reports whether the egress routes events with this key.
	// Called for every event, so it must be cheap and must not block.
	InterestedIn(kind EventKind, networkId, filterHash string) bool
	// Deliver hands over an event. It must not block: a slow egress must
	// only hurt itself, never other egresses or the ingress.
	Deliver(ev IndexedEvent)
}
