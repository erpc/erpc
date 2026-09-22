package indexer

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/erpc/erpc/common"
)

// DedupKeyForFilter returns a stable identifier for a filter-subscription
// notification payload, or "" when the payload cannot be parsed (caller
// should then fan out without deduping, since we can't prove it's a dupe).
//
//   - logs: blockHash + txHash + logIndex + removed flag (the removed
//     flag is part of the key so a reorg-out log isn't collapsed with its
//     matching reorg-in log — both are delivered to clients).
//   - newPendingTransactions: the tx hash, whether the upstream returned
//     a raw string or an object with a .hash field.
func DedupKeyForFilter(subType string, result json.RawMessage) string {
	switch subType {
	case SubTypeLogs:
		var log struct {
			BlockHash string `json:"blockHash"`
			TxHash    string `json:"transactionHash"`
			LogIndex  string `json:"logIndex"`
			Removed   bool   `json:"removed"`
		}
		if err := common.SonicCfg.Unmarshal(result, &log); err != nil {
			return ""
		}
		return fmt.Sprintf("%s:%s:%s:%t", log.BlockHash, log.TxHash, log.LogIndex, log.Removed)
	case SubTypeNewPendingTransactions:
		var asString string
		if err := common.SonicCfg.Unmarshal(result, &asString); err == nil && asString != "" {
			return asString
		}
		var asObj struct {
			Hash string `json:"hash"`
		}
		if err := common.SonicCfg.Unmarshal(result, &asObj); err == nil {
			return asObj.Hash
		}
	}
	return ""
}

// DedupWindow is a bounded FIFO seen-set: keys added past the window's
// capacity evict the oldest entries. It is safe for concurrent use; callers
// typically hold one per (network, subType, paramsHash) fan-out group.
type DedupWindow struct {
	size int

	mu    sync.Mutex
	seen  map[string]struct{}
	order []string
}

// NewDedupWindow returns a DedupWindow sized to hold up to `size` keys
// before the oldest entries are evicted. Pass 0 to use DefaultDedupWindowSize.
func NewDedupWindow(size int) *DedupWindow {
	if size <= 0 {
		size = DefaultDedupWindowSize
	}
	return &DedupWindow{
		size:  size,
		seen:  make(map[string]struct{}, size),
		order: make([]string, 0, size),
	}
}

// Mark records a key and returns true if it's newly seen. Returns false if
// the key is already in the window (caller should drop the duplicate).
func (w *DedupWindow) Mark(key string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.seen[key]; ok {
		return false
	}
	w.seen[key] = struct{}{}
	w.order = append(w.order, key)

	if len(w.order) > w.size {
		evict := len(w.order) - w.size
		for _, old := range w.order[:evict] {
			delete(w.seen, old)
		}
		w.order = w.order[evict:]
	}
	return true
}
