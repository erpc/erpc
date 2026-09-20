package consensus

import (
	"errors"
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

func permanentMissingData(code int, msg string) error {
	err := common.NewErrEndpointMissingData(
		common.NewErrJsonRpcExceptionInternal(code, common.JsonRpcErrorNumber(code), msg, nil, nil),
		nil,
	)
	err.(*common.ErrEndpointMissingData).WithPermanentMissingData(true)
	return err
}

func transientMissingData(code int, msg string) error {
	return common.NewErrEndpointMissingData(
		common.NewErrJsonRpcExceptionInternal(code, common.JsonRpcErrorNumber(code), msg, nil, nil),
		nil,
	)
}

// TestErrorToConsensusHash_PermanentMissingDataAgreesAcrossWireCodes locks that
// Alchemy -32007 and QuickNode -32009 (both permanent ErrEndpointMissingData for
// the same skipped/absent slot) share one consensus hash so returnError does
// not dispute them.
func TestErrorToConsensusHash_PermanentMissingDataAgreesAcrossWireCodes(t *testing.T) {
	t.Parallel()

	alchemy := permanentMissingData(-32007, "Slot 500281501 was skipped")
	quicknode := permanentMissingData(-32009, "Slot 500281501 was skipped, or missing in long-term storage")

	hAlchemy := errorToConsensusHash(alchemy)
	hQuicknode := errorToConsensusHash(quicknode)

	assert.Equal(t, "ErrEndpointMissingData:permanent", hAlchemy)
	assert.Equal(t, hAlchemy, hQuicknode,
		"-32007 and -32009 permanent missing-data must hash identically")

	// Wire codes must still disagree — only the consensus hash collapses them.
	var jreAlchemy, jreQN *common.ErrJsonRpcExceptionInternal
	require.True(t, errors.As(alchemy, &jreAlchemy))
	require.True(t, errors.As(quicknode, &jreQN))
	assert.EqualValues(t, -32007, jreAlchemy.NormalizedCode())
	assert.EqualValues(t, -32009, jreQN.NormalizedCode())
}

func TestErrorToConsensusHash_TransientMissingDataKeepsWireCode(t *testing.T) {
	t.Parallel()

	tipLag := transientMissingData(-32004, "Block not available for slot 500281501")
	assert.Equal(t, "jsonrpc:-32004", errorToConsensusHash(tipLag),
		"transient missing-data must not join the permanent class")

	// Transient tip-lag must not share the permanent skipped-slot hash.
	perm := permanentMissingData(-32007, "Slot skipped")
	assert.NotEqual(t, errorToConsensusHash(tipLag), errorToConsensusHash(perm))
}

func TestErrorToConsensusHash_DistinctJsonRpcErrorsDoNotCollide(t *testing.T) {
	t.Parallel()

	client := common.NewErrEndpointClientSideException(
		common.NewErrJsonRpcExceptionInternal(-32602, common.JsonRpcErrorNumber(-32602), "invalid params", nil, nil),
	)
	unsupported := common.NewErrEndpointUnsupported(
		common.NewErrJsonRpcExceptionInternal(-32601, common.JsonRpcErrorNumber(-32601), "method not found", nil, nil),
	)
	exec := common.NewErrEndpointExecutionException(
		common.NewErrJsonRpcExceptionInternal(3, 3, "execution reverted", nil, nil),
	)

	hClient := errorToConsensusHash(client)
	hUnsupported := errorToConsensusHash(unsupported)
	hExec := errorToConsensusHash(exec)
	hPerm := errorToConsensusHash(permanentMissingData(-32007, "skipped"))

	assert.Equal(t, "jsonrpc:-32602", hClient)
	assert.Equal(t, "jsonrpc:-32601", hUnsupported)
	assert.Equal(t, "jsonrpc:3", hExec)
	assert.NotEqual(t, hClient, hUnsupported)
	assert.NotEqual(t, hClient, hExec)
	assert.NotEqual(t, hPerm, hClient)
	assert.NotEqual(t, hPerm, hUnsupported)
	assert.NotEqual(t, hPerm, hExec)
}

// TestClassifyAndHash_PermanentSlotSkippedAgreesUnderReturnError: 2-of-N with
// Alchemy -32007 vs QuickNode -32009 must agree as ConsensusError, not
// ErrConsensusDispute.
func TestClassifyAndHash_PermanentSlotSkippedAgreesUnderReturnError(t *testing.T) {
	t.Parallel()

	lg := zerolog.Nop()
	alchemy := permanentMissingData(-32007, "Slot skipped")
	quicknode := permanentMissingData(-32009, "Slot skipped, or missing in long-term storage")

	responses := []*execResult{
		{Err: alchemy, Index: 0},
		{Err: quicknode, Index: 1},
	}
	cfg := &config{
		maxParticipants:         2,
		agreementThreshold:      2,
		disputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
		lowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
	}

	analysis := &consensusAnalysis{
		config:            cfg,
		groups:            make(map[string]*responseGroup),
		totalParticipants: len(responses),
		method:            "getBlock",
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
		if r.Err != nil && group.FirstError == nil {
			group.FirstError = r.Err
		}
	}

	validGroups := analysis.getValidGroups()
	require.Len(t, validGroups, 1, "Alchemy -32007 and QN -32009 must form one group")
	assert.Equal(t, 2, validGroups[0].Count)
	assert.Equal(t, ResponseTypeConsensusError, validGroups[0].ResponseType)
	assert.Equal(t, "ErrEndpointMissingData:permanent", validGroups[0].Hash)

	e := &executor{consensusPolicy: &consensusPolicy{logger: &lg, config: cfg}}
	winner := e.determineWinner(&lg, analysis)

	require.NotNil(t, winner)
	assert.True(t, common.HasErrorCode(winner.Error, common.ErrCodeEndpointMissingData),
		"winner must be missing-data, not a dispute")
	assert.False(t, common.HasErrorCode(winner.Error, common.ErrCodeConsensusDispute),
		"must NOT return ErrConsensusDispute when permanent skips agree")
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
		if r.Err != nil && group.FirstError == nil {
			group.FirstError = r.Err
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
	winner := e.determineWinner(&lg, analysis)

	require.NotNil(t, winner)
	assert.NotNil(t, winner.Error, "winner should be the consensus error (revert)")
	assert.False(t, common.HasErrorCode(winner.Error, common.ErrCodeConsensusDispute),
		"must NOT return ErrConsensusDispute when 3/5 agree")
	assert.True(t, common.HasErrorCode(winner.Error, common.ErrCodeEndpointExecutionException),
		"winner should be the agreed-upon execution revert")
}
