package indexer

import (
	"encoding/json"
	"strings"
	"sync"

	"github.com/erpc/erpc/common"
)

// DedupKeyForFilter returns the identity of a filter notification, or "" if
// it cannot be established (the caller then delivers without deduping).
//
//   - logs: blockHash + txHash + logIndex, all required. The removed flag is
//     not part of the key but the state DedupWindow tracks per key, so
//     add, remove and re-add are each delivered once.
//   - newPendingTransactions: the tx hash, from a string or an object's hash.
//
// Keys are lowercased because hex on the wire is case-insensitive.
func DedupKeyForFilter(subType string, result json.RawMessage) string {
	switch subType {
	case SubTypeLogs:
		var log struct {
			BlockHash string `json:"blockHash"`
			TxHash    string `json:"transactionHash"`
			LogIndex  string `json:"logIndex"`
		}
		if err := common.SonicCfg.Unmarshal(result, &log); err != nil {
			return ""
		}
		if log.BlockHash == "" || log.TxHash == "" || log.LogIndex == "" {
			return ""
		}
		return strings.ToLower(log.BlockHash + ":" + log.TxHash + ":" + log.LogIndex)
	case SubTypeNewPendingTransactions:
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
	}
	return ""
}

// logRemoved returns a log payload's "removed" flag, false if unparseable.
func logRemoved(payload json.RawMessage) bool {
	if len(payload) == 0 {
		return false
	}
	var probe struct {
		Removed bool `json:"removed"`
	}
	if err := common.SonicCfg.Unmarshal(payload, &probe); err != nil {
		return false
	}
	return probe.Removed
}

// DedupWindow is a bounded FIFO map from key to its last delivered removed
// state. Keys past capacity evict the oldest. Safe for concurrent use.
type DedupWindow struct {
	size int

	mu    sync.Mutex
	state map[string]bool
	order []string
}

// NewDedupWindow returns a DedupWindow holding up to size keys, or
// DefaultDedupWindowSize if size is not positive.
func NewDedupWindow(size int) *DedupWindow {
	if size <= 0 {
		size = DefaultDedupWindowSize
	}
	return &DedupWindow{
		size:  size,
		state: make(map[string]bool),
	}
}

// Mark records key in the given removed state and reports whether it should
// be delivered: the key is new or its last delivered state differs.
func (w *DedupWindow) Mark(key string, removed bool) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if last, ok := w.state[key]; ok {
		if last == removed {
			return false
		}
		w.state[key] = removed
		return true
	}
	// Decoded strings may alias the notification buffer; the window
	// outlives it.
	key = strings.Clone(key)
	w.state[key] = removed
	w.order = append(w.order, key)

	if len(w.order) > w.size {
		evict := len(w.order) - w.size
		for _, old := range w.order[:evict] {
			delete(w.state, old)
		}
		w.order = w.order[evict:]
	}
	return true
}
