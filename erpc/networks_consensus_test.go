package erpc

import (
	"context"
	"encoding/json"
	"errors"
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
		maxParticipants         int
		agreementThreshold      *int
		disputeBehavior         *common.ConsensusDisputeBehavior
		lowParticipantsBehavior *common.ConsensusLowParticipantsBehavior
		expectedCalls           []int // Number of expected calls for each upstream
		expectedResponse        *common.NormalizedResponse
		expectedError           *common.ErrorCode
		expectedMsg             *string
	}{
		{
			name:            "successful_consensus",
			maxParticipants: 3,
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
			name:            "successful_consensus_only_necessary_participants",
			maxParticipants: 2,
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
				0, // test3 will NOT be called since maxParticipants is 2
			},
			expectedResponse: common.NewNormalizedResponse().
				WithJsonRpcResponse(successResponse),
		},
		{
			name:            "low_participants_error",
			maxParticipants: 3,
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
			expectedResponse: func() *common.NormalizedResponse {
				jrr, _ := common.NewJsonRpcResponse(1, "0x7b", nil)
				return common.NewNormalizedResponse().WithJsonRpcResponse(jrr)
			}(),
		},
		{
			name:            "dispute_error",
			maxParticipants: 3,
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
			name:            "retried_dispute_error",
			maxParticipants: 2,
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
			expectedResponse: func() *common.NormalizedResponse {
				jrr, _ := common.NewJsonRpcResponse(1, "0x1", nil)
				return common.NewNormalizedResponse().WithJsonRpcResponse(jrr)
			}(),
		},
		{
			name:            "retry_on_error_and_success_on_next_upstream",
			maxParticipants: 3,
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
			name:            "error_response_on_upstreams",
			maxParticipants: 3,
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
			expectedMsg:   pointer("no clear most common result"),
		},
		{
			name:               "some_participants_return_error",
			maxParticipants:    3,
			agreementThreshold: pointer(3),
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
			name:                    "some_participants_return_error_and_return_error_on_low_participants",
			maxParticipants:         3,
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
			maxParticipants:         3,
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
			maxParticipants:         3,
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
			maxParticipants:         3,
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
			maxParticipants:         3,
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
					"result":  "0x7a", // Same result achieves consensus
				},
				// test3 not mocked since it won't be called due to early consensus
			},
			expectedCalls: []int{
				1, // test1 should be used
				1, // test2 should be used (both return 0x7a, achieving consensus)
				0, // test3 (leader) not called due to early consensus
			},
			expectedResponse: common.NewNormalizedResponse().
				WithJsonRpcResponse(successResponse),
		},
		{
			name:                    "only_block_head_leader_no_leader_available_uses_normal_selection",
			maxParticipants:         2,
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
		{
			name:                    "low_participants_with_prefer_block_head_leader_fallback",
			maxParticipants:         3,
			agreementThreshold:      pointer(2),
			lowParticipantsBehavior: pointer(common.ConsensusLowParticipantsBehaviorPreferBlockHeadLeader),
			upstreams: []*common.UpstreamConfig{
				{
					Id:       "test1",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc1-fallback.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
				{
					Id:       "test2",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc2-fallback.localhost",
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
				1, // test1 should be called
				1, // test2 should be called
			},
			expectedResponse: common.NewNormalizedResponse().
				WithJsonRpcResponse(successResponse),
		},
		{
			name:                    "low_participants_with_prefer_block_head_leader",
			maxParticipants:         3,
			agreementThreshold:      pointer(4),
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
					Endpoint: "http://rpc2-follower.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				},
				{
					Id:       "test3",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc3-follower.localhost",
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
					"result":  "0x7b", // test2 response (needed to detect low participants)
				},
				// test3 not mocked since it won't be called due to block head leader short-circuit
			},
			expectedCalls: []int{
				1, // test1 (leader) should be called and its result used
				1, // test2 (needed to detect low participants)
				0, // test3 (not called due to block head leader short-circuit)
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

			var lowParticipantsBehavior common.ConsensusLowParticipantsBehavior
			if tt.lowParticipantsBehavior != nil {
				lowParticipantsBehavior = *tt.lowParticipantsBehavior
			}

			disputeBehavior := common.ConsensusDisputeBehaviorReturnError
			if tt.disputeBehavior != nil {
				disputeBehavior = *tt.disputeBehavior
			}

			// Create consensus config and apply defaults
			consensusConfig := &common.ConsensusPolicyConfig{
				MaxParticipants:         tt.maxParticipants,
				AgreementThreshold:      agreementThreshold,
				DisputeBehavior:         disputeBehavior,
				LowParticipantsBehavior: lowParticipantsBehavior,
				PunishMisbehavior:       &common.PunishMisbehaviorConfig{},
			}
			// Apply defaults when behavior is not explicitly set
			if tt.lowParticipantsBehavior == nil {
				err := consensusConfig.SetDefaults()
				if err != nil {
					t.Fatal(err)
				}
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
							Matchers: []*common.MatcherConfig{
								{
									Method: "*",
									Action: common.MatcherInclude,
								},
							},
							Retry: retryPolicy,
							Consensus: &common.ConsensusPolicyConfig{
								MaxParticipants:         tt.maxParticipants,
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
			} else if tt.name == "low_participants_with_prefer_block_head_leader_fallback" {
				// For the fallback test, set all upstreams to same block number (no clear leader)
				upsList := upsReg.GetNetworkUpstreams(ctx, util.EvmNetworkId(123))
				for _, ups := range upsList {
					ups.EvmStatePoller().SuggestLatestBlock(100) // All at same block
				}
			} else if tt.name == "low_participants_with_prefer_block_head_leader" {
				// For the leader test, set test1 as leader with higher block
				upsList := upsReg.GetNetworkUpstreams(ctx, util.EvmNetworkId(123))
				if len(upsList) >= 3 {
					upsList[0].EvmStatePoller().SuggestLatestBlock(200) // test1 (leader)
					upsList[1].EvmStatePoller().SuggestLatestBlock(100) // test2 (follower)
					upsList[2].EvmStatePoller().SuggestLatestBlock(50)  // test3 (follower)
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
					Matchers: []*common.MatcherConfig{
						{
							Method: "*",
							Action: common.MatcherInclude,
						},
					},
					Retry: &common.RetryPolicyConfig{
						MaxAttempts: 2,
						Delay:       common.Duration(0),
					},
					Consensus: &common.ConsensusPolicyConfig{
						MaxParticipants:         2,
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
		Filter(func(request *http.Request) bool {
			body := util.SafeReadBody(request)
			return strings.Contains(body, "eth_getBlockByNumber")
		}).
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
		Filter(func(request *http.Request) bool {
			body := util.SafeReadBody(request)
			return strings.Contains(body, "eth_getBlockByNumber")
		}).
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
		Filter(func(request *http.Request) bool {
			body := util.SafeReadBody(request)
			return strings.Contains(body, "eth_getBlockByNumber")
		}).
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
	// Apply defaults to consensus policy
	err := consensusConfig.SetDefaults()
	require.NoError(t, err)

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
					Matchers: []*common.MatcherConfig{
						{
							Method: "*",
							Action: common.MatcherInclude,
						},
					},
					Consensus: consensusConfig,
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
	defer util.AssertNoPendingMocks(t, 0) // All mocks should be consumed

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
		Times(1).
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
		Times(1).
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

	// Other upstreams are slow and should be cancelled - but we still need to set up the mocks
	// Use Times(1) to expect at most 1 call, but they may be cancelled before being called
	for _, host := range []string{"upstream1", "upstream4", "upstream5"} {
		gock.New("http://" + host + ".localhost").
			Post("").
			Times(1).
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
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_chainId")
			}).
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
		MaxParticipants:    3,
		AgreementThreshold: 2, // Should short-circuit after 2 matching responses
		DisputeBehavior:    common.ConsensusDisputeBehaviorReturnError,
		PunishMisbehavior:  &common.PunishMisbehaviorConfig{},
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

// TestConsensusEmptyishShortCircuitPrevention tests that consensus short-circuiting is avoided for emptyish results
func TestConsensusEmptyishShortCircuitPrevention(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	tests := []struct {
		name                 string
		mockResponses        []map[string]interface{}
		expectedShortCircuit bool
		expectedMinDuration  time.Duration
		expectedMaxDuration  time.Duration
		description          string
	}{
		{
			name: "normal_consensus_still_short_circuits",
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x7a", // Non-emptyish result
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x7a", // Same non-emptyish result
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x7b", // Different result (won't be reached due to short-circuit)
				},
			},
			expectedShortCircuit: true,
			expectedMaxDuration:  500 * time.Millisecond,
			description:          "Normal non-emptyish consensus should still short-circuit",
		},
		{
			name: "emptyish_null_overridden_by_non_empty",
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  nil, // null result (emptyish)
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  nil, // Same null result (emptyish)
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x7a", // Non-emptyish result
				},
			},
			expectedShortCircuit: false,
			expectedMinDuration:  100 * time.Millisecond, // Should wait for all responses to prefer non-empty
			description:          "Should wait for non-empty responses even when empty consensus is reached",
		},
		{
			name: "emptyish_empty_array_overridden_by_non_empty",
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  []interface{}{}, // Empty array (emptyish)
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  []interface{}{}, // Same empty array (emptyish)
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x123", // Non-emptyish result
				},
			},
			expectedShortCircuit: false,
			expectedMinDuration:  100 * time.Millisecond,
			description:          "Should wait for non-empty responses even when empty array consensus is reached",
		},
		{
			name: "emptyish_empty_object_overridden_by_non_empty",
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  map[string]interface{}{}, // Empty object (emptyish)
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  map[string]interface{}{}, // Same empty object (emptyish)
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x456", // Non-emptyish result
				},
			},
			expectedShortCircuit: false,
			expectedMinDuration:  100 * time.Millisecond,
			description:          "Should wait for non-empty responses even when empty object consensus is reached",
		},
		{
			name: "emptyish_zero_hex_overridden_by_non_empty",
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x0", // Zero hex (emptyish)
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x0", // Same zero hex (emptyish)
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x789", // Non-emptyish result
				},
			},
			expectedShortCircuit: false,
			expectedMinDuration:  100 * time.Millisecond,
			description:          "Should wait for non-empty responses even when zero hex consensus is reached",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Reset gock for each test
			gock.Clean()
			util.SetupMocksForEvmStatePoller()

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

			// Mock responses for each upstream
			for i, upstream := range upstreams {
				if i < len(tc.mockResponses) {
					mock := gock.New(upstream.Endpoint).
						Post("").
						Times(1).
						Reply(200)

					// Add delay to the third response (non-empty) when we expect no short-circuit
					// This simulates the real-world scenario where the meaningful data arrives later
					if i == 2 && !tc.expectedShortCircuit {
						mock = mock.Delay(150 * time.Millisecond)
					}

					mock.JSON(tc.mockResponses[i])
				}
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			network := setupTestNetworkWithConsensusPolicy(t, ctx, upstreams, &common.ConsensusPolicyConfig{
				MaxParticipants:    3,
				AgreementThreshold: 2,                                                   // Should normally short-circuit after 2 matching responses
				DisputeBehavior:    common.ConsensusDisputeBehaviorAcceptAnyValidResult, // Use AcceptAnyValid to prefer non-empty
				PunishMisbehavior:  &common.PunishMisbehaviorConfig{},
			})

			// Make request
			req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}`))

			start := time.Now()
			resp, err := network.Forward(ctx, req)
			duration := time.Since(start)

			require.NoError(t, err)
			require.NotNil(t, resp)

			if tc.expectedShortCircuit {
				assert.Less(t, duration, tc.expectedMaxDuration, tc.description)
			} else {
				assert.Greater(t, duration, tc.expectedMinDuration, tc.description)
			}

			t.Logf("%s: completed in %v", tc.description, duration)
		})
	}
}

// TestConsensusEmptyishMixedScenarios tests mixed scenarios with emptyish and non-emptyish responses
func TestConsensusEmptyishMixedScenarios(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	tests := []struct {
		name           string
		mockResponses  []map[string]interface{}
		expectError    bool
		expectedResult interface{}
		description    string
	}{
		{
			name: "emptyish_consensus_overridden_by_meaningful_data",
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  nil, // Emptyish result
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  nil, // Same emptyish result
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x123", // Meaningful result
				},
			},
			expectError:    false,
			expectedResult: `"0x123"`, // Should prefer the meaningful result (1 non-empty beats 2 empty)
			description:    "Should prefer meaningful data over emptyish results regardless of count",
		},
		{
			name: "all_emptyish_responses_return_emptyish_consensus",
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  nil,
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  nil,
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  nil,
				},
			},
			expectError:    false,
			expectedResult: "null", // Should return the emptyish consensus
			description:    "When all responses are emptyish, should return emptyish consensus",
		},
		{
			name: "mixed_emptyish_types_with_meaningful_winner",
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  []interface{}{}, // Empty array
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  map[string]interface{}{}, // Empty object
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x456", // Meaningful result
				},
			},
			expectError:    false,
			expectedResult: `"0x456"`, // 1 non-empty beats 2 different empty types
			description:    "Should prefer meaningful result over mixed emptyish responses regardless of count",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Reset gock for each test
			gock.Clean()
			util.SetupMocksForEvmStatePoller()

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

			// Mock responses for each upstream
			for i, upstream := range upstreams {
				if i < len(tc.mockResponses) {
					gock.New(upstream.Endpoint).
						Post("").
						Times(1).
						Reply(200).
						JSON(tc.mockResponses[i])
				}
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			network := setupTestNetworkWithConsensusPolicy(t, ctx, upstreams, &common.ConsensusPolicyConfig{
				MaxParticipants:    3,
				AgreementThreshold: 2,
				DisputeBehavior:    common.ConsensusDisputeBehaviorAcceptAnyValidResult,
				PunishMisbehavior:  &common.PunishMisbehaviorConfig{},
			})

			// Make request
			req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}`))

			resp, err := network.Forward(ctx, req)

			if tc.expectError {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
				require.NotNil(t, resp)

				// Verify response content
				jrr, err := resp.JsonRpcResponse()
				require.NoError(t, err)
				assert.Equal(t, tc.expectedResult, string(jrr.Result))
			}

			t.Logf("%s: test completed successfully", tc.description)
		})
	}
}

