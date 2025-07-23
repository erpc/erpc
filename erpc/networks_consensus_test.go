package erpc

import (
	"context"
	"encoding/json"
	"errors"
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

var (
	successResponse, _ = common.NewJsonRpcResponse(1, "0x7a", nil)
)

func init() {
	util.ConfigureTestLogger()
}

func TestNetwork_ConsensusPolicy(t *testing.T) {
	tests := []struct {
		name                    string
		upstreams               []*common.UpstreamConfig
		hasRetries              bool
		request                 map[string]interface{}
		mockResponses           []map[string]interface{}
		requiredParticipants    int
		agreementThreshold      *int
		disputeBehavior         *common.ConsensusDisputeBehavior
		lowParticipantsBehavior *common.ConsensusLowParticipantsBehavior
		expectedCalls           []int // Number of expected calls for each upstream
		expectedResponse        *common.NormalizedResponse
		expectedError           *common.ErrorCode
		expectedMsg             *string
	}{
		{
			name:                 "successful_consensus",
			requiredParticipants: 3,
			upstreams: []*common.UpstreamConfig{
				{
					Id:       "test1",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc1-dispute.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
				{
					Id:       "test2",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc2-dispute.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
				{
					Id:       "test3",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc3-dispute.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
			},
			request: map[string]interface{}{
				"method": "eth_chainId",
				"params": []interface{}{},
			},
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x7a",
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x7a",
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x7a",
				},
			},
			expectedCalls: []int{
				1, // test1 will be called once (for its consensus execution)
				1, // test2 will be called once (for its consensus execution)
				1, // test3 will be called once (for its consensus execution)
			},
			expectedResponse: common.NewNormalizedResponse().
				WithJsonRpcResponse(successResponse),
		},
		{
			name:                 "successful_consensus_only_necessary_participants",
			requiredParticipants: 2,
			upstreams: []*common.UpstreamConfig{
				{
					Id:       "test1",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc1-dispute.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
				{
					Id:       "test2",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc2-dispute.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
				{
					Id:       "test3",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc3-dispute.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
			},
			request: map[string]interface{}{
				"method": "eth_chainId",
				"params": []interface{}{},
			},
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x7a",
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x7a",
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x7a",
				},
			},
			expectedCalls: []int{
				1, // test1 will be called once (for its consensus execution)
				1, // test2 will be called once (for its consensus execution)
				0, // test3 will NOT be called since requiredParticipants is 2
			},
			expectedResponse: common.NewNormalizedResponse().
				WithJsonRpcResponse(successResponse),
		},
		{
			name:                 "low_participants_error",
			requiredParticipants: 3,
			upstreams: []*common.UpstreamConfig{
				{
					Id:       "test1",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc1-low-participants.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
			},
			request: map[string]interface{}{
				"method": "eth_chainId",
				"params": []interface{}{},
			},
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x7b",
				},
			},
			expectedCalls: []int{1},
			expectedError: pointer(common.ErrCodeConsensusLowParticipants),
			expectedMsg:   pointer("not enough participants"),
		},
		{
			name:                 "dispute_error",
			requiredParticipants: 3,
			upstreams: []*common.UpstreamConfig{
				{
					Id:       "test1",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc1-dispute.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
				{
					Id:       "test2",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc2-dispute.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
				{
					Id:       "test3",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc3-dispute.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
			},
			request: map[string]interface{}{
				"method": "eth_chainId",
				"params": []interface{}{},
			},
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x7b",
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x7c",
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x7d",
				},
			},
			expectedCalls: []int{1, 1, 1}, // Each upstream called once
			expectedError: pointer(common.ErrCodeConsensusDispute),
			expectedMsg:   pointer("not enough agreement among responses"),
		},
		{
			name:                 "retried_dispute_error",
			requiredParticipants: 2,
			upstreams: []*common.UpstreamConfig{
				{
					Id:       "test1",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc1-dispute.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
				{
					Id:       "test2",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc2-dispute.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
			},
			request: map[string]interface{}{
				"method": "eth_getBlockByNumber",
				"params": []interface{}{"0x77777", false},
			},
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x1",
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"error": map[string]interface{}{
						"code":    -32603,
						"message": "unknown server error",
					},
				},
			},
			hasRetries:    true,
			expectedCalls: []int{1, 2},
			expectedError: pointer(common.ErrCodeConsensusLowParticipants),
			expectedMsg:   pointer("not enough participants"),
		},
		{
			name:                 "retry_on_error_and_success_on_next_upstream",
			requiredParticipants: 3,
			upstreams: []*common.UpstreamConfig{
				{
					Id:       "test1",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc1-dispute.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
				{
					Id:       "test2",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc2-dispute.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
				{
					Id:       "test3",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc3-dispute.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
			},
			request: map[string]interface{}{
				"method": "eth_getBlockByNumber",
				"params": []interface{}{"latest", false},
			},
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x7a",
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"error": map[string]interface{}{
						"code":    -32000,
						"message": "cannot query unfinalized data",
					},
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x7a",
				},
			},
			hasRetries:    true,
			expectedCalls: []int{1, 1, 1},
			expectedResponse: common.NewNormalizedResponse().
				WithJsonRpcResponse(successResponse),
		},
		{
			name:                 "error_response_on_upstreams",
			requiredParticipants: 3,
			upstreams: []*common.UpstreamConfig{
				{
					Id:       "test1",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc1-failure.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
				{
					Id:       "test2",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc2-failure.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
				{
					Id:       "test3",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc3-failure.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
			},
			request: map[string]interface{}{
				"method": "eth_chainId",
				"params": []interface{}{},
			},
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"error": map[string]interface{}{
						"code":    -32000,
						"message": "internal error",
					},
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"error": map[string]interface{}{
						"code":    -32000,
						"message": "internal error",
					},
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"error": map[string]interface{}{
						"code":    -32000,
						"message": "internal error",
					},
				},
			},
			expectedCalls: []int{1, 1, 1}, // Each upstream called once
			expectedError: pointer(common.ErrCodeConsensusLowParticipants),
			expectedMsg:   pointer("not enough participants"),
		},
		{
			name:                 "some_participants_return_error",
			requiredParticipants: 3,
			agreementThreshold:   pointer(3),
			upstreams: []*common.UpstreamConfig{
				{
					Id:       "test1",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc1-failure.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
				{
					Id:       "test2",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc2-failure.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
				{
					Id:       "test3",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc3-failure.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
			},
			request: map[string]interface{}{
				"method": "eth_chainId",
				"params": []interface{}{},
			},
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x7a",
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"error": map[string]interface{}{
						"code":    -32000,
						"message": "internal error",
					},
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x7a",
				},
			},
			expectedCalls: []int{1, 1, 1},
			expectedError: pointer(common.ErrCodeConsensusLowParticipants),
			expectedMsg:   pointer("not enough participants"),
		},
		{
			name:                    "some_participants_return_error_and_return_error_on_low_participants",
			requiredParticipants:    3,
			agreementThreshold:      pointer(3),
			lowParticipantsBehavior: pointer(common.ConsensusLowParticipantsBehaviorReturnError),
			upstreams: []*common.UpstreamConfig{
				{
					Id:       "test1",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc1-failure.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
				{
					Id:       "test2",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc2-failure.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
				{
					Id:       "test3",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc3-failure.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
			},
			request: map[string]interface{}{
				"method": "eth_chainId",
				"params": []interface{}{},
			},
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x7a",
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"error": map[string]interface{}{
						"code":    -32000,
						"message": "internal error",
					},
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x7a",
				},
			},
			expectedCalls: []int{1, 1, 1},
			expectedError: pointer(common.ErrCodeConsensusLowParticipants),
			expectedMsg:   pointer("not enough participants"),
		},
		{
			name:                    "some_participants_return_error_but_accept_most_common_valid_result",
			requiredParticipants:    3,
			agreementThreshold:      pointer(3),
			lowParticipantsBehavior: pointer(common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult),
			upstreams: []*common.UpstreamConfig{
				{
					Id:       "test1",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc1-failure.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
				{
					Id:       "test2",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc2-failure.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
				{
					Id:       "test3",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc3-failure.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
			},
			request: map[string]interface{}{
				"method": "eth_chainId",
				"params": []interface{}{},
			},
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x7a",
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"error": map[string]interface{}{
						"code":    -32000,
						"message": "internal error",
					},
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x7a",
				},
			},
			expectedCalls: []int{1, 1, 1},
			expectedResponse: common.NewNormalizedResponse().
				WithJsonRpcResponse(successResponse),
		},
		{
			name:                    "not_enough_upstreams_low_participants_behavior_accept_most_common_valid_result",
			requiredParticipants:    3,
			agreementThreshold:      pointer(3),
			lowParticipantsBehavior: pointer(common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult),
			upstreams: []*common.UpstreamConfig{
				{
					Id:       "test1",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc1-failure.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
				{
					Id:       "test2",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc2-failure.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
			},
			request: map[string]interface{}{
				"method": "eth_chainId",
				"params": []interface{}{},
			},
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x7a",
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"error": map[string]interface{}{
						"code":    -32000,
						"message": "internal error",
					},
				},
			},
			expectedCalls: []int{1, 1},
			expectedResponse: common.NewNormalizedResponse().
				WithJsonRpcResponse(successResponse),
		},
		{
			name:                    "only_block_head_leader_selects_highest_block_upstream",
			agreementThreshold:      pointer(2),
			requiredParticipants:    3,
			lowParticipantsBehavior: pointer(common.ConsensusLowParticipantsBehaviorOnlyBlockHeadLeader),
			disputeBehavior:         pointer(common.ConsensusDisputeBehaviorOnlyBlockHeadLeader),
			upstreams: []*common.UpstreamConfig{
				{
					Id:       "test1",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc1-leader.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
				{
					Id:       "test2",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc2-leader.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
				{
					Id:       "test3",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc3-leader.localhost", // This will be the leader
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
			},
			request: map[string]interface{}{
				"method": "eth_chainId",
				"params": []interface{}{},
			},
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x5a",
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x6a",
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x7a",
				},
			},
			expectedCalls: []int{
				1, // test1 should NOT be used
				1, // test2 should NOT be used
				1, // test3 (leader) should be used
			},
			expectedResponse: common.NewNormalizedResponse().
				WithJsonRpcResponse(successResponse),
		},
		{
			name:                    "prefer_block_head_leader_includes_leader_in_participants",
			agreementThreshold:      pointer(2),
			requiredParticipants:    3,
			lowParticipantsBehavior: pointer(common.ConsensusLowParticipantsBehaviorPreferBlockHeadLeader),
			upstreams: []*common.UpstreamConfig{
				{
					Id:       "test1",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc1-leader.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
				{
					Id:       "test2",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc2-leader.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
				{
					Id:       "test3",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc3-leader.localhost", // This will be the leader
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
			},
			request: map[string]interface{}{
				"method": "eth_chainId",
				"params": []interface{}{},
			},
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x7a",
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x7a",
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"error": map[string]interface{}{
						"code":    -32000,
						"message": "internal error",
					},
				},
			},
			expectedCalls: []int{
				1, // test1 should be used
				1, // test2 should NOT be used
				1, // test3 (leader) should be called
			},
			expectedResponse: common.NewNormalizedResponse().
				WithJsonRpcResponse(successResponse),
		},
		{
			name:                    "only_block_head_leader_no_leader_available_uses_normal_selection",
			requiredParticipants:    2,
			lowParticipantsBehavior: pointer(common.ConsensusLowParticipantsBehaviorOnlyBlockHeadLeader),
			upstreams: []*common.UpstreamConfig{
				{
					Id:       "test1",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc1-no-leader.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
				{
					Id:       "test2",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc2-no-leader.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
			},
			request: map[string]interface{}{
				"method": "eth_chainId",
				"params": []interface{}{},
			},
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x7a",
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x7a",
				},
			},
			expectedCalls: []int{
				1, // test1 should be called (normal selection)
				1, // test2 should be called (normal selection)
			},
			expectedResponse: common.NewNormalizedResponse().
				WithJsonRpcResponse(successResponse),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			util.ResetGock()
			defer util.ResetGock()
			util.SetupMocksForEvmStatePoller()
			defer util.AssertNoPendingMocks(t, 0)

			ctx, cancel := context.WithCancel(context.Background())
			defer func() {
				cancel()
				// Allow any hedge/consensus requests to complete before cleanup
				time.Sleep(50 * time.Millisecond)
			}()

			// Setup network with consensus policy
			mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)

			vr := thirdparty.NewVendorsRegistry()
			pr, err := thirdparty.NewProvidersRegistry(
				&log.Logger,
				vr,
				[]*common.ProviderConfig{},
				nil,
			)
			if err != nil {
				t.Fatal(err)
			}

			ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
				Connector: &common.ConnectorConfig{
					Driver: "memory",
					Memory: &common.MemoryConnectorConfig{
						MaxItems:     100_000,
						MaxTotalSize: "1MB",
					},
				},
			})
			if err != nil {
				panic(err)
			}

			upsReg := upstream.NewUpstreamsRegistry(
				ctx,
				&log.Logger,
				"prjA",
				tt.upstreams,
				ssr,
				nil,
				vr,
				pr,
				nil,
				mt,
				1*time.Second,
				nil,
			)

			var retryPolicy *common.RetryPolicyConfig
			if tt.hasRetries {
				retryPolicy = &common.RetryPolicyConfig{
					MaxAttempts: len(tt.upstreams),
					Delay:       common.Duration(0),
				}
			}

			agreementThreshold := 2
			if tt.agreementThreshold != nil {
				agreementThreshold = *tt.agreementThreshold
			}

			lowParticipantsBehavior := common.ConsensusLowParticipantsBehaviorReturnError
			if tt.lowParticipantsBehavior != nil {
				lowParticipantsBehavior = *tt.lowParticipantsBehavior
			}

			disputeBehavior := common.ConsensusDisputeBehaviorReturnError
			if tt.disputeBehavior != nil {
				disputeBehavior = *tt.disputeBehavior
			}

			ntw, err := NewNetwork(
				ctx,
				&log.Logger,
				"prjA",
				&common.NetworkConfig{
					Architecture: common.ArchitectureEvm,
					Evm: &common.EvmNetworkConfig{
						ChainId: 123,
					},
					Failsafe: []*common.FailsafeConfig{
						{
							MatchMethod: "*", // Match all methods to ensure consensus executor is selected
							Retry:       retryPolicy,
							Consensus: &common.ConsensusPolicyConfig{
								RequiredParticipants:    tt.requiredParticipants,
								AgreementThreshold:      agreementThreshold,
								DisputeBehavior:         disputeBehavior,
								LowParticipantsBehavior: lowParticipantsBehavior,
								PunishMisbehavior:       &common.PunishMisbehaviorConfig{},
							},
						},
					},
				},
				nil,
				upsReg,
				mt,
			)
			if err != nil {
				t.Fatal(err)
			}

			err = upsReg.Bootstrap(ctx)
			if err != nil {
				t.Fatal(err)
			}
			err = upsReg.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
			if err != nil {
				t.Fatal(err)
			}

			upstream.ReorderUpstreams(upsReg)

			// Set up block numbers for leader selection tests
			if tt.name == "only_block_head_leader_selects_highest_block_upstream" ||
				tt.name == "prefer_block_head_leader_includes_leader_in_participants" {
				// Get upstreams and set their block numbers
				upsList := upsReg.GetNetworkUpstreams(ctx, util.EvmNetworkId(123))
				if len(upsList) >= 3 {
					// Set block numbers: test3 will have the highest block
					upsList[0].EvmStatePoller().SuggestLatestBlock(100) // test1
					upsList[1].EvmStatePoller().SuggestLatestBlock(200) // test2
					upsList[2].EvmStatePoller().SuggestLatestBlock(300) // test3 (leader)
				}
			} else if tt.name == "only_block_head_leader_no_leader_available_uses_normal_selection" {
				// For the no-leader test, set all block numbers to 0
				upsList := upsReg.GetNetworkUpstreams(ctx, util.EvmNetworkId(123))
				for _, ups := range upsList {
					ups.EvmStatePoller().SuggestLatestBlock(0)
				}
			}

			// Setup mock responses with expected call counts
			for i, upstream := range tt.upstreams {
				if tt.expectedCalls[i] > 0 {
					gock.New(upstream.Endpoint).
						Post("/").
						Times(tt.expectedCalls[i]).
						Reply(200).
						SetHeader("Content-Type", "application/json").
						JSON(tt.mockResponses[i])
				}
			}

			// Make request
			reqBytes, err := json.Marshal(tt.request)
			if err != nil {
				require.NoError(t, err)
			}

			fakeReq := common.NewNormalizedRequest(reqBytes)

			// Log the initial request state
			t.Logf("Initial request ID: %v, ptr: %p", fakeReq.ID(), fakeReq)

			resp, err := ntw.Forward(ctx, fakeReq)

			// Log the error for debugging
			if err != nil {
				t.Logf("Got error from Forward: %v", err)
			} else if resp != nil && resp.Upstream() != nil {
				t.Logf("Got response from upstream: %s", resp.Upstream().Id())
			}

			if tt.expectedError != nil {
				assert.Error(t, err, "expected error but got nil")
				assert.True(t, common.HasErrorCode(err, *tt.expectedError), "expected error code %s, got %s", *tt.expectedError, err)
				if err != nil {
					assert.Contains(t, err.Error(), *tt.expectedMsg, "expected error message %s, got %s", *tt.expectedMsg, err.Error())
				}
				assert.Nil(t, resp, "expected nil response")
			} else {
				assert.NoError(t, err)
				assert.NotNil(t, resp)

				expectedJrr, err := tt.expectedResponse.JsonRpcResponse()
				assert.NoError(t, err)
				assert.NotNil(t, expectedJrr)

				actualJrr, err := resp.JsonRpcResponse()
				assert.NoError(t, err)
				assert.NotNil(t, actualJrr)

				// Ensure both are either nil or non-nil
				if expectedJrr != nil && actualJrr == nil {
					t.Fatal("expected a JSON-RPC response but got nil")
				} else if expectedJrr == nil && actualJrr != nil {
					t.Fatal("expected no JSON-RPC response but got one")
				} else if expectedJrr != nil && actualJrr != nil {
					// Both are non-nil, compare the results
					assert.Equal(t, string(expectedJrr.Result), string(actualJrr.Result))
				}
			}
		})
	}
}

func TestNetwork_Consensus_RetryIntermittentErrors(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		// Allow any hedge/consensus requests to complete before cleanup
		time.Sleep(500 * time.Millisecond)
	}()

	// Setup network with consensus policy
	mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)

	vr := thirdparty.NewVendorsRegistry()
	pr, err := thirdparty.NewProvidersRegistry(
		&log.Logger,
		vr,
		[]*common.ProviderConfig{},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: "memory",
			Memory: &common.MemoryConnectorConfig{
				MaxItems:     100_000,
				MaxTotalSize: "1MB",
			},
		},
	})
	if err != nil {
		panic(err)
	}

	upsReg := upstream.NewUpstreamsRegistry(
		ctx,
		&log.Logger,
		"prjA",
		[]*common.UpstreamConfig{
			{
				Id:       "test1",
				Type:     common.UpstreamTypeEvm,
				Endpoint: "http://rpc1-dispute.localhost",
				Evm: &common.EvmUpstreamConfig{
					ChainId: 123,
				},
			},
			{
				Id:       "test2",
				Type:     common.UpstreamTypeEvm,
				Endpoint: "http://rpc2-dispute.localhost",
				Evm: &common.EvmUpstreamConfig{
					ChainId: 123,
				},
			},
		},
		ssr,
		nil,
		vr,
		pr,
		nil,
		mt,
		1*time.Second,
	)

	ntw, err := NewNetwork(
		ctx,
		&log.Logger,
		"prjA",
		&common.NetworkConfig{
			Architecture: common.ArchitectureEvm,
			Evm: &common.EvmNetworkConfig{
				ChainId: 123,
			},
			Failsafe: []*common.FailsafeConfig{
				{
					MatchMethod: "*", // Match all methods to ensure consensus executor is selected
					Retry: &common.RetryPolicyConfig{
						MaxAttempts: 2,
						Delay:       common.Duration(0),
					},
					Consensus: &common.ConsensusPolicyConfig{
						RequiredParticipants:    2,
						AgreementThreshold:      2,
						DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
						LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
						PunishMisbehavior:       &common.PunishMisbehaviorConfig{},
					},
				},
			},
		},
		nil,
		upsReg,
		mt,
	)
	if err != nil {
		t.Fatal(err)
	}

	err = upsReg.Bootstrap(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = upsReg.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
	if err != nil {
		t.Fatal(err)
	}

	upstream.ReorderUpstreams(upsReg)

	gock.New("http://rpc1-dispute.localhost").
		Post("/").
		Times(1).
		Reply(200).
		SetHeader("Content-Type", "application/json").
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  "0x7a",
		})

	gock.New("http://rpc2-dispute.localhost").
		Post("/").
		Times(1).
		Reply(503).
		SetHeader("Content-Type", "application/json").
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"error": map[string]interface{}{
				"code":    -32603,
				"message": "unknown server error",
			},
		})

	gock.New("http://rpc2-dispute.localhost").
		Post("/").
		Times(1).
		Reply(200).
		SetHeader("Content-Type", "application/json").
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  "0x7a",
		})

	// Make request
	reqBytes, err := json.Marshal(map[string]interface{}{
		"method": "eth_getBlockByNumber",
		"params": []interface{}{"0x77777", false},
	})
	if err != nil {
		require.NoError(t, err)
	}

	fakeReq := common.NewNormalizedRequest(reqBytes)
	resp, err := ntw.Forward(ctx, fakeReq)
	if err != nil {
		t.Fatal(err)
	}

	assert.NoError(t, err)
	assert.NotNil(t, resp)

	expectedJrr, err := common.NewJsonRpcResponse(1, "0x7a", nil)
	assert.NoError(t, err)
	assert.NotNil(t, expectedJrr)

	actualJrr, err := resp.JsonRpcResponse()
	assert.NoError(t, err)
	assert.NotNil(t, actualJrr)
}

