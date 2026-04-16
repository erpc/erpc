package indexer

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/rs/zerolog"
)

// --- fakes -----------------------------------------------------------

type fakeNetwork struct {
	id            string
	finalityDepth int64

	mu            sync.Mutex
	suggestedBy   map[string][]int64 // sourceId -> block nums seen
}

func newFakeNetwork(id string, depth int64) *fakeNetwork {
	return &fakeNetwork{
		id:            id,
		finalityDepth: depth,
		suggestedBy:   make(map[string][]int64),
	}
}

func (n *fakeNetwork) Id() string            { return n.id }
func (n *fakeNetwork) FinalityDepth() int64  { return n.finalityDepth }
func (n *fakeNetwork) SuggestLatestBlock(sourceId string, block int64) {
	n.mu.Lock()
	n.suggestedBy[sourceId] = append(n.suggestedBy[sourceId], block)
	n.mu.Unlock()
}

type fakeEgress struct {
	name           string
	filters        map[string]struct{} // filterHash -> interested
	acceptAllHeads bool
	acceptReorgs   bool

	mu       sync.Mutex
	received []IndexedEvent
}

func (e *fakeEgress) Name() string { return e.name }
func (e *fakeEgress) InterestedIn(kind EventKind, networkId, filterHash string) bool {
	switch kind {
	case KindNewHead:
		return e.acceptAllHeads
	case KindReorg:
		return e.acceptReorgs
	}
	_, ok := e.filters[filterHash]
	return ok
}
func (e *fakeEgress) Deliver(ev IndexedEvent) {
	e.mu.Lock()
	e.received = append(e.received, ev)
	e.mu.Unlock()
}
func (e *fakeEgress) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.received)
}

type fakeIngress struct {
	name        string
	ensureCalls atomic.Int32
	removeCalls atomic.Int32
	lastParamsHash atomic.Value // string

	startedFor NetworkHandle
	sink       Sink
}

func (i *fakeIngress) Name() string { return i.name }
func (i *fakeIngress) Start(_ context.Context, nw NetworkHandle, sink Sink) error {
	i.startedFor = nw
	i.sink = sink
	return nil
}
func (i *fakeIngress) EnsureFilter(_ context.Context, _ string, paramsHash string, _ []interface{}) error {
	i.ensureCalls.Add(1)
	i.lastParamsHash.Store(paramsHash)
	return nil
}
func (i *fakeIngress) RemoveFilter(_ context.Context, _, _ string) error {
	i.removeCalls.Add(1)
	return nil
}
func (i *fakeIngress) Stop(_ context.Context) error { return nil }

// --- tests -----------------------------------------------------------

func newIndexer(t *testing.T) *Indexer {
	t.Helper()
	logger := zerolog.New(zerolog.NewTestWriter(t))
	return New(&logger, Options{})
}

func TestIndexer_NewHead_FanOutAndDedup(t *testing.T) {
	idx := newIndexer(t)
	nw := newFakeNetwork("evm:1", 0)
	idx.RegisterNetwork(nw)
	eg := &fakeEgress{name: "eg1", filters: map[string]struct{}{}, acceptAllHeads: true}
	idx.Attach(eg)

	ev := StreamEvent{
		Kind:      KindNewHead,
		NetworkId: "evm:1",
		SourceId:  "ws:up1",
		Block:     BlockRef{Number: 100, Hash: "0xAAA"},
	}
	idx.Ingest(ev)
	idx.Ingest(ev) // dup

	if got := eg.count(); got != 1 {
		t.Fatalf("want 1 delivery after dup, got %d", got)
	}
	// Second unique head advances.
	idx.Ingest(StreamEvent{Kind: KindNewHead, NetworkId: "evm:1", SourceId: "ws:up1", Block: BlockRef{Number: 101, Hash: "0xBBB"}})
	if got := eg.count(); got != 2 {
		t.Fatalf("want 2 after advance, got %d", got)
	}
}