// TestConsensusNonEmptyPreference tests that non-empty responses are preferred when finding consensus
func TestConsensusNonEmptyPreference(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	tests := []struct {
		name               string
		mockResponses      []map[string]interface{}
		maxParticipants    int
		agreementThreshold int
		disputeBehavior    *common.ConsensusDisputeBehavior
		expectedResult     interface{}
		expectedError      bool
		expectedConsensus  bool
		description        string
	}{
		{
			name: "prefer_non_empty_2v2_tie",
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  nil, // Empty result
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  nil, // Same empty result (2 empty)
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x123", // Non-empty result
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x123", // Same non-empty result (2 non-empty)
				},
			},
			maxParticipants:    4,
			agreementThreshold: 2,
			disputeBehavior:    nil,       // Default behavior
			expectedResult:     `"0x123"`, // Should prefer non-empty
			expectedConsensus:  true,
			description:        "4 participants: 2 empty, 2 non-empty → prefer non-empty",
		},
		{
			name: "prefer_non_empty_3v1_always_prefer_non_empty",
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  []interface{}{}, // Empty array
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  []interface{}{}, // Same empty array
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  []interface{}{}, // Same empty array (3 empty)
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x456", // Non-empty result (1 non-empty)
				},
			},
			maxParticipants:    4,
			agreementThreshold: 2,    // Non-empty count (1) doesn't meet threshold
			disputeBehavior:    nil,  // Default behavior (return error)
			expectedError:      true, // Should trigger dispute behavior
			expectedConsensus:  false,
			description:        "4 participants: 3 empty, 1 non-empty → prefer non-empty but fails threshold check",
		},
		{
			name: "prefer_non_empty_3v1_with_accept_behavior",
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  map[string]interface{}{}, // Empty object
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  map[string]interface{}{}, // Same empty object
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  map[string]interface{}{}, // Same empty object (3 empty)
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x789", // Non-empty result (1 non-empty)
				},
			},
			maxParticipants:    4,
			agreementThreshold: 2, // Non-empty count (1) doesn't meet threshold
			disputeBehavior:    &[]common.ConsensusDisputeBehavior{common.ConsensusDisputeBehaviorAcceptAnyValidResult}[0],
			expectedResult:     `"0x789"`, // Should accept the non-empty result via dispute behavior
			expectedConsensus:  true,
			description:        "4 participants: 3 empty, 1 non-empty → prefer non-empty, accept via dispute behavior",
		},
		{
			name: "prefer_non_empty_1v3_clear_winner",
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x0", // Empty hex
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xabc", // Non-empty result
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xabc", // Same non-empty result
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xabc", // Same non-empty result (3 non-empty)
				},
			},
			maxParticipants:    4,
			agreementThreshold: 2,
			disputeBehavior:    nil,       // Default behavior
			expectedResult:     `"0xabc"`, // Clear winner by count
			expectedConsensus:  true,
			description:        "4 participants: 1 empty, 3 non-empty → non-empty wins by count",
		},
		{
			name: "mixed_empty_types_prefer_non_empty",
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  nil, // null
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  []interface{}{}, // empty array
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xdef", // Non-empty result
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xdef", // Same non-empty result
				},
			},
			maxParticipants:    4,
			agreementThreshold: 2,
			disputeBehavior:    nil,       // Default behavior
			expectedResult:     `"0xdef"`, // Should prefer non-empty
			expectedConsensus:  true,
			description:        "Mixed empty types vs non-empty → prefer non-empty",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Reset gock for each test
			gock.Clean()
			util.SetupMocksForEvmStatePoller()

			// Create upstreams
			upstreams := make([]*common.UpstreamConfig, tc.maxParticipants)
			for i := 0; i < tc.maxParticipants; i++ {
				upstreams[i] = &common.UpstreamConfig{
					Id:       fmt.Sprintf("upstream%d", i+1),
					Endpoint: fmt.Sprintf("http://upstream%d.localhost", i+1),
					Type:     common.UpstreamTypeEvm,
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				}
			}

			// Mock responses for each upstream
			for i, upstream := range upstreams {
				if i < len(tc.mockResponses) {
					gock.New(upstream.Endpoint).
						Post("").
						Times(1).
						Reply(200).
						JSON(tc.mockResponses[i])
				}
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			disputeBehavior := common.ConsensusDisputeBehaviorReturnError
			if tc.disputeBehavior != nil {
				disputeBehavior = *tc.disputeBehavior
			}

			network := setupTestNetworkWithConsensusPolicy(t, ctx, upstreams, &common.ConsensusPolicyConfig{
				MaxParticipants:    tc.maxParticipants,
				AgreementThreshold: tc.agreementThreshold,
				DisputeBehavior:    disputeBehavior,
				PunishMisbehavior:  &common.PunishMisbehaviorConfig{},
			})

			// Make request
			req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}`))

			resp, err := network.Forward(ctx, req)

			if tc.expectedError {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
				require.NotNil(t, resp)

				// Verify response content
				jrr, err := resp.JsonRpcResponse()
				require.NoError(t, err)
				assert.Equal(t, tc.expectedResult, string(jrr.Result))
			}

			t.Logf("%s: test completed successfully", tc.description)
		})
	}
}

func TestConsensusEvmEmptyLogsPreference(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	// Transaction receipt with actual logs (what the winning upstream returns)
	receiptWithLogs := map[string]interface{}{
		"blockHash":         "0xae648a30096d5249966117b30d52210b63b6ca3467496e25c578a31eb981add5",
		"blockNumber":       "0x4723110",
		"contractAddress":   nil,
		"cumulativeGasUsed": "0x0",
		"effectiveGasPrice": "0x105",
		"from":              "0x0000000000000000000000000000000000000000",
		"gasUsed":           "0x0",
		"logs": []interface{}{
			map[string]interface{}{
				"address": "0x8f3cf7ad23cd3cadbd9735aff958023239c6a063",
				"topics": []interface{}{
					"0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef",
					"0x0000000000000000000000000000000000000000000000000000000000000000",
					"0x0000000000000000000000002ce910fbba65b454bbaf6a18c952a70f3bcd8299",
				},
				"data":             "0x000000000000000000000000000000000000000000004110907a4408eab20000",
				"blockNumber":      "0x4723110",
				"transactionHash":  "0xce77f4de5787a3bc16a12ea0c833f9041e1b664c8f2a4076e8c8c27dfffe5526",
				"transactionIndex": "0xb6",
				"blockHash":        "0xae648a30096d5249966117b30d52210b63b6ca3467496e25c578a31eb981add5",
				"logIndex":         "0x0",
				"removed":          false,
			},
		},
		"logsBloom":        "0x00000000000000001000000000000800000000000000000004000000000000000000000002000000000040000000000000000000000080002000200000000000000000000000200008000008000000000000000000000000000200000000000000000000020000000000000000000800000000000000000100004010000000000001000000000000000000000000000000000000000200000000000000000000000000000000200000000000000000000000100000000000000000000000000000000022000000000000000000000000000000000000000000000000000020000008008000022000000000000020000000000000000000000000020000000000",
		"status":           "0x1",
		"to":               "0x0000000000000000000000000000000000000000",
		"transactionHash":  "0xce77f4de5787a3bc16a12ea0c833f9041e1b664c8f2a4076e8c8c27dfffe5526",
		"transactionIndex": "0xb6",
		"type":             "0x0",
	}

	// Same receipt structure but with empty logs array
	receiptWithEmptyLogs := map[string]interface{}{
		"blockHash":         "0xae648a30096d5249966117b30d52210b63b6ca3467496e25c578a31eb981add5",
		"blockNumber":       "0x4723110",
		"contractAddress":   nil,
		"cumulativeGasUsed": "0x0",
		"effectiveGasPrice": "0x105",
		"from":              "0x0000000000000000000000000000000000000000",
		"gasUsed":           "0x0",
		"logs":              []interface{}{}, // Empty logs array
		"logsBloom":         "0x00000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000",
		"status":            "0x1",
		"to":                "0x0000000000000000000000000000000000000000",
		"transactionHash":   "0xce77f4de5787a3bc16a12ea0c833f9041e1b664c8f2a4076e8c8c27dfffe5526",
		"transactionIndex":  "0xb6",
		"type":              "0x0",
	}

	tests := []struct {
		name               string
		mockResponses      []map[string]interface{}
		maxParticipants    int
		agreementThreshold int
		disputeBehavior    *common.ConsensusDisputeBehavior
		expectedResult     interface{}
		expectedError      bool
		expectedConsensus  bool
		description        string
	}{
		{
			name: "prefer_transaction_receipt_with_logs_over_empty_logs",
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  receiptWithEmptyLogs, // Empty logs
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  receiptWithEmptyLogs, // Empty logs
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  receiptWithEmptyLogs, // Empty logs (3 total with empty logs)
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  receiptWithLogs, // Has actual logs (1 with logs)
				},
			},
			maxParticipants:    4,
			agreementThreshold: 2, // Need 2 to agree, but should prefer non-empty
			disputeBehavior:    &[]common.ConsensusDisputeBehavior{common.ConsensusDisputeBehaviorAcceptAnyValidResult}[0],
			expectedConsensus:  true,
			description:        "4 participants: 3 with empty logs, 1 with actual logs → prefer non-empty logs",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Reset gock for each test
			gock.Clean()
			util.SetupMocksForEvmStatePoller()

			// Create upstreams
			upstreams := make([]*common.UpstreamConfig, tc.maxParticipants)
			for i := 0; i < tc.maxParticipants; i++ {
				upstreams[i] = &common.UpstreamConfig{
					Id:       fmt.Sprintf("upstream%d", i+1),
					Endpoint: fmt.Sprintf("http://upstream%d.localhost", i+1),
					Type:     common.UpstreamTypeEvm,
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				}
			}

			// Mock responses for each upstream
			for i, upstream := range upstreams {
				if i < len(tc.mockResponses) {
					gock.New(upstream.Endpoint).
						Post("").
						Times(1).
						Reply(200).
						JSON(tc.mockResponses[i])
				}
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			disputeBehavior := common.ConsensusDisputeBehaviorReturnError
			if tc.disputeBehavior != nil {
				disputeBehavior = *tc.disputeBehavior
			}

			network := setupTestNetworkWithConsensusPolicy(t, ctx, upstreams, &common.ConsensusPolicyConfig{
				MaxParticipants:    tc.maxParticipants,
				AgreementThreshold: tc.agreementThreshold,
				DisputeBehavior:    disputeBehavior,
				PunishMisbehavior:  &common.PunishMisbehaviorConfig{},
			})

			// Make eth_getTransactionReceipt request
			req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getTransactionReceipt","params":["0xce77f4de5787a3bc16a12ea0c833f9041e1b664c8f2a4076e8c8c27dfffe5526"],"id":1}`))

			resp, err := network.Forward(ctx, req)

			if tc.expectedError {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
				require.NotNil(t, resp)

				// Verify response content
				jrr, err := resp.JsonRpcResponse()
				require.NoError(t, err)

				// Extract logs from the result to verify we got the non-empty logs response
				var resultObj map[string]interface{}
				err = common.SonicCfg.Unmarshal(jrr.Result, &resultObj)
				require.NoError(t, err)

				logs, exists := resultObj["logs"]
				require.True(t, exists, "Response should contain logs field")

				logsArray, ok := logs.([]interface{})
				require.True(t, ok, "Logs should be an array")

				// Should prefer the response with actual logs, not empty logs
				assert.Greater(t, len(logsArray), 0, "Should prefer response with non-empty logs array")

				t.Logf("Got response with %d logs entries", len(logsArray))
			}

			t.Logf("%s: test completed successfully", tc.description)
		})
	}
}

