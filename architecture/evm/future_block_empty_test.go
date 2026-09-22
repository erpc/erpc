package evm

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
)

// TestEmptyResultBeyondConfidence pins the default (blockHead) rule for empty results:
// any concrete block above the latest head is not-yet-produced (return the truthful
// empty, do not retry), while the head and below stay retryable. Tags / unknown blocks
// and an unknown head fail open (never treated as beyond confidence).
func TestEmptyResultBeyondConfidence(t *testing.T) {
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

	assert.False(t, emptyResultBeyondConfidence(ctx, mk(nw, 999)), "behind head → retryable, not beyond")
	assert.False(t, emptyResultBeyondConfidence(ctx, mk(nw, 1000)), "exactly head → not beyond")
	assert.True(t, emptyResultBeyondConfidence(ctx, mk(nw, 1001)), "head+1 → beyond, return empty")
	assert.True(t, emptyResultBeyondConfidence(ctx, mk(nw, 9_000_000)), "far ahead → beyond")
	assert.False(t, emptyResultBeyondConfidence(ctx, mk(nw, 0)), "no concrete block (tag/hash) → never beyond")

	// Unknown head (0) → fail open.
	noHead := &queryTestNetwork{cfg: nw.cfg, latest: 0}
	assert.False(t, emptyResultBeyondConfidence(ctx, mk(noHead, 9_000_000)), "unknown head → fail open")
}

// TestEmptyResultBeyondConfidence_Finalized pins the finalizedBlock confidence level:
// the comparison head becomes the finalized head, so blocks between finalized and
// latest (unfinalized) count as beyond-confidence (their empty is legitimately
// not-yet-confirmed), while blocks at/below the finalized head stay retryable.
func TestEmptyResultBeyondConfidence_Finalized(t *testing.T) {
	ctx := context.Background()
	nw := &queryTestNetwork{
		cfg: &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{
			ChainId:               1,
			EmptyResultConfidence: common.AvailbilityConfidenceFinalized,
		}},
		latest:    1000,
		finalized: 900,
	}
	mk := func(bn int64) *common.NormalizedRequest {
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x1",false]}`))
		req.SetNetwork(nw)
		req.SetEvmBlockNumber(bn)
		return req
	}

	assert.False(t, emptyResultBeyondConfidence(ctx, mk(899)), "below finalized → retryable")
	assert.False(t, emptyResultBeyondConfidence(ctx, mk(900)), "exactly finalized → not beyond")
	assert.True(t, emptyResultBeyondConfidence(ctx, mk(901)), "finalized+1 (unfinalized) → beyond at finalized confidence")
	assert.True(t, emptyResultBeyondConfidence(ctx, mk(1000)), "latest head but unfinalized → beyond at finalized confidence")
}

// TestEnforceNonNullBlock_FutureBlockNotErrored pins the #6 fix: the network-level
// enforceNonNullBlock must NOT convert a numeric null into a missing-data error when
// the block is beyond confidence (not yet produced) — it returns the truthful null,
// matching the upstream-level markUnexpectedEmpty guard. A behind-head numeric null
// still errors (genuinely missing/pruned data).
func TestEnforceNonNullBlock_FutureBlockNotErrored(t *testing.T) {
	ctx := context.Background()
	nw := &queryTestNetwork{
		cfg:    &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 1}},
		latest: 1000,
	}
	mkNullResp := func(bn int64) *common.NormalizedResponse {
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x3e9",false]}`))
		req.SetNetwork(nw)
		req.SetEvmBlockNumber(bn)
		jrr, _ := common.NewJsonRpcResponse(1, nil, nil)
		return common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
	}

	// Beyond confidence (head+1): null is legitimate → returned as-is, no error.
	resp := mkNullResp(1001)
	got, err := enforceNonNullBlock(ctx, resp.Request(), resp)
	assert.NoError(t, err, "beyond-confidence numeric block null must not be force-errored")
	assert.NotNil(t, got)
	assert.True(t, got.IsResultEmptyish())

	// Behind head: null is genuinely missing/pruned → still errors (unchanged).
	resp2 := mkNullResp(999)
	got2, err2 := enforceNonNullBlock(ctx, resp2.Request(), resp2)
	assert.Error(t, err2, "behind-head numeric block null still errors")
	assert.Nil(t, got2)
}
