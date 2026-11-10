package consensus

import (
	"context"
	"errors"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resultOf builds an attributed successful participant with an arbitrary
// JSON result (string, number, object).
func resultOf(t *testing.T, ups common.Upstream, value interface{}, index int) *execResult {
	t.Helper()
	jrpc, err := common.NewJsonRpcResponse(1, value, nil)
	require.NoError(t, err)
	return &execResult{
		Result:   common.NewNormalizedResponse().WithJsonRpcResponse(jrpc),
		Upstream: ups,
		Index:    index,
	}
}

func revertedErr() error {
	return common.NewErrEndpointExecutionException(
		common.NewErrJsonRpcExceptionInternal(3, 3, "execution reverted", nil, nil),
	)
}

func invalidParamsErr() error {
	return common.NewErrEndpointClientSideException(
		common.NewErrJsonRpcExceptionInternal(-32602, -32602, "invalid params", nil, nil),
	)
}

// analyzeFor runs analysis the way the executor does: with the request bound
// to the context so the method, ignoreFields and the block-head leader resolve.
func analyzeFor(t *testing.T, cfg *config, method string, leader common.Upstream, responses []*execResult) *consensusAnalysis {
	t.Helper()
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":[]}`))
	if leader != nil {
		req.SetNetwork(&leaderNetwork{leader: leader})
	}
	ctx := context.WithValue(context.Background(), common.RequestContextKey, req)
	lg := zerolog.Nop()
	return newConsensusAnalysis(&lg, ctx, cfg, responses)
}

func winnerResultString(t *testing.T, w *slotResult) string {
	t.Helper()
	require.NotNil(t, w)
	require.NoError(t, w.Error)
	require.NotNil(t, w.Result)
	jrr, err := w.Result.JsonRpcResponse()
	require.NoError(t, err)
	return jrr.GetResultString()
}

func TestDetermineWinner_Outcomes(t *testing.T) {
	a := taggedUpstream("ups-a")
	b := taggedUpstream("ups-b")
	c := taggedUpstream("ups-c")
	d := taggedUpstream("ups-d")

	acceptMost := common.ConsensusDisputeBehaviorAcceptMostCommonValidResult
	returnErr := common.ConsensusDisputeBehaviorReturnError
	lowAcceptMost := common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult

	type tc struct {
		name    string
		cfg     *config
		method  string
		leader  common.Upstream
		results func(t *testing.T) []*execResult
		// exactly one of the expectations below is set; none = an empty result
		wantValue  string
		wantCode   common.ErrorCode
		wantErrAny []common.ErrorCode
	}

	cases := []tc{
		{
			name:   "tx broadcast returns the single accepted hash despite failures",
			cfg:    &config{maxParticipants: 3, agreementThreshold: 3},
			method: "eth_sendRawTransaction",
			results: func(t *testing.T) []*execResult {
				return []*execResult{
					errorFrom(a, errors.New("dial timeout"), 0),
					resultFrom(t, b, "0xhash", 1),
					errorFrom(c, errors.New("connection reset"), 2),
				}
			},
			wantValue: `"0xhash"`,
		},
		{
			name:   "highest value wins only among values meeting the threshold",
			cfg:    &config{maxParticipants: 3, agreementThreshold: 2, preferHighestValueFor: map[string][]string{"eth_getTransactionCount": {"result"}}},
			method: "eth_getTransactionCount",
			results: func(t *testing.T) []*execResult {
				return []*execResult{
					resultFrom(t, a, "0x5", 0),
					resultFrom(t, b, "0x5", 1),
					resultFrom(t, c, "0x7", 2),
				}
			},
			wantValue: `"0x5"`,
		},
		{
			name:   "highest value with zero threshold treats a single vote as enough",
			cfg:    &config{maxParticipants: 4, agreementThreshold: 0, preferHighestValueFor: map[string][]string{"eth_getTransactionCount": {"result"}}},
			method: "eth_getTransactionCount",
			results: func(t *testing.T) []*execResult {
				return []*execResult{
					resultFrom(t, a, "0x5", 0),
					resultFrom(t, b, "not-a-number", 1),
					errorFrom(c, revertedErr(), 2),
					resultOf(t, d, 9, 3),
				}
			},
			wantValue: `9`,
		},
		{
			name:   "highest value disputes when no value reaches the threshold",
			cfg:    &config{maxParticipants: 3, agreementThreshold: 2, preferHighestValueFor: map[string][]string{"eth_getTransactionCount": {"result"}}},
			method: "eth_getTransactionCount",
			results: func(t *testing.T) []*execResult {
				return []*execResult{
					resultFrom(t, a, "0x5", 0),
					resultFrom(t, b, "0x6", 1),
					resultFrom(t, c, "0x7", 2),
				}
			},
			wantCode: common.ErrCodeConsensusDispute,
		},
		{
			name:   "highest value compares nested fields of object results",
			cfg:    &config{maxParticipants: 3, agreementThreshold: 1, preferHighestValueFor: map[string][]string{"eth_getBlockByNumber": {"number"}}},
			method: "eth_getBlockByNumber",
			results: func(t *testing.T) []*execResult {
				return []*execResult{
					resultOf(t, a, map[string]interface{}{"number": "0x10", "hash": "0xaa"}, 0),
					resultOf(t, b, map[string]interface{}{"number": "0x20", "hash": "0xbb"}, 1),
					resultOf(t, c, map[string]interface{}{"hash": "0xcc"}, 2),
				}
			},
			wantValue: `{"hash":"0xbb","number":"0x20"}`,
		},
		{
			name:   "highest value without any extractable value falls back to hash agreement",
			cfg:    &config{maxParticipants: 3, agreementThreshold: 2, preferHighestValueFor: map[string][]string{"eth_getTransactionCount": {"result"}}},
			method: "eth_getTransactionCount",
			results: func(t *testing.T) []*execResult {
				return []*execResult{
					resultFrom(t, a, "pending", 0),
					resultFrom(t, b, "pending", 1),
					resultFrom(t, c, "other", 2),
				}
			},
			wantValue: `"pending"`,
		},
		{
			name:   "only-block-head-leader dispute returns the leader's answer",
			cfg:    &config{maxParticipants: 3, agreementThreshold: 2, disputeBehavior: common.ConsensusDisputeBehaviorOnlyBlockHeadLeader},
			method: "eth_call",
			leader: c,
			results: func(t *testing.T) []*execResult {
				return []*execResult{
					resultFrom(t, a, "0xa", 0),
					resultFrom(t, b, "0xb", 1),
					resultFrom(t, c, "0xc", 2),
				}
			},
			wantValue: `"0xc"`,
		},
		{
			name:   "only-block-head-leader dispute surfaces the leader's infrastructure error",
			cfg:    &config{maxParticipants: 3, agreementThreshold: 2, disputeBehavior: common.ConsensusDisputeBehaviorOnlyBlockHeadLeader},
			method: "eth_call",
			leader: c,
			results: func(t *testing.T) []*execResult {
				return []*execResult{
					resultFrom(t, a, "0xa", 0),
					resultFrom(t, b, "0xb", 1),
					errorFrom(c, common.NewErrEndpointServerSideException(errors.New("leader down"), nil, 503), 2),
				}
			},
			wantCode: common.ErrCodeEndpointServerSideException,
		},
		{
			name:   "only-block-head-leader dispute without an elected leader disputes",
			cfg:    &config{maxParticipants: 3, agreementThreshold: 2, disputeBehavior: common.ConsensusDisputeBehaviorOnlyBlockHeadLeader},
			method: "eth_call",
			results: func(t *testing.T) []*execResult {
				return []*execResult{
					resultFrom(t, a, "0xa", 0),
					resultFrom(t, b, "0xb", 1),
				}
			},
			wantCode: common.ErrCodeConsensusDispute,
		},
		{
			name:   "only-block-head-leader dispute behavior does not override met threshold",
			cfg:    &config{maxParticipants: 3, agreementThreshold: 2, disputeBehavior: common.ConsensusDisputeBehaviorOnlyBlockHeadLeader},
			method: "eth_call",
			leader: c,
			results: func(t *testing.T) []*execResult {
				return []*execResult{
					resultFrom(t, a, "0xa", 0),
					resultFrom(t, b, "0xa", 1),
					resultFrom(t, c, "0xc", 2),
				}
			},
			wantValue: `"0xa"`,
		},
		{
			name: "only-block-head-leader on low participants returns the leader's result",
			cfg: &config{maxParticipants: 3, agreementThreshold: 3,
				lowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorOnlyBlockHeadLeader},
			method: "eth_call",
			leader: b,
			results: func(t *testing.T) []*execResult {
				return []*execResult{
					errorFrom(a, errors.New("dial timeout"), 0),
					resultFrom(t, b, "0xb", 1),
				}
			},
			wantValue: `"0xb"`,
		},
		{
			name: "only-block-head-leader low-participants behavior ignores rounds with enough participants",
			cfg: &config{maxParticipants: 2, agreementThreshold: 2,
				lowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorOnlyBlockHeadLeader},
			method: "eth_call",
			leader: b,
			results: func(t *testing.T) []*execResult {
				return []*execResult{
					resultFrom(t, a, "0xa", 0),
					resultFrom(t, b, "0xa", 1),
				}
			},
			wantValue: `"0xa"`,
		},
		{
			name:   "prefer-block-head-leader dispute picks the leader's group",
			cfg:    &config{maxParticipants: 3, agreementThreshold: 2, disputeBehavior: common.ConsensusDisputeBehaviorPreferBlockHeadLeader},
			method: "eth_call",
			leader: a,
			results: func(t *testing.T) []*execResult {
				return []*execResult{
					resultFrom(t, a, "0xa", 0),
					resultFrom(t, b, "0xb", 1),
					resultFrom(t, c, "0xc", 2),
				}
			},
			wantValue: `"0xa"`,
		},
		{
			name:   "prefer-block-head-leader with an errored leader falls back to dispute",
			cfg:    &config{maxParticipants: 3, agreementThreshold: 2, disputeBehavior: common.ConsensusDisputeBehaviorPreferBlockHeadLeader},
			method: "eth_call",
			leader: a,
			results: func(t *testing.T) []*execResult {
				return []*execResult{
					errorFrom(a, errors.New("dial timeout"), 0),
					resultFrom(t, b, "0xb", 1),
					resultFrom(t, c, "0xc", 2),
				}
			},
			wantCode: common.ErrCodeConsensusDispute,
		},
		{
			name: "prefer-larger + accept-most-common below threshold picks the largest",
			cfg: &config{maxParticipants: 3, agreementThreshold: 3, disputeBehavior: acceptMost,
				preferLargerResponses: true},
			results: func(t *testing.T) []*execResult {
				return []*execResult{
					resultFrom(t, a, "0xaa", 0),
					resultFrom(t, b, "0xbbbbbb", 1),
					resultFrom(t, c, "0xcc", 2),
				}
			},
			wantValue: `"0xbbbbbb"`,
		},
		{
			name: "prefer-larger + accept-most-common below threshold with no data disputes",
			cfg: &config{maxParticipants: 3, agreementThreshold: 3, disputeBehavior: acceptMost,
				preferLargerResponses: true},
			results: func(t *testing.T) []*execResult {
				return []*execResult{
					resultFrom(t, a, "0x", 0),
					errorFrom(b, revertedErr(), 1),
					errorFrom(c, invalidParamsErr(), 2),
				}
			},
			wantCode: common.ErrCodeConsensusDispute,
		},
		{
			name: "accept-most-common + prefer-non-empty overrides an agreed empty result",
			cfg: &config{maxParticipants: 3, agreementThreshold: 2, disputeBehavior: acceptMost,
				preferNonEmpty: true},
			results: func(t *testing.T) []*execResult {
				return []*execResult{
					resultFrom(t, a, "0x", 0),
					resultFrom(t, b, "0x", 1),
					resultFrom(t, c, "0xdata", 2),
				}
			},
			wantValue: `"0xdata"`,
		},
		{
			name: "accept-most-common + prefer-non-empty overrides an agreed execution error",
			cfg: &config{maxParticipants: 3, agreementThreshold: 2, disputeBehavior: acceptMost,
				preferNonEmpty: true},
			results: func(t *testing.T) []*execResult {
				return []*execResult{
					errorFrom(a, revertedErr(), 0),
					errorFrom(b, revertedErr(), 1),
					resultFrom(t, c, "0xdata", 2),
				}
			},
			wantValue: `"0xdata"`,
		},
		{
			name: "accept-most-common + prefer-non-empty below threshold prefers data over empties",
			cfg: &config{maxParticipants: 4, agreementThreshold: 3, disputeBehavior: acceptMost,
				preferNonEmpty: true},
			results: func(t *testing.T) []*execResult {
				return []*execResult{
					resultFrom(t, a, "0x", 0),
					resultFrom(t, b, "0x", 1),
					resultFrom(t, c, "0xdata", 2),
				}
			},
			wantValue: `"0xdata"`,
		},
		{
			name: "accept-most-common + prefer-non-empty below threshold prefers data over errors",
			cfg: &config{maxParticipants: 4, agreementThreshold: 3, disputeBehavior: acceptMost,
				preferNonEmpty: true},
			results: func(t *testing.T) []*execResult {
				return []*execResult{
					errorFrom(a, revertedErr(), 0),
					errorFrom(b, revertedErr(), 1),
					resultFrom(t, c, "0xdata", 2),
				}
			},
			wantValue: `"0xdata"`,
		},
		{
			name: "accept-most-common + prefer-non-empty picks data when data and error both meet threshold",
			cfg: &config{maxParticipants: 4, agreementThreshold: 2, disputeBehavior: acceptMost,
				preferNonEmpty: true},
			results: func(t *testing.T) []*execResult {
				return []*execResult{
					errorFrom(a, revertedErr(), 0),
					errorFrom(b, revertedErr(), 1),
					resultFrom(t, c, "0xdata", 2),
					resultFrom(t, d, "0xdata", 3),
				}
			},
			wantValue: `"0xdata"`,
		},
		{
			name: "return-error + prefer-non-empty disputes an agreed empty when data exists",
			cfg: &config{maxParticipants: 3, agreementThreshold: 2, disputeBehavior: returnErr,
				preferNonEmpty: true},
			results: func(t *testing.T) []*execResult {
				return []*execResult{
					resultFrom(t, a, "0x", 0),
					resultFrom(t, b, "0x", 1),
					resultFrom(t, c, "0xdata", 2),
				}
			},
			wantCode: common.ErrCodeConsensusDispute,
		},
		{
			name: "return-error without prefer-non-empty accepts an agreed empty",
			cfg:  &config{maxParticipants: 3, agreementThreshold: 2, disputeBehavior: returnErr},
			results: func(t *testing.T) []*execResult {
				return []*execResult{
					resultFrom(t, a, "0x", 0),
					resultFrom(t, b, "0x", 1),
					resultFrom(t, c, "0xdata", 2),
				}
			},
			wantValue: `"0x"`,
		},
		{
			name: "tie between two data groups at threshold disputes",
			cfg:  &config{maxParticipants: 5, agreementThreshold: 2},
			results: func(t *testing.T) []*execResult {
				return []*execResult{
					resultFrom(t, a, "0xa", 0),
					resultFrom(t, b, "0xa", 1),
					resultFrom(t, c, "0xb", 2),
					resultFrom(t, d, "0xb", 3),
					errorFrom(taggedUpstream("ups-e"), revertedErr(), 4),
				}
			},
			wantCode: common.ErrCodeConsensusDispute,
		},
		{
			name: "tie between data and an agreed error at threshold disputes",
			cfg:  &config{maxParticipants: 4, agreementThreshold: 2},
			results: func(t *testing.T) []*execResult {
				return []*execResult{
					errorFrom(a, revertedErr(), 0),
					errorFrom(b, revertedErr(), 1),
					resultFrom(t, c, "0xa", 2),
					resultFrom(t, d, "0xa", 3),
				}
			},
			wantCode: common.ErrCodeConsensusDispute,
		},
		{
			name: "prefer-larger picks the largest of several groups above threshold",
			cfg:  &config{maxParticipants: 4, agreementThreshold: 2, preferLargerResponses: true},
			results: func(t *testing.T) []*execResult {
				return []*execResult{
					resultFrom(t, a, "0xaa", 0),
					resultFrom(t, b, "0xaa", 1),
					resultFrom(t, c, "0xbbbbbb", 2),
					resultFrom(t, d, "0xbbbbbb", 3),
				}
			},
			wantValue: `"0xbbbbbb"`,
		},
		{
			name: "prefer-larger with only empty and error groups above threshold disputes",
			cfg:  &config{maxParticipants: 4, agreementThreshold: 2, preferLargerResponses: true},
			results: func(t *testing.T) []*execResult {
				return []*execResult{
					resultFrom(t, a, "0x", 0),
					resultFrom(t, b, "0x", 1),
					errorFrom(c, revertedErr(), 2),
					errorFrom(d, revertedErr(), 3),
				}
			},
			wantCode: common.ErrCodeConsensusDispute,
		},
		{
			name: "prefer-larger + accept-most-common replaces a smaller agreed result",
			cfg: &config{maxParticipants: 3, agreementThreshold: 2, disputeBehavior: acceptMost,
				preferLargerResponses: true},
			results: func(t *testing.T) []*execResult {
				return []*execResult{
					resultFrom(t, a, "0xaa", 0),
					resultFrom(t, b, "0xaa", 1),
					resultFrom(t, c, "0xbbbbbb", 2),
				}
			},
			wantValue: `"0xbbbbbb"`,
		},
		{
			name: "return-error + prefer-larger disputes a smaller agreed result",
			cfg: &config{maxParticipants: 3, agreementThreshold: 2, disputeBehavior: returnErr,
				preferLargerResponses: true},
			results: func(t *testing.T) []*execResult {
				return []*execResult{
					resultFrom(t, a, "0xaa", 0),
					resultFrom(t, b, "0xaa", 1),
					resultFrom(t, c, "0xbbbbbb", 2),
				}
			},
			wantCode: common.ErrCodeConsensusDispute,
		},
		{
			name: "accept-most-common below threshold returns the unique leading result",
			cfg:  &config{maxParticipants: 4, agreementThreshold: 3, disputeBehavior: acceptMost},
			results: func(t *testing.T) []*execResult {
				return []*execResult{
					resultFrom(t, a, "0xb", 0),
					resultFrom(t, b, "0xa", 1),
					resultFrom(t, c, "0xa", 2),
				}
			},
			wantValue: `"0xa"`,
		},
		{
			name: "accept-most-common below threshold returns the unique leading agreed error",
			cfg:  &config{maxParticipants: 4, agreementThreshold: 3, disputeBehavior: acceptMost},
			results: func(t *testing.T) []*execResult {
				return []*execResult{
					resultFrom(t, a, "0xb", 0),
					errorFrom(b, revertedErr(), 1),
					errorFrom(c, revertedErr(), 2),
				}
			},
			wantCode: common.ErrCodeEndpointExecutionException,
		},
		{
			name: "low participants + accept-most-common disputes a tie between data",
			cfg:  &config{maxParticipants: 3, agreementThreshold: 3, lowParticipantsBehavior: lowAcceptMost},
			results: func(t *testing.T) []*execResult {
				return []*execResult{
					resultFrom(t, a, "0xa", 0),
					resultFrom(t, b, "0xb", 1),
					errorFrom(c, errors.New("dial timeout"), 2),
				}
			},
			wantCode: common.ErrCodeConsensusDispute,
		},
		{
			name: "low participants + accept-most-common falls back to an empty result",
			cfg:  &config{maxParticipants: 3, agreementThreshold: 3, lowParticipantsBehavior: lowAcceptMost},
			results: func(t *testing.T) []*execResult {
				return []*execResult{
					resultFrom(t, a, "0x", 0),
					resultOf(t, b, []interface{}{}, 1),
					errorFrom(c, errors.New("dial timeout"), 2),
				}
			},
			wantErrAny: nil,
		},
		{
			name: "low participants + accept-most-common falls back to an agreed error",
			cfg:  &config{maxParticipants: 3, agreementThreshold: 3, lowParticipantsBehavior: lowAcceptMost},
			results: func(t *testing.T) []*execResult {
				return []*execResult{
					errorFrom(a, revertedErr(), 0),
					errorFrom(b, invalidParamsErr(), 1),
				}
			},
			wantErrAny: []common.ErrorCode{common.ErrCodeEndpointExecutionException, common.ErrCodeEndpointClientSideException},
		},
		{
			name: "agreed execution error meeting threshold is returned to the caller",
			cfg:  &config{maxParticipants: 3, agreementThreshold: 2},
			results: func(t *testing.T) []*execResult {
				return []*execResult{
					errorFrom(a, revertedErr(), 0),
					errorFrom(b, revertedErr(), 1),
					resultFrom(t, c, "0xa", 2),
				}
			},
			wantCode: common.ErrCodeEndpointExecutionException,
		},
		{
			name: "split below threshold without dispute preference disputes",
			cfg:  &config{maxParticipants: 3, agreementThreshold: 2},
			results: func(t *testing.T) []*execResult {
				return []*execResult{
					resultFrom(t, a, "0xa", 0),
					resultFrom(t, b, "0xb", 1),
					resultFrom(t, c, "0xc", 2),
				}
			},
			wantCode: common.ErrCodeConsensusDispute,
		},
		{
			name: "identical infrastructure errors meeting threshold surface that error",
			cfg:  &config{maxParticipants: 2, agreementThreshold: 2},
			results: func(t *testing.T) []*execResult {
				return []*execResult{
					errorFrom(a, common.NewErrEndpointServerSideException(errors.New("503"), nil, 503), 0),
					errorFrom(b, common.NewErrEndpointServerSideException(errors.New("503"), nil, 503), 1),
				}
			},
			wantCode: common.ErrCodeEndpointServerSideException,
		},
		{
			name: "low participants with return-error behavior returns low participants",
			cfg: &config{maxParticipants: 3, agreementThreshold: 3,
				lowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError},
			results: func(t *testing.T) []*execResult {
				return []*execResult{
					resultFrom(t, a, "0xa", 0),
					errorFrom(b, errors.New("dial timeout"), 1),
				}
			},
			wantCode: common.ErrCodeConsensusLowParticipants,
		},
		{
			name: "no responses at all is low participants",
			cfg:  &config{maxParticipants: 3, agreementThreshold: 2},
			results: func(t *testing.T) []*execResult {
				return nil
			},
			wantCode: common.ErrCodeConsensusLowParticipants,
		},
		{
			name: "a lone infrastructure error below threshold resolves to low participants",
			cfg:  &config{maxParticipants: 2, agreementThreshold: 2},
			results: func(t *testing.T) []*execResult {
				return []*execResult{
					errorFrom(a, errors.New("dial timeout"), 0),
				}
			},
			wantCode: common.ErrCodeConsensusLowParticipants,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			method := tt.method
			if method == "" {
				method = "eth_getBalance"
			}
			analysis := analyzeFor(t, tt.cfg, method, tt.leader, tt.results(t))
			winner := winnerOf(tt.cfg, analysis)
			require.NotNil(t, winner)
			switch {
			case tt.wantValue != "":
				assert.JSONEq(t, tt.wantValue, winnerResultString(t, winner))
			case tt.wantCode != "":
				require.Error(t, winner.Error)
				assert.True(t, common.HasErrorCode(winner.Error, tt.wantCode),
					"want %s, got %v", tt.wantCode, winner.Error)
			case tt.wantErrAny != nil:
				require.Error(t, winner.Error)
				assert.True(t, common.HasErrorCode(winner.Error, tt.wantErrAny...),
					"want one of %v, got %v", tt.wantErrAny, winner.Error)
			default:
				// Both empties tie at one vote; either is a valid empty answer.
				require.NoError(t, winner.Error)
				require.NotNil(t, winner.Result)
				assert.True(t, winner.Result.IsResultEmptyish(), "an empty result must be returned")
			}
		})
	}
}