// TestConsensusNonEmptyPreferenceWithDisputes tests non-empty preference with dispute scenarios
func TestConsensusNonEmptyPreferenceWithDisputes(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	tests := []struct {
		name               string
		mockResponses      []map[string]interface{}
		maxParticipants    int
		agreementThreshold int
		disputeBehavior    common.ConsensusDisputeBehavior
		expectedError      bool
		expectedResult     interface{}
		description        string
	}{
		{
			name: "dispute_with_empty_preference_return_error",
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  nil, // Empty result
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x123", // Non-empty result
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x456", // Different non-empty result
				},
			},
			maxParticipants:    3,
			agreementThreshold: 2, // No consensus possible
			disputeBehavior:    common.ConsensusDisputeBehaviorReturnError,
			expectedError:      true, // Should return dispute error
			description:        "Dispute scenario with mixed empty/non-empty → error",
		},
		{
			name: "dispute_accept_most_common_no_clear_winner",
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  []interface{}{}, // Empty array
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x789", // Non-empty result
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xabc", // Different non-empty result
				},
			},
			maxParticipants:    3,
			agreementThreshold: 2, // No consensus possible
			disputeBehavior:    common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
			expectedError:      true, // Should error because all results have count 1
			description:        "Dispute with accept most common → error when no clear winner",
		},
		{
			name: "dispute_accept_any_valid_prefers_non_empty",
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  []interface{}{}, // Empty array
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x789", // Non-empty result
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xabc", // Different non-empty result
				},
			},
			maxParticipants:    3,
			agreementThreshold: 2, // No consensus possible
			disputeBehavior:    common.ConsensusDisputeBehaviorAcceptAnyValidResult,
			expectedError:      false,
			expectedResult:     `"0x789"`, // Should return first non-empty result
			description:        "Dispute with accept any valid → prefers non-empty",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Reset gock for each test
			gock.Clean()
			util.SetupMocksForEvmStatePoller()

			// Create upstreams
			upstreams := make([]*common.UpstreamConfig, tc.maxParticipants)
			for i := 0; i < tc.maxParticipants; i++ {
				upstreams[i] = &common.UpstreamConfig{
					Id:       fmt.Sprintf("upstream%d", i+1),
					Endpoint: fmt.Sprintf("http://upstream%d.localhost", i+1),
					Type:     common.UpstreamTypeEvm,
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				}
			}

			// Mock responses for each upstream
			for i, upstream := range upstreams {
				if i < len(tc.mockResponses) {
					gock.New(upstream.Endpoint).
						Post("").
						Times(1).
						Reply(200).
						JSON(tc.mockResponses[i])
				}
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			network := setupTestNetworkWithConsensusPolicy(t, ctx, upstreams, &common.ConsensusPolicyConfig{
				MaxParticipants:    tc.maxParticipants,
				AgreementThreshold: tc.agreementThreshold,
				DisputeBehavior:    tc.disputeBehavior,
				PunishMisbehavior:  &common.PunishMisbehaviorConfig{},
			})

			// Make request
			req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}`))

			resp, err := network.Forward(ctx, req)

			if tc.expectedError {
				assert.Error(t, err)
				// Check if it's a dispute error
				assert.True(t, common.HasErrorCode(err, common.ErrCodeConsensusDispute))
			} else {
				require.NoError(t, err)
				require.NotNil(t, resp)

				// Verify response content
				jrr, err := resp.JsonRpcResponse()
				require.NoError(t, err)

				// For AcceptAnyValidResult with non-empty preference, accept any non-empty result
				if tc.name == "dispute_accept_any_valid_prefers_non_empty" {
					result := string(jrr.Result)
					assert.True(t, result == `"0x789"` || result == `"0xabc"`,
						"Expected one of the non-empty results, got %s", result)
				} else {
					assert.Equal(t, tc.expectedResult, string(jrr.Result))
				}
			}

			t.Logf("%s: test completed successfully", tc.description)
		})
	}
}

// TestConsensusNonEmptyPreferenceWithLowParticipants tests non-empty preference with low participants scenarios
func TestConsensusNonEmptyPreferenceWithLowParticipants(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	tests := []struct {
		name                    string
		mockResponses           []map[string]interface{}
		availableUpstreams      int
		maxParticipants         int
		agreementThreshold      int
		lowParticipantsBehavior common.ConsensusLowParticipantsBehavior
		expectedError           bool
		expectedResult          interface{}
		description             string
	}{
		{
			name: "low_participants_accept_most_common_prefers_non_empty",
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  map[string]interface{}{}, // Empty object
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x999", // Non-empty result
				},
			},
			availableUpstreams:      2,
			maxParticipants:         4, // More than available
			agreementThreshold:      3, // Higher than available participants (2) to trigger low participants
			lowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptAnyValidResult,
			expectedError:           false,
			expectedResult:          `"0x999"`, // Should prefer non-empty
			description:             "Low participants with accept any valid → prefer non-empty",
		},
		{
			name: "low_participants_return_error",
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  nil, // Empty result
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x888", // Non-empty result
				},
			},
			availableUpstreams:      2,
			maxParticipants:         4, // More than available
			agreementThreshold:      3, // Higher than available participants (2) to trigger low participants
			lowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
			expectedError:           true, // Should return low participants error
			description:             "Low participants with return error → error regardless of empty preference",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Reset gock for each test
			gock.Clean()
			util.SetupMocksForEvmStatePoller()

			// Create upstreams
			upstreams := make([]*common.UpstreamConfig, tc.availableUpstreams)
			for i := 0; i < tc.availableUpstreams; i++ {
				upstreams[i] = &common.UpstreamConfig{
					Id:       fmt.Sprintf("upstream%d", i+1),
					Endpoint: fmt.Sprintf("http://upstream%d.localhost", i+1),
					Type:     common.UpstreamTypeEvm,
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				}
			}

			// Mock responses for each upstream
			for i, upstream := range upstreams {
				if i < len(tc.mockResponses) {
					gock.New(upstream.Endpoint).
						Post("").
						Times(1).
						Reply(200).
						JSON(tc.mockResponses[i])
				}
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			network := setupTestNetworkWithConsensusPolicy(t, ctx, upstreams, &common.ConsensusPolicyConfig{
				MaxParticipants:         tc.maxParticipants,
				AgreementThreshold:      tc.agreementThreshold,
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				LowParticipantsBehavior: tc.lowParticipantsBehavior,
				PunishMisbehavior:       &common.PunishMisbehaviorConfig{},
			})

			// Make request
			req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}`))

			resp, err := network.Forward(ctx, req)

			if tc.expectedError {
				assert.Error(t, err)
				// Check if it's a low participants error
				assert.True(t, common.HasErrorCode(err, common.ErrCodeConsensusLowParticipants))
			} else {
				require.NoError(t, err)
				require.NotNil(t, resp)

				// Verify response content
				jrr, err := resp.JsonRpcResponse()
				require.NoError(t, err)
				assert.Equal(t, tc.expectedResult, string(jrr.Result))
			}

			t.Logf("%s: test completed successfully", tc.description)
		})
	}
}

