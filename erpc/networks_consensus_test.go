package erpc

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
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

func TestNetwork_Consensus(t *testing.T) {
	type upstreamResponses struct {
		Request  map[string]interface{}
		Response map[string]interface{}
		Calls    int
	}

	tests := []struct {
		name                 string
		upstreams            []*common.UpstreamConfig
		hasRetries           bool
		request              map[string]interface{}
		upstreamResponses    [][]upstreamResponses
		requiredParticipants int
		expectedResponse     map[string]interface{}
		expectedError        *common.ErrorCode
		expectedCause        *common.ErrorCode
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
			upstreamResponses: [][]upstreamResponses{
				{
					{
						Request: map[string]interface{}{
							"jsonrpc": "2.0",
							"id":      "*",
							"method":  "eth_chainId",
							"params":  []interface{}{},
						},
						Response: map[string]interface{}{
							"jsonrpc": "2.0",
							"id":      1,
							"result":  "0x7a",
						},
						Calls: 1,
					},
				},
				{
					{
						Request: map[string]interface{}{
							"jsonrpc": "2.0",
							"id":      "*",
							"method":  "eth_chainId",
							"params":  []interface{}{},
						},
						Response: map[string]interface{}{
							"jsonrpc": "2.0",
							"id":      1,
							"result":  "0x7a",
						},
						Calls: 1,
					},
				},
				{
					{
						Request: map[string]interface{}{
							"jsonrpc": "2.0",
							"id":      "*",
							"method":  "eth_chainId",
							"params":  []interface{}{},
						},
						Response: map[string]interface{}{
							"jsonrpc": "2.0",
							"id":      1,
							"result":  "0x7a",
						},
						Calls: 1,
					},
				},
			},
			expectedResponse: map[string]interface{}{
				"id":     1,
				"result": "0x7a",
			},
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
			upstreamResponses: [][]upstreamResponses{
				{
					{
						Request: map[string]interface{}{
							"jsonrpc": "2.0",
							"id":      "*",
							"method":  "eth_chainId",
							"params":  []interface{}{},
						},
						Response: map[string]interface{}{
							"jsonrpc": "2.0",
							"id":      1,
							"result":  "0x7b",
						},
						Calls: 3,
					},
				},
			},
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
			upstreamResponses: [][]upstreamResponses{
				{
					{
						Request: map[string]interface{}{
							"jsonrpc": "2.0",
							"id":      "*",
							"method":  "eth_chainId",
							"params":  []interface{}{},
						},
						Response: map[string]interface{}{
							"jsonrpc": "2.0",
							"id":      1,
							"result":  "0x7b",
						},
						Calls: 1,
					},
				},
				{
					{
						Request: map[string]interface{}{
							"jsonrpc": "2.0",
							"id":      "*",
							"method":  "eth_chainId",
							"params":  []interface{}{},
						},
						Response: map[string]interface{}{
							"jsonrpc": "2.0",
							"id":      1,
							"result":  "0x7c",
						},
						Calls: 1,
					},
				},
				{
					{
						Request: map[string]interface{}{
							"jsonrpc": "2.0",
							"id":      "*",
							"method":  "eth_chainId",
							"params":  []interface{}{},
						},
						Response: map[string]interface{}{
							"jsonrpc": "2.0",
							"id":      1,
							"result":  "0x7d",
						},
						Calls: 1,
					},
				},
			},
			expectedError: pointer(common.ErrCodeConsensusDispute),
			expectedMsg:   pointer("not enough agreement among responses"),
		},
		{
			name:                 "retries around `cannot query unfinalized data` error result in a low participants error",
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
			upstreamResponses: [][]upstreamResponses{
				{
					{
						Request: map[string]interface{}{
							"jsonrpc": "2.0",
							"id":      "*",
							"method":  "eth_getBlockByNumber",
							"params":  []interface{}{"latest", false},
						},
						Response: map[string]interface{}{
							"jsonrpc": "2.0",
							"id":      1,
							"result":  "0x1",
						},
						Calls: 3,
					},
				},
				{
					{
						Request: map[string]interface{}{
							"jsonrpc": "2.0",
							"id":      "*",
							"method":  "eth_getBlockByNumber",
							"params":  []interface{}{"latest", false},
						},
						Response: map[string]interface{}{
							"jsonrpc": "2.0",
							"id":      1,
							"error": map[string]interface{}{
								"code":    -32000,
								"message": "cannot query unfinalized data",
							},
						},
						Calls: 1,
					},
				},
			},
			hasRetries:    true,
			expectedError: pointer(common.ErrCodeFailsafeRetryExceeded),
			expectedCause: pointer(common.ErrCodeConsensusLowParticipants),
			expectedMsg:   pointer("gave up retrying on network-level"),
		},
		// todo: there seems to be a bug here where consensus isn't established when errors are returned.
		// {
		// 	name:                 "eth_sendRawTransaction error is returned if the transaction has any user-facing errors",
		// 	requiredParticipants: 2,
		// 	upstreams: []*common.UpstreamConfig{
		// 		{
		// 			Id:       "test1",
		// 			Type:     common.UpstreamTypeEvm,
		// 			Endpoint: "http://rpc1-dispute.localhost",
		// 			Evm: &common.EvmUpstreamConfig{
		// 				ChainId: 123,
		// 			},
		// 		},
		// 		{
		// 			Id:       "test2",
		// 			Type:     common.UpstreamTypeEvm,
		// 			Endpoint: "http://rpc2-dispute.localhost",
		// 			Evm: &common.EvmUpstreamConfig{
		// 				ChainId: 123,
		// 			},
		// 		},
		// 	},
		// 	request: map[string]interface{}{
		// 		"method": "eth_sendRawTransaction",
		// 		"params": []interface{}{"0xf8aa8085746a52880083030d4094a0b86991c6218b36c1d19d4a2e9eb0ce3606eb4880b844a9059cbb00000000000000000000000055fe002aeff02f77364de339a1292923a15844b8000000000000000000000000000000000000000000000000000016bcc41e900026a06578e9605f60007ac6293b15861c59f0d90c97a0390c4d62d2a6be890fd84472a01f3a5f5ad1330f71669324bb8a46ec174c5abd2503587fee897b818540a85dcc"},
		// 	},
		// 	mockResponses: []map[string]interface{}{
		// 		{
		// 			"jsonrpc": "2.0",
		// 			"id":      1,
		// 			"error": map[string]interface{}{
		// 				"code":    -32000,
		// 				"message": "transaction underpriced",
		// 			},
		// 		},
		// 		{
		// 			"jsonrpc": "2.0",
		// 			"id":      1,
		// 			"error": map[string]interface{}{
		// 				"code":    -32000,
		// 				"message": "transaction underpriced",
		// 			},
		// 		},
		// 	},
		// 	hasRetries:    true,
		// 	expectedCalls: []int{1, 1},
		// 	expectedResponse: common.NewNormalizedResponse().
		// 		WithJsonRpcResponse(underpricedSendRawTransactionResponse),
		// },
		{
			name:                 "eth_sendRawTransaction is idempotent within a window for identical nonces, non-incremental gas price for the same transaction hash if nonce is too low",
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
				"method": "eth_sendRawTransaction",
				"params": []interface{}{"0xf8aa8085746a52880083030d4094a0b86991c6218b36c1d19d4a2e9eb0ce3606eb4880b844a9059cbb00000000000000000000000055fe002aeff02f77364de339a1292923a15844b8000000000000000000000000000000000000000000000000000016bcc41e900026a06578e9605f60007ac6293b15861c59f0d90c97a0390c4d62d2a6be890fd84472a01f3a5f5ad1330f71669324bb8a46ec174c5abd2503587fee897b818540a85dcc"},
			},
			upstreamResponses: [][]upstreamResponses{
				{
					{
						Request: map[string]interface{}{
							"jsonrpc": "2.0",
							"id":      "*",
							"method":  "eth_sendRawTransaction",
							"params":  []interface{}{"0xf8aa8085746a52880083030d4094a0b86991c6218b36c1d19d4a2e9eb0ce3606eb4880b844a9059cbb00000000000000000000000055fe002aeff02f77364de339a1292923a15844b8000000000000000000000000000000000000000000000000000016bcc41e900026a06578e9605f60007ac6293b15861c59f0d90c97a0390c4d62d2a6be890fd84472a01f3a5f5ad1330f71669324bb8a46ec174c5abd2503587fee897b818540a85dcc"},
						},
						Response: map[string]interface{}{
							"jsonrpc": "2.0",
							"id":      1,
							"result":  "0x34dd76864329e3e79a1ed21d21952d9d809c2df4d58a5c4712ffa3b9432e5bca",
						},
						Calls: 1,
					},
				},
				{
					{
						Request: map[string]interface{}{
							"jsonrpc": "2.0",
							"id":      "*",
							"method":  "eth_sendRawTransaction",
							"params":  []interface{}{"0xf8aa8085746a52880083030d4094a0b86991c6218b36c1d19d4a2e9eb0ce3606eb4880b844a9059cbb00000000000000000000000055fe002aeff02f77364de339a1292923a15844b8000000000000000000000000000000000000000000000000000016bcc41e900026a06578e9605f60007ac6293b15861c59f0d90c97a0390c4d62d2a6be890fd84472a01f3a5f5ad1330f71669324bb8a46ec174c5abd2503587fee897b818540a85dcc"},
						},
						Response: map[string]interface{}{
							"jsonrpc": "2.0",
							"id":      1,
							"error": map[string]interface{}{
								"code":    -32000,
								"message": "nonce too low",
							},
						},
						Calls: 1,
					},
					{
						Request: map[string]interface{}{
							"jsonrpc": "2.0",
							"id":      "*",
							"method":  "eth_getTransactionByHash",
							"params":  []interface{}{"0x34dd76864329e3e79a1ed21d21952d9d809c2df4d58a5c4712ffa3b9432e5bca"},
						},
						Response: map[string]interface{}{
							"jsonrpc": "2.0",
							"id":      1,
							"result": map[string]interface{}{
								"blockHash":        "0xc6c592872e3b3bfff735bc0c4d046765dfe45c13820824e2a3d45e19928a9b6c",
								"blockNumber":      "0x158435d",
								"from":             "0x5d977b94eaa0265796591eb39b55d1f51a7cb3a4",
								"gas":              "0x30d40",
								"gasPrice":         "0x746a528800",
								"hash":             "0x34dd76864329e3e79a1ed21d21952d9d809c2df4d58a5c4712ffa3b9432e5bca",
								"input":            "0xa9059cbb00000000000000000000000055fe002aeff02f77364de339a1292923a15844b8000000000000000000000000000000000000000000000000000016bcc41e9000",
								"nonce":            "0x0",
								"to":               "0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48",
								"transactionIndex": "0x7",
								"value":            "0x0",
								"type":             "0x0",
								"chainId":          "0x1",
								"v":                "0x26",
								"r":                "0x6578e9605f60007ac6293b15861c59f0d90c97a0390c4d62d2a6be890fd84472",
								"s":                "0x1f3a5f5ad1330f71669324bb8a46ec174c5abd2503587fee897b818540a85dcc",
							},
						},
						Calls: 1,
					},
				},
			},
			expectedResponse: map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x34dd76864329e3e79a1ed21d21952d9d809c2df4d58a5c4712ffa3b9432e5bca",
			},
		},
		// {
		// 	name:                 "eth_sendRawTransaction is idempotent within a window for identical nonces, non-incremental gas price for the same transaction hash if the transaction is included in the mempool",
		// 	requiredParticipants: 2,
		// 	upstreams: []*common.UpstreamConfig{
		// 		{
		// 			Id:       "test1",
		// 			Type:     common.UpstreamTypeEvm,
		// 			Endpoint: "http://rpc1-dispute.localhost",
		// 			Evm: &common.EvmUpstreamConfig{
		// 				ChainId: 123,
		// 			},
		// 		},
		// 		{
		// 			Id:       "test2",
		// 			Type:     common.UpstreamTypeEvm,
		// 			Endpoint: "http://rpc2-dispute.localhost",
		// 			Evm: &common.EvmUpstreamConfig{
		// 				ChainId: 123,
		// 			},
		// 		},
		// 	},
		// 	request: map[string]interface{}{
		// 		"method": "eth_sendRawTransaction",
		// 		"params": []interface{}{"0xf8aa8085746a52880083030d4094a0b86991c6218b36c1d19d4a2e9eb0ce3606eb4880b844a9059cbb00000000000000000000000055fe002aeff02f77364de339a1292923a15844b8000000000000000000000000000000000000000000000000000016bcc41e900026a06578e9605f60007ac6293b15861c59f0d90c97a0390c4d62d2a6be890fd84472a01f3a5f5ad1330f71669324bb8a46ec174c5abd2503587fee897b818540a85dcc"},
		// 	},
		// 	upstreamResponses: [][]upstreamResponses{
		// 		{
		// 			{
		// 				Request: map[string]interface{}{
		// 					"jsonrpc": "2.0",
		// 					"id":      "*",
		// 					"method":  "eth_sendRawTransaction",
		// 					"params":  []interface{}{"0xf8aa8085746a52880083030d4094a0b86991c6218b36c1d19d4a2e9eb0ce3606eb4880b844a9059cbb00000000000000000000000055fe002aeff02f77364de339a1292923a15844b8000000000000000000000000000000000000000000000000000016bcc41e900026a06578e9605f60007ac6293b15861c59f0d90c97a0390c4d62d2a6be890fd84472a01f3a5f5ad1330f71669324bb8a46ec174c5abd2503587fee897b818540a85dcc"},
		// 				},
		// 				Response: map[string]interface{}{
		// 					"jsonrpc": "2.0",
		// 					"id":      1,
		// 					"result":  "0x34dd76864329e3e79a1ed21d21952d9d809c2df4d58a5c4712ffa3b9432e5bca",
		// 				},
		// 				Calls: 1,
		// 			},
		// 		},
		// 		{
		// 			{
		// 				Request: map[string]interface{}{
		// 					"jsonrpc": "2.0",
		// 					"id":      "*",
		// 					"method":  "eth_sendRawTransaction",
		// 					"params":  []interface{}{"0xf8aa8085746a52880083030d4094a0b86991c6218b36c1d19d4a2e9eb0ce3606eb4880b844a9059cbb00000000000000000000000055fe002aeff02f77364de339a1292923a15844b8000000000000000000000000000000000000000000000000000016bcc41e900026a06578e9605f60007ac6293b15861c59f0d90c97a0390c4d62d2a6be890fd84472a01f3a5f5ad1330f71669324bb8a46ec174c5abd2503587fee897b818540a85dcc"},
		// 				},
		// 				Response: map[string]interface{}{
		// 					"jsonrpc": "2.0",
		// 					"id":      1,
		// 					"error": map[string]interface{}{
		// 						"code":    -32000,
		// 						"message": "transaction already in mempool",
		// 					},
		// 				},
		// 				Calls: 1,
		// 			},
		// 		},
		// 	},
		// 	expectedResponse: map[string]interface{}{
		// 		"jsonrpc": "2.0",
		// 		"id":      1,
		// 		"result":  "0x34dd76864329e3e79a1ed21d21952d9d809c2df4d58a5c4712ffa3b9432e5bca",
		// 	},
		// },
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
			upstreamResponses: [][]upstreamResponses{
				{
					{
						Request: map[string]interface{}{
							"jsonrpc": "2.0",
							"id":      "*",
							"method":  "eth_chainId",
							"params":  []interface{}{},
						},
						Response: map[string]interface{}{
							"jsonrpc": "2.0",
							"id":      1,
							"error": map[string]interface{}{
								"code":    -32000,
								"message": "internal error",
							},
						},
						Calls: 3,
					},
				},
				{
					{
						Request: map[string]interface{}{
							"jsonrpc": "2.0",
							"id":      "*",
							"method":  "eth_chainId",
							"params":  []interface{}{},
						},
						Response: map[string]interface{}{
							"jsonrpc": "2.0",
							"id":      1,
							"error": map[string]interface{}{
								"code":    -32000,
								"message": "internal error",
							},
						},
						Calls: 3,
					},
				},
				{
					{
						Request: map[string]interface{}{
							"jsonrpc": "2.0",
							"id":      "*",
							"method":  "eth_chainId",
							"params":  []interface{}{},
						},
						Response: map[string]interface{}{
							"jsonrpc": "2.0",
							"id":      1,
							"error": map[string]interface{}{
								"code":    -32000,
								"message": "internal error",
							},
						},
						Calls: 3,
					},
				},
			},
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
				upstreamResponses := tt.upstreamResponses[i]
				for _, response := range upstreamResponses {
					gock.New(upstream.Endpoint).
						Post("/").
						AddMatcher(func(request *http.Request, gockReq *gock.Request) (bool, error) {
							body, err := io.ReadAll(request.Body)
							if err != nil {
								return false, err
							}

							jsonBody := map[string]interface{}{}
							err = json.Unmarshal(body, &jsonBody)
							if err != nil {
								return false, err
							}

							// Simulate wildcard matching, through altering the request body
							for key, value := range response.Request {
								if value == "*" {
									jsonBody[key] = "*"
								}
							}

							matched := reflect.DeepEqual(jsonBody, response.Request)

							t.Logf("matched: %v, url: %s, actual: %v, expected: %v", matched, request.URL.String(), jsonBody, response.Request)

							return matched, nil
						}).
						//JSON(response.Request).
						Times(response.Calls).
						Reply(200).
						SetHeader("Content-Type", "application/json").
						JSON(response.Response)
				}
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

			for _, request := range gock.GetUnmatchedRequests() {
				body, err := io.ReadAll(request.Body)
				if err != nil {
					t.Logf("expected no unmatched requests, got %s", request.URL.String())
				} else {
					t.Logf("expected no unmatched requests, got %s, %s", request.URL.String(), string(body))
				}
			}

			if len(gock.GetUnmatchedRequests()) > 0 {
				t.Fatalf("expected no unmatched requests, got %d", len(gock.GetUnmatchedRequests()))
			}

			if tt.expectedError != nil {
				assert.Error(t, err, "expected error but got nil")
				assert.True(t, common.HasErrorCode(err, *tt.expectedError), "expected error code %s, got %s", *tt.expectedError, err)
				assert.Contains(t, err.Error(), *tt.expectedMsg, "expected error message %s, got %s", *tt.expectedMsg, err.Error())
				assert.Nil(t, resp, "expected nil response")
				if tt.expectedCause != nil {
					if err, ok := err.(common.StandardError); ok {
						if err, ok := err.GetCause().(common.StandardError); ok {
							assert.True(t, common.HasErrorCode(err, *tt.expectedCause), "expected error code %s, got %s", *tt.expectedError, err)
						}
					}
				}
			} else {
				require.NoError(t, err)
				require.NotNil(t, resp)

				expectedJrr, err := common.NewJsonRpcResponse(tt.expectedResponse["id"], tt.expectedResponse["result"], nil)
				require.NoError(t, err)
				require.NotNil(t, expectedJrr)
				require.NoError(t, err)

				actualJrr, err := resp.JsonRpcResponse()
				require.NoError(t, err)
				require.NotNil(t, actualJrr)

				assert.Equal(t, string(expectedJrr.Result), string(actualJrr.Result))
			}
		})
	}
}

func pointer[T any](v T) *T {
	return &v
}