func pointer[T any](v T) *T {
	return &v
}

// setupTestNetworkWithConsensusPolicy creates a test network with consensus policy for integration testing
func setupTestNetworkWithConsensusPolicy(t *testing.T, ctx context.Context, upstreams []*common.UpstreamConfig, consensusConfig *common.ConsensusPolicyConfig) *Network {
	mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
	vr := thirdparty.NewVendorsRegistry()
	pr, err := thirdparty.NewProvidersRegistry(&log.Logger, vr, []*common.ProviderConfig{}, nil)
	require.NoError(t, err)

	ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: "memory",
			Memory: &common.MemoryConnectorConfig{
				MaxItems:     100_000,
				MaxTotalSize: "1MB",
			},
		},
	})
	require.NoError(t, err)

	upsReg := upstream.NewUpstreamsRegistry(
		ctx, &log.Logger, "prjA", upstreams,
		ssr, nil, vr, pr, nil, mt, 1*time.Second,
	)

	ntw, err := NewNetwork(
		ctx, &log.Logger, "prjA",
		&common.NetworkConfig{
			Architecture: common.ArchitectureEvm,
			Evm: &common.EvmNetworkConfig{
				ChainId: 123,
			},
			Failsafe: []*common.FailsafeConfig{
				{
					MatchMethod: "*",
					Consensus:   consensusConfig,
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

	return ntw
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
		RequiredParticipants:    5,
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
		assert.Equal(t, `"0x1234567890"`, string(jrr.Result))

		t.Log("Test passed: Consensus achieved without deadlock")
	case <-time.After(5 * time.Second):
		t.Fatal("Test timed out - potential deadlock detected")
	}
}

// TestConsensusShortCircuitIntegration tests that consensus short-circuits properly with real network
func TestConsensusShortCircuitIntegration(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	// Don't check for pending mocks since we use Persist() for short-circuit scenarios

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
	}

	// Mock responses - all return the same result quickly
	// Use Persist() since short-circuit may not call all upstreams
	for _, upstream := range upstreams {
		gock.New(upstream.Endpoint).
			Post("").
			Persist().
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x7a",
			})
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network := setupTestNetworkWithConsensusPolicy(t, ctx, upstreams, &common.ConsensusPolicyConfig{
		RequiredParticipants: 3,
		AgreementThreshold:   2, // Should short-circuit after 2 matching responses
		DisputeBehavior:      common.ConsensusDisputeBehaviorReturnError,
		PunishMisbehavior:    &common.PunishMisbehaviorConfig{},
	})

	// Make request
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}`))

	start := time.Now()
	resp, err := network.Forward(ctx, req)
	duration := time.Since(start)

	require.NoError(t, err)
	require.NotNil(t, resp)

	// Verify response
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	assert.Equal(t, `"0x7a"`, string(jrr.Result))

	// Should complete quickly due to short-circuit
	assert.Less(t, duration, 500*time.Millisecond, "Should short-circuit quickly")

	t.Logf("Consensus completed in %v (short-circuit working)", duration)
}

// TestConsensusInsufficientParticipants tests behavior when requiredParticipants > available upstreams
func TestConsensusInsufficientParticipants(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	// Don't check for pending mocks since we use Persist() for multiple calls

	// Create only 2 upstreams but require 5 participants
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
	}

	// Mock responses - both return the same result
	// Use Persist() since consensus may make multiple calls to same upstream
	for _, upstream := range upstreams {
		gock.New(upstream.Endpoint).
			Post("").
			Persist().
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x7a",
			})
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network := setupTestNetworkWithConsensusPolicy(t, ctx, upstreams, &common.ConsensusPolicyConfig{
		RequiredParticipants:    5, // More than available upstreams
		AgreementThreshold:      2,
		DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
		LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
		PunishMisbehavior:       &common.PunishMisbehaviorConfig{},
	})

	// Make request
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}`))

	resp, err := network.Forward(ctx, req)

	// Should succeed with AcceptMostCommonValidResult behavior
	require.NoError(t, err)
	require.NotNil(t, resp)

	// Verify response
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	assert.Equal(t, `"0x7a"`, string(jrr.Result))

	t.Log("Test passed: Consensus handled insufficient participants gracefully")
}

