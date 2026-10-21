package indexer

import (
	"encoding/json"
	"testing"
)

func mark(w *DedupWindow, key string) bool {
	deliver, _ := w.Mark(key)
	return deliver
}

func TestDedupWindow_MarkNewKey(t *testing.T) {
	w := NewDedupWindow(4)
	if !mark(w, "a") {
		t.Fatalf("first Mark of a new key must return true")
	}
	if mark(w, "a") {
		t.Fatalf("second Mark of the same key must return false")
	}
}

func TestDedupWindow_EvictsOldestPastCapacity(t *testing.T) {
	w := NewDedupWindow(3)
	for _, k := range []string{"a", "b", "c"} {
		if deliver, added := w.Mark(k); !deliver || added != 1 {
			t.Fatalf("Mark(%q) must deliver and add a key while window has room, got %v %d", k, deliver, added)
		}
	}
	// Window: a, b, c. Adding "d" evicts "a"; window becomes b, c, d.
	if deliver, added := w.Mark("d"); !deliver || added != 0 {
		t.Fatalf("Mark(d) must deliver and keep the full window's size, got %v %d", deliver, added)
	}
	// "a" was evicted and is therefore newly markable.
	if !mark(w, "a") {
		t.Fatalf("a should be evicted and therefore newly markable")
	}
	// "c" has not been evicted yet — still a dupe.
	if mark(w, "c") {
		t.Fatalf("c should still be in-window")
	}
	if n := w.Close(); n != 3 {
		t.Fatalf("Close must report the 3 keys held, got %d", n)
	}
	if deliver, added := w.Mark("c"); !deliver || added != 0 {
		t.Fatalf("a closed window must deliver and hold nothing, got %v %d", deliver, added)
	}
}

func TestDedupWindow_ZeroSizeUsesDefault(t *testing.T) {
	w := NewDedupWindow(0)
	if w.size != DefaultDedupWindowSize {
		t.Fatalf("size with 0 arg should default to %d, got %d", DefaultDedupWindowSize, w.size)
	}
}

// An idle window must not pin capacity-sized storage: every active filter
// holds one.
func TestDedupWindow_EmptyWindowDoesNotPreallocate(t *testing.T) {
	w := NewDedupWindow(DefaultDedupWindowSize)
	if cap(w.order) != 0 {
		t.Fatalf("empty window must not preallocate order, cap=%d", cap(w.order))
	}
}

func TestDedupKeyForFilter_CaseInsensitive(t *testing.T) {
	if DedupKeyForFilter(SubTypeNewPendingTransactions, json.RawMessage(`"0xBEEF"`)) != "0xbeef" {
		t.Fatalf("pending tx key must ignore hex case")
	}
}

func TestDedupKeyForFilter_PendingTxAsString(t *testing.T) {
	got := DedupKeyForFilter(SubTypeNewPendingTransactions, json.RawMessage(`"0xbeef"`))
	if got != "0xbeef" {
		t.Fatalf("pendingTx string key: got %q", got)
	}
}

func TestDedupKeyForFilter_PendingTxAsObject(t *testing.T) {
	got := DedupKeyForFilter(SubTypeNewPendingTransactions, json.RawMessage(`{"hash":"0xbeef"}`))
	if got != "0xbeef" {
		t.Fatalf("pendingTx object key: got %q", got)
	}
}

func TestDedupKeyForFilter_UnknownSubTypeReturnsEmpty(t *testing.T) {
	got := DedupKeyForFilter("unknown", json.RawMessage(`{}`))
	if got != "" {
		t.Fatalf("unknown subType should return empty key, got %q", got)
	}
}