func TestShouldShortCircuit_Reasons(t *testing.T) {
	a := taggedUpstream("ups-a")
	b := taggedUpstream("ups-b")
	c := taggedUpstream("ups-c")

	cases := []struct {
		name       string
		cfg        *config
		method     string
		results    func(t *testing.T) []*execResult
		wantReason string // empty = no short-circuit
	}{
		{
			name: "agreed execution error at threshold short-circuits",
			cfg:  &config{maxParticipants: 3, agreementThreshold: 2},
			results: func(t *testing.T) []*execResult {
				return []*execResult{errorFrom(a, revertedErr(), 0), errorFrom(b, revertedErr(), 1)}
			},
			wantReason: "consensus_error_threshold",
		},
		{
			name: "agreed execution error below threshold keeps waiting",
			cfg:  &config{maxParticipants: 3, agreementThreshold: 3},
			results: func(t *testing.T) []*execResult {
				return []*execResult{errorFrom(a, revertedErr(), 0), errorFrom(b, revertedErr(), 1)}
			},
		},
		{
			name:   "agreed error with highest-value method keeps waiting for values",
			cfg:    &config{maxParticipants: 3, agreementThreshold: 2, preferHighestValueFor: map[string][]string{"eth_getTransactionCount": {"result"}}},
			method: "eth_getTransactionCount",
			results: func(t *testing.T) []*execResult {
				return []*execResult{errorFrom(a, revertedErr(), 0), errorFrom(b, revertedErr(), 1)}
			},
		},
		{
			name: "agreed error under accept-most-common with prefer-non-empty keeps waiting",
			cfg: &config{maxParticipants: 3, agreementThreshold: 2,
				disputeBehavior: common.ConsensusDisputeBehaviorAcceptMostCommonValidResult, preferNonEmpty: true},
			results: func(t *testing.T) []*execResult {
				return []*execResult{errorFrom(a, revertedErr(), 0), errorFrom(b, revertedErr(), 1)}
			},
		},
		{
			name: "agreed error under return-error with prefer-non-empty short-circuits",
			// Defaults: returnError + preferNonEmpty=true. PreferNonEmpty only
			// promotes non-empty over Empty / MissingData-abstention results,
			// never over an already-agreed ConsensusError, so waiting cannot
			// change the winner. Regression: the blanket preferNonEmpty skip
			// held agreed reverts for all upstreams / maxWaitOnEmpty.
			cfg: &config{maxParticipants: 3, agreementThreshold: 2,
				disputeBehavior: common.ConsensusDisputeBehaviorReturnError, preferNonEmpty: true},
			results: func(t *testing.T) []*execResult {
				return []*execResult{errorFrom(a, revertedErr(), 0), errorFrom(b, revertedErr(), 1)}
			},
			wantReason: "consensus_error_threshold",
		},
		{
			name: "agreed error under return-error with prefer-larger short-circuits",
			cfg: &config{maxParticipants: 3, agreementThreshold: 2,
				disputeBehavior: common.ConsensusDisputeBehaviorReturnError, preferLargerResponses: true},
			results: func(t *testing.T) []*execResult {
				return []*execResult{errorFrom(a, revertedErr(), 0), errorFrom(b, revertedErr(), 1)}
			},
			wantReason: "consensus_error_threshold",
		},
		{
			name: "unassailable data lead short-circuits",
			cfg:  &config{maxParticipants: 3, agreementThreshold: 2},
			results: func(t *testing.T) []*execResult {
				return []*execResult{resultFrom(t, a, "0xa", 0), resultFrom(t, b, "0xa", 1)}
			},
			wantReason: "unassailable_lead",
		},
		{
			name: "lead over a dissenter still unassailable with one slot left",
			cfg:  &config{maxParticipants: 4, agreementThreshold: 2},
			results: func(t *testing.T) []*execResult {
				return []*execResult{resultFrom(t, a, "0xa", 0), resultFrom(t, b, "0xa", 1), resultFrom(t, c, "0xb", 2)}
			},
			wantReason: "unassailable_lead",
		},
		{
			name: "lead that remaining slots could tie keeps waiting",
			cfg:  &config{maxParticipants: 5, agreementThreshold: 2},
			results: func(t *testing.T) []*execResult {
				return []*execResult{resultFrom(t, a, "0xa", 0), resultFrom(t, b, "0xa", 1)}
			},
		},
		{
			name: "prefer-larger keeps waiting while slots remain",
			cfg:  &config{maxParticipants: 3, agreementThreshold: 2, preferLargerResponses: true},
			results: func(t *testing.T) []*execResult {
				return []*execResult{resultFrom(t, a, "0xa", 0), resultFrom(t, b, "0xa", 1)}
			},
		},
		{
			name:   "highest-value method keeps waiting while slots remain",
			cfg:    &config{maxParticipants: 3, agreementThreshold: 2, preferHighestValueFor: map[string][]string{"eth_getTransactionCount": {"result"}}},
			method: "eth_getTransactionCount",
			results: func(t *testing.T) []*execResult {
				return []*execResult{resultFrom(t, a, "0x1", 0), resultFrom(t, b, "0x1", 1)}
			},
		},
		{
			name: "prefer-non-empty with an empty leader keeps waiting",
			cfg:  &config{maxParticipants: 3, agreementThreshold: 2, preferNonEmpty: true},
			results: func(t *testing.T) []*execResult {
				return []*execResult{resultFrom(t, a, "0x", 0), resultFrom(t, b, "0x", 1)}
			},
		},
		{
			name: "no responses never short-circuits",
			cfg:  &config{maxParticipants: 3, agreementThreshold: 2},
			results: func(t *testing.T) []*execResult {
				return nil
			},
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			method := tt.method
			if method == "" {
				method = "eth_getBalance"
			}
			analysis := analyzeFor(t, tt.cfg, method, nil, tt.results(t))
			lg := zerolog.Nop()
			e := &executor{consensusPolicy: &consensusPolicy{logger: &lg, config: tt.cfg}}
			winner := e.determineWinner(context.Background(), &lg, analysis)
			reason, ok := e.shouldShortCircuit(winner, analysis)
			assert.Equal(t, tt.wantReason != "", ok)
			assert.Equal(t, tt.wantReason, reason)
		})
	}
}

