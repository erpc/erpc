package evm

import (
	"context"
	"sync"

	"github.com/erpc/erpc/common"
)

const blockHistoryCap = 2048

// blockHistory is a bounded, per-network number→hash store that feeds the
// continuity checks. Last-write-wins per number (so a reorg's new canonical hash
// replaces the old), bounded by insertion order to cap memory. It implements
// integrity.History.
type blockHistory struct {
	mu       sync.RWMutex
	byNumber map[int64]string
	order    []int64
	cap      int
}

func newBlockHistory(capacity int) *blockHistory {
	return &blockHistory{byNumber: make(map[int64]string, capacity), cap: capacity}
}

func (h *blockHistory) HashAt(number int64) (string, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	v, ok := h.byNumber[number]
	return v, ok
}

func (h *blockHistory) Observe(number int64, hash string) {
	if number < 0 || hash == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, exists := h.byNumber[number]; !exists {
		h.order = append(h.order, number)
		if len(h.order) > h.cap {
			delete(h.byNumber, h.order[0])
			h.order = h.order[1:]
		}
	}
	h.byNumber[number] = hash
}

var historyStore sync.Map // networkId -> *blockHistory

// networkHistory returns the per-network block history, creating it on first
// use. Returns nil only when the network is nil.
func networkHistory(n common.Network) *blockHistory {
	if n == nil {
		return nil
	}
	if v, ok := historyStore.Load(n.Id()); ok {
		return v.(*blockHistory)
	}
	created := newBlockHistory(blockHistoryCap)
	actual, _ := historyStore.LoadOrStore(n.Id(), created)
	return actual.(*blockHistory)
}

func isBlockMethod(methodLower string) bool {
	return methodLower == "eth_getblockbynumber" || methodLower == "eth_getblockbyhash"
}

// observeBlock records a validated block's number→hash so later requests can be
// linked against it by the continuity checks.
func observeBlock(ctx context.Context, h *blockHistory, rs *common.NormalizedResponse) {
	if h == nil || rs == nil {
		return
	}
	jrr, err := rs.JsonRpcResponse(ctx)
	if err != nil || jrr == nil {
		return
	}
	var blk struct {
		Number string `json:"number"`
		Hash   string `json:"hash"`
	}
	if common.SonicCfg.Unmarshal(jrr.GetResultBytes(), &blk) != nil || blk.Number == "" || blk.Hash == "" {
		return
	}
	if n, err := common.HexToInt64(blk.Number); err == nil {
		h.Observe(n, blk.Hash)
	}
}
