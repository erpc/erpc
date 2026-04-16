package indexer

import (
	"encoding/json"
	"testing"
)

func TestDedupWindow_MarkNewKey(t *testing.T) {
	w := NewDedupWindow(4)
	if !w.Mark("a") {
		t.Fatalf("first Mark of a new key must return true")
	}
	if w.Mark("a") {
		t.Fatalf("second Mark of the same key must return false")
	}
}

func TestDedupWindow_EvictsOldestPastCapacity(t *testing.T) {
	w := NewDedupWindow(3)
	for _, k := range []string{"a", "b", "c"} {
		if !w.Mark(k) {
			t.Fatalf("Mark(%q) must be true while window has room", k)
		}
	}
	// Window: a, b, c. Adding "d" evicts "a"; window becomes b, c, d.
	if !w.Mark("d") {
		t.Fatalf("Mark(d) must be true")
	}
	// "a" was evicted and is therefore newly markable.
	if !w.Mark("a") {
		t.Fatalf("a should be evicted and therefore newly markable")
	}
	// "c" has not been evicted yet — still a dupe.
	if w.Mark("c") {
		t.Fatalf("c should still be in-window")
	}
}

func TestDedupWindow_ZeroSizeUsesDefault(t *testing.T) {
	w := NewDedupWindow(0)
	if w.size != DefaultDedupWindowSize {
		t.Fatalf("size with 0 arg should default to %d, got %d", DefaultDedupWindowSize, w.size)
	}
}

func TestDedupKeyForFilter_Logs(t *testing.T) {
	payload := json.RawMessage(`{"blockHash":"0xabc","transactionHash":"0xdef","logIndex":"0x1","removed":false}`)
	got := DedupKeyForFilter(SubTypeLogs, payload)
	want := "0xabc:0xdef:0x1:false"
	if got != want {
		t.Fatalf("log dedup key: got %q want %q", got, want)
	}
}

func TestDedupKeyForFilter_LogsRemovedFlagIsPartOfKey(t *testing.T) {
	// A reorged-out log must not collapse with its matching reorged-in log;
	// both are delivered to clients so the removed flag must be in the key.
	in := DedupKeyForFilter(SubTypeLogs, json.RawMessage(`{"blockHash":"0xabc","transactionHash":"0xdef","logIndex":"0x1","removed":false}`))
	out := DedupKeyForFilter(SubTypeLogs, json.RawMessage(`{"blockHash":"0xabc","transactionHash":"0xdef","logIndex":"0x1","removed":true}`))
	if in == out {
		t.Fatalf("removed=true / removed=false must produce distinct dedup keys")
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
