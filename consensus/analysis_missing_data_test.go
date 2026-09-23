package consensus

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/erpc/erpc/architecture/svm"
	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func missingData(code int, permanent bool) error {
	err := common.NewErrEndpointMissingData(
		common.NewErrJsonRpcExceptionInternal(code, common.JsonRpcErrorMissingData, "missing", nil, nil),
		nil,
	)
	if permanent {
		err.(*common.ErrEndpointMissingData).WithPermanentMissingData(true)
	}
	return err
}

func TestErrorToConsensusHash_PermanentAndTransientMissingDataDiffer(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "jsonrpc:-32014", errorToConsensusHash(missingData(-32004, false)))
	assert.Equal(t, "jsonrpc:-32014:permanent", errorToConsensusHash(missingData(-32007, true)))
	assert.Equal(t, errorToConsensusHash(missingData(-32007, true)), errorToConsensusHash(missingData(-32009, true)))
}

// svmUpstreamError runs an agave-shaped error body through the real SVM
// normalizer, the way a consensus participant receives it.
func svmUpstreamError(t *testing.T, upstreamId string, code int, msg string) *execResult {
	t.Helper()
	up := common.NewFakeUpstream(upstreamId)
	up.Config().Type = common.UpstreamTypeSvm
	r := &http.Response{StatusCode: 200, Header: http.Header{}}
	err := svm.NewJsonRpcErrorExtractor().Extract(r, nil,
		common.MustNewJsonRpcResponse(1, nil, common.NewErrJsonRpcExceptionExternal(code, msg, "")), up)
	require.Error(t, err)
	return &execResult{Err: err, Upstream: up}
}

func TestErrorToConsensusHash_SvmMissingDataGroupsByVerdictNotProviderCode(t *testing.T) {
	t.Parallel()

	skippedLocal := svmUpstreamError(t, "a", -32007, "Slot 500281501 was skipped, or missing due to ledger jump to recent snapshot").Err
	skippedArchive := svmUpstreamError(t, "b", -32009, "Slot 500281501 was skipped, or missing in long-term storage").Err
	notYet := svmUpstreamError(t, "c", -32004, "Block not available for slot 500281501").Err
	pruned := svmUpstreamError(t, "d", -32001, "Block 500281501 cleaned up, does not exist on node. First available block: 500300000").Err

	assert.Equal(t, "jsonrpc:-32014:permanent", errorToConsensusHash(skippedLocal))
	assert.Equal(t, errorToConsensusHash(skippedLocal), errorToConsensusHash(skippedArchive))
	assert.Equal(t, "jsonrpc:-32014", errorToConsensusHash(notYet))
	assert.Equal(t, errorToConsensusHash(notYet), errorToConsensusHash(pruned))
}

func returnErrorConfig() *config {
	return &config{
		maxParticipants:         2,
		agreementThreshold:      2,
		disputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
		lowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
	}
}

func TestDetermineWinner_SkippedSlotAgreesAcrossProviderCodes(t *testing.T) {
	t.Parallel()
	lg := zerolog.Nop()
	cfg := returnErrorConfig()

	responses := []*execResult{
		svmUpstreamError(t, "local-ledger", -32007, "Slot 500281501 was skipped, or missing due to ledger jump to recent snapshot"),
		svmUpstreamError(t, "archive", -32009, "Slot 500281501 was skipped, or missing in long-term storage"),
	}
	analysis := newConsensusAnalysis(&lg, context.Background(), cfg, responses)
	require.Len(t, analysis.getValidGroups(), 1)

	e := &executor{consensusPolicy: &consensusPolicy{logger: &lg, config: cfg}}
	winner := e.determineWinner(&lg, analysis)
	require.NotNil(t, winner)
	assert.False(t, common.HasErrorCode(winner.Error, common.ErrCodeConsensusDispute))
	assert.True(t, common.HasErrorCode(winner.Error, common.ErrCodeEndpointMissingData))
	assert.True(t, common.IsPermanentlyMissingData(winner.Error))

	var jre *common.ErrJsonRpcExceptionInternal
	require.True(t, errors.As(winner.Error, &jre))
	assert.Equal(t, common.JsonRpcErrorNumber(-32007), jre.WireCode(), "the client still receives the provider's own code")
}

func TestDetermineWinner_NotYetIndexedStillDisputesSkipped(t *testing.T) {
	t.Parallel()
	lg := zerolog.Nop()
	cfg := returnErrorConfig()

	responses := []*execResult{
		svmUpstreamError(t, "lagging", -32004, "Block not available for slot 500281501"),
		svmUpstreamError(t, "local-ledger", -32007, "Slot 500281501 was skipped, or missing due to ledger jump to recent snapshot"),
	}
	analysis := newConsensusAnalysis(&lg, context.Background(), cfg, responses)
	require.Len(t, analysis.getValidGroups(), 2)

	e := &executor{consensusPolicy: &consensusPolicy{logger: &lg, config: cfg}}
	winner := e.determineWinner(&lg, analysis)
	require.NotNil(t, winner)
	assert.True(t, common.HasErrorCode(winner.Error, common.ErrCodeConsensusDispute))
}
