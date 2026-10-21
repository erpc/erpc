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
	hash, err := idx.EnsureFilter(context.Background(), "evm:1", indexer.SubTypeNewPendingTransactions,
		[]interface{}{indexer.SubTypeNewPendingTransactions})
	if err != nil {
		t.Fatalf("EnsureFilter: %v", err)
	}

	eg := &recordingEgress{interested: func(k indexer.EventKind, _, filterHash string) bool {
		return k == indexer.KindPendingTx && filterHash == hash
	}}
	idx.Attach(eg)

	// Push two distinct transactions and a dup — expect 2 deliveries.
	ing.Push(pendingTxEvent("evm:1", "null:test", hash, "0xa"))
	ing.Push(pendingTxEvent("evm:1", "null:test", hash, "0xa")) // dup
	ing.Push(pendingTxEvent("evm:1", "null:test", hash, "0xb"))

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
	time.Sleep(50 * time.Millisecond)
	if got := eg.count(); got != 2 {
		t.Fatalf("want 2 deliveries (dup suppressed), got %d", got)
	}
}

type stubNetwork struct {
	id string
}

func (s *stubNetwork) Id() string                     { return s.id }
func (s *stubNetwork) DedupDropped(indexer.EventKind) {}
func (s *stubNetwork) DedupEntriesAdded(int)          {}
func (s *stubNetwork) Undelivered(indexer.EventKind)  {}

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

func pendingTxEvent(networkID, sourceID, filterHash, txHash string) indexer.StreamEvent {
	return indexer.StreamEvent{
		Kind:       indexer.KindPendingTx,
		NetworkId:  networkID,
		SourceId:   sourceID,
		FilterHash: filterHash,
		Payload:    json.RawMessage(`"` + txHash + `"`),
	}
}
