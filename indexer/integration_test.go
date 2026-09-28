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

// TestIntegration_NullIngress_EndToEnd drives ingest, dedup and fan-out
// through a transport-free ingress.
func TestIntegration_NullIngress_EndToEnd(t *testing.T) {
	logger := zerolog.New(zerolog.NewTestWriter(t))
	idx := indexer.New(&logger, indexer.Options{})

	nw := &stubNetwork{id: "evm:1"}
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
	nw.mu.Lock()
	defer nw.mu.Unlock()
	if n := len(nw.suggestions["null:test"]); n < 2 {
		t.Fatalf("SuggestLatestBlock should fire on every observation (incl. dup), got %d", n)
	}
}

type stubNetwork struct {
	id string

	mu          sync.Mutex
	suggestions map[string][]int64
}

func (s *stubNetwork) Id() string { return s.id }
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
		Block:     indexer.BlockRef{Number: num, Hash: hash},
		Payload:   payload,
	}
}