// TestConsensusInsufficientParticipants tests behavior when maxParticipants > available upstreams
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
		MaxParticipants:         5, // More than available upstreams
		AgreementThreshold:      3, // Higher than available participants (2) to trigger low participants
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
		name               string
		upstreams          []*common.UpstreamConfig
		request            map[string]interface{}
		mockResponses      []map[string]interface{}
		maxParticipants    int
		agreementThreshold int
		ignoreFields       map[string][]string
		expectedSuccess    bool
		expectedResult     string
		expectedError      *common.ErrorCode
		expectedMsg        *string
	}{
		{
			name:               "consensus_achieved_with_ignored_timestamp",
			maxParticipants:    3,
			agreementThreshold: 2,
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
			name:               "consensus_dispute_with_ignored_timestamp_but_different_core_fields",
			maxParticipants:    3,
			agreementThreshold: 2,
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
			name:               "consensus_achieved_with_ignored_multiple_fields",
			maxParticipants:    3,
			agreementThreshold: 3,
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
			name:               "consensus_for_different_method_no_ignore_fields",
			maxParticipants:    3,
			agreementThreshold: 2,
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
			name:               "consensus_method_specific_ignore_fields_dispute",
			maxParticipants:    3,
			agreementThreshold: 2,
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
			name:               "consensus_achieved_when_all_responses_identical",
			maxParticipants:    3,
			agreementThreshold: 2,
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
							Matchers: []*common.MatcherConfig{
								{
									Method: "*",
									Action: common.MatcherInclude,
								},
							},
							Consensus: &common.ConsensusPolicyConfig{
								MaxParticipants:         tt.maxParticipants,
								AgreementThreshold:      tt.agreementThreshold,
								DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
								LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
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
					Filter(func(request *http.Request) bool {
						body := util.SafeReadBody(request)
						// Filter for the specific method in the test request
						if methodVal, ok := tt.request["method"]; ok {
							method := methodVal.(string)
							return strings.Contains(body, method)
						}
						return true
					}).
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

// TestConsensusAcceptMostCommonValidResultScenarios tests AcceptMostCommonValidResult behavior with various scenarios
func TestConsensusAcceptMostCommonValidResultScenarios(t *testing.T) {
	tests := []struct {
		name           string
		mockResponses  []map[string]interface{}
		expectedError  bool
		expectedResult string
		description    string
	}{
		{
			name: "most_common_wins_2v1",
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xaaa",
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xaaa", // Same as first
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xbbb", // Different
				},
			},
			expectedError:  false,
			expectedResult: `"0xaaa"`,
			description:    "2 identical non-empty vs 1 different → returns most common",
		},
		{
			name: "empty_array_wins_2v1",
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  []interface{}{}, // Empty array
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  []interface{}{}, // Empty array
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xccc", // Non-empty
				},
			},
			expectedError:  false,
			expectedResult: `"0xccc"`,
			description:    "2 empty arrays vs 1 non-empty → non-empty preferred",
		},
		{
			name: "tie_2v2_no_clear_winner",
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xaaa",
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xaaa",
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xbbb",
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xbbb",
				},
			},
			expectedError: true,
			description:   "2 of result A, 2 of result B → errors (no clear winner)",
		},
		{
			name: "clear_majority_4v1",
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x123",
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x123",
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x123",
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x123",
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x999",
				},
			},
			expectedError:  false,
			expectedResult: `"0x123"`,
			description:    "4 identical vs 1 different → returns result with count 4",
		},
		{
			name: "clear_majority_3v2_alt",
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xabc",
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xabc",
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xabc",
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xdef",
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xdef",
				},
			},
			expectedError:  false,
			expectedResult: `"0xabc"`,
			description:    "3 identical vs 2 different → returns result with count 3",
		},
		{
			name: "all_different_no_consensus",
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x111",
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x222",
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x333",
				},
			},
			expectedError: true,
			description:   "All different responses → errors (no clear winner)",
		},
		{
			name: "many_empty_few_nonempty_high_threshold",
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  []interface{}{},
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  []interface{}{},
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  []interface{}{},
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  []interface{}{},
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xdata",
				},
			},
			expectedError:  false, // Non-empty preferred
			expectedResult: `"0xdata"`,
			description:    "4 empty, 1 non-empty → non-empty preferred",
		},
		{
			name: "only_empty_results_insufficient_participants",
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  []interface{}{},
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  []interface{}{},
				},
			},
			expectedError: true,
			description:   "Only empty results with participants (2) < threshold (3) → error instead of empty result",
		},
		{
			name: "only_empty_results_meets_threshold",
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  []interface{}{},
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  []interface{}{},
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  []interface{}{},
				},
			},
			expectedError:  false,
			expectedResult: `[]`,
			description:    "Only empty results with participants (3) >= threshold (2) → accept empty result",
		},
		{
			name: "non_empty_winner_ignores_threshold",
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xwinner",
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xwinner",
				},
			},
			expectedError:  false,
			expectedResult: `"0xwinner"`,
			description:    "Non-empty winner with participants (2) < threshold (5) → accept anyway (ignore threshold for non-empty)",
		},
		{
			name: "competing_nonempty_with_empty_results_error",
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  []interface{}{}, // Empty
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  []interface{}{}, // Empty
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  []interface{}{}, // Empty
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xresult1", // Non-empty 1
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xresult2", // Non-empty 2 (competing)
				},
			},
			expectedError: true,
			description:   "2 competing non-empty + 3 empty → error (no clear winner among non-empty)",
		},
		{
			name: "nonempty_agreement_wins_over_minority_and_empty",
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  []interface{}{}, // Empty
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  []interface{}{}, // Empty
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  []interface{}{}, // Empty
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xwinner", // Non-empty winner (2 votes)
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xwinner", // Non-empty winner (2 votes)
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xminority", // Non-empty minority (1 vote)
				},
			},
			expectedError:  false,
			expectedResult: `"0xwinner"`,
			description:    "2 non-empty agreements + 1 different + 3 empty, threshold=3 → accepts most common non-empty",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			util.ResetGock()
			defer func() {
				// Wait a moment before cleanup to avoid race with ongoing requests
				time.Sleep(50 * time.Millisecond)
				util.ResetGock()
			}()
			util.SetupMocksForEvmStatePoller()

			// Create upstreams with fully unique endpoints to guarantee deterministic mock matching
			upstreams := make([]*common.UpstreamConfig, len(tc.mockResponses))
			for i := range tc.mockResponses {
				upstreamId := fmt.Sprintf("upstream%d", i+1)
				// Use completely unique hostnames to eliminate any possibility of cross-contamination
				uniqueHost := fmt.Sprintf("test-upstream-%d-%s.localhost", i+1, tc.name)
				fullEndpoint := fmt.Sprintf("http://%s/rpc", uniqueHost)

				upstreams[i] = &common.UpstreamConfig{
					Id:       upstreamId,
					Endpoint: fullEndpoint,
					Type:     common.UpstreamTypeEvm,
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				}
			}

			// Create highly specific mocks with unique hosts - one mock per upstream
			for i := range upstreams {
				uniqueHost := fmt.Sprintf("test-upstream-%d-%s.localhost", i+1, tc.name)

				gock.New(fmt.Sprintf("http://%s", uniqueHost)).
					Post("/rpc").
					Times(1).
					Reply(200).
					JSON(tc.mockResponses[i])
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			// Create network with AcceptMostCommonValidResult for disputes
			// For tie test, we need higher threshold to force dispute
			threshold := 2
			if tc.name == "tie_2v2_no_clear_winner" {
				threshold = 3 // This ensures neither result meets threshold
			}
			if tc.name == "only_empty_results_insufficient_participants" {
				threshold = 3 // Set threshold higher than participants (2) to test insufficient participants scenario
			}
			if tc.name == "only_empty_results_meets_threshold" {
				threshold = 2 // Set threshold lower than participants (3) to test sufficient participants scenario
			}
			if tc.name == "non_empty_winner_ignores_threshold" {
				threshold = 5 // Set threshold much higher than participants (2) to test ignoring threshold for non-empty
			}
			if tc.name == "nonempty_agreement_wins_over_minority_and_empty" {
				threshold = 3 // Set threshold=3 to test non-empty winner (2 votes) below threshold but still wins
			}
			if tc.name == "clear_majority_3v2_alt" {
				threshold = 3 // Set threshold=3 so only majority (3 votes) meets threshold, minority (2 votes) doesn't
			}

			network := setupTestNetworkWithConsensusPolicy(t, ctx, upstreams, &common.ConsensusPolicyConfig{
				MaxParticipants:    len(upstreams),
				AgreementThreshold: threshold,
				DisputeBehavior:    common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
				PunishMisbehavior:  &common.PunishMisbehaviorConfig{},
			})

			// Make request
			req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}`))
			resp, err := network.Forward(ctx, req)

			if tc.expectedError {
				if err == nil && resp != nil {
					jrr, _ := resp.JsonRpcResponse()
					if jrr != nil {
						t.Errorf("Expected error but got result: %s", string(jrr.Result))
					}
				}
				assert.Error(t, err)
				assert.True(t, common.HasErrorCode(err, common.ErrCodeConsensusDispute, common.ErrCodeConsensusLowParticipants))
			} else {
				require.NoError(t, err)
				require.NotNil(t, resp)

				jrr, err := resp.JsonRpcResponse()
				require.NoError(t, err)
				assert.Equal(t, tc.expectedResult, string(jrr.Result))
			}

			t.Logf("%s: %s", tc.name, tc.description)
		})
	}
}

// TestConsensusAcceptAnyValidResultScenarios tests AcceptAnyValidResult behavior with various scenarios
func TestConsensusAcceptAnyValidResultScenarios(t *testing.T) {
	tests := []struct {
		name           string
		mockResponses  []map[string]interface{}
		hasErrors      bool
		expectedError  bool
		expectedResult interface{} // Can be string or predicate function
		description    string
	}{
		{
			name: "all_empty_returns_consensus",
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  []interface{}{},
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  []interface{}{},
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  []interface{}{},
				},
			},
			expectedError:  false,
			expectedResult: `[]`,
			description:    "All empty arrays → returns empty (primary consensus, not dispute)",
		},
		{
			name: "mix_empty_nonempty_prefers_nonempty",
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  []interface{}{},
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  []interface{}{},
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xpreferred",
				},
			},
			expectedError:  false,
			expectedResult: `"0xpreferred"`,
			description:    "2 empty, 1 non-empty → returns non-empty (AcceptAnyValid allows single non-empty)",
		},
		{
			name: "all_different_nonempty_returns_any",
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xfirst",
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xsecond",
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xthird",
				},
			},
			expectedError: false,
			expectedResult: func(result string) bool {
				// Accept any of the non-empty results
				return result == `"0xfirst"` || result == `"0xsecond"` || result == `"0xthird"`
			},
			description: "All different non-empty → returns first non-empty",
		},
		{
			name: "errors_and_empty_low_participants",
			mockResponses: []map[string]interface{}{
				nil, // Will trigger error
				nil, // Will trigger error
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  []interface{}{},
				},
			},
			hasErrors:     true,
			expectedError: true, // Low participants (only 1 valid response)
			description:   "2 errors, 1 empty → low participants error",
		},
		{
			name: "errors_empty_nonempty_prefers_nonempty",
			mockResponses: []map[string]interface{}{
				nil, // Will trigger error
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  []interface{}{},
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xbest",
				},
			},
			hasErrors:      true,
			expectedError:  false, // AcceptAnyValidResult should select non-empty result
			expectedResult: `"0xbest"`,
			description:    "1 error, 1 empty, 1 non-empty → prefer non-empty",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			util.ResetGock()
			defer util.ResetGock()
			util.SetupMocksForEvmStatePoller()

			// Create upstreams
			upstreams := make([]*common.UpstreamConfig, len(tc.mockResponses))
			for i := range tc.mockResponses {
				upstreams[i] = &common.UpstreamConfig{
					Id:       fmt.Sprintf("upstream%d", i+1),
					Endpoint: fmt.Sprintf("http://upstream%d.localhost", i+1),
					Type:     common.UpstreamTypeEvm,
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				}
			}

			// Mock responses
			for i, upstream := range upstreams {
				if tc.hasErrors && tc.mockResponses[i] == nil {
					// Simulate JSON-RPC error response
					gock.New(upstream.Endpoint).
						Post("").
						Times(1).
						Reply(200). // JSON-RPC errors use 200 status
						JSON(map[string]interface{}{
							"jsonrpc": "2.0",
							"id":      1,
							"error": map[string]interface{}{
								"code":    -32603,
								"message": "Internal Server Error",
							},
						})
				} else {
					gock.New(upstream.Endpoint).
						Post("").
						Times(1).
						Reply(200).
						JSON(tc.mockResponses[i])
				}
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			// Create network with AcceptAnyValidResult for disputes
			network := setupTestNetworkWithConsensusPolicy(t, ctx, upstreams, &common.ConsensusPolicyConfig{
				MaxParticipants:    len(upstreams),
				AgreementThreshold: 2,
				DisputeBehavior:    common.ConsensusDisputeBehaviorAcceptAnyValidResult,
				PunishMisbehavior:  &common.PunishMisbehaviorConfig{},
			})

			// Make request
			req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}`))
			resp, err := network.Forward(ctx, req)

			if tc.expectedError {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
				require.NotNil(t, resp)

				jrr, err := resp.JsonRpcResponse()
				require.NoError(t, err)
				result := string(jrr.Result)

				// Handle both string and function expectations
				switch expected := tc.expectedResult.(type) {
				case string:
					assert.Equal(t, expected, result)
				case func(string) bool:
					assert.True(t, expected(result), "Result %s did not match predicate", result)
				}
			}

			t.Logf("%s: %s", tc.name, tc.description)
		})
	}
}

