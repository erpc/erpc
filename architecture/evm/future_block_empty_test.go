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
// (spec §6.4): at or below Latest an empty is a miss; above it, in the band
// or future, it is a miss once, then final. Tags / unknown blocks and an
// unknown head fail open (never final).
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
	assert.Equal(t, common.MissRetry, HeadRefusalOf(ctx, band).Miss(&common.RefusalLeg{}), "the band's one miss")
	assert.True(t, EmptyResultBeyondConfidence(ctx, band), "band after its miss → final")
	future := mk(nw, 9_000_000)
	assert.False(t, EmptyResultBeyondConfidence(ctx, future), "future → a retryable miss first")
	assert.Equal(t, common.MissRetry, HeadRefusalOf(ctx, future).Miss(&common.RefusalLeg{}), "the future block's one miss")
	assert.True(t, EmptyResultBeyondConfidence(ctx, future), "future after its miss → final")
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
// rule: the network-level enforceNonNullBlock converts the first null for a
// block above Latest, band or future, into a retryable miss; after the
// request's one miss it returns the null as the upstream gave it, matching
// the upstream-level markUnexpectedEmpty guard. A behind-head numeric null
// still errors.
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

	for _, c := range []struct {
		name string
		bn   int64
	}{{"future", 1002}, {"band", 1001}} {
		resp := mkNullResp(c.bn)
		_, err := enforceNonNullBlock(ctx, resp.Request(), resp)
		assert.Error(t, err, "a %s block's first null is a retryable miss", c.name)
		assert.Equal(t, common.MissRetry, HeadRefusalOf(ctx, resp.Request()).Miss(&common.RefusalLeg{}))
		got, err := enforceNonNullBlock(ctx, resp.Request(), resp)
		assert.NoError(t, err, "a %s block's null after its miss is the answer", c.name)
		assert.True(t, got.IsResultEmptyish())
	}

	// Behind head: null is genuinely missing/pruned → still errors (unchanged).
	resp2 := mkNullResp(999)
	got2, err2 := enforceNonNullBlock(ctx, resp2.Request(), resp2)
	assert.Error(t, err2, "behind-head numeric block null still errors")
	assert.Nil(t, got2)
}
