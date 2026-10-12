package indexer

import (
	"encoding/json"
	"testing"
)

func TestDedupWindow_MarkNewKey(t *testing.T) {
	w := NewDedupWindow(4)
	if !w.Mark("a", false) {
		t.Fatalf("first Mark of a new key must return true")
	}
	if w.Mark("a", false) {
		t.Fatalf("second Mark of the same key and state must return false")
	}
}

func TestDedupWindow_StateChangeIsDelivered(t *testing.T) {
	w := NewDedupWindow(4)
	steps := []struct {
		removed bool
		want    bool
	}{
		{false, true},  // add
		{false, false}, // duplicate add
		{true, true},   // remove
		{true, false},  // duplicate remove
		{false, true},  // re-add
	}
	for i, s := range steps {
		if got := w.Mark("k", s.removed); got != s.want {
			t.Fatalf("step %d (removed=%v): got %v want %v", i, s.removed, got, s.want)
		}
	}
}

func TestDedupWindow_EvictsOldestPastCapacity(t *testing.T) {
	w := NewDedupWindow(3)
	for _, k := range []string{"a", "b", "c"} {
		if !w.Mark(k, false) {
			t.Fatalf("Mark(%q) must be true while window has room", k)
		}
	}
	// Window: a, b, c. Adding "d" evicts "a"; window becomes b, c, d.
	if !w.Mark("d", false) {
		t.Fatalf("Mark(d) must be true")
	}
	// "a" was evicted and is therefore newly markable.
	if !w.Mark("a", false) {
		t.Fatalf("a should be evicted and therefore newly markable")
	}
	// "c" has not been evicted yet — still a dupe.
	if w.Mark("c", false) {
		t.Fatalf("c should still be in-window")
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

func TestDedupKeyForFilter_Logs(t *testing.T) {
	payload := json.RawMessage(`{"blockHash":"0xabc","transactionHash":"0xdef","logIndex":"0x1","removed":false}`)
	got := DedupKeyForFilter(SubTypeLogs, payload)
	want := "0xabc:0xdef:0x1"
	if got != want {
		t.Fatalf("log dedup key: got %q want %q", got, want)
	}
}

func TestDedupKeyForFilter_LogsRemovedFlagIsNotPartOfKey(t *testing.T) {
	// removed is the log's state, tracked by DedupWindow per key; the key
	// is the log's identity.
	in := DedupKeyForFilter(SubTypeLogs, json.RawMessage(`{"blockHash":"0xabc","transactionHash":"0xdef","logIndex":"0x1","removed":false}`))
	out := DedupKeyForFilter(SubTypeLogs, json.RawMessage(`{"blockHash":"0xabc","transactionHash":"0xdef","logIndex":"0x1","removed":true}`))
	if in != out {
		t.Fatalf("removed=true / removed=false must share a key, got %q vs %q", in, out)
	}
}

func TestDedupKeyForFilter_LogsMissingIdentityReturnsEmpty(t *testing.T) {
	for _, p := range []string{
		`{}`,
		`{"transactionHash":"0xdef","logIndex":"0x1"}`,
		`{"blockHash":"0xabc","logIndex":"0x1"}`,
		`{"blockHash":"0xabc","transactionHash":"0xdef"}`,
		`not json`,
	} {
		if got := DedupKeyForFilter(SubTypeLogs, json.RawMessage(p)); got != "" {
			t.Fatalf("payload %s: want empty key, got %q", p, got)
		}
	}
}

func TestDedupKeyForFilter_CaseInsensitive(t *testing.T) {
	a := DedupKeyForFilter(SubTypeLogs, json.RawMessage(`{"blockHash":"0xABC","transactionHash":"0xDEF","logIndex":"0xA"}`))
	b := DedupKeyForFilter(SubTypeLogs, json.RawMessage(`{"blockHash":"0xabc","transactionHash":"0xdef","logIndex":"0xa"}`))
	if a != b {
		t.Fatalf("log keys must ignore hex case, got %q vs %q", a, b)
	}
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