// TestConsensusDisputeBehaviorComparison tests different dispute behaviors with the same scenario
func TestConsensusDisputeBehaviorComparison(t *testing.T) {
	// Same dispute scenario: 3 different responses (no consensus possible)
	disputeResponses := []map[string]interface{}{
		{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  "0xaaa",
		},
		{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  "0xbbb",
		},
		{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  []interface{}{}, // Empty array
		},
	}

	tests := []struct {
		name            string
		disputeBehavior common.ConsensusDisputeBehavior
		expectedError   bool
		expectedResult  interface{} // Can be string or predicate function
		description     string
	}{
		{
			name:            "return_error_behavior",
			disputeBehavior: common.ConsensusDisputeBehaviorReturnError,
			expectedError:   true,
			description:     "ReturnError → always returns error on dispute",
		},
		{
			name:            "accept_most_common_no_winner",
			disputeBehavior: common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
			expectedError:   true,
			description:     "AcceptMostCommon → errors when no clear winner (all count 1)",
		},
		{
			name:            "accept_any_valid_prefers_nonempty",
			disputeBehavior: common.ConsensusDisputeBehaviorAcceptAnyValidResult,
			expectedError:   false,
			expectedResult: func(result string) bool {
				// Should return one of the non-empty results
				return result == `"0xaaa"` || result == `"0xbbb"`
			},
			description: "AcceptAnyValid → returns any valid result (prefers non-empty)",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			util.ResetGock()
			defer util.ResetGock()
			util.SetupMocksForEvmStatePoller()

			// Create 3 upstreams
			upstreams := make([]*common.UpstreamConfig, 3)
			for i := range upstreams {
				upstreams[i] = &common.UpstreamConfig{
					Id:       fmt.Sprintf("upstream%d", i+1),
					Endpoint: fmt.Sprintf("http://upstream%d.localhost", i+1),
					Type:     common.UpstreamTypeEvm,
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				}
			}

			// Mock the same dispute responses for all tests
			for i, upstream := range upstreams {
				gock.New(upstream.Endpoint).
					Post("").
					Times(1).
					Reply(200).
					JSON(disputeResponses[i])
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			// Create network with specific dispute behavior
			network := setupTestNetworkWithConsensusPolicy(t, ctx, upstreams, &common.ConsensusPolicyConfig{
				MaxParticipants:    3,
				AgreementThreshold: 2,
				DisputeBehavior:    tc.disputeBehavior,
				PunishMisbehavior:  &common.PunishMisbehaviorConfig{},
			})

			// Make request
			req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}`))
			resp, err := network.Forward(ctx, req)

			if tc.expectedError {
				assert.Error(t, err)
				assert.True(t, common.HasErrorCode(err, common.ErrCodeConsensusDispute))
			} else {
				require.NoError(t, err)
				require.NotNil(t, resp)

				jrr, err := resp.JsonRpcResponse()
				require.NoError(t, err)
				result := string(jrr.Result)

				// Handle both string and function expectations
				switch expected := tc.expectedResult.(type) {
				case string:
					assert.Equal(t, expected, result)
				case func(string) bool:
					assert.True(t, expected(result), "Result %s did not match predicate", result)
				}
			}

			t.Logf("%s: %s", tc.name, tc.description)
		})
	}
}

// TestConsensusLowParticipantsBehaviorComparison tests different low participants behaviors
func TestConsensusLowParticipantsBehaviorComparison(t *testing.T) {
	// Low participants scenario: Required 4, but only 2 respond
	lowParticipantResponses := []map[string]interface{}{
		{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  "0xresponse1",
		},
		{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  []interface{}{}, // Empty array
		},
	}

	tests := []struct {
		name                    string
		lowParticipantsBehavior common.ConsensusLowParticipantsBehavior
		expectedError           bool
		expectedErrorCode       *common.ErrorCode
		expectedResult          interface{} // Can be string or predicate function
		description             string
	}{
		{
			name:                    "return_error_behavior",
			lowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
			expectedError:           true,
			expectedErrorCode:       pointer(common.ErrCodeConsensusLowParticipants),
			description:             "ReturnError → returns error on low participants",
		},
		{
			name:                    "accept_most_common_with_data",
			lowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
			expectedError:           true, // Tie between two results → no clear winner
			expectedErrorCode:       pointer(common.ErrCodeConsensusLowParticipants),
			description:             "AcceptMostCommon → errors when no clear winner (tie: all count 1)",
		},
		{
			name:                    "accept_any_valid_prefers_nonempty",
			lowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptAnyValidResult,
			expectedError:           false,
			expectedResult:          `"0xresponse1"`, // Should prefer non-empty
			description:             "AcceptAnyValid → returns non-empty result",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			util.ResetGock()
			defer util.ResetGock()
			util.SetupMocksForEvmStatePoller()

			// Create 4 upstreams (but only 2 will respond)
			upstreams := make([]*common.UpstreamConfig, 4)
			for i := range upstreams {
				upstreams[i] = &common.UpstreamConfig{
					Id:       fmt.Sprintf("upstream%d", i+1),
					Endpoint: fmt.Sprintf("http://upstream%d.localhost", i+1),
					Type:     common.UpstreamTypeEvm,
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				}
			}

			// Mock only 2 responses (simulating low participants)
			for i := 0; i < 2; i++ {
				gock.New(upstreams[i].Endpoint).
					Post("").
					Times(1).
					Reply(200).
					JSON(lowParticipantResponses[i])
			}

			// Other upstreams timeout
			for i := 2; i < 4; i++ {
				gock.New(upstreams[i].Endpoint).
					Post("").
					Times(1).
					Reply(500).
					BodyString("timeout")
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			// Create network with specific low participants behavior
			network := setupTestNetworkWithConsensusPolicy(t, ctx, upstreams, &common.ConsensusPolicyConfig{
				MaxParticipants:         4, // Require 4 but only 2 will respond
				AgreementThreshold:      3, // Higher than participant count (2) to trigger low participants
				LowParticipantsBehavior: tc.lowParticipantsBehavior,
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				PunishMisbehavior:       &common.PunishMisbehaviorConfig{},
			})

			// Make request
			req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}`))
			resp, err := network.Forward(ctx, req)

			if tc.expectedError {
				assert.Error(t, err)
				if tc.expectedErrorCode != nil {
					assert.True(t, common.HasErrorCode(err, *tc.expectedErrorCode))
				}
			} else {
				require.NoError(t, err)
				require.NotNil(t, resp)

				jrr, err := resp.JsonRpcResponse()
				require.NoError(t, err)
				result := string(jrr.Result)

				// Handle both string and function expectations
				switch expected := tc.expectedResult.(type) {
				case string:
					assert.Equal(t, expected, result)
				case func(string) bool:
					assert.True(t, expected(result), "Result %s did not match predicate", result)
				}
			}

			t.Logf("%s: %s", tc.name, tc.description)
		})
	}
}

