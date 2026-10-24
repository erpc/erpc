package evm

import (
	"context"
	"fmt"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
)

// blockReq is eth_getBlockByNumber(bn) on n; bn 0 asks the latest tag.
func blockReq(n common.Network, bn int64) *common.NormalizedRequest {
	param := "latest"
	if bn > 0 {
		param = fmt.Sprintf("0x%x", bn)
	}
	req := common.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":[%q,false]}`, param)))
	req.SetNetwork(n)
	return req
}

// TestEmptyResultBeyondConfidence pins the refusal rule's rows for empty results
// (spec §6.4): at or below Latest an empty is a miss; in the band (up to the
// ceiling) it is a miss once, then final; above a fresh ceiling it is final.
// Tags / unknown blocks and an unknown head fail open (never final).
func TestEmptyResultBeyondConfidence(t *testing.T) {
	ctx := context.Background()
	nw := &queryTestNetwork{
		cfg:     &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 1}},
		latest:  1000,
		ceiling: 1005,
	}

	mk := func(n *queryTestNetwork, bn int64) *common.NormalizedRequest { return blockReq(n, bn) }

	assert.False(t, EmptyResultBeyondConfidence(ctx, mk(nw, 999)), "behind head → retryable, not final")
	assert.False(t, EmptyResultBeyondConfidence(ctx, mk(nw, 1000)), "exactly head → not final")
	band := mk(nw, 1003)
	assert.False(t, EmptyResultBeyondConfidence(ctx, band), "band → a retryable miss first")
	assert.True(t, HeadRefusalOf(ctx, band).Miss(), "the band's one miss")
	assert.True(t, EmptyResultBeyondConfidence(ctx, band), "band after its miss → final")
	assert.True(t, EmptyResultBeyondConfidence(ctx, mk(nw, 9_000_000)), "above a fresh ceiling → final")
	assert.False(t, EmptyResultBeyondConfidence(ctx, mk(nw, 0)), "no concrete block (tag/hash) → never final")

	// Unknown head (0) → fail open.
	noHead := &queryTestNetwork{cfg: nw.cfg, latest: 0}
	assert.False(t, EmptyResultBeyondConfidence(ctx, mk(noHead, 9_000_000)), "unknown head → fail open")
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
	mk := func(bn int64) *common.NormalizedRequest { return blockReq(nw, bn) }

	assert.False(t, EmptyResultBeyondConfidence(ctx, mk(899)), "below finalized → retryable")
	assert.False(t, EmptyResultBeyondConfidence(ctx, mk(900)), "exactly finalized → not beyond")
	assert.True(t, EmptyResultBeyondConfidence(ctx, mk(901)), "finalized+1 (unfinalized) → beyond at finalized confidence")
	assert.True(t, EmptyResultBeyondConfidence(ctx, mk(1000)), "latest head but unfinalized → beyond at finalized confidence")
}

// TestEnforceNonNullBlock_FutureBlockNotErrored pins the #6 fix under the refusal
// rule: the network-level enforceNonNullBlock must NOT convert a numeric null into
// a missing-data error when the block is future (above a fresh ceiling) — it
// returns the truthful null, matching the upstream-level markUnexpectedEmpty
// guard. A band block's first null and a behind-head numeric null still error.
func TestEnforceNonNullBlock_FutureBlockNotErrored(t *testing.T) {
	ctx := context.Background()
	nw := &queryTestNetwork{
		cfg:     &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 1}},
		latest:  1000,
		ceiling: 1001,
	}
	mkNullResp := func(bn int64) *common.NormalizedResponse {
		req := blockReq(nw, bn)
		jrr, _ := common.NewJsonRpcResponse(1, nil, nil)
		return common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
	}

	// Future (above the ceiling): null is legitimate → returned as-is, no error.
	resp := mkNullResp(1002)
	got, err := enforceNonNullBlock(ctx, resp.Request(), resp)
	assert.NoError(t, err, "future numeric block null must not be force-errored")
	assert.NotNil(t, got)
	assert.True(t, got.IsResultEmptyish())

	// Band (head+1, at the ceiling): the first null is a retryable miss.
	band := mkNullResp(1001)
	_, err1 := enforceNonNullBlock(ctx, band.Request(), band)
	assert.Error(t, err1, "a band block's first null is a retryable miss")

	// Behind head: null is genuinely missing/pruned → still errors (unchanged).
	resp2 := mkNullResp(999)
	got2, err2 := enforceNonNullBlock(ctx, resp2.Request(), resp2)
	assert.Error(t, err2, "behind-head numeric block null still errors")
	assert.Nil(t, got2)
}
