package erpc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/thirdparty"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type consensusTestCase struct {
	name                 string
	description          string
	upstreams            []*common.UpstreamConfig
	consensusConfig      *common.ConsensusPolicyConfig
	retryPolicy          *common.RetryPolicyConfig
	timeoutPolicy        *common.TimeoutPolicyConfig
	circuitBreakerPolicy *common.CircuitBreakerPolicyConfig
	hedgePolicy          *common.HedgePolicyConfig
	mockResponses        []mockResponse
	expectedCalls        []int
	expectedResult       *expectedResult
	expectedError        *expectedError
	expectedPendingMocks int
	setupFn              func(t *testing.T, ctx context.Context, reg *upstream.UpstreamsRegistry)
	requestMethod        string
	requestParams        []interface{}
}

type mockResponse struct {
	status int
	body   map[string]interface{}
	delay  time.Duration
}

type expectedResult struct {
	contains      string
	check         func(t *testing.T, resp *common.NormalizedResponse, duration time.Duration)
	jsonRpcResult string
}

type expectedError struct {
	code     common.ErrorCode
	contains string
}

func init() {
	util.ConfigureTestLogger()
}

func TestConsensusPolicy(t *testing.T) {
	tests := []consensusTestCase{
		{
			name:        "only_block_head_leader_dispute_leader_error_returns_leader_error",
			description: "Dispute with OnlyBlockHeadLeader and leader has only an error -> return leader's error",
			upstreams:   createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:       3,
				AgreementThreshold:    3, // force dispute context
				DisputeBehavior:       common.ConsensusDisputeBehaviorOnlyBlockHeadLeader,
				PreferNonEmpty:        &common.TRUE,
				PreferLargerResponses: &common.TRUE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcError(-32000, "leader error")}, // leader
				{status: 200, body: jsonRpcSuccess("0xaaa")},
				{status: 200, body: jsonRpcSuccess("0xbbb")},
			},
			expectedCalls: []int{1, 1, 1},
			expectedError: &expectedError{code: common.ErrCodeUpstreamRequest, contains: "leader error"},
			setupFn: func(t *testing.T, ctx context.Context, reg *upstream.UpstreamsRegistry) {
				ups := reg.GetNetworkUpstreams(ctx, util.EvmNetworkId(123))
				// upstream 1 is leader
				ups[0].EvmStatePoller().SuggestLatestBlock(500)
				ups[1].EvmStatePoller().SuggestLatestBlock(100)
				ups[2].EvmStatePoller().SuggestLatestBlock(100)
				time.Sleep(50 * time.Millisecond)
			},
		},
		{
			name:        "accept_most_common_below_threshold_tie_empty_vs_non_empty_prefer_non_empty",
			description: "Below threshold: empty x2 vs non-empty x2 (tie), infra x1; preferNonEmpty selects non-empty",
			upstreams:   createTestUpstreams(5),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         5,
				AgreementThreshold:      3,
				DisputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
				PreferNonEmpty:          &common.TRUE,
				PreferLargerResponses:   &common.FALSE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess([]interface{}{})},              // empty 1
				{status: 200, body: jsonRpcSuccess([]interface{}{})},              // empty 2
				{status: 200, body: jsonRpcSuccess("0xnonempty")},                 // non-empty 1
				{status: 200, body: jsonRpcSuccess("0xnonempty")},                 // non-empty 2
				{status: 200, body: jsonRpcError(-32603, "server error (infra)")}, // infra
			},
			expectedCalls:  []int{1, 1, 1, 1, 1},
			expectedResult: &expectedResult{jsonRpcResult: "\"0xnonempty\""},
		},
		{
			name:        "accept_most_common_above_threshold_tie_error_vs_non_empty_with_preferences",
			description: "Above threshold tie counts between consensus error and non-empty; prefer non-empty + prefer larger -> choose largest non-empty",
			upstreams:   createTestUpstreams(4),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         4,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
				PreferNonEmpty:          &common.TRUE,
				PreferLargerResponses:   &common.TRUE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess("0x" + strings.Repeat("A", 2000))}, // non-empty large 1
				{status: 200, body: jsonRpcSuccess("0x" + strings.Repeat("A", 2000))}, // non-empty large 2 (>= threshold)
				{status: 200, body: jsonRpcError(-32000, "call exception")},           // error 1
				{status: 200, body: jsonRpcError(-32000, "call exception")},           // error 2 (>= threshold)
			},
			expectedCalls:  []int{1, 1, 1, 1},
			expectedResult: &expectedResult{jsonRpcResult: "\"0x" + strings.Repeat("A", 2000) + "\""},
		},
		{
			name:        "return_error_prefer_non_empty_does_not_override_agreed_error",
			description: "ReturnError: two identical consensus errors vs one non-empty; preferNonEmpty=true must still return error",
			upstreams:   createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         3,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
				PreferNonEmpty:          &common.TRUE,
				PreferLargerResponses:   &common.FALSE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcError(-32000, "execution reverted")},
				{status: 200, body: jsonRpcError(-32000, "execution reverted")},
				{status: 200, body: jsonRpcSuccess("0xdata")},
			},
			expectedCalls:        []int{1, 1, -1}, // -1 means Persist() for the third mock which may or may not be called
			expectedError:        &expectedError{code: common.ErrCodeUpstreamRequest, contains: "execution reverted"},
			expectedPendingMocks: 1, // Third upstream may not be called due to short-circuit on error consensus
		},
		{
			name:        "accept_most_common_below_threshold_tie_sizes_prefer_larger",
			description: "AcceptMostCommon + preferLarger: below threshold tie by counts among non-empty -> choose largest by size",
			upstreams:   createTestUpstreams(4),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         4,
				AgreementThreshold:      3,
				DisputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
				PreferLargerResponses:   &common.TRUE,
				PreferNonEmpty:          &common.FALSE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess("0x" + strings.Repeat("A", 2000))},
				{status: 200, body: jsonRpcSuccess("0x" + strings.Repeat("b", 10))},
				{status: 200, body: jsonRpcSuccess("0x" + strings.Repeat("c", 10))},
				{status: 200, body: jsonRpcError(-32603, "infra")},
			},
			expectedCalls:  []int{1, 1, 1, 1},
			expectedResult: &expectedResult{jsonRpcResult: "\"0x" + strings.Repeat("A", 2000) + "\""},
		},
		{
			name:        "low_participants_accept_most_common_tie_sizes_prefer_larger",
			description: "Low participants + AcceptMostCommon + preferLarger: tie counts among non-empty -> choose largest by size",
			upstreams:   createTestUpstreams(2),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         2,
				AgreementThreshold:      3,
				DisputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
				PreferLargerResponses:   &common.TRUE,
				PreferNonEmpty:          &common.FALSE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess("0x" + strings.Repeat("A", 2000))},
				{status: 200, body: jsonRpcSuccess("0x" + strings.Repeat("b", 10))},
			},
			expectedCalls:  []int{1, 1},
			expectedResult: &expectedResult{jsonRpcResult: "\"0x" + strings.Repeat("A", 2000) + "\""},
		},
		{
			name:        "return_error_prefer_larger_misfire_should_dispute",
			description: "LowParticipantsBehavior=AcceptMostCommon but not low participants; prefer larger must not override ReturnError path",
			upstreams:   createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         3,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
				PreferLargerResponses:   &common.TRUE,
				PreferNonEmpty:          &common.FALSE,
			},
			// Two small meet threshold; one large minority present
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess("0x" + strings.Repeat("s", 10))},
				{status: 200, body: jsonRpcSuccess("0x" + strings.Repeat("s", 10))},
				{status: 200, body: jsonRpcSuccess("0x" + strings.Repeat("L", 2000))},
			},
			expectedCalls: []int{1, 1, 1},
			expectedError: &expectedError{code: common.ErrCodeConsensusDispute, contains: "not enough agreement"},
		},
		{
			name:        "return_error_prefer_non_empty_misfire_should_dispute",
			description: "LowParticipantsBehavior=AcceptMostCommon but not low participants; prefer non-empty must not override ReturnError path",
			upstreams:   createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         3,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
				PreferNonEmpty:          &common.TRUE,
				PreferLargerResponses:   &common.FALSE,
			},
			// Two empty meet threshold; one non-empty minority present
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess([]interface{}{})},
				{status: 200, body: jsonRpcSuccess([]interface{}{})},
				{status: 200, body: jsonRpcSuccess("0xnonempty")},
			},
			expectedCalls: []int{1, 1, 1},
			expectedError: &expectedError{code: common.ErrCodeConsensusDispute, contains: "not enough agreement"},
		},
		{
			name:        "return_error_two_non_empty_vs_one_empty_threshold_two_group_wins",
			description: "ReturnError: 2 non-empty vs 1 empty -> 2-group wins",
			upstreams:   createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         3,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
				PreferNonEmpty:          &common.FALSE,
				PreferLargerResponses:   &common.FALSE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess("0xA")},
				{status: 200, body: jsonRpcSuccess("0xA")},
				{status: 200, body: jsonRpcSuccess([]interface{}{})},
			},
			expectedCalls:  []int{1, 1, 0},
			expectedResult: &expectedResult{jsonRpcResult: "\"0xA\""},
		},
		{
			name:        "return_error_two_larger_vs_one_smaller_threshold_two_group_wins",
			description: "ReturnError: 2 larger non-empty vs 1 smaller non-empty -> 2-group wins",
			upstreams:   createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         3,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
				PreferLargerResponses:   &common.FALSE,
				PreferNonEmpty:          &common.FALSE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess(strings.Repeat("A", 2000))},
				{status: 200, body: jsonRpcSuccess(strings.Repeat("A", 2000))},
				{status: 200, body: jsonRpcSuccess(strings.Repeat("b", 10))},
			},
			expectedCalls:  []int{1, 1, 0},
			expectedResult: &expectedResult{jsonRpcResult: "\"" + strings.Repeat("A", 2000) + "\""},
		},
		{
			name:        "return_error_two_smaller_vs_one_larger_prefer_larger_disabled_two_group_wins",
			description: "ReturnError: 2 smaller vs 1 larger; prefer larger disabled -> 2-group wins",
			upstreams:   createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         3,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
				PreferLargerResponses:   &common.FALSE,
				PreferNonEmpty:          &common.FALSE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess(strings.Repeat("x", 10))},
				{status: 200, body: jsonRpcSuccess(strings.Repeat("x", 10))},
				{status: 200, body: jsonRpcSuccess(strings.Repeat("A", 2000))},
			},
			expectedCalls:  []int{1, 1, 0},
			expectedResult: &expectedResult{jsonRpcResult: "\"" + strings.Repeat("x", 10) + "\""},
		},
		{
			name:        "return_error_two_empty_vs_one_non_empty_prefer_non_empty_disabled_two_group_wins",
			description: "ReturnError: 2 empty vs 1 non-empty; prefer non-empty disabled -> 2-group wins (empty)",
			upstreams:   createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         3,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
				PreferNonEmpty:          &common.FALSE,
				PreferLargerResponses:   &common.FALSE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess([]interface{}{})},
				{status: 200, body: jsonRpcSuccess([]interface{}{})},
				{status: 200, body: jsonRpcSuccess("0xdata")},
			},
			expectedCalls:  []int{1, 1, 0},
			expectedResult: &expectedResult{jsonRpcResult: "[]"},
		},
		{
			name:        "return_error_two_smaller_vs_one_larger_prefer_larger_enabled_dispute",
			description: "ReturnError: 2 smaller meet threshold but single larger exists; prefer larger enabled -> dispute",
			upstreams:   createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         3,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
				PreferLargerResponses:   &common.TRUE,
				PreferNonEmpty:          &common.FALSE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess(strings.Repeat("x", 10))},
				{status: 200, body: jsonRpcSuccess(strings.Repeat("x", 10))},
				{status: 200, body: jsonRpcSuccess(strings.Repeat("A", 2000))},
			},
			expectedCalls: []int{1, 1, 1},
			expectedError: &expectedError{code: common.ErrCodeConsensusDispute, contains: "not enough agreement"},
		},
		{
			name:        "return_error_two_empty_vs_one_non_empty_prefer_non_empty_enabled_dispute",
			description: "ReturnError: 2 empty meet threshold but single non-empty exists; prefer non-empty enabled -> dispute",
			upstreams:   createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         3,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
				PreferNonEmpty:          &common.TRUE,
				PreferLargerResponses:   &common.FALSE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess([]interface{}{})},
				{status: 200, body: jsonRpcSuccess([]interface{}{})},
				{status: 200, body: jsonRpcSuccess("0xdata")},
			},
			expectedCalls: []int{1, 1, 1},
			expectedError: &expectedError{code: common.ErrCodeConsensusDispute, contains: "not enough agreement"},
		},
		{
			name:        "only_block_head_leader_dispute_selects_leader_non_error",
			description: "Dispute with OnlyBlockHeadLeader: pick leader's non-error result",
			upstreams:   createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:    3,
				AgreementThreshold: 3, // ensure dispute path (no group meets threshold, not low participants)
				DisputeBehavior:    common.ConsensusDisputeBehaviorOnlyBlockHeadLeader,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess("0xleader")}, // leader upstream
				{status: 200, body: jsonRpcSuccess("0xaaa")},
				{status: 200, body: jsonRpcSuccess("0xbbb")},
			},
			expectedCalls:  []int{1, 1, 1},
			expectedResult: &expectedResult{jsonRpcResult: `"0xleader"`},
			setupFn: func(t *testing.T, ctx context.Context, reg *upstream.UpstreamsRegistry) {
				ups := reg.GetNetworkUpstreams(ctx, util.EvmNetworkId(123))
				// Set unique leader: upstream 1 has highest latest block
				ups[0].EvmStatePoller().SuggestLatestBlock(300)
				ups[1].EvmStatePoller().SuggestLatestBlock(100)
				ups[2].EvmStatePoller().SuggestLatestBlock(100)
				time.Sleep(50 * time.Millisecond)
			},
		},
		{
			name:        "prefer_block_head_leader_dispute_prefers_leader_non_error",
			description: "Dispute with PreferBlockHeadLeader: prefer leader's non-error result",
			upstreams:   createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:    3,
				AgreementThreshold: 3,
				DisputeBehavior:    common.ConsensusDisputeBehaviorPreferBlockHeadLeader,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess("0xleader")},
				{status: 200, body: jsonRpcSuccess("0xaaa")},
				{status: 200, body: jsonRpcSuccess("0xbbb")},
			},
			expectedCalls:  []int{1, 1, 1},
			expectedResult: &expectedResult{jsonRpcResult: `"0xleader"`},
			setupFn: func(t *testing.T, ctx context.Context, reg *upstream.UpstreamsRegistry) {
				ups := reg.GetNetworkUpstreams(ctx, util.EvmNetworkId(123))
				ups[0].EvmStatePoller().SuggestLatestBlock(300)
				ups[1].EvmStatePoller().SuggestLatestBlock(100)
				ups[2].EvmStatePoller().SuggestLatestBlock(100)
				time.Sleep(50 * time.Millisecond)
			},
		},
		{
			name:        "prefer_block_head_leader_leader_error_fallback_to_consensus",
			description: "PreferBlockHeadLeader with leader error falls back to accept-most-common logic",
			upstreams:   createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:    3,
				AgreementThreshold: 3, // dispute path
				DisputeBehavior:    common.ConsensusDisputeBehaviorPreferBlockHeadLeader,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcError(-32000, "leader error")}, // leader
				{status: 200, body: jsonRpcSuccess("0xagreed")},           // best non-empty group (count 2)
				{status: 200, body: jsonRpcSuccess("0xagreed")},
			},
			expectedCalls:  []int{1, 1, 1},
			expectedResult: &expectedResult{jsonRpcResult: `"0xagreed"`},
			setupFn: func(t *testing.T, ctx context.Context, reg *upstream.UpstreamsRegistry) {
				ups := reg.GetNetworkUpstreams(ctx, util.EvmNetworkId(123))
				ups[0].EvmStatePoller().SuggestLatestBlock(300)
				ups[1].EvmStatePoller().SuggestLatestBlock(100)
				ups[2].EvmStatePoller().SuggestLatestBlock(100)
				time.Sleep(50 * time.Millisecond)
			},
		},
		{
			name:        "only_block_head_leader_low_participants_selects_leader",
			description: "Low participants with OnlyBlockHeadLeader selects leader's non-error result",
			upstreams:   createTestUpstreams(2),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         2,
				AgreementThreshold:      3, // force low participants
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorOnlyBlockHeadLeader,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess("0xleader")},
				{status: 200, body: jsonRpcError(-32603, "infra")},
			},
			expectedCalls:  []int{1, 1},
			expectedResult: &expectedResult{jsonRpcResult: `"0xleader"`},
			setupFn: func(t *testing.T, ctx context.Context, reg *upstream.UpstreamsRegistry) {
				ups := reg.GetNetworkUpstreams(ctx, util.EvmNetworkId(123))
				ups[0].EvmStatePoller().SuggestLatestBlock(300)
				ups[1].EvmStatePoller().SuggestLatestBlock(100)
				time.Sleep(50 * time.Millisecond)
			},
		},
		{
			name:        "only_block_head_leader_low_participants_no_leader_non_error_errors",
			description: "Low participants with OnlyBlockHeadLeader and no leader non-error -> low participants error",
			upstreams:   createTestUpstreams(2),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         2,
				AgreementThreshold:      3,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorOnlyBlockHeadLeader,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcError(-32000, "leader error")}, // leader error only
				{status: 200, body: jsonRpcSuccess("0xother")},
			},
			expectedCalls: []int{1, 1},
			expectedError: &expectedError{code: common.ErrCodeConsensusLowParticipants, contains: "not enough participants"},
			setupFn: func(t *testing.T, ctx context.Context, reg *upstream.UpstreamsRegistry) {
				ups := reg.GetNetworkUpstreams(ctx, util.EvmNetworkId(123))
				ups[0].EvmStatePoller().SuggestLatestBlock(300)
				ups[1].EvmStatePoller().SuggestLatestBlock(100)
				time.Sleep(50 * time.Millisecond)
			},
		},
		{
			name:        "non_empty_over_errors_and_empty_mixed_below_threshold",
			description: "Below threshold mix: 2 identical errors, 1 empty, 1 non-empty; AcceptMostCommon + preferNonEmpty should select the non-empty",
			upstreams:   createTestUpstreams(4),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         4,
				AgreementThreshold:      3,
				DisputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
				PreferNonEmpty:          &common.TRUE,
				PreferLargerResponses:   &common.FALSE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcError(-32000, "call exception")},
				{status: 200, body: jsonRpcError(-32000, "call exception")},
				{status: 200, body: jsonRpcSuccess([]interface{}{})},
				{status: 200, body: jsonRpcSuccess("0xnonempty")},
			},
			expectedCalls:  []int{1, 1, 1, 1},
			expectedResult: &expectedResult{jsonRpcResult: "\"0xnonempty\""},
		},
		{
			name:        "consensus_error_over_empty_at_threshold_no_non_empty",
			description: "3 consensus-valid errors vs 1 empty; AcceptMostCommon (+ PreferNonEmpty) should return the error since no non-empty exists",
			upstreams:   createTestUpstreams(4),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         4,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
				PreferNonEmpty:          &common.TRUE,
				PreferLargerResponses:   &common.FALSE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcError(3, "execution reverted")},
				{status: 200, body: jsonRpcError(3, "execution reverted")},
				{status: 200, body: jsonRpcError(3, "execution reverted")},
				{status: 200, body: jsonRpcSuccess([]interface{}{})},
			},
			expectedCalls: []int{1, 1, 1, 1},
			expectedError: &expectedError{code: common.ErrCodeUpstreamRequest, contains: "execution reverted"},
		},
		{
			name:        "error_has_higher_count_but_non_empty_preferred_above_threshold",
			description: "Above threshold both non-empty and error groups; error has higher count; AcceptMostCommon + preferNonEmpty should still choose non-empty",
			upstreams:   createTestUpstreams(5),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         5,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
				PreferNonEmpty:          &common.TRUE,
				PreferLargerResponses:   &common.FALSE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcError(-32000, "call exception")},
				{status: 200, body: jsonRpcError(-32000, "call exception")},
				{status: 200, body: jsonRpcError(-32000, "call exception")},
				{status: 200, body: jsonRpcSuccess("0xaaa")},
				{status: 200, body: jsonRpcSuccess("0xaaa")},
			},
			expectedCalls:  []int{1, 1, 1, 1, 1},
			expectedResult: &expectedResult{jsonRpcResult: "\"0xaaa\""},
		},
		{
			name:        "participant_requirement_more_than_available_low_participants",
			description: "Agreement threshold exceeds available valid participants -> low participants error",
			upstreams:   createTestUpstreams(2),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         2,
				AgreementThreshold:      3,
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess("0xdata")},
				{status: 200, body: jsonRpcSuccess("0xdata")},
			},
			expectedCalls: []int{1, 1},
			expectedError: &expectedError{code: common.ErrCodeConsensusLowParticipants, contains: "not enough participants"},
		},
		{
			name:        "minority_success_over_majority_execution_errors_with_preference",
			description: "Prefer non-empty success when below threshold vs multiple execution errors under AcceptMostCommon+preferNonEmpty",
			upstreams:   createTestUpstreams(4),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         4,
				AgreementThreshold:      3,
				DisputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
				PreferNonEmpty:          &common.TRUE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcError(-32000, "execution reverted: out of gas")},
				{status: 200, body: jsonRpcError(-32000, "execution reverted: out of gas")},
				{status: 200, body: jsonRpcError(-32000, "execution reverted: out of gas")},
				{status: 200, body: jsonRpcSuccess("0xsuccess")},
			},
			expectedCalls:  []int{1, 1, 1, 1},
			expectedResult: &expectedResult{jsonRpcResult: "\"0xsuccess\""},
		},
		{
			name:        "return_error_behavior_different_execution_errors_returns_one_error",
			description: "With ReturnError behavior and different execution errors, return one of the execution errors",
			upstreams:   createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         3,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcError(-32000, "execution reverted: reason A")},
				{status: 200, body: jsonRpcError(-32000, "execution reverted: reason B")},
				{status: 200, body: jsonRpcError(-32000, "execution reverted: reason C")},
			},
			expectedCalls: []int{1, 1, 0},
			expectedError: &expectedError{code: common.ErrCodeUpstreamRequest, contains: "execution reverted"},
		},
		{
			name:        "mixed_infra_and_execution_errors_consensus_on_execution_error",
			description: "1 infra error (-32603) and 2 identical execution errors (-32000) -> consensus on execution error",
			upstreams:   createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         3,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcError(-32603, "network timeout")},
				{status: 200, body: jsonRpcError(-32000, "execution reverted: insufficient balance")},
				{status: 200, body: jsonRpcError(-32000, "execution reverted: insufficient balance")},
			},
			expectedCalls: []int{1, 1, 1},
			expectedError: &expectedError{code: common.ErrCodeUpstreamRequest, contains: "execution reverted"},
		},
		{
			name:        "agreed_invalid_params_error",
			description: "All participants return the same JSON-RPC invalid params error -> return that error",
			upstreams:   createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         3,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcError(-32602, "Invalid params")},
				{status: 200, body: jsonRpcError(-32602, "Invalid params")},
				{status: 200, body: jsonRpcError(-32602, "Invalid params")},
			},
			expectedCalls: []int{1, 1, 0},
			expectedError: &expectedError{code: common.ErrCodeUpstreamRequest, contains: "Invalid params"},
		},
		{
			name:        "agreed_method_not_found_error",
			description: "All participants return method not found error -> return that error",
			upstreams:   createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         3,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcError(-32601, "Method not found")},
				{status: 200, body: jsonRpcError(-32601, "Method not found")},
				{status: 200, body: jsonRpcError(-32601, "Method not found"), delay: 100 * time.Millisecond},
			},
			expectedCalls: []int{1, 1, 0},
			expectedError: &expectedError{code: common.ErrCodeUpstreamRequest, contains: "Method not found"},
		},
		{
			name:        "agreed_missing_data_error",
			description: "All participants return missing data (-32014) -> return that error",
			upstreams:   createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         3,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcError(-32014, "requested data is not available")},
				{status: 200, body: jsonRpcError(-32014, "requested data is not available")},
				{status: 200, body: jsonRpcError(-32014, "requested data is not available")},
			},
			expectedCalls: []int{1, 1, 1},
			expectedError: &expectedError{code: common.ErrCodeUpstreamRequest, contains: "requested data is not available"},
		},
		{
			name:        "agreed_execution_reverted_error",
			description: "All participants return execution reverted (code 3) -> return that error",
			upstreams:   createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         3,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcError(3, "execution reverted")},
				{status: 200, body: jsonRpcError(3, "execution reverted")},
				{status: 200, body: jsonRpcError(3, "execution reverted")},
			},
			expectedCalls: []int{1, 1, 0},
			expectedError: &expectedError{code: common.ErrCodeUpstreamRequest, contains: "execution reverted"},
		},
		{
			name:        "all_server_errors_infra_retry_exhausted",
			description: "All participants return server error (-32603) which is infra error -> retry exhausted",
			upstreams:   createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         3,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
			},
			retryPolicy: &common.RetryPolicyConfig{
				MaxAttempts: 2,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcError(-32603, "internal server error")},
				{status: 200, body: jsonRpcError(-32603, "internal server error")},
				{status: 200, body: jsonRpcError(-32603, "internal server error")},
			},
			expectedCalls: []int{1, 1, 1},
			expectedError: &expectedError{code: common.ErrCodeFailsafeRetryExceeded, contains: "gave up retrying"},
		},
		{
			name:        "one_success_rest_server_errors",
			description: "1 success response + rest server errors (-32603 infra) -> success wins (infra errors don't count as valid participants)",
			upstreams:   createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         3,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess("0x1")},
				{status: 200, body: jsonRpcError(-32603, "internal server error")},
				{status: 200, body: jsonRpcError(-32603, "internal server error")},
			},
			expectedCalls:  []int{1, 1, 1},
			expectedResult: &expectedResult{jsonRpcResult: `"0x1"`},
		},
		{
			name:        "one_success_rest_consensus_errors_dispute",
			description: "1 success response + 2 consensus errors (code 3) -> dispute (1 success vs 2 errors, neither reaches threshold=3)",
			upstreams:   createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         3,
				AgreementThreshold:      3,
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess("0x1")},
				{status: 200, body: jsonRpcError(3, "execution reverted")},
				{status: 200, body: jsonRpcError(3, "execution reverted")},
			},
			expectedCalls: []int{1, 1, 1},
			expectedError: &expectedError{code: common.ErrCodeConsensusDispute, contains: "not enough agreement"},
		},
		{
			name:        "mixed_errors_returns_dispute",
			description: "Different JSON-RPC errors from participants below threshold -> dispute",
			upstreams:   createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         3,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcError(-32602, "Invalid params")},
				{status: 200, body: jsonRpcError(-32601, "Method not found")},
				{status: 200, body: jsonRpcError(-32000, "Internal error")},
			},
			expectedCalls: []int{1, 1, 1},
			expectedError: &expectedError{code: common.ErrCodeConsensusDispute, contains: "not enough agreement"},
		},
		{
			name:        "majority_error_over_single_success_returns_error",
			description: "Two identical JSON-RPC errors vs one success at threshold -> return the agreed error",
			upstreams:   createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         3,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
				PreferNonEmpty:          &common.FALSE,
				PreferLargerResponses:   &common.FALSE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcError(-32602, "Invalid params")},
				{status: 200, body: jsonRpcError(-32602, "Invalid params")},
				{status: 200, body: jsonRpcSuccess("0x1234"), delay: 300 * time.Millisecond},
			},
			expectedCalls: []int{1, 1, 1},
			expectedError: &expectedError{code: common.ErrCodeUpstreamRequest, contains: "Invalid params"},
		},
		{
			name:        "errors_should_not_win_by_size_preference_return_error",
			description: "PreferLargerResponses enabled with ReturnError: 2 empty vs 1 large error -> empty wins, errors do not win by size",
			upstreams:   createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         3,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
				PreferLargerResponses:   &common.TRUE,
				PreferNonEmpty:          &common.TRUE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess([]interface{}{})},
				{status: 200, body: jsonRpcSuccess([]interface{}{})},
				{status: 200, body: jsonRpcError(-32000, strings.Repeat("E", 500))},
			},
			expectedCalls:  []int{1, 1, 1},
			expectedResult: &expectedResult{jsonRpcResult: "[]"},
		},
		{
			name:        "prefer_non_empty_over_consensus_error_at_threshold_accept_most_common",
			description: "PreferNonEmpty + AcceptMostCommon: consensus error meets threshold but single non-empty exists -> select non-empty",
			upstreams:   createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         3,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
				PreferNonEmpty:          &common.TRUE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcError(-32602, "Invalid params")},
				{status: 200, body: jsonRpcError(-32602, "Invalid params")},
				{status: 200, body: jsonRpcSuccess("0x1234")},
			},
			expectedCalls:  []int{1, 1, 1},
			expectedResult: &expectedResult{jsonRpcResult: `"0x1234"`},
		},
		{
			name:        "accept_most_common_below_threshold_empty_unique_no_preference",
			description: "Below threshold unique empty leader vs one non-empty and infra error; no non-empty preference -> select empty",
			upstreams:   createTestUpstreams(4),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         4,
				AgreementThreshold:      3,
				DisputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
				PreferNonEmpty:          &common.FALSE,
				PreferLargerResponses:   &common.FALSE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess([]interface{}{})},              // empty 1
				{status: 200, body: jsonRpcSuccess([]interface{}{})},              // empty 2 (unique leader below threshold)
				{status: 200, body: jsonRpcSuccess("0xnonempty")},                 // non-empty 1
				{status: 200, body: jsonRpcError(-32603, "server error (infra)")}, // infra error
			},
			expectedCalls:  []int{1, 1, 1, 1},
			expectedResult: &expectedResult{jsonRpcResult: "[]"},
		},
		{
			name:        "empty_meets_threshold_short_circuits_without_preference",
			description: "Empty meets threshold quickly; no non-empty preference -> short-circuit to empty",
			upstreams:   createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         3,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
				PreferNonEmpty:          &common.FALSE,
				PreferLargerResponses:   &common.FALSE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess([]interface{}{})},                         // empty fast 1
				{status: 200, body: jsonRpcSuccess([]interface{}{})},                         // empty fast 2 (meets threshold)
				{status: 200, body: jsonRpcSuccess("0xlate"), delay: 300 * time.Millisecond}, // slow third
			},
			expectedCalls: []int{1, 1, 0},
			expectedResult: &expectedResult{
				jsonRpcResult: "[]",
				check: func(t *testing.T, resp *common.NormalizedResponse, duration time.Duration) {
					assert.LessOrEqual(t, duration, 100*time.Millisecond, "should short-circuit on empty when no non-empty preference")
				},
			},
		},
		{
			name:        "accept_most_common_below_threshold_empty_unique_with_preference_non_empty",
			description: "Below threshold unique empty leader vs single non-empty; with preferNonEmpty select non-empty",
			upstreams:   createTestUpstreams(4),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         4,
				AgreementThreshold:      3,
				DisputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
				PreferNonEmpty:          &common.TRUE,
				PreferLargerResponses:   &common.FALSE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess([]interface{}{})},              // empty 1
				{status: 200, body: jsonRpcSuccess([]interface{}{})},              // empty 2 (unique leader below threshold)
				{status: 200, body: jsonRpcSuccess("0xnonempty")},                 // non-empty 1
				{status: 200, body: jsonRpcError(-32603, "server error (infra)")}, // infra error
			},
			expectedCalls:  []int{1, 1, 1, 1},
			expectedResult: &expectedResult{jsonRpcResult: "\"0xnonempty\""},
		},
		{
			name:        "threshold_2_two_identical_success",
			description: "2 participants, threshold 2, both non-empty identical -> success",
			upstreams:   createTestUpstreams(2),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         2,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess("0xabc")},
				{status: 200, body: jsonRpcSuccess("0xabc")},
			},
			expectedCalls:  []int{1, 1},
			expectedResult: &expectedResult{jsonRpcResult: "\"0xabc\""},
		},
		{
			name:        "threshold_2_two_different_dispute",
			description: "2 participants, threshold 2, different non-empty -> dispute",
			upstreams:   createTestUpstreams(2),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         2,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess("0xaaa")},
				{status: 200, body: jsonRpcSuccess("0xbbb")},
			},
			expectedCalls: []int{1, 1},
			expectedError: &expectedError{code: common.ErrCodeConsensusDispute, contains: "not enough agreement"},
		},
		{
			name:        "threshold_2_two_empty_success",
			description: "2 participants, threshold 2, both empty -> success with empty",
			upstreams:   createTestUpstreams(2),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         2,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
				PreferNonEmpty:          &common.FALSE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess([]interface{}{})},
				{status: 200, body: jsonRpcSuccess([]interface{}{})},
			},
			expectedCalls:  []int{1, 1},
			expectedResult: &expectedResult{jsonRpcResult: "[]"},
		},
		{
			name:        "above_threshold_empty_wins_without_preference",
			description: "Empty group meets threshold with higher count; no non-empty preference -> select empty",
			upstreams:   createTestUpstreams(5),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         5,
				AgreementThreshold:      3,
				DisputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
				PreferNonEmpty:          &common.FALSE,
				PreferLargerResponses:   &common.FALSE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess([]interface{}{})}, // empty 1
				{status: 200, body: jsonRpcSuccess([]interface{}{})}, // empty 2
				{status: 200, body: jsonRpcSuccess([]interface{}{})}, // empty 3 (meets threshold)
				{status: 200, body: jsonRpcSuccess("0xnonempty")},    // non-empty 1
				{status: 200, body: jsonRpcSuccess("0xnonempty")},    // non-empty 2
			},
			expectedCalls:  []int{1, 1, 1, 1, 1},
			expectedResult: &expectedResult{jsonRpcResult: "[]"},
		},
		{
			name:        "low_participants_return_error_when_only_empty_valid",
			description: "Low participants with only empty valid responses and ReturnError -> low participants error",
			upstreams:   createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         3,
				AgreementThreshold:      4, // force low participants
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
				PreferNonEmpty:          &common.FALSE,
				PreferLargerResponses:   &common.FALSE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess([]interface{}{})},
				{status: 200, body: jsonRpcSuccess([]interface{}{})},
				{status: 200, body: jsonRpcSuccess([]interface{}{})},
			},
			expectedCalls: []int{1, 1, 1},
			expectedError: &expectedError{code: common.ErrCodeConsensusLowParticipants, contains: "not enough participants"},
		},
		{
			name:        "low_participants_accept_most_common_empty_vs_errors",
			description: "Low participants with AcceptMostCommon: empty valid beats infra errors",
			upstreams:   createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         3,
				AgreementThreshold:      4, // force low participants
				DisputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
				PreferNonEmpty:          &common.FALSE,
				PreferLargerResponses:   &common.FALSE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess([]interface{}{})},                // empty valid
				{status: 200, body: jsonRpcError(-32603, "upstream error (infra)")}, // infra error
				{status: 200, body: jsonRpcError(-32603, "upstream error (infra)")}, // infra error
			},
			expectedCalls:  []int{1, 1, 1},
			expectedResult: &expectedResult{jsonRpcResult: "[]"},
		},
		{
			name:        "non_consensus_error_vs_non_empty_below_threshold_prefer_non_empty",
			description: "Below threshold with a non-consensus (infra) error vs a single non-empty: prefer the valid non-empty",
			upstreams:   createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         3,
				AgreementThreshold:      3,
				DisputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
				PreferNonEmpty:          &common.TRUE,
				PreferLargerResponses:   &common.FALSE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcError(-32603, "internal server error")},
				{status: 200, body: jsonRpcError(-32603, "internal server error")},
				{status: 200, body: jsonRpcSuccess("0xres")},
			},
			expectedCalls:  []int{1, 1, 1},
			expectedResult: &expectedResult{jsonRpcResult: "\"0xres\""},
		},
		{
			name:        "below_threshold_error_unique_vs_single_non_empty_prefer_non_empty",
			description: "Below threshold unique consensus-valid error vs single non-empty; prefer non-empty under AcceptMostCommon + preferNonEmpty",
			upstreams:   createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         3,
				AgreementThreshold:      3,
				DisputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
				PreferNonEmpty:          &common.TRUE,
				PreferLargerResponses:   &common.FALSE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcError(-32000, "call exception")},
				{status: 200, body: jsonRpcError(-32000, "call exception")},
				{status: 200, body: jsonRpcSuccess("0xabc")},
			},
			expectedCalls:  []int{1, 1, 1},
			expectedResult: &expectedResult{jsonRpcResult: "\"0xabc\""},
		},
		{
			name:        "above_threshold_non_empty_vs_error_counts_choose_non_empty_when_higher",
			description: "Above threshold both non-empty and error groups; higher count wins. If tie without preference, dispute; with preferNonEmpty=true, choose non-empty",
			upstreams:   createTestUpstreams(4),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         4,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
				PreferNonEmpty:          &common.TRUE,
				PreferLargerResponses:   &common.FALSE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess("0xaaa")},
				{status: 200, body: jsonRpcSuccess("0xaaa")},
				{status: 200, body: jsonRpcError(-32000, "call exception")},
				{status: 200, body: jsonRpcError(-32000, "call exception")},
			},
			expectedCalls:  []int{1, 1, 0, 0},
			expectedResult: &expectedResult{jsonRpcResult: "\"0xaaa\""},
		},
		{
			name:        "ignore_fields_enable_consensus_on_block_timestamp",
			description: "Ignore block timestamp differences to achieve consensus",
			upstreams:   createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:    3,
				AgreementThreshold: 2,
				DisputeBehavior:    common.ConsensusDisputeBehaviorReturnError,
				IgnoreFields: map[string][]string{
					"eth_getBlockByNumber": {"timestamp"},
				},
			},
			requestMethod: "eth_getBlockByNumber",
			requestParams: []interface{}{"0x1", false},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess(map[string]interface{}{"number": "0x1", "hash": "0xabc123", "timestamp": "0xaaa", "gasLimit": "0x1", "gasUsed": "0x1"})},
				{status: 200, body: jsonRpcSuccess(map[string]interface{}{"number": "0x1", "hash": "0xabc123", "timestamp": "0xbbb", "gasLimit": "0x1", "gasUsed": "0x1"})},
				{status: 200, body: jsonRpcSuccess(map[string]interface{}{"number": "0x1", "hash": "0xdef456", "timestamp": "0xccc", "gasLimit": "0x1", "gasUsed": "0x1"})},
			},
			expectedCalls:  []int{1, 1, 1},
			expectedResult: &expectedResult{contains: "\"hash\":\"0xabc123\""},
		},
		{
			name:        "ignore_fields_dispute_when_core_fields_differ",
			description: "Even with timestamp ignored, different core fields should cause dispute",
			upstreams:   createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:    3,
				AgreementThreshold: 2,
				DisputeBehavior:    common.ConsensusDisputeBehaviorReturnError,
				IgnoreFields: map[string][]string{
					"eth_getBlockByNumber": {"timestamp"},
				},
			},
			requestMethod: "eth_getBlockByNumber",
			requestParams: []interface{}{"0x1", false},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess(map[string]interface{}{"number": "0x1", "hash": "0xaaa111", "timestamp": "0x1", "gasLimit": "0x1", "gasUsed": "0x1"})},
				{status: 200, body: jsonRpcSuccess(map[string]interface{}{"number": "0x2", "hash": "0xbbb222", "timestamp": "0x2", "gasLimit": "0x1", "gasUsed": "0x1"})},
				{status: 200, body: jsonRpcSuccess(map[string]interface{}{"number": "0x3", "hash": "0xccc333", "timestamp": "0x3", "gasLimit": "0x1", "gasUsed": "0x1"})},
			},
			expectedCalls: []int{1, 1, 1},
			expectedError: &expectedError{code: common.ErrCodeConsensusDispute, contains: "not enough agreement"},
		},
		{
			name:        "accept_most_common_below_threshold_tie_no_clear_winner_dispute",
			description: "Below threshold with AcceptMostCommon and tie between different non-empty results -> dispute",
			upstreams:   createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         3,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
				PreferNonEmpty:          &common.TRUE,
				PreferLargerResponses:   &common.FALSE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess(nil)},
				{status: 200, body: jsonRpcSuccess("0x1")},
				{status: 200, body: jsonRpcSuccess("0x2")},
			},
			expectedCalls: []int{1, 1, 1},
			expectedError: &expectedError{code: common.ErrCodeConsensusDispute, contains: "not enough agreement"},
		},
		{
			name:        "low_participants_accept_most_common_empty_only",
			description: "Low participants with AcceptMostCommon and only empty valid responses present -> return best empty",
			upstreams:   createTestUpstreams(5),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         5,
				AgreementThreshold:      3,
				DisputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
				PreferNonEmpty:          &common.TRUE,
				PreferLargerResponses:   &common.FALSE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess([]interface{}{})},
				{status: 200, body: jsonRpcSuccess([]interface{}{})},
				{status: 200, body: jsonRpcError(-32603, "server error")},
				{status: 200, body: jsonRpcError(-32603, "server error")},
				{status: 200, body: jsonRpcError(-32603, "server error")},
			},
			expectedCalls:  []int{1, 1, 1, 1, 1},
			expectedResult: &expectedResult{contains: "[]"},
		},
		{
			name:        "prefer_transaction_receipt_with_logs_over_empty_logs",
			description: "Prefer receipt with non-empty logs over receipts with empty logs under AcceptMostCommon",
			upstreams:   createTestUpstreams(4),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         4,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
				PreferNonEmpty:          &common.TRUE,
				PreferLargerResponses:   &common.TRUE,
			},
			requestMethod: "eth_getTransactionReceipt",
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess(map[string]interface{}{"logs": []interface{}{}})},
				{status: 200, body: jsonRpcSuccess(map[string]interface{}{"logs": []interface{}{}})},
				{status: 200, body: jsonRpcSuccess(map[string]interface{}{"logs": []interface{}{}})},
				{status: 200, body: jsonRpcSuccess(map[string]interface{}{"logs": []interface{}{map[string]interface{}{"address": "0x1"}}})},
			},
			expectedCalls:  []int{1, 1, 1, 1},
			expectedResult: &expectedResult{contains: "\"address\""},
		},
		{
			name:        "dispute_with_empty_preference_return_error",
			description: "Mixed empty and two different non-empty below threshold with ReturnError should dispute",
			upstreams:   createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         3,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
				PreferNonEmpty:          &common.TRUE,
				PreferLargerResponses:   &common.FALSE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess(nil)},
				{status: 200, body: jsonRpcSuccess("0x123")},
				{status: 200, body: jsonRpcSuccess("0x456")},
			},
			expectedCalls: []int{1, 1, 1},
			expectedError: &expectedError{code: common.ErrCodeConsensusDispute, contains: "not enough agreement"},
		},
		{
			name:        "prefer_non_empty_3v1_with_accept_behavior",
			description: "3 empty vs 1 non-empty; with AcceptMostCommon + preferNonEmpty, choose the non-empty",
			upstreams:   createTestUpstreams(4),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         4,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
				PreferNonEmpty:          &common.TRUE,
				PreferLargerResponses:   &common.FALSE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess(nil)},
				{status: 200, body: jsonRpcSuccess(nil)},
				{status: 200, body: jsonRpcSuccess(nil)},
				{status: 200, body: jsonRpcSuccess("0x789")},
			},
			expectedCalls:  []int{1, 1, 1, 1},
			expectedResult: &expectedResult{jsonRpcResult: "\"0x789\""},
		},
		{
			name:        "emptyish_consensus_overridden_by_meaningful_data",
			description: "Two emptyish (null) vs one non-empty; AcceptMostCommon + preferNonEmpty should pick the non-empty",
			upstreams:   createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         3,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
				PreferNonEmpty:          &common.TRUE,
				PreferLargerResponses:   &common.FALSE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess(nil)},
				{status: 200, body: jsonRpcSuccess(nil)},
				{status: 200, body: jsonRpcSuccess("0x123")},
			},
			expectedCalls:  []int{1, 1, 1},
			expectedResult: &expectedResult{jsonRpcResult: "\"0x123\""},
		},
		{
			name:        "all_emptyish_responses_return_emptyish_consensus",
			description: "All participants return emptyish (null); should return emptyish consensus",
			upstreams:   createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         3,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
				PreferNonEmpty:          &common.TRUE,
				PreferLargerResponses:   &common.FALSE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess(nil)},
				{status: 200, body: jsonRpcSuccess(nil)},
				{status: 200, body: jsonRpcSuccess(nil)},
			},
			expectedCalls:  []int{1, 1, 1},
			expectedResult: &expectedResult{jsonRpcResult: "null"},
		},
		{
			name:        "emptyish_zero_hex_overridden_by_non_empty_no_short_circuit",
			description: "Two zero-hex emptyish meet threshold first; with preferNonEmpty we should wait and select the non-empty",
			upstreams:   createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         3,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
				PreferNonEmpty:          &common.TRUE,
				PreferLargerResponses:   &common.FALSE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess("0x0")},
				{status: 200, body: jsonRpcSuccess("0x0")},
				{status: 200, body: jsonRpcSuccess("0x789"), delay: 300 * time.Millisecond},
			},
			expectedCalls: []int{1, 1, 1},
			expectedResult: &expectedResult{
				jsonRpcResult: "\"0x789\"",
				check: func(t *testing.T, resp *common.NormalizedResponse, duration time.Duration) {
					assert.GreaterOrEqual(t, duration, 300*time.Millisecond, "should not short-circuit when emptyish leads and preferNonEmpty is enabled")
				},
			},
		},
		{
			name:        "mixed_emptyish_types_overridden_by_non_empty",
			description: "Empty array and empty object vs one non-empty; prefer non-empty below threshold",
			upstreams:   createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         3,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
				PreferNonEmpty:          &common.TRUE,
				PreferLargerResponses:   &common.FALSE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess([]interface{}{})},
				{status: 200, body: jsonRpcSuccess(map[string]interface{}{})},
				{status: 200, body: jsonRpcSuccess("0xdef")},
			},
			expectedCalls:  []int{1, 1, 1},
			expectedResult: &expectedResult{jsonRpcResult: "\"0xdef\""},
		},
		{
			name:        "successful_consensus_2_of_3_prefer_larger_responses_enabled",
			description: "A simple successful consensus where 2 out of 3 upstreams agree but waits for all upstreams due to prefer larger responses flag.",
			upstreams:   createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         3,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
				PreferLargerResponses:   &common.TRUE,
				PreferNonEmpty:          &common.FALSE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess("0x7a")},
				{status: 200, body: jsonRpcSuccess("0x7a")},
				{status: 200, body: jsonRpcSuccess("0x7b"), delay: 300 * time.Millisecond},
			},
			expectedCalls: []int{1, 1, 1}, // will not short circuit because prefer larger responses is enabled
			expectedResult: &expectedResult{
				jsonRpcResult: `"0x7a"`,
				check: func(t *testing.T, resp *common.NormalizedResponse, duration time.Duration) {
					assert.GreaterOrEqual(t, duration, 300*time.Millisecond, "response should take more than 300ms and no short circuit")
				},
			},
		},
		{
			name:        "successful_consensus_2_of_3_prefer_larger_responses_disabled",
			description: "A simple successful consensus where 2 out of 3 upstreams agree.",
			upstreams:   createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         3,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
				PreferLargerResponses:   &common.FALSE,
				PreferNonEmpty:          &common.FALSE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess("0x7a")},
				{status: 200, body: jsonRpcSuccess("0x7a")},
				{status: 200, body: jsonRpcSuccess("0x7b"), delay: 300 * time.Millisecond},
			},
			expectedCalls: []int{1, 1, 0}, // last request is not sent due to short circuit
			expectedResult: &expectedResult{
				jsonRpcResult: `"0x7a"`,
				check: func(t *testing.T, resp *common.NormalizedResponse, duration time.Duration) {
					assert.Less(t, duration, 100*time.Millisecond, "response should short circuit not wait for the slowest upstream")
				},
			},
		},
		{
			name:        "low_participants_only_one_participant",
			description: "A single participant returns a result, but no consensus is reached.",
			upstreams:   createTestUpstreams(1),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         3,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess("0x7a")},
			},
			expectedCalls: []int{1},
			expectedError: &expectedError{
				code:     common.ErrCodeConsensusLowParticipants,
				contains: "not enough participants",
			},
		},
		{
			name:        "prefer_non_empty_over_empty_above_threshold_accept_most_common",
			description: "Prefer non-empty when empty meets threshold (3) but non-empty has 2; AcceptMostCommon allows selecting the non-empty",
			upstreams:   createTestUpstreams(5),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         5,
				AgreementThreshold:      3,
				DisputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
				PreferNonEmpty:          &common.TRUE,
				PreferLargerResponses:   &common.FALSE,
			},
			retryPolicy: &common.RetryPolicyConfig{MaxAttempts: 1},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess("0x11")},          // non-empty 1
				{status: 200, body: jsonRpcSuccess("0x11")},          // non-empty 2
				{status: 200, body: jsonRpcSuccess([]interface{}{})}, // empty 1
				{status: 200, body: jsonRpcSuccess([]interface{}{})}, // empty 2
				{status: 200, body: jsonRpcSuccess([]interface{}{})}, // empty 3 -> meets threshold
			},
			expectedCalls:  []int{1, 1, 1, 1, 1},
			expectedResult: &expectedResult{jsonRpcResult: `"0x11"`},
		},
		{
			name:        "prefer_non_empty_but_return_error_when_empty_wins_with_threshold",
			description: "Prefer non-empty enabled, but with ReturnError behavior and empty meeting threshold, return dispute error",
			upstreams:   createTestUpstreams(5),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         5,
				AgreementThreshold:      3,
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
				PreferNonEmpty:          &common.TRUE,
				PreferLargerResponses:   &common.FALSE,
			},
			retryPolicy: &common.RetryPolicyConfig{MaxAttempts: 1},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess("0x22")},          // non-empty 1
				{status: 200, body: jsonRpcSuccess("0x22")},          // non-empty 2
				{status: 200, body: jsonRpcSuccess([]interface{}{})}, // empty 1
				{status: 200, body: jsonRpcSuccess([]interface{}{})}, // empty 2
				{status: 200, body: jsonRpcSuccess([]interface{}{})}, // empty 3 -> meets threshold
			},
			expectedCalls: []int{1, 1, 1, 1, 1},
			expectedError: &expectedError{
				code:     common.ErrCodeConsensusDispute,
				contains: "not enough agreement among responses",
			},
		},
		{
			name:        "prefer_non_empty_when_both_groups_above_threshold",
			description: "Two groups empty and non-empty both meet threshold; preferNonEmpty selects non-empty",
			upstreams:   createTestUpstreams(6),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         6,
				AgreementThreshold:      3,
				DisputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
				PreferNonEmpty:          &common.TRUE,
				PreferLargerResponses:   &common.FALSE,
			},
			retryPolicy: &common.RetryPolicyConfig{MaxAttempts: 1},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess("0x33")},          // non-empty 1
				{status: 200, body: jsonRpcSuccess("0x33")},          // non-empty 2
				{status: 200, body: jsonRpcSuccess("0x33")},          // non-empty 3 -> meets threshold
				{status: 200, body: jsonRpcSuccess([]interface{}{})}, // empty 1
				{status: 200, body: jsonRpcSuccess([]interface{}{})}, // empty 2
				{status: 200, body: jsonRpcSuccess([]interface{}{})}, // empty 3 -> meets threshold
			},
			expectedCalls:  []int{1, 1, 0, 1, 1, 0}, // short-circuit when non-empty meets threshold and no size pref
			expectedResult: &expectedResult{jsonRpcResult: `"0x33"`},
		},
		{
			name:        "empty_vs_non_empty_tie_above_threshold_without_preference_results_in_fastest",
			description: "Two groups empty and non-empty both meet threshold with equal counts; no preference -> choose fastest to return",
			upstreams:   createTestUpstreams(6),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         6,
				AgreementThreshold:      3,
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
				PreferNonEmpty:          &common.FALSE,
				PreferLargerResponses:   &common.FALSE,
			},
			retryPolicy: &common.RetryPolicyConfig{MaxAttempts: 1},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess([]interface{}{})},                       // empty 2
				{status: 200, body: jsonRpcSuccess("0x44")},                                // non-empty 1
				{status: 200, body: jsonRpcSuccess([]interface{}{})},                       // empty 1
				{status: 200, body: jsonRpcSuccess("0x44")},                                // non-empty 2
				{status: 200, body: jsonRpcSuccess([]interface{}{})},                       // empty 3 -> meets threshold
				{status: 200, body: jsonRpcSuccess("0x44"), delay: 100 * time.Millisecond}, // non-empty 3 -> meets threshold
			},
			expectedCalls:        []int{1, 1, 1, 1, 1, -1},
			expectedResult:       &expectedResult{jsonRpcResult: `[]`},
			expectedPendingMocks: 1,
		},
		{
			name:        "dispute_on_all_different_responses",
			description: "A dispute occurs when all upstreams return different results.",
			upstreams:   createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         3,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess("0x7a")},
				{status: 200, body: jsonRpcSuccess("0x8b")},
				{status: 200, body: jsonRpcSuccess("0x9c")},
			},
			expectedCalls: []int{1, 1, 1},
			expectedError: &expectedError{
				code:     common.ErrCodeConsensusDispute,
				contains: "not enough agreement among responses",
			},
		},
		{
			name:        "retry_on_error_and_success_on_next_upstream",
			description: "Retry enabled; second upstream errors; first and third succeed, reaching 2/3 consensus.",
			upstreams:   createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         3,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
				PreferLargerResponses:   &common.TRUE,
				PreferNonEmpty:          &common.FALSE,
			},
			retryPolicy: &common.RetryPolicyConfig{
				MaxAttempts: 2,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess("0x7a")},
				{status: 200, body: jsonRpcError(-32000, "cannot query unfinalized data")},
				{status: 200, body: jsonRpcSuccess("0x7a")},
			},
			expectedCalls: []int{1, 1, 1},
			expectedResult: &expectedResult{
				jsonRpcResult: `"0x7a"`,
			},
		},
		{
			name:        "retried_failing_usptream_reach_consensus",
			description: "retry with fresh upstreams reaches consensus after first attempt fails",
			upstreams:   createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         2,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
			},
			retryPolicy: &common.RetryPolicyConfig{
				MaxAttempts: 2,
			},
			// Attempt 1: up1 succeeds, up2 fails with retryable error -> low participants (1 valid)
			// Attempt 2: up1 and up2 stay consumed, up3 is used, one participant exhausts
			// With AcceptMostCommonValidResult on low participants, we accept up1's result
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess("0x1")},                        // up1: success (Attempt 1)
				{status: 200, body: jsonRpcError(-32603, "unknown server error")}, // up2: error (Attempt 1)
				{status: 200, body: jsonRpcSuccess("0x1")},                        // up3: success (Attempt 2)
			},
			expectedCalls: []int{1, 1, 1},
			expectedResult: &expectedResult{
				jsonRpcResult: `"0x1"`,
			},
			expectedPendingMocks: 0,
		},
		{
			name:        "prefer_larger_disabled_above_threshold_different_counts_choose_higher_count",
			description: "PreferLargerResponses disabled; both non-empty groups >= threshold with different counts -> higher count wins",
			upstreams:   createTestUpstreams(5),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         5,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
				PreferLargerResponses:   &common.FALSE,
				PreferNonEmpty:          &common.FALSE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess("0x" + strings.Repeat("A", 1000))}, // larger 1 (>= threshold but fewer votes)
				{status: 200, body: jsonRpcSuccess("0x" + strings.Repeat("A", 1000))}, // larger 2
				{status: 200, body: jsonRpcSuccess("0x" + strings.Repeat("b", 10))},   // smaller 1
				{status: 200, body: jsonRpcSuccess("0x" + strings.Repeat("b", 10))},   // smaller 2
				{status: 200, body: jsonRpcSuccess("0x" + strings.Repeat("b", 10))},   // smaller 3 (higher count)
			},
			expectedCalls:  []int{1, 1, 1, 1, 1},
			expectedResult: &expectedResult{jsonRpcResult: "\"0x" + strings.Repeat("b", 10) + "\""},
		},
		{
			name:        "prefer_larger_over_error_tie_above_threshold_accept_most_common",
			description: "PreferLargerResponses enabled; non-empty and consensus-error both meet threshold with equal counts -> choose largest non-empty",
			upstreams:   createTestUpstreams(4),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         4,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
				PreferLargerResponses:   &common.TRUE,
				PreferNonEmpty:          &common.FALSE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess("0x" + strings.Repeat("C", 1000))},     // non-empty 1
				{status: 200, body: jsonRpcSuccess("0x" + strings.Repeat("C", 1000))},     // non-empty 2 (>= threshold)
				{status: 200, body: jsonRpcError(-32000, "execution reverted: reason X")}, // error 1
				{status: 200, body: jsonRpcError(-32000, "execution reverted: reason X")}, // error 2 (>= threshold)
			},
			expectedCalls:  []int{1, 1, 1, 1},
			expectedResult: &expectedResult{jsonRpcResult: "\"0x" + strings.Repeat("C", 1000) + "\""},
		},
		{
			name:        "some_participants_return_error_success_below_threshold",
			description: "3 upstreams; threshold 3; two successes and one error -> low participants",
			upstreams:   createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         3,
				AgreementThreshold:      3,
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
				PreferLargerResponses:   &common.FALSE,
				PreferNonEmpty:          &common.FALSE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess("0x7a")},
				{status: 200, body: jsonRpcError(-32000, "internal error")},
				{status: 200, body: jsonRpcSuccess("0x7a")},
			},
			expectedCalls: []int{1, 1, 1},
			expectedError: &expectedError{
				code:     common.ErrCodeConsensusLowParticipants,
				contains: "not enough participants",
			},
		},
		{
			name:        "some_participants_return_error_success_above_threshold",
			description: "3 upstreams; threshold 2; two successes and one error -> success",
			upstreams:   createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         3,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
				PreferLargerResponses:   &common.FALSE,
				PreferNonEmpty:          &common.FALSE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess("0x7a")},
				{status: 200, body: jsonRpcError(-32000, "internal error")},
				{status: 200, body: jsonRpcSuccess("0x7a")},
			},
			expectedCalls:  []int{1, 1, 1},
			expectedResult: &expectedResult{jsonRpcResult: `"0x7a"`},
		},
		{
			name:        "some_participants_return_error_and_return_error_on_low_participants",
			description: "Explicit low participants ReturnError; two successes below threshold -> error",
			upstreams:   createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         3,
				AgreementThreshold:      3,
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
				PreferLargerResponses:   &common.FALSE,
				PreferNonEmpty:          &common.FALSE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess("0x7a")},
				{status: 200, body: jsonRpcError(-32000, "internal error")},
				{status: 200, body: jsonRpcSuccess("0x7a")},
			},
			expectedCalls: []int{1, 1, 1},
			expectedError: &expectedError{
				code:     common.ErrCodeConsensusLowParticipants,
				contains: "not enough participants",
			},
		},
		{
			name:        "not_enough_upstreams_low_participants_behavior_accept_most_common_valid_result",
			description: "2 upstreams only; threshold 3; AcceptMostCommon -> return best valid success",
			upstreams:   createTestUpstreams(2),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         2,
				AgreementThreshold:      3,
				DisputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
				PreferLargerResponses:   &common.FALSE,
				PreferNonEmpty:          &common.FALSE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess("0x7a")},
				{status: 200, body: jsonRpcError(-32000, "internal error")},
			},
			expectedCalls:  []int{1, 1},
			expectedResult: &expectedResult{jsonRpcResult: `"0x7a"`},
		},
		{
			name:        "agreed_unknown_error_response_on_upstreams",
			description: "All participants return the same error; consensus on error is returned",
			upstreams:   createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         3,
				AgreementThreshold:      3,
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcError(-32000, "random error")},
				{status: 200, body: jsonRpcError(-32000, "random error")},
				{status: 200, body: jsonRpcError(-32000, "random error")},
			},
			expectedCalls: []int{1, 1, 1},
			expectedError: &expectedError{
				code:     common.ErrCodeUpstreamRequest,
				contains: "random error",
			},
		},
		{
			name:        "dispute_behavior_return_error_on_three_different",
			description: "3 upstreams, all different; ReturnError dispute",
			upstreams:   createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:    3,
				AgreementThreshold: 2,
				DisputeBehavior:    common.ConsensusDisputeBehaviorReturnError,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess("0xaaa")},
				{status: 200, body: jsonRpcSuccess("0xbbb")},
				{status: 200, body: jsonRpcSuccess("0xccc")},
			},
			expectedCalls: []int{1, 1, 1},
			expectedError: &expectedError{code: common.ErrCodeConsensusDispute, contains: "not enough agreement among responses"},
		},
		{
			name:        "low_participants_accept_most_common_picks_non_empty",
			description: "5 upstreams, only 2 valid respond (empty + non-empty), low participants AcceptMostCommon → non-empty",
			upstreams:   createTestUpstreams(5),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         5,
				AgreementThreshold:      3,
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess([]interface{}{})},
				{status: 200, body: jsonRpcSuccess("0xdata")},
				{status: 200, body: jsonRpcError(-32603, "server error")},
				{status: 200, body: jsonRpcError(-32603, "server error")},
				{status: 200, body: jsonRpcError(-32603, "server error")},
			},
			expectedCalls:  []int{1, 1, 1, 1, 1},
			expectedResult: &expectedResult{jsonRpcResult: "\"0xdata\""},
		},
		{
			name:        "threshold_1_single_response_succeeds",
			description: "1 upstream only; threshold 1; should succeed with that response",
			upstreams:   createTestUpstreams(1),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:    1,
				AgreementThreshold: 1,
			},
			mockResponses:  []mockResponse{{status: 200, body: jsonRpcSuccess("0xsingle")}},
			expectedCalls:  []int{1},
			expectedResult: &expectedResult{jsonRpcResult: "\"0xsingle\""},
		},
		{
			name:        "some_participants_return_error_success_below_threshold",
			description: "3 upstreams; threshold 3; two successes and one error -> low participants",
			upstreams:   createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         3,
				AgreementThreshold:      3,
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
				PreferLargerResponses:   &common.FALSE,
				PreferNonEmpty:          &common.FALSE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess("0x7a")},
				{status: 200, body: jsonRpcError(-32000, "internal error")},
				{status: 200, body: jsonRpcSuccess("0x7a")},
			},
			expectedCalls: []int{1, 1, 1},
			expectedError: &expectedError{code: common.ErrCodeConsensusLowParticipants, contains: "not enough participants"},
		},
		{
			name:        "some_participants_return_error_success_above_threshold",
			description: "3 upstreams; threshold 2; two successes and one error -> success",
			upstreams:   createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         3,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
				PreferLargerResponses:   &common.FALSE,
				PreferNonEmpty:          &common.FALSE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess("0x7a")},
				{status: 200, body: jsonRpcError(-32000, "internal error")},
				{status: 200, body: jsonRpcSuccess("0x7a")},
			},
			expectedCalls:  []int{1, 1, 1},
			expectedResult: &expectedResult{jsonRpcResult: "\"0x7a\""},
		},
		{
			name:        "some_participants_return_error_and_return_error_on_low_participants",
			description: "Explicit low participants ReturnError; two successes below threshold -> error",
			upstreams:   createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         3,
				AgreementThreshold:      3,
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
				PreferLargerResponses:   &common.FALSE,
				PreferNonEmpty:          &common.FALSE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess("0x7a")},
				{status: 200, body: jsonRpcError(-32000, "internal error")},
				{status: 200, body: jsonRpcSuccess("0x7a")},
			},
			expectedCalls: []int{1, 1, 1},
			expectedError: &expectedError{code: common.ErrCodeConsensusLowParticipants, contains: "not enough participants"},
		},
		{
			name:        "not_enough_upstreams_low_participants_behavior_accept_most_common_valid_result",
			description: "2 upstreams only; threshold 3; AcceptMostCommon -> return best valid success",
			upstreams:   createTestUpstreams(2),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         2,
				AgreementThreshold:      3,
				DisputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
				PreferLargerResponses:   &common.FALSE,
				PreferNonEmpty:          &common.FALSE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess("0x7a")},
				{status: 200, body: jsonRpcError(-32000, "internal error")},
			},
			expectedCalls:  []int{1, 1},
			expectedResult: &expectedResult{jsonRpcResult: "\"0x7a\""},
		},
		{
			name:        "agreed_unknown_error_response_on_upstreams",
			description: "All participants return the same error; consensus on error is returned",
			upstreams:   createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         3,
				AgreementThreshold:      3,
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcError(-32000, "random error")},
				{status: 200, body: jsonRpcError(-32000, "random error")},
				{status: 200, body: jsonRpcError(-32000, "random error")},
			},
			expectedCalls: []int{1, 1, 1},
			expectedError: &expectedError{code: common.ErrCodeUpstreamRequest, contains: "random error"},
		},
		{
			name:        "accept_most_common_below_threshold_5_participants",
			description: "5 upstreams, threshold 3; only 2 agree, AcceptMostCommon should return the best group",
			upstreams:   createTestUpstreams(5),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         5,
				AgreementThreshold:      3,
				DisputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
				PreferNonEmpty:          &common.FALSE,
				PreferLargerResponses:   &common.FALSE,
			},
			retryPolicy: &common.RetryPolicyConfig{MaxAttempts: 1},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess("0xaaa")},                          // up1
				{status: 200, body: jsonRpcSuccess("0xaaa")},                          // up2 -> best group size 2
				{status: 200, body: jsonRpcSuccess("0xbbb")},                          // up3
				{status: 200, body: jsonRpcError(-32000, "cannot query unfinalized")}, // up4 (error group)
				{status: 200, body: jsonRpcError(-32003, "tx rejected")},              // up5 (another error)
			},
			expectedCalls: []int{1, 1, 1, 1, 1},
			expectedResult: &expectedResult{
				jsonRpcResult: `"0xaaa"`,
			},
		},
		{
			name:        "prefer_larger_1_big_below_threshold_vs_3_small_below_threshold_accept_most_common",
			description: "Prefer larger enabled; below threshold; AcceptMostCommon -> pick 1 largest non-empty; no short circuit",
			upstreams:   createTestUpstreams(4),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         4,
				AgreementThreshold:      3,
				DisputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
				PreferLargerResponses:   &common.TRUE,
				PreferNonEmpty:          &common.FALSE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess(strings.Repeat("A", 2000))}, // large non-empty (unique)
				{status: 200, body: jsonRpcSuccess(strings.Repeat("b", 10))},   // small non-empty 1
				{status: 200, body: jsonRpcSuccess(strings.Repeat("c", 10))},   // small non-empty 2
				{status: 200, body: jsonRpcSuccess(strings.Repeat("d", 10))},   // small non-empty 3
			},
			expectedCalls:  []int{1, 1, 1, 1},
			expectedResult: &expectedResult{jsonRpcResult: "\"" + strings.Repeat("A", 2000) + "\""},
		},
		{
			name:        "no_larger_response_preference_threshold_met_short_circuit",
			description: "non-empty response meeting threshold + PreferLargerResponses=false should short-circuit",
			upstreams:   createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         3,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
				PreferNonEmpty:          &common.TRUE,
				PreferLargerResponses:   &common.FALSE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess("0xabc")},
				{status: 200, body: jsonRpcSuccess("0xabc")},
				{status: 200, body: jsonRpcSuccess("0xabc"), delay: 300 * time.Millisecond},
			},
			expectedCalls: []int{1, 1, 0},
			expectedResult: &expectedResult{
				jsonRpcResult: "\"0xabc\"",
				check: func(t *testing.T, resp *common.NormalizedResponse, duration time.Duration) {
					assert.LessOrEqual(t, duration, 100*time.Millisecond, "should short-circuit when non-empty response meets threshold and PreferLargerResponses is disabled")
				},
			},
		},
		{
			name:        "prefer_non_empty_above_threshold_empty_has_higher_count_accept_most_common",
			description: "Both groups above threshold, empty has higher count; AcceptMostCommon + preferNonEmpty should pick non-empty",
			upstreams:   createTestUpstreams(5),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         5,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
				PreferNonEmpty:          &common.TRUE,
				PreferLargerResponses:   &common.FALSE,
			},
			retryPolicy: &common.RetryPolicyConfig{MaxAttempts: 1},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess("0x55")},          // non-empty 1
				{status: 200, body: jsonRpcSuccess("0x55")},          // non-empty 2 (>= threshold)
				{status: 200, body: jsonRpcSuccess([]interface{}{})}, // empty 1
				{status: 200, body: jsonRpcSuccess([]interface{}{})}, // empty 2
				{status: 200, body: jsonRpcSuccess([]interface{}{})}, // empty 3 (empty has higher count)
			},
			expectedCalls:  []int{1, 1, 1, 1, 1},
			expectedResult: &expectedResult{jsonRpcResult: "\"0x55\""},
		},
		{
			name:        "prefer_non_empty_above_threshold_empty_arrives_first_no_short_circuit",
			description: "Empty responses arrive earlier and meet threshold first; with preferNonEmpty we should not short-circuit and wait for non-empty",
			upstreams:   createTestUpstreams(5),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         5,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
				PreferNonEmpty:          &common.TRUE,
				PreferLargerResponses:   &common.FALSE,
			},
			retryPolicy: &common.RetryPolicyConfig{MaxAttempts: 1},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess([]interface{}{})},                       // empty 1 (fast)
				{status: 200, body: jsonRpcSuccess([]interface{}{})},                       // empty 2 (meets threshold fast)
				{status: 200, body: jsonRpcSuccess("0x55"), delay: 300 * time.Millisecond}, // non-empty 1 (slow)
				{status: 200, body: jsonRpcSuccess("0x55"), delay: 300 * time.Millisecond}, // non-empty 2 (slow, meets threshold)
				{status: 200, body: jsonRpcSuccess([]interface{}{})},                       // empty 3
			},
			expectedCalls: []int{1, 1, 1, 1, 1},
			expectedResult: &expectedResult{
				jsonRpcResult: "\"0x55\"",
				check: func(t *testing.T, resp *common.NormalizedResponse, duration time.Duration) {
					assert.GreaterOrEqual(t, duration, 300*time.Millisecond, "should not short-circuit when preferNonEmpty is enabled and empty leads with participants remaining")
				},
			},
		},
		{
			name:        "prefer_larger_above_threshold_small_has_higher_count_accept_most_common",
			description: "Prefer larger enabled; both groups above threshold but smaller has higher count; choose largest",
			upstreams:   createTestUpstreams(5),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         5,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
				PreferLargerResponses:   &common.TRUE,
				PreferNonEmpty:          &common.FALSE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess(strings.Repeat("x", 10))},   // small 1
				{status: 200, body: jsonRpcSuccess(strings.Repeat("x", 10))},   // small 2
				{status: 200, body: jsonRpcSuccess(strings.Repeat("A", 2000))}, // large 1
				{status: 200, body: jsonRpcSuccess(strings.Repeat("A", 2000))}, // large 2 (>= threshold)
				{status: 200, body: jsonRpcSuccess(strings.Repeat("x", 10))},   // small 3 (small has higher count)
			},
			expectedCalls:  []int{1, 1, 1, 1, 1},
			expectedResult: &expectedResult{jsonRpcResult: "\"" + strings.Repeat("A", 2000) + "\""},
		},
		{
			name:        "prefer_larger_above_threshold_small_has_higher_count_no_short_circuit_when_empty_leads",
			description: "Empty responses lead early; with PreferNonEmpty enabled we should not short-circuit and wait to select the larger non-empty under AcceptMostCommon",
			upstreams:   createTestUpstreams(7),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         7,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
				PreferLargerResponses:   &common.TRUE,
				PreferNonEmpty:          &common.TRUE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess([]interface{}{})},                                          // empty 1 (fast)
				{status: 200, body: jsonRpcSuccess([]interface{}{})},                                          // empty 2 (empty meets threshold fast)
				{status: 200, body: jsonRpcSuccess(strings.Repeat("x", 10))},                                  // small 1
				{status: 200, body: jsonRpcSuccess(strings.Repeat("x", 10))},                                  // small 2 (small meets threshold)
				{status: 200, body: jsonRpcSuccess(strings.Repeat("x", 10))},                                  // small 3 (small has higher count)
				{status: 200, body: jsonRpcSuccess(strings.Repeat("A", 2000)), delay: 300 * time.Millisecond}, // large 1 (slow)
				{status: 200, body: jsonRpcSuccess(strings.Repeat("A", 2000)), delay: 300 * time.Millisecond}, // large 2 (slow, meets threshold)
			},
			expectedCalls: []int{1, 1, 1, 1, 1, -1, -1},
			expectedResult: &expectedResult{
				jsonRpcResult: "\"" + strings.Repeat("A", 2000) + "\"",
				check: func(t *testing.T, resp *common.NormalizedResponse, duration time.Duration) {
					assert.GreaterOrEqual(t, duration, 300*time.Millisecond, "should not short-circuit when preferences are enabled and empty is leading with remaining participants")
				},
			},
			expectedPendingMocks: 2,
		},
		{
			name:        "accept_most_common_below_threshold_prefer_non_empty_over_empty",
			description: "Below threshold; empty uniquely leads; preferNonEmpty selects best non-empty",
			upstreams:   createTestUpstreams(4),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         4,
				AgreementThreshold:      3,
				DisputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
				PreferNonEmpty:          &common.TRUE,
				PreferLargerResponses:   &common.FALSE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess([]interface{}{})}, // empty 1
				{status: 200, body: jsonRpcSuccess([]interface{}{})}, // empty 2 (unique leader below threshold)
				{status: 200, body: jsonRpcSuccess("0x1")},           // non-empty 1
				{status: 200, body: jsonRpcSuccess("0x1")},           // non-empty 2
			},
			expectedCalls:  []int{1, 1, 1, 1},
			expectedResult: &expectedResult{jsonRpcResult: "\"0x1\""},
		},
		{
			name:        "dispute_below_threshold_return_error_5_participants",
			description: "5 upstreams, threshold 3; only 2 agree, ReturnError should yield dispute",
			upstreams:   createTestUpstreams(5),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         5,
				AgreementThreshold:      3,
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
				PreferNonEmpty:          &common.FALSE,
				PreferLargerResponses:   &common.FALSE,
			},
			retryPolicy: &common.RetryPolicyConfig{MaxAttempts: 1},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess("0xaaa")},                          // up1
				{status: 200, body: jsonRpcSuccess("0xaaa")},                          // up2 -> best group size 2
				{status: 200, body: jsonRpcSuccess("0xbbb")},                          // up3
				{status: 200, body: jsonRpcError(-32000, "cannot query unfinalized")}, // up4 (error group)
				{status: 200, body: jsonRpcError(-32003, "tx rejected")},              // up5 (another error)
			},
			expectedCalls: []int{1, 1, 1, 1, 1},
			expectedError: &expectedError{
				code:     common.ErrCodeConsensusDispute,
				contains: "not enough agreement among responses",
			},
		},
		{
			name:        "prefer_larger_1_big_below_threshold_vs_3_small_above_threshold_return_error",
			description: "Prefer larger enabled; smaller group meets threshold; ReturnError -> dispute (do not accept smaller)",
			upstreams:   createTestUpstreams(4),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         4,
				AgreementThreshold:      3,
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
				PreferLargerResponses:   &common.TRUE,
				PreferNonEmpty:          &common.FALSE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess(strings.Repeat("A", 2000))}, // large non-empty (unique)
				{status: 200, body: jsonRpcSuccess(strings.Repeat("x", 10))},   // small non-empty 1
				{status: 200, body: jsonRpcSuccess(strings.Repeat("x", 10))},   // small non-empty 2
				{status: 200, body: jsonRpcSuccess(strings.Repeat("x", 10))},   // small non-empty 3 (meets threshold)
			},
			expectedCalls: []int{1, 1, 1, 1},
			expectedError: &expectedError{code: common.ErrCodeConsensusDispute, contains: "not enough agreement"},
		},
		{
			name:        "prefer_larger_2_large_meet_threshold_vs_2_small_meet_threshold_accept_most_common",
			description: "Prefer larger enabled; both groups meet threshold; AcceptMostCommon -> choose larger",
			upstreams:   createTestUpstreams(4),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         4,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
				PreferLargerResponses:   &common.TRUE,
				PreferNonEmpty:          &common.FALSE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess(strings.Repeat("A", 2000))}, // large 1
				{status: 200, body: jsonRpcSuccess(strings.Repeat("A", 2000))}, // large 2 (meets threshold)
				{status: 200, body: jsonRpcSuccess(strings.Repeat("b", 10))},   // small 1
				{status: 200, body: jsonRpcSuccess(strings.Repeat("b", 10))},   // small 2 (meets threshold)
			},
			expectedCalls:  []int{1, 1, 1, 1},
			expectedResult: &expectedResult{jsonRpcResult: "\"" + strings.Repeat("A", 2000) + "\""},
		},
		{
			name:        "prefer_larger_2_large_meet_threshold_vs_2_small_meet_threshold_return_error",
			description: "Prefer larger enabled; both groups meet threshold; ReturnError -> still choose larger (preference applies)",
			upstreams:   createTestUpstreams(4),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         4,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
				PreferLargerResponses:   &common.TRUE,
				PreferNonEmpty:          &common.FALSE,
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess(strings.Repeat("A", 2000))}, // large 1
				{status: 200, body: jsonRpcSuccess(strings.Repeat("A", 2000))}, // large 2 (meets threshold)
				{status: 200, body: jsonRpcSuccess(strings.Repeat("b", 10))},   // small 1
				{status: 200, body: jsonRpcSuccess(strings.Repeat("b", 10))},   // small 2 (meets threshold)
			},
			expectedCalls:  []int{1, 1, 1, 1},
			expectedResult: &expectedResult{jsonRpcResult: "\"" + strings.Repeat("A", 2000) + "\""},
		},
		{
			name:        "dispute_below_threshold_return_error_5_participants",
			description: "5 upstreams, threshold 3; only 2 agree, ReturnError should yield dispute",
			upstreams:   createTestUpstreams(5),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         5,
				AgreementThreshold:      3,
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
				PreferNonEmpty:          &common.FALSE,
				PreferLargerResponses:   &common.FALSE,
			},
			retryPolicy: &common.RetryPolicyConfig{MaxAttempts: 1},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess("0xaaa")},                          // up1
				{status: 200, body: jsonRpcSuccess("0xaaa")},                          // up2 -> best group size 2
				{status: 200, body: jsonRpcSuccess("0xbbb")},                          // up3
				{status: 200, body: jsonRpcError(-32000, "cannot query unfinalized")}, // up4
				{status: 200, body: jsonRpcError(-32003, "tx rejected")},              // up5
			},
			expectedCalls: []int{1, 1, 1, 1, 1},
			expectedError: &expectedError{
				code:     common.ErrCodeConsensusDispute,
				contains: "not enough agreement among responses",
			},
		},
		{
			name:        "retried_failing_usptream_but_cannot_reach_consensus",
			description: "retry with fresh upstreams but all fail, exhausts retries",
			upstreams:   createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         2,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
			},
			retryPolicy: &common.RetryPolicyConfig{
				MaxAttempts: 2,
			},
			// All upstreams return errors -> exhausts all retries
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcError(-32603, "unknown server error")}, // up1: error
				{status: 200, body: jsonRpcError(-32603, "unknown server error")}, // up2: error
				{status: 200, body: jsonRpcError(-32603, "unknown server error")}, // up3: error
			},
			expectedCalls: []int{1, 1, 1},
			expectedError: &expectedError{
				code:     common.ErrCodeFailsafeRetryExceeded,
				contains: "gave up retrying",
			},
			expectedPendingMocks: 0,
		},

		// ======== PreferHighestValueFor tests ========

		// ======== PreferHighestValueFor tests with agreementThreshold ========

		// Basic highest value tests (threshold=1: any single response qualifies)
		{
			name:          "prefer_highest_value_eth_getTransactionCount_returns_highest_nonce",
			description:   "eth_getTransactionCount with preferHighestValueFor returns the highest nonce value when threshold=1",
			requestMethod: "eth_getTransactionCount",
			requestParams: []interface{}{"0x1234567890123456789012345678901234567890", "latest"},
			upstreams:     createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:    3,
				AgreementThreshold: 1, // Any single response qualifies
				PreferHighestValueFor: map[string][]string{
					"eth_getTransactionCount": {"result"},
				},
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess("0x5")}, // nonce = 5
				{status: 200, body: jsonRpcSuccess("0xa")}, // nonce = 10 (highest)
				{status: 200, body: jsonRpcSuccess("0x3")}, // nonce = 3
			},
			expectedCalls:  []int{1, 1, 1},
			expectedResult: &expectedResult{jsonRpcResult: `"0xa"`},
		},
		{
			name:          "prefer_highest_value_eth_getTransactionCount_all_same_nonce",
			description:   "eth_getTransactionCount with all upstreams returning same nonce",
			requestMethod: "eth_getTransactionCount",
			requestParams: []interface{}{"0x1234567890123456789012345678901234567890", "latest"},
			upstreams:     createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:    3,
				AgreementThreshold: 2,
				PreferHighestValueFor: map[string][]string{
					"eth_getTransactionCount": {"result"},
				},
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess("0x7")},
				{status: 200, body: jsonRpcSuccess("0x7")},
				{status: 200, body: jsonRpcSuccess("0x7")},
			},
			expectedCalls:  []int{1, 1, 1},
			expectedResult: &expectedResult{jsonRpcResult: `"0x7"`},
		},

		// Threshold tests - agreed value beats single higher value
		{
			name:          "prefer_highest_value_threshold_agreed_value_beats_single_higher",
			description:   "With threshold=2, two upstreams agreeing on lower nonce beats single upstream with higher nonce",
			requestMethod: "eth_getTransactionCount",
			requestParams: []interface{}{"0x1234567890123456789012345678901234567890", "latest"},
			upstreams:     createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:    3,
				AgreementThreshold: 2, // Need at least 2 to agree
				PreferHighestValueFor: map[string][]string{
					"eth_getTransactionCount": {"result"},
				},
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess("0x5")},  // nonce = 5 (agreed)
				{status: 200, body: jsonRpcSuccess("0x5")},  // nonce = 5 (agreed) - 2 upstreams agree
				{status: 200, body: jsonRpcSuccess("0x10")}, // nonce = 16 (higher but only 1 agrees)
			},
			expectedCalls:  []int{1, 1, 1},
			expectedResult: &expectedResult{jsonRpcResult: `"0x5"`}, // Lower value wins because it meets threshold
		},
		{
			name:          "prefer_highest_value_threshold_no_value_meets_threshold_returns_error",
			description:   "When no value meets the threshold, return dispute error",
			requestMethod: "eth_getTransactionCount",
			requestParams: []interface{}{"0x1234567890123456789012345678901234567890", "latest"},
			upstreams:     createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:    3,
				AgreementThreshold: 2, // Need at least 2 to agree
				PreferHighestValueFor: map[string][]string{
					"eth_getTransactionCount": {"result"},
				},
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess("0x5")}, // unique
				{status: 200, body: jsonRpcSuccess("0xa")}, // unique
				{status: 200, body: jsonRpcSuccess("0x3")}, // unique - no 2 upstreams agree
			},
			expectedCalls: []int{1, 1, 1},
			expectedError: &expectedError{code: common.ErrCodeConsensusDispute, contains: "no value met agreement threshold"},
		},
		{
			name:          "prefer_highest_value_threshold_highest_among_qualifying",
			description:   "Among values that meet threshold, return the highest",
			requestMethod: "eth_getTransactionCount",
			requestParams: []interface{}{"0x1234567890123456789012345678901234567890", "latest"},
			upstreams:     createTestUpstreams(5),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:    5,
				AgreementThreshold: 2, // Need at least 2 to agree
				PreferHighestValueFor: map[string][]string{
					"eth_getTransactionCount": {"result"},
				},
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess("0x5")}, // nonce = 5 (agreed x2)
				{status: 200, body: jsonRpcSuccess("0x5")}, // nonce = 5 (agreed x2)
				{status: 200, body: jsonRpcSuccess("0x8")}, // nonce = 8 (agreed x2) - highest qualifying
				{status: 200, body: jsonRpcSuccess("0x8")}, // nonce = 8 (agreed x2)
				{status: 200, body: jsonRpcSuccess("0xf")}, // nonce = 15 (only 1 agrees - doesn't qualify)
			},
			expectedCalls:  []int{1, 1, 1, 1, 1},
			expectedResult: &expectedResult{jsonRpcResult: `"0x8"`}, // Highest among qualifying values
		},
		{
			name:          "prefer_highest_value_eth_getTransactionCount_with_errors_ignores_errors",
			description:   "eth_getTransactionCount with preferHighestValueFor ignores error responses and returns highest from valid ones",
			requestMethod: "eth_getTransactionCount",
			requestParams: []interface{}{"0x1234567890123456789012345678901234567890", "latest"},
			upstreams:     createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:    3,
				AgreementThreshold: 1, // threshold=1 so any single response qualifies
				PreferHighestValueFor: map[string][]string{
					"eth_getTransactionCount": {"result"},
				},
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess("0x5")},                     // nonce = 5
				{status: 200, body: jsonRpcError(-32000, "server overloaded")}, // error (ignored)
				{status: 200, body: jsonRpcSuccess("0x8")},                     // nonce = 8 (highest valid)
			},
			expectedCalls:  []int{1, 1, 1},
			expectedResult: &expectedResult{jsonRpcResult: `"0x8"`},
		},
		{
			name:          "prefer_highest_value_eth_getTransactionCount_large_hex_values",
			description:   "eth_getTransactionCount correctly compares large hex values",
			requestMethod: "eth_getTransactionCount",
			requestParams: []interface{}{"0x1234567890123456789012345678901234567890", "latest"},
			upstreams:     createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:    3,
				AgreementThreshold: 1, // threshold=1 so any single response qualifies
				PreferHighestValueFor: map[string][]string{
					"eth_getTransactionCount": {"result"},
				},
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess("0xffffff")}, // 16777215
				{status: 200, body: jsonRpcSuccess("0x100000")}, // 1048576
				{status: 200, body: jsonRpcSuccess("0xfffffe")}, // 16777214
			},
			expectedCalls:  []int{1, 1, 1},
			expectedResult: &expectedResult{jsonRpcResult: `"0xffffff"`},
		},
		{
			name:          "prefer_highest_value_not_configured_for_method_uses_normal_consensus",
			description:   "Method not in preferHighestValueFor falls back to normal hash-based consensus",
			requestMethod: "eth_blockNumber",
			upstreams:     createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:    3,
				AgreementThreshold: 2,
				DisputeBehavior:    common.ConsensusDisputeBehaviorReturnError,
				PreferHighestValueFor: map[string][]string{
					"eth_getTransactionCount": {"result"}, // Only configured for eth_getTransactionCount
				},
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess("0x100")}, // block 256
				{status: 200, body: jsonRpcSuccess("0x100")}, // block 256 (same - meets consensus)
				{status: 200, body: jsonRpcSuccess("0x200")}, // block 512 (different)
			},
			expectedCalls:  []int{1, 1, 0},                            // Short-circuits after 2 agree
			expectedResult: &expectedResult{jsonRpcResult: `"0x100"`}, // Returns consensus value, not highest
		},
		{
			name:          "prefer_highest_value_all_errors_falls_through_to_upstream_error",
			description:   "When all responses are server errors (infra errors), falls through to normal consensus which returns the error",
			requestMethod: "eth_getTransactionCount",
			requestParams: []interface{}{"0x1234567890123456789012345678901234567890", "latest"},
			upstreams:     createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:    3,
				AgreementThreshold: 2,
				PreferHighestValueFor: map[string][]string{
					"eth_getTransactionCount": {"result"},
				},
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcError(-32603, "internal server error")}, // infra error
				{status: 200, body: jsonRpcError(-32603, "internal server error")}, // infra error
				{status: 200, body: jsonRpcError(-32603, "internal server error")}, // infra error
			},
			expectedCalls: []int{1, 1, 1},
			// When all are infra errors, preferHighestValueFor can't extract values, falls through to normal consensus
			// Normal consensus sees all infra errors with same hash and returns the error
			expectedError: &expectedError{code: common.ErrCodeUpstreamRequest, contains: "internal server error"},
		},

		// eth_getTransactionByHash with nested field tests
		{
			name:          "prefer_highest_value_nested_field_nonce",
			description:   "eth_getTransactionByHash with nested nonce field returns tx with highest nonce",
			requestMethod: "eth_getTransactionByHash",
			requestParams: []interface{}{"0xabcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"},
			upstreams:     createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:    3,
				AgreementThreshold: 1, // threshold=1 so any single response qualifies
				PreferHighestValueFor: map[string][]string{
					"eth_getTransactionByHash": {"nonce"},
				},
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess(map[string]interface{}{"nonce": "0x5", "hash": "0xaaa"})},
				{status: 200, body: jsonRpcSuccess(map[string]interface{}{"nonce": "0xf", "hash": "0xbbb"})}, // highest
				{status: 200, body: jsonRpcSuccess(map[string]interface{}{"nonce": "0x3", "hash": "0xccc"})},
			},
			expectedCalls:  []int{1, 1, 1},
			expectedResult: &expectedResult{contains: `"nonce":"0xf"`},
		},
		{
			name:          "prefer_highest_value_multiple_fields_tiebreaker",
			description:   "Multiple fields act as tie-breakers: same nonce, different blockNumber",
			requestMethod: "eth_getTransactionByHash",
			requestParams: []interface{}{"0xabcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"},
			upstreams:     createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:    3,
				AgreementThreshold: 1, // threshold=1 so any single response qualifies
				PreferHighestValueFor: map[string][]string{
					"eth_getTransactionByHash": {"nonce", "blockNumber"},
				},
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess(map[string]interface{}{"nonce": "0x5", "blockNumber": "0x100", "hash": "0xaaa"})},
				{status: 200, body: jsonRpcSuccess(map[string]interface{}{"nonce": "0x5", "blockNumber": "0x200", "hash": "0xbbb"})}, // same nonce, higher block
				{status: 200, body: jsonRpcSuccess(map[string]interface{}{"nonce": "0x5", "blockNumber": "0x50", "hash": "0xccc"})},
			},
			expectedCalls:  []int{1, 1, 1},
			expectedResult: &expectedResult{contains: `"blockNumber":"0x200"`}, // Tie-broken by blockNumber
		},
		{
			name:          "prefer_highest_value_first_field_wins_over_second",
			description:   "First field has priority: higher nonce wins even if blockNumber is lower",
			requestMethod: "eth_getTransactionByHash",
			requestParams: []interface{}{"0xabcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"},
			upstreams:     createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:    3,
				AgreementThreshold: 1, // threshold=1 so any single response qualifies
				PreferHighestValueFor: map[string][]string{
					"eth_getTransactionByHash": {"nonce", "blockNumber"},
				},
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess(map[string]interface{}{"nonce": "0x5", "blockNumber": "0x1000", "hash": "0xaaa"})},
				{status: 200, body: jsonRpcSuccess(map[string]interface{}{"nonce": "0xa", "blockNumber": "0x10", "hash": "0xbbb"})}, // higher nonce, lower block
				{status: 200, body: jsonRpcSuccess(map[string]interface{}{"nonce": "0x3", "blockNumber": "0x2000", "hash": "0xccc"})},
			},
			expectedCalls:  []int{1, 1, 1},
			expectedResult: &expectedResult{contains: `"nonce":"0xa"`}, // Higher nonce wins despite lower blockNumber
		},
		{
			name:          "prefer_highest_value_empty_response_ignored",
			description:   "Empty/null responses are ignored when comparing highest values",
			requestMethod: "eth_getTransactionCount",
			requestParams: []interface{}{"0x1234567890123456789012345678901234567890", "latest"},
			upstreams:     createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:    3,
				AgreementThreshold: 1, // threshold=1 so any single response qualifies
				PreferHighestValueFor: map[string][]string{
					"eth_getTransactionCount": {"result"},
				},
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess("0x5")},
				{status: 200, body: jsonRpcSuccess(nil)},   // null result
				{status: 200, body: jsonRpcSuccess("0x8")}, // highest valid
			},
			expectedCalls:  []int{1, 1, 1},
			expectedResult: &expectedResult{jsonRpcResult: `"0x8"`},
		},
		{
			name:          "prefer_highest_value_decimal_string_handling",
			description:   "Decimal strings are also handled correctly",
			requestMethod: "eth_getTransactionCount",
			requestParams: []interface{}{"0x1234567890123456789012345678901234567890", "latest"},
			upstreams:     createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:    3,
				AgreementThreshold: 1, // threshold=1 so any single response qualifies
				PreferHighestValueFor: map[string][]string{
					"eth_getTransactionCount": {"result"},
				},
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess("0x10")}, // 16 in hex
				{status: 200, body: jsonRpcSuccess("0x14")}, // 20 in hex (highest)
				{status: 200, body: jsonRpcSuccess("0xc")},  // 12 in hex
			},
			expectedCalls:  []int{1, 1, 1},
			expectedResult: &expectedResult{jsonRpcResult: `"0x14"`},
		},
		{
			name:          "prefer_highest_value_two_upstreams_returns_highest",
			description:   "With only 2 upstreams, returns the highest value",
			requestMethod: "eth_getTransactionCount",
			requestParams: []interface{}{"0x1234567890123456789012345678901234567890", "latest"},
			upstreams:     createTestUpstreams(2),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:    2,
				AgreementThreshold: 1, // threshold=1 so any single response qualifies
				PreferHighestValueFor: map[string][]string{
					"eth_getTransactionCount": {"result"},
				},
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess("0x5")},
				{status: 200, body: jsonRpcSuccess("0x9")}, // highest
			},
			expectedCalls:  []int{1, 1},
			expectedResult: &expectedResult{jsonRpcResult: `"0x9"`},
		},
		{
			name:          "prefer_highest_value_mixed_valid_and_infra_errors",
			description:   "Infrastructure errors (like timeouts) are ignored, only valid responses compared",
			requestMethod: "eth_getTransactionCount",
			requestParams: []interface{}{"0x1234567890123456789012345678901234567890", "latest"},
			upstreams:     createTestUpstreams(3),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:    3,
				AgreementThreshold: 1, // threshold=1 so any single response qualifies
				PreferHighestValueFor: map[string][]string{
					"eth_getTransactionCount": {"result"},
				},
			},
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess("0x3")},                         // valid
				{status: 200, body: jsonRpcError(-32603, "internal server error")}, // infra error
				{status: 200, body: jsonRpcSuccess("0x7")},                         // valid, highest
			},
			expectedCalls:  []int{1, 1, 1},
			expectedResult: &expectedResult{jsonRpcResult: `"0x7"`},
		},
	}

	for _, tc := range tests {
		t.Run(util.SanitizeTestName(tc.name), func(t *testing.T) {
			runConsensusTest(t, tc)
		})
	}
}