func TestAnalysis_IgnoreFieldsMergeResponsesDifferingOnlyInIgnoredFields(t *testing.T) {
	a := taggedUpstream("ups-a")
	b := taggedUpstream("ups-b")
	responses := func(t *testing.T) []*execResult {
		return []*execResult{
			resultOf(t, a, map[string]interface{}{"hash": "0x1", "timestamp": "0x2"}, 0),
			resultOf(t, b, map[string]interface{}{"hash": "0x1", "timestamp": "0x3"}, 1),
		}
	}

	t.Run("without ignoreFields the responses disagree", func(t *testing.T) {
		cfg := &config{maxParticipants: 2, agreementThreshold: 2}
		analysis := analyzeFor(t, cfg, "eth_getBlockByNumber", nil, responses(t))
		assert.Len(t, analysis.groups, 2)
	})

	t.Run("ignored field is excluded from the agreement hash", func(t *testing.T) {
		cfg := &config{maxParticipants: 2, agreementThreshold: 2,
			ignoreFields: map[string][]string{"eth_getBlockByNumber": {"timestamp"}}}
		analysis := analyzeFor(t, cfg, "eth_getBlockByNumber", nil, responses(t))
		require.Len(t, analysis.groups, 1)
		winner := winnerOf(cfg, analysis)
		require.NoError(t, winner.Error)
	})

	t.Run("ignoreFields for another method has no effect", func(t *testing.T) {
		cfg := &config{maxParticipants: 2, agreementThreshold: 2,
			ignoreFields: map[string][]string{"eth_getLogs": {"timestamp"}}}
		analysis := analyzeFor(t, cfg, "eth_getBlockByNumber", nil, responses(t))
		assert.Len(t, analysis.groups, 2)
	})
}

