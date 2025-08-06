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
	mockResponses        []mockResponse
	expectedCalls        []int
	expectedResult       *expectedResult
	expectedError        *expectedError
	expectedPendingMocks int
	setupFn              func(t *testing.T, ctx context.Context, reg *upstream.UpstreamsRegistry)
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
		// {
		// 	name:        "successful_consensus_2_of_3_prefer_larger_responses_enabled",
		// 	description: "A simple successful consensus where 2 out of 3 upstreams agree.",
		// 	upstreams:   createTestUpstreams(3),
		// 	consensusConfig: &common.ConsensusPolicyConfig{
		// 		MaxParticipants:         3,
		// 		AgreementThreshold:      2,
		// 		DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
		// 		LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
		// 		PreferLargerResponses:   &common.TRUE,
		// 		PreferNonEmpty:          &common.FALSE,
		// 	},
		// 	mockResponses: []mockResponse{
		// 		{status: 200, body: jsonRpcSuccess("0x7a")},
		// 		{status: 200, body: jsonRpcSuccess("0x7a")},
		// 		{status: 200, body: jsonRpcSuccess("0x7b"), delay: 300 * time.Millisecond},
		// 	},
		// 	expectedCalls: []int{1, 1, 1}, // will not short circuit because prefer larger responses is enabled
		// 	expectedResult: &expectedResult{
		// 		jsonRpcResult: `"0x7a"`,
		// 		check: func(t *testing.T, resp *common.NormalizedResponse, duration time.Duration) {
		// 			assert.GreaterOrEqual(t, duration, 300*time.Millisecond, "response should take more than 300ms and no short circuit")
		// 		},
		// 	},
		// },
		// {
		// 	name:        "successful_consensus_2_of_3_prefer_larger_responses_disabled",
		// 	description: "A simple successful consensus where 2 out of 3 upstreams agree.",
		// 	upstreams:   createTestUpstreams(3),
		// 	consensusConfig: &common.ConsensusPolicyConfig{
		// 		MaxParticipants:    3,
		// 		AgreementThreshold: 2,
		// 		DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
		// 		LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
		// 		PreferLargerResponses:   &common.FALSE,
		// 		PreferNonEmpty:          &common.FALSE,
		// 	},
		// 	mockResponses: []mockResponse{
		// 		{status: 200, body: jsonRpcSuccess("0x7a")},
		// 		{status: 200, body: jsonRpcSuccess("0x7a")},
		// 		{status: 200, body: jsonRpcSuccess("0x7b"), delay: 300 * time.Millisecond},
		// 	},
		// 	expectedCalls: []int{1, 1, 0}, // last request is not sent due to short circuit
		// 	expectedResult: &expectedResult{
		// 		jsonRpcResult: `"0x7a"`,
		// 		check: func(t *testing.T, resp *common.NormalizedResponse, duration time.Duration) {
		// 			assert.Less(t, duration, 100*time.Millisecond, "response should short circuit not wait for the slowest upstream")
		// 		},
		// 	},
		// },
		// {
		// 	name:        "low_participants_no_majority",
		// 	description: "A low participants error occurs when no two upstreams return the same result, all below the agreement threshold.",
		// 	upstreams:   createTestUpstreams(3),
		// 	consensusConfig: &common.ConsensusPolicyConfig{
		// 		MaxParticipants:         3,
		// 		AgreementThreshold:      2,
		// 		DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
		// 		LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
		// 	},
		// 	mockResponses: []mockResponse{
		// 		{status: 200, body: jsonRpcSuccess("0x7a")},
		// 		{status: 200, body: jsonRpcSuccess("0x7b")},
		// 		{status: 200, body: jsonRpcSuccess("0x7c")},
		// 	},
		// 	expectedCalls: []int{1, 1, 1},
		// 	expectedError: &expectedError{
		// 		code:     common.ErrCodeConsensusLowParticipants,
		// 		contains: "not enough participants",
		// 	},
		// },
		// {
		// 	name:        "low_participants_only_one_participant",
		// 	description: "A single participant returns a result, but no consensus is reached.",
		// 	upstreams:   createTestUpstreams(1),
		// 	consensusConfig: &common.ConsensusPolicyConfig{
		// 		MaxParticipants:         3,
		// 		AgreementThreshold:      2,
		// 		DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
		// 		LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
		// 	},
		// 	mockResponses: []mockResponse{
		// 		{status: 200, body: jsonRpcSuccess("0x7a")},
		// 	},
		// 	expectedCalls: []int{1},
		// 	expectedError: &expectedError{
		// 		code:     common.ErrCodeConsensusLowParticipants,
		// 		contains: "not enough participants",
		// 	},
		// },
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
			expectedCalls: []int{1},
			expectedError: &expectedError{
				code:     common.ErrCodeConsensusDispute,
				contains: "not enough agreement among responses",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runConsensusTest(t, tc)
		})
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
					return strings.Contains(string(body), "eth_randomMethod")
				}).
				Reply(mock.status).
				Delay(mock.delay).
				SetHeader("Content-Type", "application/json").
				JSON(mock.body)
		}
	}

	// Make request
	reqBytes, err := json.Marshal(map[string]interface{}{"method": "eth_randomMethod", "params": []interface{}{}})
	require.NoError(t, err)
	fakeReq := common.NewNormalizedRequest(reqBytes)
	fakeReq.SetNetwork(ntw)

	// Execute and get result
	start := time.Now()
	resp, err := ntw.Forward(ctx, fakeReq)
	duration := time.Since(start)

	// Assertions
	if tc.expectedError != nil {
		require.Error(t, err, "expected an error but got none response: %v", resp)
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
					MatchMethod: "*",
					Consensus:   tc.consensusConfig,
					Retry:       tc.retryPolicy,
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