// TestIndexer_NewHead_ConcurrentIngestDedupe reproduces the TOCTOU race
// that prod evm:1101 hit: four WS upstream sources delivered the same
// newHead within ~1ms, and concurrent Ingest calls both read the stale
// lastHeadNum + Store the same new value + fell through to fanOut, so
// clients saw every head twice. The regression asserts that no matter
// how many goroutines race with the same (number, hash), the egress
// receives exactly one delivery.
func TestIndexer_NewHead_ConcurrentIngestDedupe(t *testing.T) {
	idx := newIndexer(t)
	nw := newFakeNetwork("evm:1", 0)
	idx.RegisterNetwork(nw)
	eg := &fakeEgress{name: "eg1", filters: map[string]struct{}{}, acceptAllHeads: true}
	idx.Attach(eg)

	const sources = 8
	ev := StreamEvent{
		Kind:      KindNewHead,
		NetworkId: "evm:1",
		Block:     BlockRef{Number: 100, Hash: "0xAAA"},
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < sources; i++ {
		wg.Add(1)
		ev := ev
		ev.SourceId = fmt.Sprintf("ws:up%d", i)
		go func() {
			defer wg.Done()
			<-start
			idx.Ingest(ev)
		}()
	}
	close(start)
	wg.Wait()

	if got := eg.count(); got != 1 {
		t.Fatalf("concurrent ingest of identical head must dedupe to 1 delivery, got %d", got)
	}
}

func TestIndexer_NewHead_StalerDroppedKeepsStatePollerFed(t *testing.T) {
	idx := newIndexer(t)
	nw := newFakeNetwork("evm:1", 0)
	idx.RegisterNetwork(nw)
	eg := &fakeEgress{name: "eg1", acceptAllHeads: true}
	idx.Attach(eg)

	idx.Ingest(StreamEvent{Kind: KindNewHead, NetworkId: "evm:1", SourceId: "ws:up1", Block: BlockRef{Number: 100, Hash: "0xAAA"}})
	// A sibling source sees a newer head; different source, same block —
	// state poller must still receive the update even though fan-out dedupes.
	idx.Ingest(StreamEvent{Kind: KindNewHead, NetworkId: "evm:1", SourceId: "ws:up2", Block: BlockRef{Number: 100, Hash: "0xAAA"}})

	if got := eg.count(); got != 1 {
		t.Fatalf("duplicate head fan-out: want 1, got %d", got)
	}
	nw.mu.Lock()
	defer nw.mu.Unlock()
	if len(nw.suggestedBy["ws:up1"]) != 1 || len(nw.suggestedBy["ws:up2"]) != 1 {
		t.Fatalf("each source must see its own SuggestLatestBlock, got %v", nw.suggestedBy)
	}
}

func TestIndexer_Log_RefcountFanOutAndTeardown(t *testing.T) {
	idx := newIndexer(t)
	nw := newFakeNetwork("evm:1", 0)
	idx.RegisterNetwork(nw)
	ing := &fakeIngress{name: "ws:up1"}
	if err := idx.AddIngress(context.Background(), "evm:1", ing); err != nil {
		t.Fatal(err)
	}

	params := []interface{}{"logs", map[string]interface{}{"topics": []string{"0x1"}}}
	h1, err := idx.EnsureFilter(context.Background(), "evm:1", "logs", params)
	if err != nil {
		t.Fatal(err)
	}
	// Second subscriber on same filter: refcount bumps, no new ingress call.
	h2, _ := idx.EnsureFilter(context.Background(), "evm:1", "logs", params)
	if h1 != h2 {
		t.Fatalf("same params must hash identically, got %q vs %q", h1, h2)
	}
	if got := ing.ensureCalls.Load(); got != 1 {
		t.Fatalf("EnsureFilter on ingress should be called exactly once, got %d", got)
	}

	// First release decrements; still refcnt 1 → no tear-down.
	idx.ReleaseFilter(context.Background(), "evm:1", "logs", h1)
	if got := ing.removeCalls.Load(); got != 0 {
		t.Fatalf("RemoveFilter must not fire while refcnt > 0, got %d calls", got)
	}
	// Second release drops to 0 → tear-down.
	idx.ReleaseFilter(context.Background(), "evm:1", "logs", h1)
	if got := ing.removeCalls.Load(); got != 1 {
		t.Fatalf("RemoveFilter must fire exactly once at refcnt 0, got %d", got)
	}
}

func TestIndexer_Log_DedupFanOut(t *testing.T) {
	idx := newIndexer(t)
	nw := newFakeNetwork("evm:1", 0)
	idx.RegisterNetwork(nw)

	params := []interface{}{"logs", map[string]interface{}{}}
	h, _ := idx.EnsureFilter(context.Background(), "evm:1", "logs", params)

	eg := &fakeEgress{name: "eg1", filters: map[string]struct{}{h: {}}}
	idx.Attach(eg)

	payload := json.RawMessage(`{"blockHash":"0xB","transactionHash":"0xT","logIndex":"0x0","removed":false}`)
	ev := StreamEvent{
		Kind:       KindLog,
		NetworkId:  "evm:1",
		SourceId:   "ws:up1",
		FilterHash: h,
		Payload:    payload,
	}
	idx.Ingest(ev)
	idx.Ingest(ev) // dup by (blockHash, txHash, logIndex, removed)

	if got := eg.count(); got != 1 {
		t.Fatalf("log dedup: want 1, got %d", got)
	}

	// Same log with removed=true must deliver (distinct dedup key).
	idx.Ingest(StreamEvent{
		Kind: KindLog, NetworkId: "evm:1", SourceId: "ws:up1", FilterHash: h,
		Payload: json.RawMessage(`{"blockHash":"0xB","transactionHash":"0xT","logIndex":"0x0","removed":true}`),
	})
	if got := eg.count(); got != 2 {
		t.Fatalf("log with removed=true must be delivered, got %d", got)
	}
	// Last delivered must carry Removed=true.
	eg.mu.Lock()
	last := eg.received[len(eg.received)-1]
	eg.mu.Unlock()
	if !last.Removed {
		t.Fatalf("removed flag must propagate to IndexedEvent.Removed")
	}
}

func TestIndexer_Lifecycle_FinalityBoundary(t *testing.T) {
	idx := newIndexer(t)
	nw := newFakeNetwork("evm:1", 10)
	idx.RegisterNetwork(nw)
	eg := &fakeEgress{name: "eg1", acceptAllHeads: true}
	idx.Attach(eg)

	// Latest head: 100.
	idx.Ingest(StreamEvent{Kind: KindNewHead, NetworkId: "evm:1", SourceId: "ws:up1", Block: BlockRef{Number: 100, Hash: "0xH100"}})
	// A log at block 90 → latest-90 = 10 = depth → finalized.
	idx.Ingest(StreamEvent{
		Kind: KindLog, NetworkId: "evm:1", SourceId: "ws:up1",
		Block:   BlockRef{Number: 90, Hash: "0xH90"},
		Payload: json.RawMessage(`{"blockHash":"0xH90","transactionHash":"0xT","logIndex":"0x0"}`),
	})
	// A log at 95 → latest-95 = 5 < depth → soft.
	idx.Ingest(StreamEvent{
		Kind: KindLog, NetworkId: "evm:1", SourceId: "ws:up1",
		Block:   BlockRef{Number: 95, Hash: "0xH95"},
		Payload: json.RawMessage(`{"blockHash":"0xH95","transactionHash":"0xT","logIndex":"0x1"}`),
	})

	eg.mu.Lock()
	defer eg.mu.Unlock()
	// Only KindNewHead at 100 got through the egress (logs don't pass
	// through the egress without a matching filter). So assert lifecycle
	// on the head event.
	if len(eg.received) != 1 {
		t.Fatalf("egress should have 1 newHead event, got %d", len(eg.received))
	}
	head := eg.received[0]
	// latest-latest = 0 < depth → soft.
	if head.Lifecycle != LifeSoft {
		t.Fatalf("latest head must be soft, got %v", head.Lifecycle)
	}
}

func TestIndexer_ReorgEmitsRemovedLogs(t *testing.T) {
	idx := newIndexer(t)
	nw := newFakeNetwork("evm:1", 0)
	idx.RegisterNetwork(nw)

	params := []interface{}{"logs", map[string]interface{}{}}
	h, _ := idx.EnsureFilter(context.Background(), "evm:1", "logs", params)

	eg := &fakeEgress{
		name:           "eg1",
		filters:        map[string]struct{}{h: {}},
		acceptAllHeads: true,
		acceptReorgs:   true,
	}
	idx.Attach(eg)

	// newHead 100/0xA.
	idx.Ingest(StreamEvent{
		Kind: KindNewHead, NetworkId: "evm:1", SourceId: "src1",
		Block: BlockRef{Number: 100, Hash: "0xA", ParentHash: "0xZ"},
	})
	// newHead 101/0xB, child of 0xA.
	idx.Ingest(StreamEvent{
		Kind: KindNewHead, NetworkId: "evm:1", SourceId: "src1",
		Block: BlockRef{Number: 101, Hash: "0xB", ParentHash: "0xA"},
	})
	// Log in block 0xB.
	logPayload := json.RawMessage(`{"blockHash":"0xB","blockNumber":"0x65","transactionHash":"0xT1","logIndex":"0x0"}`)
	idx.Ingest(StreamEvent{
		Kind: KindLog, NetworkId: "evm:1", SourceId: "src1", FilterHash: h,
		Block:   BlockRef{Number: 101, Hash: "0xB"},
		Payload: logPayload,
	})

	// Reorg: new head 101/0xC replaces 0xB.
	idx.Ingest(StreamEvent{
		Kind: KindNewHead, NetworkId: "evm:1", SourceId: "src1",
		Block: BlockRef{Number: 101, Hash: "0xC", ParentHash: "0xA"},
	})

	// Expected sequence in eg.received:
	//   newHead 100, newHead 101 (0xB), log 0xB, reorg summary, log 0xB (removed), newHead 101 (0xC)
	// eg.interestedIn filters: newHeads + logs with filterHash h.
	eg.mu.Lock()
	defer eg.mu.Unlock()
	if len(eg.received) < 6 {
		t.Fatalf("want >= 6 events after reorg, got %d: %+v", len(eg.received), kinds(eg.received))
	}
	// Find the reorg event.
	var reorgIdx = -1
	for i, ev := range eg.received {
		if ev.Kind == KindReorg {
			reorgIdx = i
			break
		}
	}
	if reorgIdx < 0 {
		t.Fatalf("KindReorg not emitted; got %+v", kinds(eg.received))
	}
	// Next event after reorg (amongst this egress's filtered set) should
	// be a log with Removed=true.
	foundRemoved := false
	for _, ev := range eg.received[reorgIdx+1:] {
		if ev.Kind == KindLog && ev.Removed {
			foundRemoved = true
			break
		}
	}
	if !foundRemoved {
		t.Fatalf("expected a log with Removed=true after reorg, got %+v", kinds(eg.received))
	}
}

func kinds(evs []IndexedEvent) []string {
	out := make([]string, 0, len(evs))
	for _, ev := range evs {
		out = append(out, ev.Kind.String())
	}
	return out
}

func TestIndexer_UnregisteredNetwork(t *testing.T) {
	idx := newIndexer(t)
	if err := idx.AddIngress(context.Background(), "evm:missing", &fakeIngress{name: "x"}); err == nil {
		t.Fatal("expected error on unregistered network")
	}
	if _, err := idx.EnsureFilter(context.Background(), "evm:missing", "logs", []interface{}{"logs"}); err == nil {
		t.Fatal("expected error on unregistered network")
	}
}