func TestAnalysis_ClassifiesUnparseableResponsesAsInfrastructure(t *testing.T) {
	cfg := &config{maxParticipants: 2, agreementThreshold: 2}
	r := &execResult{Result: nil, Upstream: taggedUpstream("ups-a")}
	classifyAndHashResponse(r, context.Background(), cfg)
	assert.Equal(t, ResponseTypeInfrastructureError, r.CachedResponseType)
	assert.Equal(t, "error:generic", r.CachedHash)
}

func TestErrorToConsensusHash(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil error has no hash", nil, ""},
		{"json-rpc error hashes by normalized code", revertedErr(), "jsonrpc:3"},
		{"standard error without json-rpc cause hashes by its code",
			common.NewErrEndpointServerSideException(errors.New("boom"), nil, 500), string(common.ErrCodeEndpointServerSideException)},
		{"plain error hashes generically", errors.New("boom"), "error:generic"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, errorToConsensusHash(tt.err))
		})
	}
}

func TestResponseType_String(t *testing.T) {
	cases := map[ResponseType]string{
		ResponseTypeNonEmpty:            "non_empty",
		ResponseTypeEmpty:               "empty",
		ResponseTypeConsensusError:      "consensus_error",
		ResponseTypeInfrastructureError: "infrastructure_error",
		ResponseType(99):                "unknown",
	}
	for rt, want := range cases {
		assert.Equal(t, want, rt.String())
	}
}

