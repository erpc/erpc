package consensus

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

func nonEmptyReceiptResp() *common.NormalizedResponse {
	jrr := common.MustNewJsonRpcResponseFromBytes(
		[]byte("1"),
		[]byte(`{"status":"0x1","blockNumber":"0xe57e13","logs":[{"logIndex":"0x0"}]}`),
		nil,
	)
	return common.NewNormalizedResponse().WithJsonRpcResponse(jrr)
}

// TestConsensus_ContentValidationErrorDoesNotDisputeMajority locks in the
// architectural contract that the logIndex-integrity fix relies on: a corrupt
// upstream response is converted to an ErrEndpointContentValidation by the EVM
// post-forward hook *before* consensus sees it, and consensus already excludes
// error responses from the preferLargerResponses logic. So two honest agreeing
// upstreams win cleanly — no ErrConsensusDispute — without any consensus-side
// integrity handling.
func TestConsensus_ContentValidationErrorDoesNotDisputeMajority(t *testing.T) {
	lg := zerolog.Nop()
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getTransactionReceipt","params":["0xabf61f02a6c77b28a9465a2256e26d2fe25714b60bb8edabb7d0ce794fba932e"]}`))
	ctx := context.WithValue(context.Background(), common.RequestContextKey, req)

	responses := []*execResult{
		{Result: nonEmptyReceiptResp(), Index: 0},
		{Result: nonEmptyReceiptResp(), Index: 1},
		{Err: common.NewErrEndpointContentValidation(nil, nil), Index: 2},
	}

	cfg := &config{
		maxParticipants:         3,
		agreementThreshold:      2,
		disputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
		lowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
		preferNonEmpty:          true,
		preferLargerResponses:   true,
		logger:                  &lg,
	}

	analysis := newConsensusAnalysis(&lg, ctx, cfg, responses)
	require.Equal(t, 2, analysis.validParticipants, "the content-validation error must not count as a valid participant")

	e := &executor{consensusPolicy: &consensusPolicy{logger: &lg, config: cfg}}
	winner := e.determineWinner(&lg, analysis)

	require.NotNil(t, winner)
	require.Nil(t, winner.Error, "two honest agreeing upstreams must win, not ErrConsensusDispute")
	require.NotNil(t, winner.Result)
}