// TestConsensusThresholdEdgeCases tests edge cases around agreement threshold
func TestConsensusThresholdEdgeCases(t *testing.T) {
	tests := []struct {
		name               string
		maxParticipants    int
		agreementThreshold int
		mockResponses      []map[string]interface{}
		expectedError      bool
		expectedErrorCode  *common.ErrorCode
		expectedResult     string
		description        string
	}{
		{
			name:               "threshold_1_single_response",
			maxParticipants:    1,
			agreementThreshold: 1,
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xsingle",
				},
			},
			expectedError:  false,
			expectedResult: `"0xsingle"`,
			description:    "Threshold 1, 1 response → consensus achieved",
		},
		{
			name:               "threshold_2_single_response_dispute",
			maxParticipants:    2,
			agreementThreshold: 2,
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xonly",
				},
				nil, // Second upstream fails
			},
			expectedError:     true,
			expectedErrorCode: pointer(common.ErrCodeConsensusLowParticipants),
			description:       "Threshold 2, 1 response → low participants",
		},
		{
			name:               "threshold_2_two_identical",
			maxParticipants:    2,
			agreementThreshold: 2,
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xmatching",
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xmatching",
				},
			},
			expectedError:  false,
			expectedResult: `"0xmatching"`,
			description:    "Threshold 2, 2 identical → consensus achieved",
		},
		{
			name:               "threshold_3_two_identical_dispute",
			maxParticipants:    3,
			agreementThreshold: 3,
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xpair",
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xpair",
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xdifferent",
				},
			},
			expectedError: true,
			// Don't check specific error code - could be dispute or other depending on logic
			description: "Threshold 3, 2 identical → error (below threshold)",
		},
		{
			name:               "threshold_2_with_empty_consensus",
			maxParticipants:    3,
			agreementThreshold: 2,
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  []interface{}{},
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  []interface{}{},
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xnonEmpty",
				},
			},
			expectedError:     true,
			expectedErrorCode: pointer(common.ErrCodeConsensusDispute),
			description:       "Threshold 2, 2 empty arrays vs 1 non-empty → dispute (non-empty preference)",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			util.ResetGock()
			defer util.ResetGock()
			util.SetupMocksForEvmStatePoller()

			// Create upstreams
			upstreams := make([]*common.UpstreamConfig, tc.maxParticipants)
			for i := range upstreams {
				upstreams[i] = &common.UpstreamConfig{
					Id:       fmt.Sprintf("upstream%d", i+1),
					Endpoint: fmt.Sprintf("http://upstream%d.localhost", i+1),
					Type:     common.UpstreamTypeEvm,
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				}
			}

			// Mock responses
			for i, upstream := range upstreams {
				if i < len(tc.mockResponses) && tc.mockResponses[i] != nil {
					gock.New(upstream.Endpoint).
						Post("").
						Times(1).
						Reply(200).
						JSON(tc.mockResponses[i])
				} else {
					// Simulate failure
					gock.New(upstream.Endpoint).
						Post("").
						Times(1).
						Reply(500).
						BodyString("error")
				}
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			// Create network
			network := setupTestNetworkWithConsensusPolicy(t, ctx, upstreams, &common.ConsensusPolicyConfig{
				MaxParticipants:         tc.maxParticipants,
				AgreementThreshold:      tc.agreementThreshold,
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
				PunishMisbehavior:       &common.PunishMisbehaviorConfig{},
			})

			// Make request
			req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}`))
			resp, err := network.Forward(ctx, req)

			if tc.expectedError {
				assert.Error(t, err)
				if tc.expectedErrorCode != nil {
					assert.True(t, common.HasErrorCode(err, *tc.expectedErrorCode))
				}
			} else {
				require.NoError(t, err)
				require.NotNil(t, resp)

				jrr, err := resp.JsonRpcResponse()
				require.NoError(t, err)
				assert.Equal(t, tc.expectedResult, string(jrr.Result))
			}

			t.Logf("%s: %s", tc.name, tc.description)
		})
	}
}

// TestConsensusEmptyNonEmptyPreferenceBehaviors tests empty/non-empty preference with different behaviors
func TestConsensusEmptyNonEmptyPreferenceBehaviors(t *testing.T) {
	// Same scenario: 3 empty, 1 non-empty with threshold 2
	testResponses := []map[string]interface{}{
		{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  []interface{}{},
		},
		{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  []interface{}{},
		},
		{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  []interface{}{},
		},
		{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  "0xvaluable",
		},
	}

	tests := []struct {
		name            string
		disputeBehavior common.ConsensusDisputeBehavior
		expectedError   bool
		expectedResult  string
		description     string
	}{
		{
			name:            "accept_most_common_accepts_clear_nonempty",
			disputeBehavior: common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
			expectedError:   false,
			expectedResult:  `"0xvaluable"`,
			description:     "AcceptMostCommon → accepts clear non-empty winner (ignores threshold)",
		},
		{
			name:            "accept_any_valid_returns_nonempty",
			disputeBehavior: common.ConsensusDisputeBehaviorAcceptAnyValidResult,
			expectedError:   false,
			expectedResult:  `"0xvaluable"`,
			description:     "AcceptAnyValid → returns non-empty (ignores threshold)",
		},
		{
			name:            "return_error_always_errors",
			disputeBehavior: common.ConsensusDisputeBehaviorReturnError,
			expectedError:   true,
			description:     "ReturnError → always errors on dispute",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			util.ResetGock()
			defer util.ResetGock()
			util.SetupMocksForEvmStatePoller()

			// Create 4 upstreams
			upstreams := make([]*common.UpstreamConfig, 4)
			for i := range upstreams {
				upstreams[i] = &common.UpstreamConfig{
					Id:       fmt.Sprintf("upstream%d", i+1),
					Endpoint: fmt.Sprintf("http://upstream%d.localhost", i+1),
					Type:     common.UpstreamTypeEvm,
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				}
			}

			// Mock responses
			for i, upstream := range upstreams {
				gock.New(upstream.Endpoint).
					Post("").
					Times(1).
					Reply(200).
					JSON(testResponses[i])
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			// Create network
			network := setupTestNetworkWithConsensusPolicy(t, ctx, upstreams, &common.ConsensusPolicyConfig{
				MaxParticipants:    4,
				AgreementThreshold: 2,
				DisputeBehavior:    tc.disputeBehavior,
				PunishMisbehavior:  &common.PunishMisbehaviorConfig{},
			})

			// Make request
			req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}`))
			resp, err := network.Forward(ctx, req)

			if tc.expectedError {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
				require.NotNil(t, resp)

				jrr, err := resp.JsonRpcResponse()
				require.NoError(t, err)
				assert.Equal(t, tc.expectedResult, string(jrr.Result))
			}

			t.Logf("%s: %s", tc.name, tc.description)
		})
	}
}