func TestNetwork_ConsensusWithIgnoreFields(t *testing.T) {
	tests := []struct {
		name                 string
		upstreams            []*common.UpstreamConfig
		request              map[string]interface{}
		mockResponses        []map[string]interface{}
		requiredParticipants int
		agreementThreshold   int
		ignoreFields         map[string][]string
		expectedSuccess      bool
		expectedResult       string
		expectedError        *common.ErrorCode
		expectedMsg          *string
	}{
		{
			name:                 "consensus_achieved_with_ignored_timestamp",
			requiredParticipants: 3,
			agreementThreshold:   2,
			ignoreFields: map[string][]string{
				"eth_getBlockByNumber": {"timestamp"},
			},
			upstreams: []*common.UpstreamConfig{
				{
					Id:       "test1",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc1-ignore-fields.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
				{
					Id:       "test2",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc2-ignore-fields.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
				{
					Id:       "test3",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc3-ignore-fields.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
			},
			request: map[string]interface{}{
				"method": "eth_getBlockByNumber",
				"params": []interface{}{"0x1", false},
			},
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result": map[string]interface{}{
						"number":    "0x1",
						"hash":      "0xabc123",
						"timestamp": "0x1234567890", // Different timestamp - ignored
						"gasLimit":  "0x1c9c380",
						"gasUsed":   "0x5208",
					},
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result": map[string]interface{}{
						"number":    "0x1",
						"hash":      "0xabc123",
						"timestamp": "0x1234567899", // Different timestamp - ignored
						"gasLimit":  "0x1c9c380",
						"gasUsed":   "0x5208",
					},
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result": map[string]interface{}{
						"number":    "0x1",
						"hash":      "0xdef456",     // Different hash - should not affect consensus since first 2 agree
						"timestamp": "0x1234567888", // Different timestamp - ignored
						"gasLimit":  "0x1c9c380",
						"gasUsed":   "0x5208",
					},
				},
			},
			expectedSuccess: true,
			expectedResult:  "0x1", // Should get consensus from first 2 responses
		},
		{
			name:                 "consensus_dispute_with_ignored_timestamp_but_different_core_fields",
			requiredParticipants: 3,
			agreementThreshold:   2,
			ignoreFields: map[string][]string{
				"eth_getBlockByNumber": {"timestamp"},
			},
			upstreams: []*common.UpstreamConfig{
				{
					Id:       "test1",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc1-ignore-fields.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
				{
					Id:       "test2",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc2-ignore-fields.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
				{
					Id:       "test3",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc3-ignore-fields.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
			},
			request: map[string]interface{}{
				"method": "eth_getBlockByNumber",
				"params": []interface{}{"0x1", false},
			},
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result": map[string]interface{}{
						"number":    "0x1",
						"hash":      "0xabc123",
						"timestamp": "0x1234567890", // Different timestamp - ignored
						"gasLimit":  "0x1c9c380",
						"gasUsed":   "0x5208",
					},
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result": map[string]interface{}{
						"number":    "0x1",
						"hash":      "0xdef456",     // Different hash - not ignored
						"timestamp": "0x1234567899", // Different timestamp - ignored
						"gasLimit":  "0x1c9c380",
						"gasUsed":   "0x5208",
					},
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result": map[string]interface{}{
						"number":    "0x1",
						"hash":      "0x789abc",     // Different hash - not ignored
						"timestamp": "0x1234567888", // Different timestamp - ignored
						"gasLimit":  "0x1c9c380",
						"gasUsed":   "0x5208",
					},
				},
			},
			expectedSuccess: false,
			expectedError:   pointer(common.ErrCodeConsensusDispute),
			expectedMsg:     pointer("not enough agreement among responses"),
		},
		{
			name:                 "consensus_achieved_with_ignored_multiple_fields",
			requiredParticipants: 3,
			agreementThreshold:   3,
			ignoreFields: map[string][]string{
				"eth_getBlockByNumber": {"timestamp", "gasLimit", "gasUsed"},
			},
			upstreams: []*common.UpstreamConfig{
				{
					Id:       "test1",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc1-ignore-fields.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
				{
					Id:       "test2",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc2-ignore-fields.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
				{
					Id:       "test3",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc3-ignore-fields.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
			},
			request: map[string]interface{}{
				"method": "eth_getBlockByNumber",
				"params": []interface{}{"0x1", false},
			},
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result": map[string]interface{}{
						"number":    "0x1",
						"hash":      "0xabc123",
						"timestamp": "0x1234567890", // Different - ignored
						"gasLimit":  "0x1c9c380",    // Different - ignored
						"gasUsed":   "0x5208",       // Different - ignored
					},
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result": map[string]interface{}{
						"number":    "0x1",
						"hash":      "0xabc123",
						"timestamp": "0x1234567899", // Different - ignored
						"gasLimit":  "0x1c9c390",    // Different - ignored
						"gasUsed":   "0x5209",       // Different - ignored
					},
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result": map[string]interface{}{
						"number":    "0x1",
						"hash":      "0xabc123",
						"timestamp": "0x1234567888", // Different - ignored
						"gasLimit":  "0x1c9c3a0",    // Different - ignored
						"gasUsed":   "0x520a",       // Different - ignored
					},
				},
			},
			expectedSuccess: true,
			expectedResult:  "0x1", // Should get consensus on the number field
		},
		{
			name:                 "consensus_for_different_method_no_ignore_fields",
			requiredParticipants: 3,
			agreementThreshold:   2,
			ignoreFields: map[string][]string{
				"eth_getBlockByNumber": {"timestamp"}, // Only for eth_getBlockByNumber
			},
			upstreams: []*common.UpstreamConfig{
				{
					Id:       "test1",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc1-ignore-fields.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
				{
					Id:       "test2",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc2-ignore-fields.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
				{
					Id:       "test3",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc3-ignore-fields.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
			},
			request: map[string]interface{}{
				"method": "eth_chainId", // Different method - no ignore fields
				"params": []interface{}{},
			},
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x7a",
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x7a",
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x7b", // Different result - but first 2 match, so consensus achieved
				},
			},
			expectedSuccess: true,
			expectedResult:  "0x7a", // Should get consensus from first 2 responses
		},
		{
			name:                 "consensus_method_specific_ignore_fields_dispute",
			requiredParticipants: 3,
			agreementThreshold:   2,
			ignoreFields: map[string][]string{
				"eth_getBlockByNumber": {"timestamp"}, // Only for eth_getBlockByNumber
			},
			upstreams: []*common.UpstreamConfig{
				{
					Id:       "test1",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc1-ignore-fields.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
				{
					Id:       "test2",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc2-ignore-fields.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
				{
					Id:       "test3",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc3-ignore-fields.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
			},
			request: map[string]interface{}{
				"method": "eth_chainId", // Different method - no ignore fields applied
				"params": []interface{}{},
			},
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x7a",
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x7b", // Different result
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x7c", // Different result
				},
			},
			expectedSuccess: false,
			expectedError:   pointer(common.ErrCodeConsensusDispute),
			expectedMsg:     pointer("not enough agreement among responses"),
		},
		{
			name:                 "consensus_achieved_when_all_responses_identical",
			requiredParticipants: 3,
			agreementThreshold:   2,
			ignoreFields: map[string][]string{
				"eth_getBlockByNumber": {"timestamp", "gasLimit"},
			},
			upstreams: []*common.UpstreamConfig{
				{
					Id:       "test1",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc1-ignore-fields.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
				{
					Id:       "test2",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc2-ignore-fields.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
				{
					Id:       "test3",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc3-ignore-fields.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
			},
			request: map[string]interface{}{
				"method": "eth_getBlockByNumber",
				"params": []interface{}{"0x1", false},
			},
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result": map[string]interface{}{
						"number":    "0x1",
						"hash":      "0xabc123",
						"timestamp": "0x1234567890", // Different - ignored
						"gasLimit":  "0x1c9c380",    // Different - ignored
						"gasUsed":   "0x5208",
					},
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result": map[string]interface{}{
						"number":    "0x1",
						"hash":      "0xabc123",
						"timestamp": "0x1234567899", // Different - ignored
						"gasLimit":  "0x1c9c390",    // Different - ignored
						"gasUsed":   "0x5208",
					},
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result": map[string]interface{}{
						"number":    "0x1",
						"hash":      "0xabc123",
						"timestamp": "0x1234567888", // Different - ignored
						"gasLimit":  "0x1c9c3a0",    // Different - ignored
						"gasUsed":   "0x5208",
					},
				},
			},
			expectedSuccess: true,
			expectedResult:  "0x1", // Should get consensus
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			util.ResetGock()
			defer util.ResetGock()
			util.SetupMocksForEvmStatePoller()
			defer util.AssertNoPendingMocks(t, 0)

			ctx, cancel := context.WithCancel(context.Background())
			defer func() {
				cancel()
				time.Sleep(100 * time.Millisecond)
			}()

			// Setup network infrastructure
			mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
			vr := thirdparty.NewVendorsRegistry()
			pr, err := thirdparty.NewProvidersRegistry(&log.Logger, vr, []*common.ProviderConfig{}, nil)
			require.NoError(t, err)

			ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
				Connector: &common.ConnectorConfig{
					Driver: "memory",
					Memory: &common.MemoryConnectorConfig{
						MaxItems:     100_000,
						MaxTotalSize: "1MB",
					},
				},
			})
			require.NoError(t, err)

			upsReg := upstream.NewUpstreamsRegistry(
				ctx, &log.Logger, "prjA", tt.upstreams,
				ssr, nil, vr, pr, nil, mt, 1*time.Second,
			)

			// Create network with consensus policy that includes ignore fields
			ntw, err := NewNetwork(
				ctx, &log.Logger, "prjA",
				&common.NetworkConfig{
					Architecture: common.ArchitectureEvm,
					Evm: &common.EvmNetworkConfig{
						ChainId: 123,
					},
					Failsafe: []*common.FailsafeConfig{
						{
							MatchMethod: "*",
							Consensus: &common.ConsensusPolicyConfig{
								RequiredParticipants:    tt.requiredParticipants,
								AgreementThreshold:      tt.agreementThreshold,
								DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
								LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
								IgnoreFields:            tt.ignoreFields,
							},
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

			// Setup mock responses
			for i, upstream := range tt.upstreams {
				gock.New(upstream.Endpoint).
					Post("/").
					Times(1).
					Reply(200).
					SetHeader("Content-Type", "application/json").
					JSON(tt.mockResponses[i])
			}

			// Make request
			reqBytes, err := json.Marshal(tt.request)
			require.NoError(t, err)

			fakeReq := common.NewNormalizedRequest(reqBytes)
			resp, err := ntw.Forward(ctx, fakeReq)

			// Verify results
			if tt.expectedSuccess {
				assert.NoError(t, err)
				assert.NotNil(t, resp)

				if tt.expectedResult != "" {
					actualJrr, err := resp.JsonRpcResponse()
					require.NoError(t, err)
					assert.NotNil(t, actualJrr)

					// For complex results, just verify it's not nil and has some expected structure
					if strings.HasPrefix(tt.expectedResult, "0x") {
						// Simple hex result
						assert.Contains(t, string(actualJrr.Result), tt.expectedResult)
					}
				}
			} else {
				assert.Error(t, err)
				assert.Nil(t, resp)

				if tt.expectedError != nil {
					assert.True(t, common.HasErrorCode(err, *tt.expectedError),
						"expected error code %s but got %v", *tt.expectedError, err)
				}
				if tt.expectedMsg != nil {
					assert.Contains(t, err.Error(), *tt.expectedMsg)
				}
			}
		})
	}
}

