package consensus

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

// TestErrUpstreamsExhausted_NotMisclassifiedAsConsensusError verifies that
// ErrUpstreamsExhausted is always classified as infrastructure error even when
// its Cause contains consensus-valid errors from other participants via the
// shared ErrorsByUpstream map.
func TestErrUpstreamsExhausted_NotMisclassifiedAsConsensusError(t *testing.T) {
	t.Run("exhausted wrapping execution exception stays infrastructure", func(t *testing.T) {
		// Simulate the shared ErrorsByUpstream map containing an execution
		// revert from another consensus participant.
		errMap := &sync.Map{}
		execRevert := common.NewErrEndpointExecutionException(
			common.NewErrJsonRpcExceptionInternal(3, 3, "execution reverted", nil, nil),
		)
		errMap.Store("upstream-A", execRevert)

		exhaustedErr := common.NewErrUpstreamsExhausted(
			nil, errMap, "proj", "evm:999", "eth_call",
			100*time.Millisecond, 1, 0, 0, 1,
		)

		// Confirm HasErrorCode DOES find the wrapped execution exception
		// (this is the traversal that previously caused misclassification).
		assert.True(t, common.HasErrorCode(exhaustedErr, common.ErrCodeEndpointExecutionException),
			"HasErrorCode should find the wrapped execution exception")

		r := &execResult{Err: exhaustedErr}
		classifyAndHashResponse(r, nil, &config{})

		assert.Equal(t, ResponseTypeInfrastructureError, r.CachedResponseType,
			"ErrUpstreamsExhausted must be infrastructure regardless of wrapped errors")
		assert.Equal(t, "error:exhausted", r.CachedHash)
	})

	t.Run("exhausted without wrapped errors stays infrastructure", func(t *testing.T) {
		errMap := &sync.Map{}
		exhaustedErr := common.NewErrUpstreamsExhausted(
			nil, errMap, "proj", "evm:999", "eth_call",
			50*time.Millisecond, 1, 0, 0, 0,
		)

		r := &execResult{Err: exhaustedErr}
		classifyAndHashResponse(r, nil, &config{})

		assert.Equal(t, ResponseTypeInfrastructureError, r.CachedResponseType)
		assert.Equal(t, "error:exhausted", r.CachedHash)
	})
}

// TestConsensusWithExhaustedParticipants_StillReachesThreshold verifies that
// when 3 participants return an execution revert and 2 return ErrUpstreamsExhausted
// (wrapping the same reverts via the shared map), the consensus engine correctly
// returns the agreed-upon revert instead of a dispute.
func TestConsensusWithExhaustedParticipants_StillReachesThreshold(t *testing.T) {
	lg := zerolog.Nop()

	revertErr := common.NewErrEndpointExecutionException(
		common.NewErrJsonRpcExceptionInternal(3, 3, "execution reverted", nil, nil),
	)

	// Shared ErrorsByUpstream — simulates participants 1-3 storing their errors.
	errMap := &sync.Map{}
	errMap.Store("upstream-A", revertErr)

	exhaustedErr := common.NewErrUpstreamsExhausted(
		nil, errMap, "proj", "evm:999", "eth_call",
		100*time.Millisecond, 1, 0, 0, 1,
	)

	responses := []*execResult{
		{Err: revertErr, Index: 0},
		{Err: revertErr, Index: 1},
		{Err: revertErr, Index: 2},
		{Err: exhaustedErr, Index: 3},
		{Err: exhaustedErr, Index: 4},
	}

	cfg := &config{
		maxParticipants:         5,
		agreementThreshold:      2,
		disputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
		lowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
	}

	// Build the analysis manually (classifyAndHashResponse + grouping) since
	// newConsensusAnalysis requires a non-nil failsafe Execution for context.
	analysis := &consensusAnalysis{
		config:            cfg,
		groups:            make(map[string]*responseGroup),
		totalParticipants: len(responses),
		method:            "eth_call",
	}
	for _, r := range responses {
		classifyAndHashResponse(r, nil, cfg)
		if r.CachedResponseType != ResponseTypeInfrastructureError {
			analysis.validParticipants++
		}
		group, exists := analysis.groups[r.CachedHash]
		if !exists {
			group = &responseGroup{
				Hash:         r.CachedHash,
				ResponseType: r.CachedResponseType,
				ResponseSize: r.CachedResponseSize,
			}
			analysis.groups[r.CachedHash] = group
		}
		group.Count++
		group.Results = append(group.Results, r)
		if r.Err != nil && group.RepresentativeError == nil {
			group.RepresentativeError = r.Err
		}
	}

	// Exhausted participants must not count as valid.
	assert.Equal(t, 3, analysis.validParticipants,
		"only the 3 actual revert responses should be valid participants")

	// The 3 reverts form one consensus-error group; the 2 exhausted form one infra group.
	validGroups := analysis.getValidGroups()
	require.Len(t, validGroups, 1, "should have exactly 1 valid group (the reverts)")
	assert.Equal(t, 3, validGroups[0].Count)
	assert.Equal(t, ResponseTypeConsensusError, validGroups[0].ResponseType)

	// determineWinner must return the agreed-upon revert, not a dispute.
	e := &executor{consensusPolicy: &consensusPolicy{logger: &lg, config: cfg}}
	winner := e.determineWinner(context.Background(), &lg, analysis)

	require.NotNil(t, winner)
	assert.NotNil(t, winner.Error, "winner should be the consensus error (revert)")
	assert.False(t, common.HasErrorCode(winner.Error, common.ErrCodeConsensusDispute),
		"must NOT return ErrConsensusDispute when 3/5 agree")
	assert.True(t, common.HasErrorCode(winner.Error, common.ErrCodeEndpointExecutionException),
		"winner should be the agreed-upon execution revert")
}

