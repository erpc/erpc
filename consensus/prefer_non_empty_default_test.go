package consensus

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
)

// Pin the default-outcome cases Aram's review flagged (rules.go): the
// preferNonEmpty+ReturnError path must match main except for the deliberate
// deterministic empty-tie dispute.
func TestPreferNonEmpty_DefaultOutcomes(t *testing.T) {
	cfg := &config{
		maxParticipants:         5,
		agreementThreshold:      2,
		disputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
		preferNonEmpty:          true,
		lowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
	}
	u1, u2 := taggedUpstream("u1"), taggedUpstream("u2")
	u3, u4, u5 := taggedUpstream("u3"), taggedUpstream("u4"), taggedUpstream("u5")

	winFor := func(results []*execResult) *slotResult {
		lg := zerolog.Nop()
		analysis := newConsensusAnalysis(&lg, context.Background(), cfg, results)
		e := &executor{consensusPolicy: &consensusPolicy{logger: &lg, config: cfg}}
		return e.determineWinner(context.Background(), &lg, analysis)
	}
	mkMissing := func(u common.Upstream) *execResult {
		return &execResult{Upstream: u, Err: common.NewErrEndpointMissingData(nil, u)}
	}
	mkRevert := func(u common.Upstream) *execResult {
		return &execResult{Upstream: u, Err: common.NewErrEndpointExecutionException(
			common.NewErrJsonRpcExceptionInternal(3, 3, "execution reverted", nil, nil))}
	}
	mkData := func(u common.Upstream) *execResult {
		jrpc, _ := common.NewJsonRpcResponse(1, "0xabc", nil)
		return &execResult{Result: common.NewNormalizedResponse().WithJsonRpcResponse(jrpc), Upstream: u}
	}
	mkEmpty := func(u common.Upstream) *execResult {
		jrpc, _ := common.NewJsonRpcResponse(1, nil, nil)
		return &execResult{Result: common.NewNormalizedResponse().WithJsonRpcResponse(jrpc), Upstream: u}
	}

	// missing×2 + data×2: map-order tie (no waiver) — must NOT deterministically dispute.
	for i := 0; i < 4; i++ {
		w := winFor([]*execResult{mkMissing(u1), mkMissing(u2), mkData(u3), mkData(u4)})
		assert.False(t, common.HasErrorCode(w.Error, common.ErrCodeConsensusDispute),
			"missing×2+data×2 must not deterministically dispute (no waiver opt-in)")
	}

	// revert×3 + data×2: agreed execution error wins (same as main).
	w := winFor([]*execResult{mkRevert(u1), mkRevert(u2), mkRevert(u3), mkData(u4), mkData(u5)})
	assert.Error(t, w.Error, "revert×3+data×2 must surface the agreed revert")

	// empty×2 + data×2: deliberate deterministic dispute (removes main's map-order flip).
	w = winFor([]*execResult{mkEmpty(u1), mkEmpty(u2), mkData(u3), mkData(u4)})
	assert.True(t, common.HasErrorCode(w.Error, common.ErrCodeConsensusDispute),
		"empty×2+data×2 must deterministically dispute")
}
