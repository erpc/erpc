package indexer

import (
	"encoding/json"
	"testing"
)

func mark(w *DedupWindow, key string, removed bool) bool {
	deliver, _ := w.Mark(key, removed)
	return deliver
}

func TestDedupWindow_MarkNewKey(t *testing.T) {
	w := NewDedupWindow(4)
	if !mark(w, "a", false) {
		t.Fatalf("first Mark of a new key must return true")
	}
	if mark(w, "a", false) {
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
		if got := mark(w, "k", s.removed); got != s.want {
			t.Fatalf("step %d (removed=%v): got %v want %v", i, s.removed, got, s.want)
		}
	}
}

func TestDedupWindow_EvictsOldestPastCapacity(t *testing.T) {
	w := NewDedupWindow(3)
	for _, k := range []string{"a", "b", "c"} {
		if deliver, added := w.Mark(k, false); !deliver || added != 1 {
			t.Fatalf("Mark(%q) must deliver and add a key while window has room, got %v %d", k, deliver, added)
		}
	}
	// Window: a, b, c. Adding "d" evicts "a"; window becomes b, c, d.
	if deliver, added := w.Mark("d", false); !deliver || added != 0 {
		t.Fatalf("Mark(d) must deliver and keep the full window's size, got %v %d", deliver, added)
	}
	// "a" was evicted and is therefore newly markable.
	if !mark(w, "a", false) {
		t.Fatalf("a should be evicted and therefore newly markable")
	}
	// "c" has not been evicted yet — still a dupe.
	if mark(w, "c", false) {
		t.Fatalf("c should still be in-window")
	}
	if n := w.Close(); n != 3 {
		t.Fatalf("Close must report the 3 keys held, got %d", n)
	}
	if deliver, added := w.Mark("c", false); !deliver || added != 0 {
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

func TestDedupKeyForFilter_NewFlashblocks(t *testing.T) {
	got := DedupKeyForFilter(SubTypeNewFlashblocks, json.RawMessage(`{"payload_id":"0xABC","index":0}`))
	if got != "0xabc:0" {
		t.Fatalf("flashblock key: got %q", got)
	}
	got = DedupKeyForFilter(SubTypeNewFlashblocks, json.RawMessage(`{"payload_id":"0xabc","index":1,"diff":{}}`))
	if got != "0xabc:1" {
		t.Fatalf("flashblock key with diff: got %q", got)
	}
}

func TestDedupKeyForFilter_NewFlashblocksBlockSnapshot(t *testing.T) {
	// A Chainstack snapshot is a block object: hash may be zero and each
	// transaction blockHash null. Identity is number + transactionsRoot.
	const zeroHash = "0x0000000000000000000000000000000000000000000000000000000000000000"
	a := DedupKeyForFilter(SubTypeNewFlashblocks, json.RawMessage(
		`{"number":"0x10","transactionsRoot":"0xABC","hash":"`+zeroHash+`","transactions":[{"blockHash":null}]}`,
	))
	b := DedupKeyForFilter(SubTypeNewFlashblocks, json.RawMessage(
		`{"number":"0x10","transactionsRoot":"0xabc","hash":"`+zeroHash+`","transactions":[{"blockHash":null}]}`,
	))
	if a != "0x10:0xabc" || a != b {
		t.Fatalf("same number and transactionsRoot must share a key, got %q vs %q", a, b)
	}
	c := DedupKeyForFilter(SubTypeNewFlashblocks, json.RawMessage(
		`{"number":"0x10","transactionsRoot":"0xdef","hash":"`+zeroHash+`"}`,
	))
	if c == a || c != "0x10:0xdef" {
		t.Fatalf("a new transactionsRoot at the same number is a different flashblock, got %q", c)
	}
	// number + transactionsRoot wins when a Base identity is also present.
	both := DedupKeyForFilter(SubTypeNewFlashblocks, json.RawMessage(
		`{"number":"0x10","transactionsRoot":"0xabc","payload_id":"0xFFF","index":0}`,
	))
	if both != a {
		t.Fatalf("block snapshot identity must win over payload_id, got %q", both)
	}
}

func TestDedupKeyForFilter_NewFlashblocksPartialBlockUsesPayloadID(t *testing.T) {
	missingRoot := DedupKeyForFilter(SubTypeNewFlashblocks, json.RawMessage(`{"number":"0x10","payload_id":"0xABC","index":0}`))
	if missingRoot != "0xabc:0" {
		t.Fatalf("missing transactionsRoot must use payload_id and index, got %q", missingRoot)
	}
	missingNumber := DedupKeyForFilter(SubTypeNewFlashblocks, json.RawMessage(`{"transactionsRoot":"0xabc","payload_id":"0xABC","index":1}`))
	if missingNumber != "0xabc:1" {
		t.Fatalf("missing number must use payload_id and index, got %q", missingNumber)
	}
}

func TestDedupKeyForFilter_NewFlashblocksMissingIdentityReturnsEmpty(t *testing.T) {
	for _, p := range []string{
		`{}`,
		`{"payload_id":"0xabc"}`,
		`{"index":0}`,
		`{"number":"0x10"}`,
		`{"transactionsRoot":"0xabc"}`,
		`not json`,
	} {
		if got := DedupKeyForFilter(SubTypeNewFlashblocks, json.RawMessage(p)); got != "" {
			t.Fatalf("payload %s: want empty key, got %q", p, got)
		}
	}
}

func TestDedupKeyForFilter_UnknownSubTypeReturnsEmpty(t *testing.T) {
	got := DedupKeyForFilter("unknown", json.RawMessage(`{}`))
	if got != "" {
		t.Fatalf("unknown subType should return empty key, got %q", got)
	}
}
