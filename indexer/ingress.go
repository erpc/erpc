package indexer

import "context"

// Sink receives the StreamEvents an ingress produces; the Indexer is the
// Sink. Duplicates are expected and handled downstream.
type Sink interface {
	Ingest(ev StreamEvent)
}

// NetworkHandle is the part of a network an ingress may touch.
type NetworkHandle interface {
	Id() string
	// SuggestLatestBlock reports a head observed by source and whether
	// clients may receive it. It is called before dedup and fan-out, so
	// every source's observation counts and the network tip moves before
	// clients see the head; a head it does not deliver is dropped.
	SuggestLatestBlock(sourceId string, blockNumber int64) (deliver bool)
}

// EventIngress turns a transport-specific subscription into StreamEvents
// pushed at a Sink.
type EventIngress interface {
	// Name identifies the ingress and must be stable for its lifetime.
	Name() string
	// Start begins pumping events and returns once the background workers
	// run, not once the first event arrives.
	Start(ctx context.Context, nw NetworkHandle, sink Sink) error
	// EnsureFilter subscribes the filter with paramsHash (the
	// BuildParamsKey of params); a no-op if it is already subscribed.
	EnsureFilter(ctx context.Context, subType string, paramsHash string, params []interface{}) error
	// RemoveFilter unsubscribes the filter; a no-op if it was never
	// subscribed. Called when its last client leaves or its subscribe failed.
	RemoveFilter(ctx context.Context, subType string, paramsHash string) error
	// FilterLive reports whether the filter is subscribed and live now.
	FilterLive(subType string, paramsHash string) bool
}

// IngressSelector chooses which ingresses carry a filter subscription, by
// Name(). A filter is kept on every default, and on the fallbacks while no
// default has it live (or, when it is first subscribed, if every default
// failed). Ingresses named in neither do not carry it. The choice is
// rechecked on every head. Without a selector every ingress is a default.
type IngressSelector interface {
	Select(networkId, subType string, params []interface{}) (defaults, fallbacks []string)
}