// TestConsensusGoroutineCancellationIntegration tests the consensus goroutine cancellation behavior with real network
func TestConsensusGoroutineCancellationIntegration(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	// Create upstreams
	upstreams := []*common.UpstreamConfig{
		{
			Id:       "upstream1",
			Endpoint: "http://rpc1.localhost",
			Type:     common.UpstreamTypeEvm,
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		},
		{
			Id:       "upstream2",
			Endpoint: "http://rpc2.localhost",
			Type:     common.UpstreamTypeEvm,
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		},
		{
			Id:       "upstream3",
			Endpoint: "http://rpc3.localhost",
			Type:     common.UpstreamTypeEvm,
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		},
		{
			Id:       "upstream4",
			Endpoint: "http://rpc4.localhost",
			Type:     common.UpstreamTypeEvm,
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		},
		{
			Id:       "upstream5",
			Endpoint: "http://rpc5.localhost",
			Type:     common.UpstreamTypeEvm,
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		},
	}

	// Mock responses - upstream2 and upstream3 return consensus result quickly
	gock.New("http://rpc2.localhost").
		Post("").
		Filter(func(request *http.Request) bool {
			body := util.SafeReadBody(request)
			return strings.Contains(body, "eth_getBalance")
		}).
		Reply(200).
		Delay(10 * time.Millisecond).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  "0x1234567890",
		})

	gock.New("http://rpc3.localhost").
		Post("").
		Filter(func(request *http.Request) bool {
			body := util.SafeReadBody(request)
			return strings.Contains(body, "eth_getBalance")
		}).
		Reply(200).
		Delay(10 * time.Millisecond).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  "0x1234567890",
		})

	// Other upstreams are slow and should be cancelled
	for _, host := range []string{"rpc1", "rpc4", "rpc5"} {
		gock.New("http://" + host + ".localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getBalance")
			}).
			Reply(200).
			Delay(2 * time.Second).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x9876543210",
			})
	}

	// Create network with consensus policy
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ntw, _ := setupNetworkForConsensusTest(t, ctx, consensusTestCase{
		upstreams: upstreams,
		consensusConfig: &common.ConsensusPolicyConfig{
			MaxParticipants:         5,
			AgreementThreshold:      2, // Consensus with just 2 responses to trigger early short-circuit
			DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
			LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
			PunishMisbehavior:       &common.PunishMisbehaviorConfig{},
		},
	})

	// Make request
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123","latest"],"id":1}`))

	// Execute with timeout to catch deadlocks
	done := make(chan struct{})
	var resp *common.NormalizedResponse
	var err error

	go func() {
		defer close(done)
		resp, err = ntw.Forward(ctx, req)
	}()

	select {
	case <-done:
		// Test completed successfully
		require.NoError(t, err)
		require.NotNil(t, resp)

		// Verify consensus was achieved
		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)

		// The test is about goroutine cancellation, not which specific result is returned
		// Accept either the fast response or slow response as long as consensus was achieved
		result := jrr.GetResultString()
		assert.True(t, result == `"0x1234567890"` || result == `"0x9876543210"`,
			"Expected either fast or slow response, got %s", result)

		t.Log("Test passed: Consensus achieved without deadlock")
	case <-time.After(5 * time.Second):
		t.Fatal("Test timed out - potential deadlock detected")
	}
}

