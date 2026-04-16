package indexer_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/indexer"
	"github.com/erpc/erpc/indexer/adapters/nullingress"
	"github.com/rs/zerolog"
)

// TestIntegration_NullIngress_EndToEnd wires a real indexer.Indexer to a
// transport-free nullingress.Adapter and asserts the full pipeline
// (ingest → dedup → lifecycle → fan-out) works with zero WebSocket-shaped
// dependencies. This is the forcing-function test proving the interface
// is transport-neutral.
func TestIntegration_NullIngress_EndToEnd(t *testing.T) {
	logger := zerolog.New(zerolog.NewTestWriter(t))
	idx := indexer.New(&logger, indexer.Options{})

	nw := &stubNetwork{id: "evm:1", finality: 10}
	idx.RegisterNetwork(nw)

	ing := nullingress.New("null:test")
	if err := idx.AddIngress(context.Background(), "evm:1", ing); err != nil {
		t.Fatalf("AddIngress: %v", err)
	}

	// Egress that records newHeads.
	eg := &recordingEgress{interested: func(k indexer.EventKind, _, _ string) bool {
		return k == indexer.KindNewHead
	}}
	idx.Attach(eg)

	// Push two distinct heads and a dup — expect 2 deliveries.
	ing.Push(headEvent("evm:1", "null:test", 100, "0xA", "0x0"))
	ing.Push(headEvent("evm:1", "null:test", 100, "0xA", "0x0")) // dup
	ing.Push(headEvent("evm:1", "null:test", 101, "0xB", "0xA"))

	// Drain: give the pump goroutine + indexer a chance to run.
	deadline := time.After(2 * time.Second)
	for {
		if eg.count() >= 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for 2 deliveries, got %d", eg.count())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if got := eg.count(); got != 2 {
		t.Fatalf("want 2 deliveries (dup suppressed), got %d", got)
	}
	// Verify state poller was called per source.
	nw.mu.Lock()
	defer nw.mu.Unlock()
	if n := len(nw.suggestions["null:test"]); n < 2 {
		t.Fatalf("SuggestLatestBlock should fire on every observation (incl. dup), got %d", n)
	}
}

func TestIntegration_NullIngress_FilterRefcountAndTeardown(t *testing.T) {
	logger := zerolog.New(zerolog.NewTestWriter(t))
	idx := indexer.New(&logger, indexer.Options{})

	nw := &stubNetwork{id: "evm:1"}
	idx.RegisterNetwork(nw)
	ing := nullingress.New("null:test")
	if err := idx.AddIngress(context.Background(), "evm:1", ing); err != nil {
		t.Fatal(err)
	}

	params := []interface{}{"logs", map[string]interface{}{"topics": []string{"0x1"}}}
	h1, _ := idx.EnsureFilter(context.Background(), "evm:1", "logs", params)
	h2, _ := idx.EnsureFilter(context.Background(), "evm:1", "logs", params)
	if h1 != h2 {
		t.Fatalf("same params must hash identically, got %q vs %q", h1, h2)
	}
	if n := len(ing.ActiveFilters()); n != 1 {
		t.Fatalf("EnsureFilter should register exactly one filter on the ingress, got %d", n)
	}

	// Only the second release tears down (refcount hits 0).
	idx.ReleaseFilter(context.Background(), "evm:1", "logs", h1)
	if n := len(ing.ActiveFilters()); n != 1 {
		t.Fatalf("filter still held by 1 client — expected 1 active, got %d", n)
	}
	idx.ReleaseFilter(context.Background(), "evm:1", "logs", h1)
	if n := len(ing.ActiveFilters()); n != 0 {
		t.Fatalf("filter should be torn down, got %d active", n)
	}
}

// --- stubs ------------------------------------------------------------

type stubNetwork struct {
	id       string
	finality int64

	mu          sync.Mutex
	suggestions map[string][]int64
}

func (s *stubNetwork) Id() string           { return s.id }
func (s *stubNetwork) FinalityDepth() int64 { return s.finality }
func (s *stubNetwork) SuggestLatestBlock(sourceID string, block int64) {
	s.mu.Lock()
	if s.suggestions == nil {
		s.suggestions = make(map[string][]int64)
	}
	s.suggestions[sourceID] = append(s.suggestions[sourceID], block)
	s.mu.Unlock()
}

type recordingEgress struct {
	interested func(indexer.EventKind, string, string) bool

	mu   sync.Mutex
	recv []indexer.IndexedEvent
}

func (r *recordingEgress) Name() string { return "test:recording" }
func (r *recordingEgress) InterestedIn(k indexer.EventKind, networkID, filterHash string) bool {
	return r.interested(k, networkID, filterHash)
}
func (r *recordingEgress) Deliver(ev indexer.IndexedEvent) {
	r.mu.Lock()
	r.recv = append(r.recv, ev)
	r.mu.Unlock()
}
func (r *recordingEgress) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.recv)
}

func headEvent(networkID, sourceID string, num int64, hash, parent string) indexer.StreamEvent {
	payload := json.RawMessage(`{"number":"0x0","hash":"` + hash + `","parentHash":"` + parent + `"}`)
	return indexer.StreamEvent{
		Kind:      indexer.KindNewHead,
		NetworkId: networkID,
		SourceId:  sourceID,
		Block:     indexer.BlockRef{Number: num, Hash: hash, ParentHash: parent},
		Payload:   payload,
	}
}