func TestExtractFieldValues(t *testing.T) {
	resp := func(t *testing.T, v interface{}) *common.NormalizedResponse {
		return resultOf(t, nil, v, 0).Result
	}
	cases := []struct {
		name   string
		resp   func(t *testing.T) *common.NormalizedResponse
		fields []string
		want   []int64 // nil = not extractable
	}{
		{"nil response", func(t *testing.T) *common.NormalizedResponse { return nil }, []string{"result"}, nil},
		{"no fields", func(t *testing.T) *common.NormalizedResponse { return resp(t, "0x1") }, nil, nil},
		{"hex string result", func(t *testing.T) *common.NormalizedResponse { return resp(t, "0x1f") }, []string{"result"}, []int64{31}},
		{"numeric result", func(t *testing.T) *common.NormalizedResponse { return resp(t, 42) }, []string{"result"}, []int64{42}},
		{"non-numeric result", func(t *testing.T) *common.NormalizedResponse { return resp(t, "abc") }, []string{"result"}, nil},
		{"nested fields in order", func(t *testing.T) *common.NormalizedResponse {
			return resp(t, map[string]interface{}{"nonce": "0x2", "balance": "10"})
		}, []string{"nonce", "balance"}, []int64{2, 10}},
		{"missing nested field", func(t *testing.T) *common.NormalizedResponse {
			return resp(t, map[string]interface{}{"nonce": "0x2"})
		}, []string{"nonce", "balance"}, nil},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got := extractFieldValues(tt.resp(t), tt.fields)
			if tt.want == nil {
				assert.Nil(t, got)
				return
			}
			require.Len(t, got, len(tt.want))
			for i, w := range tt.want {
				assert.Equal(t, w, got[i].Int64())
			}
		})
	}
}