// Helper functions for test setup

func runConsensusTest(t *testing.T, tc consensusTestCase) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, tc.expectedPendingMocks)

	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		time.Sleep(50 * time.Millisecond) // allow goroutines to settle
	}()

	// Setup mocks BEFORE any network components initialize
	for i, upstreamCfg := range tc.upstreams {
		if i < len(tc.mockResponses) && (tc.expectedCalls == nil || (len(tc.expectedCalls) > i && tc.expectedCalls[i] != 0)) {
			mock := tc.mockResponses[i]
			m := gock.New(upstreamCfg.Endpoint).
				Post("").
				Filter(func(request *http.Request) bool {
					body := util.SafeReadBody(request)
					method := tc.requestMethod
					if method == "" {
						method = "eth_randomMethod"
					}
					return strings.Contains(string(body), method)
				})

			// Handle special case: -1 means Persist() (may or may not be called)
			if tc.expectedCalls != nil && len(tc.expectedCalls) > i && tc.expectedCalls[i] == -1 {
				m.Persist()
			} else if tc.expectedCalls != nil && len(tc.expectedCalls) > i {
				m.Times(tc.expectedCalls[i])
			} else {
				m.Times(1) // default to 1 if not specified
			}

			m.Reply(mock.status).
				Delay(mock.delay).
				SetHeader("Content-Type", "application/json").
				JSON(mock.body)
		}
	}

	// Setup Network AFTER mocks to avoid races during background initialization
	ntw, upsReg := setupNetworkForConsensusTest(t, ctx, tc)

	// Apply special setup if any (e.g., block heights/leader)
	if tc.setupFn != nil {
		tc.setupFn(t, ctx, upsReg)
	}

	// Allow background bootstrap to settle to avoid flakiness in tests
	time.Sleep(200 * time.Millisecond)

	// Make request
	method := tc.requestMethod
	if method == "" {
		method = "eth_randomMethod"
	}
	params := tc.requestParams
	if params == nil {
		params = []interface{}{}
	}
	reqBytes, err := json.Marshal(map[string]interface{}{"method": method, "params": params})
	require.NoError(t, err)
	fakeReq := common.NewNormalizedRequest(reqBytes)
	fakeReq.SetNetwork(ntw)

	// Execute and get result
	start := time.Now()
	resp, err := ntw.Forward(ctx, fakeReq)
	duration := time.Since(start)

	// Assertions
	if tc.expectedError != nil {
		respString := ""
		if resp != nil {
			if jrr, _ := resp.JsonRpcResponse(); jrr != nil {
				respString = jrr.GetResultString()
			}
		}
		require.Error(t, err, "expected an error but got response: %v", respString)
		if tc.expectedError.code != "" {
			assert.True(t, common.HasErrorCode(err, tc.expectedError.code), "expected error code %s, but got error: %v", tc.expectedError.code, err)
		}
		if tc.expectedError.contains != "" {
			assert.Contains(t, err.Error(), tc.expectedError.contains, "error message mismatch")
		}
		assert.Nil(t, resp, "response should be nil when an error is expected")
	} else {
		require.NoError(t, err, "did not expect an error but got one: %v", err)
		require.NotNil(t, resp, "response should not be nil")

		if tc.expectedResult != nil {
			jrr, jrrErr := resp.JsonRpcResponse()
			require.NoError(t, jrrErr)
			require.NotNil(t, jrr)

			if tc.expectedResult.jsonRpcResult != "" {
				assert.Equal(t, tc.expectedResult.jsonRpcResult, jrr.GetResultString(), "jsonrpc result mismatch")
			}
			if tc.expectedResult.contains != "" {
				assert.Contains(t, jrr.GetResultString(), tc.expectedResult.contains, "response body mismatch")
			}
			if tc.expectedResult.check != nil {
				tc.expectedResult.check(t, resp, duration)
			}
		}
	}
}