func TestNetwork_ConsensusOnAgreedErrors(t *testing.T) {
	// Helper to create JSON-RPC error response
	createErrorResponse := func(code int, message string) map[string]interface{} {
		return map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"error": map[string]interface{}{
				"code":    code,
				"message": message,
			},
		}
	}

	tests := []struct {
		name                 string
		upstreams            []*common.UpstreamConfig
		request              map[string]interface{}
		mockResponses        []map[string]interface{}
		requiredParticipants int
		expectedError        bool
		expectedErrorCode    *int // Expected JSON-RPC error code
		expectedErrorMsg     string
	}{
		{
			name:                 "consensus_on_invalid_params_error",
			requiredParticipants: 3,
			upstreams: []*common.UpstreamConfig{
				{
					Id:       "test1",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc1-agreed-error.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
				{
					Id:       "test2",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc2-agreed-error.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
				{
					Id:       "test3",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc3-agreed-error.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
			},
			request: map[string]interface{}{
				"method": "eth_call",
				"params": []interface{}{}, // Invalid params
			},
			mockResponses: []map[string]interface{}{
				createErrorResponse(-32602, "Invalid params"),
				createErrorResponse(-32602, "Invalid params"),
				createErrorResponse(-32602, "Invalid params"),
			},
			expectedError:     true,
			expectedErrorCode: pointer(-32602),
			expectedErrorMsg:  "Invalid params",
		},
		{
			name:                 "consensus_on_method_not_found",
			requiredParticipants: 3,
			upstreams: []*common.UpstreamConfig{
				{
					Id:       "test1",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc1-agreed-error.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
				{
					Id:       "test2",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc2-agreed-error.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
				{
					Id:       "test3",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc3-agreed-error.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
			},
			request: map[string]interface{}{
				"method": "eth_unknownMethod",
				"params": []interface{}{},
			},
			mockResponses: []map[string]interface{}{
				createErrorResponse(-32601, "Method not found"),
				createErrorResponse(-32601, "Method not found"),
				createErrorResponse(-32601, "Method not found"),
			},
			expectedError:     true,
			expectedErrorCode: pointer(-32601),
			expectedErrorMsg:  "Method not found",
		},
		{
			name:                 "consensus_on_missing_data",
			requiredParticipants: 3,
			upstreams: []*common.UpstreamConfig{
				{
					Id:       "test1",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc1-agreed-error.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
				{
					Id:       "test2",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc2-agreed-error.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
				{
					Id:       "test3",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc3-agreed-error.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
			},
			request: map[string]interface{}{
				"method": "eth_getBlockByNumber",
				"params": []interface{}{"0x999999999", false}, // Very old block
			},
			mockResponses: []map[string]interface{}{
				createErrorResponse(-32014, "requested data is not available"),
				createErrorResponse(-32014, "requested data is not available"),
				createErrorResponse(-32014, "requested data is not available"),
			},
			expectedError:     true,
			expectedErrorCode: pointer(-32014),
			expectedErrorMsg:  "requested data is not available",
		},
		{
			name:                 "consensus_on_execution_reverted",
			requiredParticipants: 3,
			upstreams: []*common.UpstreamConfig{
				{
					Id:       "test1",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc1-agreed-error.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
				{
					Id:       "test2",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc2-agreed-error.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
				{
					Id:       "test3",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc3-agreed-error.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
			},
			request: map[string]interface{}{
				"method": "eth_call",
				"params": []interface{}{
					map[string]interface{}{
						"to":   "0x0000000000000000000000000000000000000000",
						"data": "0x12345678",
					},
					"latest",
				},
			},
			mockResponses: []map[string]interface{}{
				createErrorResponse(3, "execution reverted"),
				createErrorResponse(3, "execution reverted"),
				createErrorResponse(3, "execution reverted"),
			},
			expectedError:     true,
			expectedErrorCode: pointer(3),
			expectedErrorMsg:  "execution reverted",
		},
		{
			name:                 "mixed_errors_returns_dispute",
			requiredParticipants: 3,
			upstreams: []*common.UpstreamConfig{
				{
					Id:       "test1",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc1-agreed-error.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
				{
					Id:       "test2",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc2-agreed-error.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
				{
					Id:       "test3",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc3-agreed-error.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
			},
			request: map[string]interface{}{
				"method": "eth_call",
				"params": []interface{}{},
			},
			mockResponses: []map[string]interface{}{
				createErrorResponse(-32602, "Invalid params"),
				createErrorResponse(-32601, "Method not found"),
				createErrorResponse(-32000, "Internal error"),
			},
			expectedError:     true,
			expectedErrorCode: nil, // Should get dispute error, not specific JSON-RPC error
			expectedErrorMsg:  "not enough participants",
		},
		{
			name:                 "majority_agreement_on_error_with_one_success",
			requiredParticipants: 3,
			upstreams: []*common.UpstreamConfig{
				{
					Id:       "test1",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc1-agreed-error.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
				{
					Id:       "test2",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc2-agreed-error.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
				{
					Id:       "test3",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc3-agreed-error.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
			},
			request: map[string]interface{}{
				"method": "eth_call",
				"params": []interface{}{},
			},
			mockResponses: []map[string]interface{}{
				createErrorResponse(-32602, "Invalid params"),
				createErrorResponse(-32602, "Invalid params"),
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x1234",
				},
			},
			expectedError:     true,
			expectedErrorCode: pointer(-32602),
			expectedErrorMsg:  "Invalid params",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			util.ResetGock()
			defer util.ResetGock()
			util.SetupMocksForEvmStatePoller()
			// Don't check for pending mocks since we use Persist() for consensus scenarios

			ctx, cancel := context.WithCancel(context.Background())
			defer func() {
				cancel()
				time.Sleep(100 * time.Millisecond)
			}()

			// Setup network infrastructure
			mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)
			vr := thirdparty.NewVendorsRegistry()
			pr, err := thirdparty.NewProvidersRegistry(&log.Logger, vr, []*common.ProviderConfig{}, nil)
			require.NoError(t, err)

			ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
				Connector: &common.ConnectorConfig{
					Driver: "memory",
					Memory: &common.MemoryConnectorConfig{
						MaxItems:     100_000,
						MaxTotalSize: "1MB",
					},
				},
			})
			require.NoError(t, err)

			upsReg := upstream.NewUpstreamsRegistry(
				ctx, &log.Logger, "prjA", tt.upstreams,
				ssr, nil, vr, pr, nil, mt, 1*time.Second,
			)

			// Create network with consensus policy
			ntw, err := NewNetwork(
				ctx, &log.Logger, "prjA",
				&common.NetworkConfig{
					Architecture: common.ArchitectureEvm,
					Evm: &common.EvmNetworkConfig{
						ChainId: 123,
					},
					Failsafe: []*common.FailsafeConfig{
						{
							MatchMethod: "*",
							Consensus: &common.ConsensusPolicyConfig{
								RequiredParticipants:    tt.requiredParticipants,
								AgreementThreshold:      2, // Majority agreement
								DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
								LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
							},
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

			// Setup mock responses
			for i, upstream := range tt.upstreams {
				gock.New(upstream.Endpoint).
					Post("/").
					Persist().
					Reply(200).
					SetHeader("Content-Type", "application/json").
					JSON(tt.mockResponses[i])
			}

			// Make request
			reqBytes, err := json.Marshal(tt.request)
			require.NoError(t, err)

			fakeReq := common.NewNormalizedRequest(reqBytes)
			resp, err := ntw.Forward(ctx, fakeReq)

			// Verify results
			if tt.expectedError {
				assert.Error(t, err)
				assert.Nil(t, resp)

				if tt.expectedErrorCode != nil {
					// Should get the specific JSON-RPC error
					var jre *common.ErrJsonRpcExceptionInternal
					if assert.True(t, errors.As(err, &jre), "expected JSON-RPC error but got %T: %v", err, err) {
						assert.Equal(t, common.JsonRpcErrorNumber(*tt.expectedErrorCode), jre.NormalizedCode(),
							"expected error code %d but got %d", *tt.expectedErrorCode, jre.NormalizedCode())
						assert.Contains(t, jre.Message, tt.expectedErrorMsg)
					}
				} else {
					// Should get consensus dispute error
					assert.True(t, common.HasErrorCode(err, common.ErrCodeConsensusDispute, common.ErrCodeConsensusLowParticipants),
						"expected consensus dispute error but got %v", err)
					assert.Contains(t, err.Error(), tt.expectedErrorMsg)
				}
			} else {
				assert.NoError(t, err)
				assert.NotNil(t, resp)
			}
		})
	}
}
