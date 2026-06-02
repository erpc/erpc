package evm

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
)

// TestEmptyResultIsFutureBlock pins the tight future-block rule for empty
// results: a concrete block more than one beyond the network head is a
// not-yet-produced block (return the truthful empty, do not retry), while the
// head and head+1 stay retryable. Tags / unknown blocks and an unknown head
// fail open (never treated as future).
func TestEmptyResultIsFutureBlock(t *testing.T) {
	ctx := context.Background()
	nw := &queryTestNetwork{
		cfg:    &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 1}},
		latest: 1000,
	}

	mk := func(n *queryTestNetwork, bn int64) *common.NormalizedRequest {
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x1",false]}`))
		req.SetNetwork(n)
		req.SetEvmBlockNumber(bn)
		return req
	}

	assert.False(t, emptyResultIsFutureBlock(ctx, mk(nw, 999)), "behind head → retryable, not future")
	assert.False(t, emptyResultIsFutureBlock(ctx, mk(nw, 1000)), "exactly head → not future")
	assert.False(t, emptyResultIsFutureBlock(ctx, mk(nw, 1001)), "head+1 (next block) → still retried, not future")
	assert.True(t, emptyResultIsFutureBlock(ctx, mk(nw, 1002)), "head+2 → future, return empty")
	assert.True(t, emptyResultIsFutureBlock(ctx, mk(nw, 9_000_000)), "far future → future")
	assert.False(t, emptyResultIsFutureBlock(ctx, mk(nw, 0)), "no concrete block (tag/hash) → never future")

	// Unknown head (0) → fail open.
	noHead := &queryTestNetwork{cfg: nw.cfg, latest: 0}
	assert.False(t, emptyResultIsFutureBlock(ctx, mk(noHead, 9_000_000)), "unknown head → fail open")
}

// TestEmptyResultIsFutureBlock_ConfiguredDistance pins MaxFutureBlockRetryDistance:
// 0 means only the head is retryable, a larger value widens the window, and a
// negative value disables the guard (retry all empties).
func TestEmptyResultIsFutureBlock_ConfiguredDistance(t *testing.T) {
	ctx := context.Background()
	mk := func(distance int64, bn int64) *common.NormalizedRequest {
		nw := &queryTestNetwork{
			cfg:    &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 1, MaxFutureBlockRetryDistance: &distance}},
			latest: 1000,
		}
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x1",false]}`))
		req.SetNetwork(nw)
		req.SetEvmBlockNumber(bn)
		return req
	}

	// distance 0: only the head itself is retryable.
	assert.False(t, emptyResultIsFutureBlock(ctx, mk(0, 1000)), "distance 0: head → not future")
	assert.True(t, emptyResultIsFutureBlock(ctx, mk(0, 1001)), "distance 0: head+1 → future")

	// distance 5: head..head+5 retryable.
	assert.False(t, emptyResultIsFutureBlock(ctx, mk(5, 1005)), "distance 5: head+5 → not future")
	assert.True(t, emptyResultIsFutureBlock(ctx, mk(5, 1006)), "distance 5: head+6 → future")

	// negative distance disables the guard (retry all empties).
	assert.False(t, emptyResultIsFutureBlock(ctx, mk(-1, 9_000_000)), "negative distance → guard disabled")
}
