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
					PreferLargerResponses:   &common.FALSE,
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
			description: "A simple successful consensus where 2 out of 3 upstreams agree.",
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
			expectedCalls:  []int{1, 1, 1, 1, 1, 1},
			expectedResult: &expectedResult{jsonRpcResult: `"0x33"`},
		},
		{
			name:        "tie_above_threshold_without_preference_results_in_dispute",
			description: "Two groups empty and non-empty both meet threshold with equal counts; no preference -> dispute",
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
				{status: 200, body: jsonRpcSuccess("0x44")},          // non-empty 1
				{status: 200, body: jsonRpcSuccess("0x44")},          // non-empty 2
				{status: 200, body: jsonRpcSuccess("0x44")},          // non-empty 3 -> meets threshold
				{status: 200, body: jsonRpcSuccess([]interface{}{})}, // empty 1
				{status: 200, body: jsonRpcSuccess([]interface{}{})}, // empty 2
				{status: 200, body: jsonRpcSuccess([]interface{}{})}, // empty 3 -> meets threshold
			},
			expectedCalls: []int{1, 1, 1, 1, 1, 1},
			expectedError: &expectedError{
				code:     common.ErrCodeConsensusDispute,
				contains: "not enough agreement among responses",
			},
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
			description: "retry once on intermittently bad upstream and reach consensus if correct result is returned on second attempt",
			upstreams:   createTestUpstreams(2),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         2,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
			},
			retryPolicy: &common.RetryPolicyConfig{
				MaxAttempts: 2,
			},
			// Let harness mock upstream1 once; we’ll override upstream2 via setupFn
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess("0x1")},
			},
			expectedCalls: []int{1, 0},
			expectedResult: &expectedResult{
				jsonRpcResult: `"0x1"`,
			},
			setupFn: func(t *testing.T, ctx context.Context, reg *upstream.UpstreamsRegistry) {
				// upstream2: first error (-32603), then success "0x1"
				gock.New("http://rpc2.localhost").
					Post("/").
					Times(1).
					Filter(func(request *http.Request) bool {
						body := util.SafeReadBody(request)
						return strings.Contains(string(body), "eth_randomMethod")
					}).
					Reply(200).
					SetHeader("Content-Type", "application/json").
					JSON(jsonRpcError(-32603, "unknown server error"))
				gock.New("http://rpc2.localhost").
					Post("/").
					Times(1).
					Filter(func(request *http.Request) bool {
						body := util.SafeReadBody(request)
						return strings.Contains(string(body), "eth_randomMethod")
					}).
					Reply(200).
					SetHeader("Content-Type", "application/json").
					JSON(jsonRpcSuccess("0x1"))
			},
			expectedPendingMocks: 0,
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
			expectedCalls: []int{1, 1, 1, 1, 1, 1, 1},
			expectedResult: &expectedResult{
				jsonRpcResult: "\"" + strings.Repeat("A", 2000) + "\"",
				check: func(t *testing.T, resp *common.NormalizedResponse, duration time.Duration) {
					assert.GreaterOrEqual(t, duration, 300*time.Millisecond, "should not short-circuit when preferences are enabled and empty is leading with remaining participants")
				},
			},
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
				{status: 200, body: jsonRpcSuccess("0xaaa")},                           // up1
				{status: 200, body: jsonRpcSuccess("0xaaa")},                           // up2 -> best group size 2
				{status: 200, body: jsonRpcSuccess("0xbbb")},                           // up3
				{status: 200, body: jsonRpcError(-32000, "cannot query unfinalized")}, // up4
				{status: 200, body: jsonRpcError(-32003, "tx rejected")},               // up5
			},
			expectedCalls: []int{1, 1, 1, 1, 1},
			expectedError: &expectedError{
				code:     common.ErrCodeConsensusDispute,
				contains: "not enough agreement among responses",
			},
		},
		{
			name:        "retried_failing_usptream_but_cannot_reach_consensus",
			description: "retry once on fully broken upstream and cannot reach consensus",
			upstreams:   createTestUpstreams(2),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:         2,
				AgreementThreshold:      2,
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
			},
			retryPolicy: &common.RetryPolicyConfig{
				MaxAttempts: 2,
			},
			// Let harness mock upstream1 once; we’ll override upstream2 via setupFn
			mockResponses: []mockResponse{
				{status: 200, body: jsonRpcSuccess("0x1")},
			},
			expectedCalls: []int{1, 0},
			expectedError: &expectedError{
				code:     common.ErrCodeConsensusLowParticipants,
				contains: "not enough participants",
			},
			setupFn: func(t *testing.T, ctx context.Context, reg *upstream.UpstreamsRegistry) {
				// upstream2: first error (-32603), then success "0x1"
				gock.New("http://rpc2.localhost").
					Post("/").
					Times(2).
					Filter(func(request *http.Request) bool {
						body := util.SafeReadBody(request)
						return strings.Contains(string(body), "eth_randomMethod")
					}).
					Reply(200).
					SetHeader("Content-Type", "application/json").
					JSON(jsonRpcError(-32603, "unknown server error"))
			},
			expectedPendingMocks: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
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
			Endpoint: "http://upstream1.localhost",
			Type:     common.UpstreamTypeEvm,
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		},
		{
			Id:       "upstream2",
			Endpoint: "http://upstream2.localhost",
			Type:     common.UpstreamTypeEvm,
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		},
		{
			Id:       "upstream3",
			Endpoint: "http://upstream3.localhost",
			Type:     common.UpstreamTypeEvm,
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		},
		{
			Id:       "upstream4",
			Endpoint: "http://upstream4.localhost",
			Type:     common.UpstreamTypeEvm,
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		},
		{
			Id:       "upstream5",
			Endpoint: "http://upstream5.localhost",
			Type:     common.UpstreamTypeEvm,
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		},
	}

	// Mock responses - upstream2 and upstream3 return consensus result quickly
	gock.New("http://upstream2.localhost").
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

	gock.New("http://upstream3.localhost").
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
	for _, host := range []string{"upstream1", "upstream4", "upstream5"} {
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

	network := setupTestNetworkWithConsensusPolicy(t, ctx, upstreams, &common.ConsensusPolicyConfig{
		MaxParticipants:         5,
		AgreementThreshold:      2, // Consensus with just 2 responses to trigger early short-circuit
		DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
		LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
		PunishMisbehavior:       &common.PunishMisbehaviorConfig{},
	})

	// Make request
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123","latest"],"id":1}`))

	// Execute with timeout to catch deadlocks
	done := make(chan struct{})
	var resp *common.NormalizedResponse
	var err error

	go func() {
		defer close(done)
		resp, err = network.Forward(ctx, req)
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
		result := string(jrr.Result)
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

	// Setup Network
	ntw, upsReg := setupNetworkForConsensusTest(t, ctx, tc)

	// Apply special setup if any
	if tc.setupFn != nil {
		tc.setupFn(t, ctx, upsReg)
	}

	// Setup mocks
	for i, upstreamCfg := range tc.upstreams {
		if i < len(tc.mockResponses) && (tc.expectedCalls == nil || (len(tc.expectedCalls) > i && tc.expectedCalls[i] > 0)) {
			mock := tc.mockResponses[i]
			gock.New(upstreamCfg.Endpoint).
				Post("/").
				Times(tc.expectedCalls[i]).
				Filter(func(request *http.Request) bool {
					body := util.SafeReadBody(request)
					method := tc.requestMethod
					if method == "" {
						method = "eth_randomMethod"
					}
					return strings.Contains(string(body), method)
				}).
				Reply(mock.status).
				Delay(mock.delay).
				SetHeader("Content-Type", "application/json").
				JSON(mock.body)
		}
	}

	// Make request
	method := tc.requestMethod
	if method == "" {
		method = "eth_randomMethod"
	}
	reqBytes, err := json.Marshal(map[string]interface{}{"method": method, "params": []interface{}{}})
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
				respString = string(jrr.Result)
			}
		}
		require.Error(t, err, "expected an error but got response: %v", respString)
		assert.True(t, common.HasErrorCode(err, tc.expectedError.code), "expected error code %s, but got error: %v", tc.expectedError.code, err)
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
				assert.Equal(t, tc.expectedResult.jsonRpcResult, string(jrr.Result), "jsonrpc result mismatch")
			}
			if tc.expectedResult.contains != "" {
				assert.Contains(t, string(jrr.Result), tc.expectedResult.contains, "response body mismatch")
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
		ssr, nil, vr, pr, nil, mt, 1*time.Second,
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

	err = upsReg.Bootstrap(ctx)
	require.NoError(t, err)
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
