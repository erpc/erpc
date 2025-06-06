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

func TestNetwork_Consensus(t *testing.T) {
	tests := []struct {
		name                 string
		upstreams            []*common.UpstreamConfig
		hasRetries           bool
		request              map[string]interface{}
		mockResponses        []map[string]interface{}
		requiredParticipants int
		expectedCalls        []int // Number of expected calls for each upstream
		expectedResponse     *common.NormalizedResponse
		expectedError        *common.ErrorCode
		expectedMsg          *string
	}{
		{
			name:                 "successful consensus",
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
			expectedCalls: []int{1, 1, 1}, // Each upstream called once
			expectedResponse: common.NewNormalizedResponse().
				WithJsonRpcResponse(successResponse),
		},
		{
			name:                 "low participants error",
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
			expectedCalls: []int{3}, // One upstream, called 3 times
			expectedError: pointer(common.ErrCodeConsensusLowParticipants),
			expectedMsg:   pointer("not enough participants"),
		},
		{
			name:                 "dispute error",
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
			name:                 "retried dispute error",
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
				"params": []interface{}{"latest", false},
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
						"code":    -32000,
						"message": "cannot query unfinalized data",
					},
				},
			},
			hasRetries:    true,
			expectedCalls: []int{2, 1}, // Each upstream called twice
			expectedError: pointer(common.ErrCodeConsensusDispute),
			expectedMsg:   pointer("not enough agreement among responses"),
		},
		{
			name:                 "error response on upstreams",
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
			expectedError: pointer(common.ErrCodeConsensusDispute),
			expectedMsg:   pointer("not enough agreement among responses"),
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
					MaxAttempts: 2,
					Delay:       common.Duration(0),
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
					Failsafe: &common.FailsafeConfig{
						Retry: retryPolicy,
						Consensus: &common.ConsensusPolicyConfig{
							RequiredParticipants:    tt.requiredParticipants,
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

			// Setup mock responses with expected call counts
			for i, upstream := range tt.upstreams {
				gock.New(upstream.Endpoint).
					Post("/").
					Times(tt.expectedCalls[i]).
					Reply(200).
					SetHeader("Content-Type", "application/json").
					JSON(tt.mockResponses[i])
			}

			// Make request
			reqBytes, err := json.Marshal(tt.request)
			if err != nil {
				require.NoError(t, err)
			}

			fakeReq := common.NewNormalizedRequest(reqBytes)
			resp, err := ntw.Forward(ctx, fakeReq)

			// Log the error for debugging
			if err != nil {
				log.Debug().Err(err).Msg("Got error from Forward")
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

				assert.Equal(t, string(expectedJrr.Result), string(actualJrr.Result))
			}
		})
	}
}

func pointer[T any](v T) *T {
	return &v
}