func setupNetworkForConsensusTest(t *testing.T, ctx context.Context, tc consensusTestCase) (*Network, *upstream.UpstreamsRegistry) {
	mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
	vr := thirdparty.NewVendorsRegistry()
	pr, err := thirdparty.NewProvidersRegistry(&log.Logger, vr, []*common.ProviderConfig{}, nil)
	require.NoError(t, err)

	ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: "memory",
			Memory: &common.MemoryConnectorConfig{
				MaxItems:     100_000,
				MaxTotalSize: "100MB",
			},
		},
	})
	require.NoError(t, err)

	upsReg := upstream.NewUpstreamsRegistry(
		ctx, &log.Logger, "prjA", tc.upstreams,
		ssr, nil, vr, pr, nil, mt, 1*time.Second, nil,
	)

	if err := tc.consensusConfig.SetDefaults(); err != nil {
		t.Fatalf("failed to set defaults on consensus config: %v", err)
	}

	ntw, err := NewNetwork(
		ctx, &log.Logger, "prjA",
		&common.NetworkConfig{
			Architecture: common.ArchitectureEvm,
			Evm:          &common.EvmNetworkConfig{ChainId: 123},
			Failsafe: []*common.FailsafeConfig{
				{
					MatchMethod:    "*",
					Consensus:      tc.consensusConfig,
					Retry:          tc.retryPolicy,
					Timeout:        tc.timeoutPolicy,
					Hedge:          tc.hedgePolicy,
					CircuitBreaker: tc.circuitBreakerPolicy,
				},
			},
		},
		nil, upsReg, mt,
	)
	require.NoError(t, err)

	upsReg.Bootstrap(ctx)
	time.Sleep(100 * time.Millisecond)
	err = upsReg.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
	require.NoError(t, err)
	upstream.ReorderUpstreams(upsReg)

	return ntw, upsReg
}

func createTestUpstreams(count int) []*common.UpstreamConfig {
	upstreams := make([]*common.UpstreamConfig, count)
	for i := 0; i < count; i++ {
		upstreams[i] = &common.UpstreamConfig{
			Id:       fmt.Sprintf("test-up-%d", i+1),
			Type:     common.UpstreamTypeEvm,
			Endpoint: fmt.Sprintf("http://rpc%d.localhost", i+1),
			Evm:      &common.EvmUpstreamConfig{ChainId: 123},
		}
	}
	return upstreams
}

func jsonRpcSuccess(result interface{}) map[string]interface{} {
	return map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"result":  result,
	}
}

func jsonRpcError(code int, message string) map[string]interface{} {
	return map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"error": map[string]interface{}{
			"code":    code,
			"message": message,
		},
	}
}
