package erpc

import (
	"context"
	"encoding/json"
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
		failureBehavior         *common.ConsensusFailureBehavior
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
			requiredParticipants:    2,
			lowParticipantsBehavior: pointer(common.ConsensusLowParticipantsBehaviorOnlyBlockHeadLeader),
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
					"result":  "0x7a",
				},
			},
			expectedCalls: []int{
				0, // test1 should NOT be called
				0, // test2 should NOT be called
				1, // test3 (leader) should be called
			},
			expectedResponse: common.NewNormalizedResponse().
				WithJsonRpcResponse(successResponse),
		},
		{
			name:                    "prefer_block_head_leader_includes_leader_in_participants",
			requiredParticipants:    2,
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
					"result":  "0x7a",
				},
			},
			expectedCalls: []int{
				1, // test1 should be called
				0, // test2 should NOT be called (replaced by leader)
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
			defer cancel()

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

			failureBehavior := common.ConsensusFailureBehaviorReturnError
			if tt.failureBehavior != nil {
				failureBehavior = *tt.failureBehavior
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
					Failsafe: &common.FailsafeConfig{
						Retry: retryPolicy,
						Consensus: &common.ConsensusPolicyConfig{
							RequiredParticipants:    tt.requiredParticipants,
							AgreementThreshold:      agreementThreshold,
							FailureBehavior:         failureBehavior,
							DisputeBehavior:         disputeBehavior,
							LowParticipantsBehavior: lowParticipantsBehavior,
							PunishMisbehavior:       &common.PunishMisbehaviorConfig{},
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
			} else if resp != nil {
				t.Logf("Got response from upstream: %v", resp.Upstream())
			}

			if tt.expectedError != nil {
				assert.Error(t, err, "expected error but got nil")
				assert.True(t, common.HasErrorCode(err, *tt.expectedError), "expected error code %s, got %s", *tt.expectedError, err)
				assert.Contains(t, err.Error(), *tt.expectedMsg, "expected error message %s, got %s", *tt.expectedMsg, err.Error())
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
	defer cancel()

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
			Failsafe: &common.FailsafeConfig{
				Retry: &common.RetryPolicyConfig{
					MaxAttempts: 2,
					Delay:       common.Duration(0),
				},
				Consensus: &common.ConsensusPolicyConfig{
					RequiredParticipants:    2,
					AgreementThreshold:      2,
					FailureBehavior:         common.ConsensusFailureBehaviorReturnError,
					DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
					LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
					PunishMisbehavior:       &common.PunishMisbehaviorConfig{},
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
