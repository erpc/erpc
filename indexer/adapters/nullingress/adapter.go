// Package nullingress is a minimal, transport-free EventIngress
// implementation. It exists to (a) prove indexer.EventIngress isn't
// secretly shaped around WebSocket semantics and (b) give tests a
// deterministic way to inject StreamEvents into the indexer pipeline.
//
// It is NOT a production ingress — the Push method lets tests feed raw
// events with no backing transport. A future Kafka consumer would slot
// in at the same seam.
package nullingress

import (
	"context"
	"sync"

	"github.com/erpc/erpc/indexer"
)

// Adapter implements indexer.EventIngress as an in-memory feed.
type Adapter struct {
	name string

	mu      sync.Mutex
	started bool
	cancel  context.CancelFunc
	sink    indexer.Sink
	events  chan indexer.StreamEvent

	// Active filter bookkeeping — tests can assert on this to verify
	// EnsureFilter/RemoveFilter were invoked by the indexer.
	filters map[string]int // subType:paramsHash -> refcount (the indexer itself refcounts, this is just for observation)
}

// New constructs a nullingress adapter. name is surfaced via Name()
// for indexer registries/logs ("null:mock1").
func New(name string) *Adapter {
	return &Adapter{
		name:    name,
		events:  make(chan indexer.StreamEvent, 64),
		filters: make(map[string]int),
	}
}

// Name implements indexer.EventIngress.
func (a *Adapter) Name() string { return a.name }

// Start implements indexer.EventIngress. Spins up a goroutine that
// forwards any Push()ed event to the indexer's Sink.
func (a *Adapter) Start(ctx context.Context, _ indexer.NetworkHandle, sink indexer.Sink) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.started {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	a.cancel = cancel
	a.sink = sink
	a.started = true
	go a.pump(ctx)
	return nil
}

func (a *Adapter) pump(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-a.events:
			a.sink.Ingest(ev)
		}
	}
}

// EnsureFilter implements indexer.EventIngress.
func (a *Adapter) EnsureFilter(_ context.Context, subType, paramsHash string, _ []interface{}) error {
	key := subType + ":" + paramsHash
	a.mu.Lock()
	a.filters[key]++
	a.mu.Unlock()
	return nil
}

// RemoveFilter implements indexer.EventIngress.
func (a *Adapter) RemoveFilter(_ context.Context, subType, paramsHash string) error {
	key := subType + ":" + paramsHash
	a.mu.Lock()
	delete(a.filters, key)
	a.mu.Unlock()
	return nil
}

// Stop implements indexer.EventIngress.
func (a *Adapter) Stop(_ context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cancel != nil {
		a.cancel()
	}
	a.started = false
	return nil
}

// Push injects a StreamEvent into the ingress. Test-only. Will block if
// the internal channel fills up — intentional, so slow-consumer bugs in
// the indexer show up as a test timeout rather than silent drops.
func (a *Adapter) Push(ev indexer.StreamEvent) {
	a.events <- ev
}

// ActiveFilters returns a snapshot of currently-active filter keys, for
// test assertions on EnsureFilter / RemoveFilter plumbing.
func (a *Adapter) ActiveFilters() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.filters))
	for k := range a.filters {
		out = append(out, k)
	}
	return out
}