// TestConsensusComplexRealWorldScenarios tests complex real-world consensus scenarios
func TestConsensusComplexRealWorldScenarios(t *testing.T) {
	tests := []struct {
		name                    string
		maxParticipants         int
		agreementThreshold      int
		disputeBehavior         common.ConsensusDisputeBehavior
		lowParticipantsBehavior common.ConsensusLowParticipantsBehavior
		mockResponses           []map[string]interface{}
		responseDelays          []time.Duration // Delays for each response
		expectedError           bool
		expectedResult          interface{} // Can be string or predicate function
		description             string
	}{
		{
			name:                    "low_participants_accept_any_mixed",
			maxParticipants:         5,
			agreementThreshold:      3,
			disputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
			lowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptAnyValidResult,
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  []interface{}{}, // Empty
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xdata", // Non-empty
				},
				nil, // Error
				nil, // Error
				nil, // Error
			},
			expectedError:  false,
			expectedResult: `"0xdata"`, // AcceptAnyValid prefers non-empty
			description:    "Low participants + AcceptAnyValid → prefers non-empty over empty",
		},
		{
			name:                    "dispute_most_common_tie_empty_nonempty",
			maxParticipants:         4,
			agreementThreshold:      3, // Forces dispute
			disputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
			lowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  []interface{}{},
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  []interface{}{},
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xvalue",
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xvalue",
				},
			},
			expectedError: true, // Tie between empty (2) and non-empty (2)
			description:   "Dispute + AcceptMostCommon + tie → error",
		},
		{
			name:                    "short_circuit_prevention_late_nonempty",
			maxParticipants:         3,
			agreementThreshold:      2,
			disputeBehavior:         common.ConsensusDisputeBehaviorAcceptAnyValidResult,
			lowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  []interface{}{},
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  []interface{}{},
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xlate", // Late non-empty response
				},
			},
			responseDelays: []time.Duration{
				0,
				0,
				200 * time.Millisecond, // Delayed response
			},
			expectedError:  false,
			expectedResult: `"0xlate"`, // Should wait and prefer non-empty
			description:    "Short-circuit prevention waits for late non-empty response",
		},
		{
			name:                    "complex_mixed_scenario",
			maxParticipants:         6,
			agreementThreshold:      3,
			disputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
			lowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptAnyValidResult,
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xcommon",
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xcommon",
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xcommon",
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  []interface{}{},
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xdifferent",
				},
				nil, // Error
			},
			expectedError:  false,
			expectedResult: `"0xcommon"`, // Clear winner with count 3
			description:    "Complex scenario with clear consensus despite mixed responses",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			util.ResetGock()
			defer util.ResetGock()
			util.SetupMocksForEvmStatePoller()

			// Create upstreams
			upstreams := make([]*common.UpstreamConfig, tc.maxParticipants)
			for i := range upstreams {
				upstreams[i] = &common.UpstreamConfig{
					Id:       fmt.Sprintf("upstream%d", i+1),
					Endpoint: fmt.Sprintf("http://upstream%d.localhost", i+1),
					Type:     common.UpstreamTypeEvm,
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				}
			}

			// Mock responses with delays
			for i, upstream := range upstreams {
				if i < len(tc.mockResponses) {
					if tc.mockResponses[i] == nil {
						// Error response
						gock.New(upstream.Endpoint).
							Post("").
							Times(1).
							Reply(500).
							BodyString("error")
					} else {
						mock := gock.New(upstream.Endpoint).
							Post("").
							Times(1).
							Reply(200)

						// Add delay if specified
						if tc.responseDelays != nil && i < len(tc.responseDelays) && tc.responseDelays[i] > 0 {
							mock = mock.Delay(tc.responseDelays[i])
						}

						mock.JSON(tc.mockResponses[i])
					}
				}
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			// Create network
			network := setupTestNetworkWithConsensusPolicy(t, ctx, upstreams, &common.ConsensusPolicyConfig{
				MaxParticipants:         tc.maxParticipants,
				AgreementThreshold:      tc.agreementThreshold,
				DisputeBehavior:         tc.disputeBehavior,
				LowParticipantsBehavior: tc.lowParticipantsBehavior,
				PunishMisbehavior:       &common.PunishMisbehaviorConfig{},
			})

			// Make request
			req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}`))
			resp, err := network.Forward(ctx, req)

			if tc.expectedError {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
				require.NotNil(t, resp)

				jrr, err := resp.JsonRpcResponse()
				require.NoError(t, err)
				result := string(jrr.Result)

				// Handle both string and function expectations
				switch expected := tc.expectedResult.(type) {
				case string:
					assert.Equal(t, expected, result)
				case func(string) bool:
					assert.True(t, expected(result), "Result %s did not match predicate", result)
				}
			}

			t.Logf("%s: %s", tc.name, tc.description)
		})
	}
}

// TestConsensusMixedErrorScenarios tests consensus with various error types
func TestConsensusMixedErrorScenarios(t *testing.T) {
	tests := []struct {
		name           string
		mockResponses  []interface{} // Can be map for success or error config
		expectedError  bool
		expectedResult interface{} // Can be string or error matcher function
		description    string
	}{
		{
			name: "consensus_on_same_execution_error",
			mockResponses: []interface{}{
				map[string]interface{}{
					"error": map[string]interface{}{
						"code":    -32000,
						"message": "execution reverted: insufficient balance",
					},
				},
				map[string]interface{}{
					"error": map[string]interface{}{
						"code":    -32000,
						"message": "execution reverted: insufficient balance",
					},
				},
				map[string]interface{}{
					"error": map[string]interface{}{
						"code":    -32000,
						"message": "execution reverted: insufficient balance",
					},
				},
			},
			expectedError: true,
			expectedResult: func(err error) bool {
				if err == nil {
					return false
				}
				// Should return the agreed-upon execution error
				return strings.Contains(err.Error(), "execution reverted") &&
					strings.Contains(err.Error(), "insufficient balance")
			},
			description: "3 identical execution errors → consensus on error",
		},
		{
			name: "different_execution_errors_return_first_error",
			mockResponses: []interface{}{
				map[string]interface{}{
					"error": map[string]interface{}{
						"code":    -32000,
						"message": "execution reverted: reason A",
					},
				},
				map[string]interface{}{
					"error": map[string]interface{}{
						"code":    -32000,
						"message": "execution reverted: reason B",
					},
				},
				map[string]interface{}{
					"error": map[string]interface{}{
						"code":    -32000,
						"message": "execution reverted: reason C",
					},
				},
			},
			expectedError: true,
			expectedResult: func(err error) bool {
				if err == nil {
					return false
				}
				// With ReturnError dispute behavior, it returns one of the execution errors
				return strings.Contains(err.Error(), "execution reverted") &&
					(strings.Contains(err.Error(), "reason A") ||
						strings.Contains(err.Error(), "reason B") ||
						strings.Contains(err.Error(), "reason C"))
			},
			description: "3 different execution errors → returns first error (ReturnError behavior)",
		},
		{
			name: "success_wins_over_fewer_execution_errors",
			mockResponses: []interface{}{
				map[string]interface{}{
					"error": map[string]interface{}{
						"code":    3,
						"message": "execution reverted: token paused",
					},
				},
				map[string]interface{}{
					"error": map[string]interface{}{
						"code":    3,
						"message": "execution reverted: token paused",
					},
				},
				map[string]interface{}{
					"result": "0x123", // Success response
				},
			},
			expectedError:  false, // Success response wins due to non-empty preference
			expectedResult: `"0x123"`,
			description:    "2 same execution errors, 1 success → success wins (non-empty preference)",
		},
		{
			name: "success_wins_over_execution_errors_non_empty_preference",
			mockResponses: []interface{}{
				map[string]interface{}{
					"error": map[string]interface{}{
						"code":    -32000, // Use standard execution error code
						"message": "execution reverted: out of gas",
					},
				},
				map[string]interface{}{
					"error": map[string]interface{}{
						"code":    -32000,
						"message": "execution reverted: out of gas",
					},
				},
				map[string]interface{}{
					"error": map[string]interface{}{
						"code":    -32000,
						"message": "execution reverted: out of gas",
					},
				},
				map[string]interface{}{
					"result": "0x456", // Success response wins due to non-empty preference
				},
			},
			expectedError:  false, // Success response wins due to non-empty preference
			expectedResult: `"0x456"`,
			description:    "3 same execution errors, 1 success → success wins (non-empty preference)",
		},
		{
			name: "mix_network_and_execution_errors",
			mockResponses: []interface{}{
				"network_error",
				map[string]interface{}{
					"error": map[string]interface{}{
						"code":    3,
						"message": "execution reverted: out of gas",
					},
				},
				map[string]interface{}{
					"error": map[string]interface{}{
						"code":    3,
						"message": "execution reverted: out of gas",
					},
				},
			},
			expectedError: true,
			expectedResult: func(err error) bool {
				// Execution errors should form consensus
				return strings.Contains(err.Error(), "execution reverted") &&
					strings.Contains(err.Error(), "out of gas")
			},
			description: "1 network error, 2 same execution errors → consensus on execution error",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			util.ResetGock()
			defer util.ResetGock()
			util.SetupMocksForEvmStatePoller()

			// Create upstreams with unique endpoints from the start to avoid race conditions
			upstreams := make([]*common.UpstreamConfig, len(tc.mockResponses))
			for i := range upstreams {
				upstreamId := fmt.Sprintf("upstream%d", i+1)
				baseUrl := fmt.Sprintf("http://upstream%d.localhost", i+1)
				uniquePath := fmt.Sprintf("/%s", upstreamId)
				fullEndpoint := baseUrl + uniquePath

				upstreams[i] = &common.UpstreamConfig{
					Id:       upstreamId,
					Endpoint: fullEndpoint, // Use unique endpoint from creation
					Type:     common.UpstreamTypeEvm,
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				}
			}

			// Mock responses with unique paths
			for i, upstream := range upstreams {
				upstreamId := upstream.Id
				baseUrl := fmt.Sprintf("http://upstream%d.localhost", i+1)
				uniquePath := fmt.Sprintf("/%s", upstreamId)

				switch resp := tc.mockResponses[i].(type) {
				case string:
					if resp == "network_error" {
						// Simulate network error
						gock.New(baseUrl).
							Post(uniquePath).
							Times(1). // Use Times(1) with unique paths for deterministic behavior
							ReplyError(fmt.Errorf("network timeout"))
					}
				case map[string]interface{}:
					// Normal JSON-RPC response (success or error)
					response := map[string]interface{}{
						"jsonrpc": "2.0",
						"id":      1,
					}
					// Copy fields from test response
					for k, v := range resp {
						response[k] = v
					}
					gock.New(baseUrl).
						Post(uniquePath).
						Times(1). // Use Times(1) with unique paths for deterministic behavior
						Reply(200).
						JSON(response)
				}
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			// Create network with consensus
			network := setupTestNetworkWithConsensusPolicy(t, ctx, upstreams, &common.ConsensusPolicyConfig{
				MaxParticipants:    len(upstreams),
				AgreementThreshold: 2,
				DisputeBehavior:    common.ConsensusDisputeBehaviorReturnError,
				PunishMisbehavior:  &common.PunishMisbehaviorConfig{},
			})

			// Make request
			req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_call","params":[],"id":1}`))
			resp, err := network.Forward(ctx, req)

			if tc.expectedError {
				assert.Error(t, err)
				// Check error matches expectation
				switch expected := tc.expectedResult.(type) {
				case func(error) bool:
					assert.True(t, expected(err), "Error %v did not match predicate", err)
				}
			} else {
				require.NoError(t, err)
				require.NotNil(t, resp)

				jrr, err := resp.JsonRpcResponse()
				require.NoError(t, err)
				assert.Equal(t, tc.expectedResult, string(jrr.Result))
			}

			t.Logf("%s: %s", tc.name, tc.description)
		})
	}
}