// TestPreferNonEmpty_TiedEmptyAndNonEmptyAboveThreshold locks Rule 7: when
// empty and non-empty both meet threshold with equal counts under
// PreferNonEmpty + AcceptMostCommon, the non-empty must win every time.
// Gating PreferNonEmpty on getBestByCount() used to flake — Go map iteration
// could return the non-empty as "best", skip Rule 7, and let the generic
// threshold winner randomly pick empty.
func TestPreferNonEmpty_TiedEmptyAndNonEmptyAboveThreshold(t *testing.T) {
	lg := zerolog.Nop()
	emptyJrpc, err := common.NewJsonRpcResponse(1, []interface{}{}, nil)
	require.NoError(t, err)
	nonEmptyJrpc, err := common.NewJsonRpcResponse(1, "0x33", nil)
	require.NoError(t, err)
	emptyResp := common.NewNormalizedResponse().WithJsonRpcResponse(emptyJrpc)
	nonEmptyResp := common.NewNormalizedResponse().WithJsonRpcResponse(nonEmptyJrpc)

	cfg := &config{
		maxParticipants:         6,
		agreementThreshold:      3,
		preferNonEmpty:          true,
		disputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
		lowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
	}
	e := &executor{consensusPolicy: &consensusPolicy{logger: &lg, config: cfg}}

	for i := 0; i < 200; i++ {
		analysis := &consensusAnalysis{
			config:             cfg,
			groups:             make(map[string]*responseGroup),
			totalParticipants:  6,
			validParticipants:  6,
			collectedResponses: 6,
			method:             "eth_call",
		}
		analysis.groups["empty"] = &responseGroup{
			Hash:          "empty",
			Count:         3,
			ResponseType:  ResponseTypeEmpty,
			LargestResult: emptyResp,
			HasResult:     true,
		}
		analysis.groups["nonempty"] = &responseGroup{
			Hash:          "nonempty",
			Count:         3,
			ResponseType:  ResponseTypeNonEmpty,
			LargestResult: nonEmptyResp,
			HasResult:     true,
			ResponseSize:  4,
		}

		winner := e.determineWinner(context.Background(), &lg, analysis)
		require.NotNil(t, winner, "iter %d", i)
		require.NoError(t, winner.Error, "iter %d: unexpected error %v", i, winner.Error)
		require.NotNil(t, winner.Result, "iter %d", i)
		assert.False(t, winner.Result.IsResultEmptyish(context.Background()),
			"iter %d: PreferNonEmpty must pick non-empty on tied above-threshold groups", i)
	}
}
