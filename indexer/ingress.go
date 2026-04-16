package indexer

import "context"

// Sink is the interface an ingress uses to push StreamEvents into the
// indexer pipeline. The indexer itself implements Sink — a pointer to the
// indexer is what adapters call on every upstream notification.
//
// Ingest is non-blocking and best-effort. Dedup, reorg handling, lifecycle
// tagging, and per-egress drop policy all live downstream; an ingress that
// re-delivers an already-seen notification is expected and handled.
type Sink interface {
	Ingest(ev StreamEvent)
}

// NetworkHandle is the narrow slice of network state an ingress is
// allowed to touch. It intentionally excludes almost everything on
// *erpc.Network — the invariant is that an ingress only needs
// identification, finality info, and per-source bookkeeping hooks.
type NetworkHandle interface {
	// Id returns the network identifier ("evm:<chainId>").
	Id() string
	// FinalityDepth returns the number of blocks below the latest head
	// that are considered finalized on this network. Used by the
	// indexer when tagging IndexedEvent.Lifecycle.
	FinalityDepth() int64
	// SuggestLatestBlock advances the per-source latest-block tracker
	// before the indexer dedupes. Preserving "update-before-dedup"
	// ordering is critical — the state poller needs to see every
	// observation, even ones we'll drop in the fan-out stage.
	SuggestLatestBlock(sourceId string, blockNumber int64)
}

// EventIngress is an adapter that converts some transport-specific
// subscription (WS eth_subscribe, Kafka consumer, HTTP long-poll, …)
// into StreamEvents pushed at a Sink.
//
// Lifecycle expectations:
//
//   - Start is called once when the indexer takes ownership. The ingress
//     is expected to spin up its own goroutine(s) and push events at the
//     sink until Stop is called or the context is cancelled.
//   - EnsureFilter / RemoveFilter are invoked when the first/last client
//     subscribes to a filter. Idempotent: repeated EnsureFilter calls for
//     the same (subType, paramsHash) are no-ops.
//   - Stop is best-effort; implementations should return promptly even
//     if upstream unsubscribe RPCs time out.
type EventIngress interface {
	// Name is a human-readable identifier used in logs/metrics
	// ("ws:<upstreamId>", "kafka:<topic>"). Must be stable for the life
	// of the ingress.
	Name() string
	// Start begins pumping events. Returns only once the ingress has
	// established its background workers — not once the first event
	// arrives (that would race with upstream connectivity).
	Start(ctx context.Context, nw NetworkHandle, sink Sink) error
	// EnsureFilter subscribes (on the ingress's transport) to the given
	// filter. The paramsHash is precomputed by the caller to match
	// BuildParamsKey(params). Subsequent EnsureFilter calls with the
	// same hash are no-ops.
	EnsureFilter(ctx context.Context, subType string, paramsHash string, params []interface{}) error
	// RemoveFilter unsubscribes the given filter from the transport.
	// A no-op if the filter was never subscribed. Called when the last
	// client for a filter unsubscribes.
	RemoveFilter(ctx context.Context, subType string, paramsHash string) error
	// Stop shuts down the ingress and releases resources. After Stop
	// returns, no further events should be delivered to the sink.
	Stop(ctx context.Context) error
}