// TestConsensusDisputeBehaviorWithMinorityNonEmpty tests specific scenarios where non-empty responses are minority
func TestConsensusDisputeBehaviorWithMinorityNonEmpty(t *testing.T) {
	tests := []struct {
		name            string
		mockResponses   []interface{}
		disputeBehavior common.ConsensusDisputeBehavior
		expectedError   bool
		expectedResult  interface{} // Can be string or error matcher function
		description     string
	}{

		{
			name: "accept_any_valid_prefers_single_success_over_execution_errors",
			mockResponses: []interface{}{
				map[string]interface{}{
					"error": map[string]interface{}{
						"code":    -32000,
						"message": "execution reverted: insufficient balance",
					},
				},
				map[string]interface{}{
					"error": map[string]interface{}{
						"code":    -32000,
						"message": "execution reverted: insufficient balance",
					},
				},
				map[string]interface{}{
					"result": "0x456",
				},
			},
			disputeBehavior: common.ConsensusDisputeBehaviorAcceptAnyValidResult,
			expectedError:   false,
			expectedResult:  `"0x456"`,
			description:     "1 valid non-empty response, 2 execution errors + acceptAnyValidResult → returns non-empty",
		},
		{
			name: "accept_most_common_returns_success_vs_execution_errors",
			mockResponses: []interface{}{
				map[string]interface{}{
					"error": map[string]interface{}{
						"code":    -32000,
						"message": "execution reverted: insufficient balance",
					},
				},
				map[string]interface{}{
					"error": map[string]interface{}{
						"code":    -32000,
						"message": "execution reverted: insufficient balance",
					},
				},
				map[string]interface{}{
					"result": "0x456",
				},
			},
			disputeBehavior: common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
			expectedError:   false, // Success response wins due to non-empty preference
			expectedResult:  `"0x456"`,
			description:     "1 valid non-empty response, 2 execution errors + acceptMostCommonValidResult → returns non-empty (non-empty preference)",
		},
		{
			name: "accept_most_common_errors_with_different_non_empty_responses",
			mockResponses: []interface{}{
				map[string]interface{}{
					"result": "0x111",
				},
				map[string]interface{}{
					"result": "0x222",
				},
				map[string]interface{}{
					"result": "0x333",
				},
			},
			disputeBehavior: common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
			expectedError:   true, // All different non-empty responses, no clear winner
			expectedResult: func(err error) bool {
				if err == nil {
					return false
				}
				// Should be a dispute error because no clear most common result
				return common.HasErrorCode(err, common.ErrCodeConsensusDispute)
			},
			description: "3 different non-empty responses + acceptMostCommonValidResult → error (no clear winner)",
		},
		{
			name: "accept_any_valid_rejects_empty_with_missing_data_errors",
			mockResponses: []interface{}{
				map[string]interface{}{
					"result": []interface{}{}, // Empty array response
				},
				map[string]interface{}{
					"error": map[string]interface{}{
						"code":    -32602, // Missing data error
						"message": "missing trie node",
					},
				},
				map[string]interface{}{
					"error": map[string]interface{}{
						"code":    -32602, // Missing data error
						"message": "missing trie node",
					},
				},
			},
			disputeBehavior: common.ConsensusDisputeBehaviorAcceptAnyValidResult,
			expectedError:   false, // AcceptAnyValidResult accepts the empty array as a valid result
			expectedResult:  "[]",  // Empty array result
			description:     "1 empty response, 2 missing data errors + acceptAnyValidResult → empty array accepted",
		},
		{
			name: "accept_most_common_empty_consensus_only_empty_responses",
			mockResponses: []interface{}{
				map[string]interface{}{
					"result": []interface{}{}, // Empty array response
				},
				map[string]interface{}{
					"result": []interface{}{}, // Empty array response
				},
			},
			disputeBehavior: common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
			expectedError:   false, // Empty responses achieve consensus (2 empty, threshold 2)
			expectedResult:  `[]`,
			description:     "2 empty responses + acceptMostCommonValidResult → returns empty (consensus)",
		},
		{
			name: "accept_any_valid_empty_consensus_only_empty_responses",
			mockResponses: []interface{}{
				map[string]interface{}{
					"result": []interface{}{}, // Empty array response
				},
				map[string]interface{}{
					"result": []interface{}{}, // Empty array response
				},
			},
			disputeBehavior: common.ConsensusDisputeBehaviorAcceptAnyValidResult,
			expectedError:   false, // Empty responses achieve consensus (2 empty, threshold 2)
			expectedResult:  `[]`,
			description:     "2 empty responses + acceptAnyValidResult → returns empty (consensus)",
		},
		{
			name: "accept_any_valid_returns_consensus_error_correctly",
			mockResponses: []interface{}{
				map[string]interface{}{
					"error": map[string]interface{}{
						"code":    -32000,
						"message": "execution reverted: insufficient balance",
					},
				},
				map[string]interface{}{
					"error": map[string]interface{}{
						"code":    -32000,
						"message": "execution reverted: insufficient balance",
					},
				},
			},
			disputeBehavior: common.ConsensusDisputeBehaviorAcceptAnyValidResult,
			expectedError:   true, // Should return the consensus-valid error
			expectedResult: func(err error) bool {
				if err == nil {
					return false
				}
				// Should be the execution error, not a dispute error
				return strings.Contains(err.Error(), "execution reverted: insufficient balance")
			},
			description: "2 consensus-valid errors + acceptAnyValidResult → returns consensus error (not dispute)",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			util.ResetGock()
			defer util.ResetGock()
			util.SetupMocksForEvmStatePoller()

			// Create upstreams
			upstreams := make([]*common.UpstreamConfig, len(tc.mockResponses))
			for i := range upstreams {
				upstreams[i] = &common.UpstreamConfig{
					Id:       fmt.Sprintf("upstream%d", i+1),
					Endpoint: fmt.Sprintf("http://upstream%d.localhost", i+1),
					Type:     common.UpstreamTypeEvm,
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				}
			}

			// Mock responses
			for i, upstream := range upstreams {
				switch resp := tc.mockResponses[i].(type) {
				case string:
					if resp == "network_error" {
						// Simulate network error
						gock.New(upstream.Endpoint).
							Post("").
							Times(1).
							ReplyError(fmt.Errorf("network timeout"))
					}
				case map[string]interface{}:
					// Normal JSON-RPC response (success or error)
					response := map[string]interface{}{
						"jsonrpc": "2.0",
						"id":      1,
					}
					// Copy fields from test response
					for k, v := range resp {
						response[k] = v
					}
					gock.New(upstream.Endpoint).
						Post("").
						Times(1).
						Reply(200).
						JSON(response)
				}
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			// Create network with the specific dispute behavior
			network := setupTestNetworkWithConsensusPolicy(t, ctx, upstreams, &common.ConsensusPolicyConfig{
				MaxParticipants:    len(upstreams),
				AgreementThreshold: 2,
				DisputeBehavior:    tc.disputeBehavior,
				PunishMisbehavior:  &common.PunishMisbehaviorConfig{},
			})

			// Make request
			req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_call","params":[],"id":1}`))
			resp, err := network.Forward(ctx, req)

			if tc.expectedError {
				assert.Error(t, err)
				// Check error matches expectation
				switch expected := tc.expectedResult.(type) {
				case func(error) bool:
					assert.True(t, expected(err), "Error %v did not match predicate", err)
				}
			} else {
				require.NoError(t, err)
				require.NotNil(t, resp)

				jrr, err := resp.JsonRpcResponse()
				require.NoError(t, err)
				assert.Equal(t, tc.expectedResult, string(jrr.Result))
			}

			t.Logf("%s: %s", tc.name, tc.description)
		})
	}
}

// TestConsensusParticipantRequirements tests basic participant requirement scenarios
// Note: Complex timeout scenarios are covered by existing integration tests
func TestConsensusParticipantRequirements(t *testing.T) {
	tests := []struct {
		name               string
		upstreams          int
		maxParticipants    int
		agreementThreshold int
		mockResponses      []string
		expectedError      bool
		expectedErrorCode  *common.ErrorCode
		expectedResult     string
		description        string
	}{
		{
			name:               "normal_consensus_achieved",
			upstreams:          3,
			maxParticipants:    3,
			agreementThreshold: 2,
			mockResponses:      []string{"0xaaa", "0xaaa", "0xbbb"},
			expectedError:      false,
			expectedResult:     `"0xaaa"`,
			description:        "Normal consensus: 3 participants, 2 agree → consensus",
		},
		{
			name:               "low_threshold_single_response",
			upstreams:          3,
			maxParticipants:    1,
			agreementThreshold: 1,
			mockResponses:      []string{"0xsame", "0xsame", "0xsame"},
			expectedError:      false,
			expectedResult:     `"0xsame"`, // All same response, threshold 1
			description:        "Low threshold: 1 required, threshold 1 → consensus achieved",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			util.ResetGock()
			defer util.ResetGock()
			util.SetupMocksForEvmStatePoller()

			// Create upstreams
			upstreams := make([]*common.UpstreamConfig, tc.upstreams)
			for i := 0; i < tc.upstreams; i++ {
				upstreams[i] = &common.UpstreamConfig{
					Id:       fmt.Sprintf("upstream%d", i+1),
					Endpoint: fmt.Sprintf("http://upstream%d.localhost", i+1),
					Type:     common.UpstreamTypeEvm,
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				}

				// Mock successful responses for all upstreams
				gock.New(upstreams[i].Endpoint).
					Post("").
					Times(1).
					Reply(200).
					JSON(map[string]interface{}{
						"jsonrpc": "2.0",
						"id":      1,
						"result":  tc.mockResponses[i],
					})
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			// Create network
			network := setupTestNetworkWithConsensusPolicy(t, ctx, upstreams, &common.ConsensusPolicyConfig{
				MaxParticipants:         tc.maxParticipants,
				AgreementThreshold:      tc.agreementThreshold,
				DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
				PunishMisbehavior:       &common.PunishMisbehaviorConfig{},
			})

			// Make request
			req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}`))
			resp, err := network.Forward(ctx, req)

			if tc.expectedError {
				assert.Error(t, err)
				if tc.expectedErrorCode != nil {
					assert.True(t, common.HasErrorCode(err, *tc.expectedErrorCode),
						"Expected error code %v but got: %v", *tc.expectedErrorCode, err)
				}
			} else {
				require.NoError(t, err)
				require.NotNil(t, resp)

				jrr, err := resp.JsonRpcResponse()
				require.NoError(t, err)
				assert.Equal(t, tc.expectedResult, string(jrr.Result))
			}

			t.Logf("%s: %s", tc.name, tc.description)
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
		name              string
		upstreams         []*common.UpstreamConfig
		request           map[string]interface{}
		mockResponses     []map[string]interface{}
		maxParticipants   int
		expectedError     bool
		expectedErrorCode *int // Expected JSON-RPC error code
		expectedErrorMsg  string
	}{
		{
			name:            "consensus_on_invalid_params_error",
			maxParticipants: 3,
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
			name:            "consensus_on_method_not_found",
			maxParticipants: 3,
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
			name:            "consensus_on_missing_data",
			maxParticipants: 3,
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
			name:            "consensus_on_execution_reverted",
			maxParticipants: 3,
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
			name:            "mixed_errors_returns_dispute",
			maxParticipants: 3,
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
			name:            "majority_agreement_on_error_with_one_success",
			maxParticipants: 3,
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
							Matchers: []*common.MatcherConfig{
								{
									Method: "*",
									Action: common.MatcherInclude,
								},
							},
							Consensus: &common.ConsensusPolicyConfig{
								MaxParticipants:         tt.maxParticipants,
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
					Filter(func(request *http.Request) bool {
						body := util.SafeReadBody(request)
						// Filter for the specific method in the test request
						if methodVal, ok := tt.request["method"]; ok {
							method := methodVal.(string)
							return strings.Contains(body, method)
						}
						return true
					}).
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
