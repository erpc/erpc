package indexer

import (
	"encoding/json"
	"strings"
	"sync"

	"github.com/erpc/erpc/common"
)

// DedupKeyForFilter returns the identity of a newPendingTransactions
// notification, the tx hash from a string or an object's hash, or "" if it
// cannot be established (the caller then delivers without deduping). Keys
// are lowercased because hex on the wire is case-insensitive.
func DedupKeyForFilter(subType string, result json.RawMessage) string {
	if subType != SubTypeNewPendingTransactions {
		return ""
	}
	var asString string
	if err := common.SonicCfg.Unmarshal(result, &asString); err == nil && asString != "" {
		return strings.ToLower(asString)
	}
	var asObj struct {
		Hash string `json:"hash"`
	}
	if err := common.SonicCfg.Unmarshal(result, &asObj); err == nil {
		return strings.ToLower(asObj.Hash)
	}
	return ""
}

// DedupWindow is a bounded FIFO set of delivered keys. Keys past capacity
// evict the oldest. Safe for concurrent use.
type DedupWindow struct {
	size int

	mu     sync.Mutex
	seen   map[string]struct{}
	order  []string
	closed bool
}

// NewDedupWindow returns a DedupWindow holding up to size keys, or
// DefaultDedupWindowSize if size is not positive.
func NewDedupWindow(size int) *DedupWindow {
	if size <= 0 {
		size = DefaultDedupWindowSize
	}
	return &DedupWindow{
		size: size,
		seen: make(map[string]struct{}),
	}
}

// Mark records key and reports whether it should be delivered (the key is
// new) and how many keys the window gained: 1 while it fills, else 0. A
// closed window delivers everything and holds nothing.
func (w *DedupWindow) Mark(key string) (deliver bool, added int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return true, 0
	}
	if _, ok := w.seen[key]; ok {
		return false, 0
	}
	// Decoded strings may alias the notification buffer; the window
	// outlives it.
	key = strings.Clone(key)
	w.seen[key] = struct{}{}
	w.order = append(w.order, key)

	if len(w.order) > w.size {
		evict := len(w.order) - w.size
		for _, old := range w.order[:evict] {
			delete(w.seen, old)
		}
		w.order = w.order[evict:]
		return true, 1 - evict
	}
	return true, 1
}

// Close empties the window for good and returns how many keys it held.
func (w *DedupWindow) Close() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(w.order)
	w.closed, w.seen, w.order = true, nil, nil
	return n
}
