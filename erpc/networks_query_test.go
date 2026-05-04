package erpc

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/clients"
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

func setupQueryTestNetwork(t *testing.T, ctx context.Context, ntwCfg *common.NetworkConfig) (*Network, *upstream.UpstreamsRegistry) {
	t.Helper()

	clr := clients.NewClientRegistry(&log.Logger, "prjA", nil, evm.NewJsonRpcErrorExtractor())
	rlr, err := upstream.NewRateLimitersRegistry(ctx, &common.RateLimiterConfig{
		Budgets: []*common.RateLimitBudgetConfig{},
	}, &log.Logger)
	require.NoError(t, err)
	mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)

	up1 := &common.UpstreamConfig{
		Id:       "rpc1",
		Type:     common.UpstreamTypeEvm,
		Endpoint: "http://rpc1.localhost",
		Evm: &common.EvmUpstreamConfig{
			ChainId: 123,
			QueryShim: &common.EvmQueryShimConfig{
				Enabled:       util.BoolPtr(true),
				DefaultLimit:  100,
				MaxLimit:      1000,
				MaxBlockRange: 1000,
			},
		},
	}

	vr := thirdparty.NewVendorsRegistry()
	pr, err := thirdparty.NewProvidersRegistry(&log.Logger, vr, []*common.ProviderConfig{}, nil)
	require.NoError(t, err)

	ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: "memory",
			Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"},
		},
	})
	require.NoError(t, err)

	upr := upstream.NewUpstreamsRegistry(ctx, &log.Logger, "prjA",
		[]*common.UpstreamConfig{up1}, ssr, rlr, vr, pr, nil, mt, 1*time.Second, nil, nil,
	)
	upr.Bootstrap(ctx)
	time.Sleep(100 * time.Millisecond)

	err = upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
	require.NoError(t, err)

	pup1, err := upr.NewUpstream(up1)
	require.NoError(t, err)
	require.NoError(t, pup1.Bootstrap(ctx))
	cl1, err := clr.GetOrCreateClient(ctx, pup1)
	require.NoError(t, err)
	pup1.Client = cl1

	ntw, err := NewNetwork(ctx, &log.Logger, "prjA", ntwCfg, rlr, upr, mt)
	require.NoError(t, err)
	ntw.Bootstrap(ctx)
	time.Sleep(100 * time.Millisecond)

	poller := pup1.EvmStatePoller()
	poller.SuggestLatestBlock(1000)
	poller.SuggestFinalizedBlock(990)
	upstream.ReorderUpstreams(upr)

	return ntw, upr
}

func defaultQueryNetworkConfig() *common.NetworkConfig {
	return &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm: &common.EvmNetworkConfig{
			ChainId: 123,
		},
		Failsafe: []*common.FailsafeConfig{{
			Retry: &common.RetryPolicyConfig{MaxAttempts: 1},
		}},
	}
}

func defaultQueryShimUpstreamConfig() *common.EvmUpstreamConfig {
	return &common.EvmUpstreamConfig{
		ChainId: 123,
		QueryShim: &common.EvmQueryShimConfig{
			Enabled:       util.BoolPtr(true),
			DefaultLimit:  100,
			MaxLimit:      1000,
			MaxBlockRange: 1000,
		},
	}
}

func TestNetworkQuery_QueryBlocks_ShimDecomposesToGetBlockByNumber(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	gock.New("http://rpc1.localhost").
		Post("").
		Filter(func(request *http.Request) bool {
			body := util.SafeReadBody(request)
			return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, "0x64")
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result": map[string]interface{}{
				"number": "0x64", "hash": "0xaaa1", "parentHash": "0xaaa0",
				"timestamp": "0x100", "gasUsed": "0x5208", "gasLimit": "0x7a120",
				"transactions": []interface{}{},
			},
		})

	gock.New("http://rpc1.localhost").
		Post("").
		Filter(func(request *http.Request) bool {
			body := util.SafeReadBody(request)
			return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, "0x65")
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result": map[string]interface{}{
				"number": "0x65", "hash": "0xaaa2", "parentHash": "0xaaa1",
				"timestamp": "0x101", "gasUsed": "0x5208", "gasLimit": "0x7a120",
				"transactions": []interface{}{},
			},
		})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ntw, _ := setupQueryTestNetwork(t, ctx, defaultQueryNetworkConfig())

	fakeReq := common.NewNormalizedRequest([]byte(`{
		"jsonrpc":"2.0","id":1,"method":"eth_queryBlocks",
		"params":[{"fromBlock":"0x64","toBlock":"0x65","fields":{"blocks":["number","hash","timestamp"]}}]
	}`))

	resp, err := ntw.Forward(ctx, fakeReq)
	require.NoError(t, err)
	require.NotNil(t, resp)

	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)

	result := jrr.GetResultString()
	assert.Contains(t, result, `"0x64"`)
	assert.Contains(t, result, `"0x65"`)
	assert.Contains(t, result, "blocks")
}

func TestNetworkQuery_QueryTransactions_FiltersFromAddress(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	gock.New("http://rpc1.localhost").
		Post("").
		Filter(func(request *http.Request) bool {
			body := util.SafeReadBody(request)
			return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, "0x64") && strings.Contains(body, "true")
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0", "id": 1,
			"result": map[string]interface{}{
				"number": "0x64", "hash": "0xb001", "parentHash": "0xb000",
				"timestamp": "0x100", "gasUsed": "0x0", "gasLimit": "0x7a120",
				"transactions": []interface{}{
					map[string]interface{}{
						"hash": "0xtx1", "from": "0x0000000000000000000000000000000000000001",
						"to": "0x0000000000000000000000000000000000000002", "value": "0x1",
						"input": "0x", "nonce": "0x0", "gas": "0x5208", "gasPrice": "0x1",
						"blockNumber": "0x64", "blockHash": "0xb001", "transactionIndex": "0x0",
						"type": "0x0", "r": "0x01", "s": "0x02", "v": "0x1b",
					},
					map[string]interface{}{
						"hash": "0xtx2", "from": "0x0000000000000000000000000000000000000099",
						"to": "0x0000000000000000000000000000000000000002", "value": "0x2",
						"input": "0x", "nonce": "0x0", "gas": "0x5208", "gasPrice": "0x1",
						"blockNumber": "0x64", "blockHash": "0xb001", "transactionIndex": "0x1",
						"type": "0x0", "r": "0x01", "s": "0x02", "v": "0x1b",
					},
				},
			},
		})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ntw, _ := setupQueryTestNetwork(t, ctx, defaultQueryNetworkConfig())

	fakeReq := common.NewNormalizedRequest([]byte(`{
		"jsonrpc":"2.0","id":2,"method":"eth_queryTransactions",
		"params":[{
			"fromBlock":"0x64","toBlock":"0x64",
			"filter":{"from":["0x0000000000000000000000000000000000000001"]},
			"fields":{"transactions":["hash","from","to","value"]}
		}]
	}`))

	resp, err := ntw.Forward(ctx, fakeReq)
	require.NoError(t, err)
	require.NotNil(t, resp)

	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)

	result := jrr.GetResultString()
	assert.Contains(t, result, "0xtx1")
	assert.NotContains(t, result, "0xtx2")
}

func TestNetworkQuery_QueryLogs_ForwardsFilterToEthGetLogs(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 1)

	gock.New("http://rpc1.localhost").
		Post("").
		Filter(func(request *http.Request) bool {
			body := util.SafeReadBody(request)
			return strings.Contains(body, "eth_getLogs") &&
				strings.Contains(body, "0x64") && strings.Contains(body, "0x65") &&
				strings.Contains(body, "0xddf252ad")
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0", "id": 1,
			"result": []interface{}{
				map[string]interface{}{
					"address": "0xtoken", "blockNumber": "0x64", "blockHash": "0xb001",
					"transactionHash": "0xtxA", "transactionIndex": "0x0", "logIndex": "0x0",
					"topics": []interface{}{"0xddf252ad"}, "data": "0x01",
				},
				map[string]interface{}{
					"address": "0xtoken", "blockNumber": "0x65", "blockHash": "0xb002",
					"transactionHash": "0xtxB", "transactionIndex": "0x0", "logIndex": "0x0",
					"topics": []interface{}{"0xddf252ad"}, "data": "0x02",
				},
			},
		})

	gock.New("http://rpc1.localhost").
		Post("").
		Persist().
		Filter(func(request *http.Request) bool {
			body := util.SafeReadBody(request)
			return strings.Contains(body, "eth_getBlockByNumber") &&
				(strings.Contains(body, "0x64") || strings.Contains(body, "0x65")) &&
				!strings.Contains(body, "latest") && !strings.Contains(body, "finalized")
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0", "id": 1,
			"result": map[string]interface{}{
				"number": "0x64", "hash": "0xb001", "parentHash": "0xb000",
				"timestamp": "0x100", "gasUsed": "0x0", "gasLimit": "0x7a120",
				"transactions": []interface{}{},
			},
		})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ntw, _ := setupQueryTestNetwork(t, ctx, defaultQueryNetworkConfig())

	fakeReq := common.NewNormalizedRequest([]byte(`{
		"jsonrpc":"2.0","id":3,"method":"eth_queryLogs",
		"params":[{
			"fromBlock":"0x64","toBlock":"0x65",
			"filter":{"topics":[["0xddf252ad"]]},
			"fields":{"logs":["blockNumber","logIndex","address","data"]}
		}]
	}`))

	resp, err := ntw.Forward(ctx, fakeReq)
	require.NoError(t, err)
	require.NotNil(t, resp)

	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)

	result := jrr.GetResultString()
	assert.Contains(t, result, "0xtoken")
	assert.Contains(t, result, "logs")
}

func TestNetworkQuery_QueryBlocks_PaginationWithCursor(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	for i := uint64(100); i <= 104; i++ {
		blockNum := fmt.Sprintf("0x%x", i)
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(bn string) func(*http.Request) bool {
				return func(request *http.Request) bool {
					body := util.SafeReadBody(request)
					return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, bn) && !strings.Contains(body, "latest") && !strings.Contains(body, "finalized")
				}
			}(blockNum)).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0", "id": 1,
				"result": map[string]interface{}{
					"number": blockNum, "hash": fmt.Sprintf("0x%064x", i), "parentHash": fmt.Sprintf("0x%064x", i-1),
					"timestamp": "0x100", "gasUsed": "0x0", "gasLimit": "0x7a120",
					"transactions": []interface{}{},
				},
			})
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ntw, _ := setupQueryTestNetwork(t, ctx, defaultQueryNetworkConfig())

	fakeReq := common.NewNormalizedRequest([]byte(`{
		"jsonrpc":"2.0","id":4,"method":"eth_queryBlocks",
		"params":[{"fromBlock":"0x64","toBlock":"0x68","limit":2,"fields":{"blocks":["number","hash"]}}]
	}`))

	resp, err := ntw.Forward(ctx, fakeReq)
	require.NoError(t, err)
	require.NotNil(t, resp)

	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)

	result := jrr.GetResultString()
	assert.Contains(t, result, "0x64")
	assert.Contains(t, result, "0x65")
	assert.Contains(t, result, "cursorBlock")
	assert.NotContains(t, result, "0x66")
}

func TestNetworkQuery_QueryBlocks_DescOrder(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	for i := uint64(100); i <= 102; i++ {
		blockNum := fmt.Sprintf("0x%x", i)
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(bn string) func(*http.Request) bool {
				return func(request *http.Request) bool {
					body := util.SafeReadBody(request)
					return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, bn) && !strings.Contains(body, "latest") && !strings.Contains(body, "finalized")
				}
			}(blockNum)).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0", "id": 1,
				"result": map[string]interface{}{
					"number": blockNum, "hash": fmt.Sprintf("0x%064x", i), "parentHash": fmt.Sprintf("0x%064x", i-1),
					"timestamp": "0x100", "gasUsed": "0x0", "gasLimit": "0x7a120",
					"transactions": []interface{}{},
				},
			})
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ntw, _ := setupQueryTestNetwork(t, ctx, defaultQueryNetworkConfig())

	fakeReq := common.NewNormalizedRequest([]byte(`{
		"jsonrpc":"2.0","id":5,"method":"eth_queryBlocks",
		"params":[{"fromBlock":"0x64","toBlock":"0x66","order":"desc","fields":{"blocks":["number"]}}]
	}`))

	resp, err := ntw.Forward(ctx, fakeReq)
	require.NoError(t, err)
	require.NotNil(t, resp)

	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)

	result := jrr.GetResultString()
	assert.Contains(t, result, "0x66")
	assert.Contains(t, result, "0x65")
	assert.Contains(t, result, "0x64")
}

func TestNetworkQuery_QueryTraces_UsesTraceBlock(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	gock.New("http://rpc1.localhost").
		Post("").
		Filter(func(request *http.Request) bool {
			body := util.SafeReadBody(request)
			return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, "0x64") && strings.Contains(body, "true")
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0", "id": 1,
			"result": map[string]interface{}{
				"number": "0x64", "hash": "0xb001", "parentHash": "0xb000",
				"timestamp": "0x100", "gasUsed": "0x0", "gasLimit": "0x7a120",
				"transactions": []interface{}{},
			},
		})

	gock.New("http://rpc1.localhost").
		Post("").
		Filter(func(request *http.Request) bool {
			body := util.SafeReadBody(request)
			return strings.Contains(body, "trace_block") && strings.Contains(body, "0x64")
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0", "id": 1,
			"result": []interface{}{
				map[string]interface{}{
					"type": "call",
					"action": map[string]interface{}{
						"callType": "call",
						"from":     "0x0000000000000000000000000000000000000001",
						"to":       "0x0000000000000000000000000000000000000002",
						"value":    "0x1", "input": "0x", "gas": "0x5208",
					},
					"result": map[string]interface{}{
						"output": "0x", "gasUsed": "0x5208",
					},
					"subtraces":        "0x0",
					"traceAddress":     []interface{}{},
					"transactionHash":  "0x0000000000000000000000000000000000000000000000000000000000000001",
					"transactionIndex": "0x0",
				},
			},
		})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ntw, _ := setupQueryTestNetwork(t, ctx, defaultQueryNetworkConfig())

	fakeReq := common.NewNormalizedRequest([]byte(`{
		"jsonrpc":"2.0","id":6,"method":"eth_queryTraces",
		"params":[{
			"fromBlock":"0x64","toBlock":"0x64",
			"fields":{"traces":["from","to","value","transactionHash"]}
		}]
	}`))

	resp, err := ntw.Forward(ctx, fakeReq)
	require.NoError(t, err)
	require.NotNil(t, resp)

	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)

	result := jrr.GetResultString()
	assert.Contains(t, result, "traces")
	assert.Contains(t, result, "0x0000000000000000000000000000000000000000000000000000000000000001")
}

func TestNetworkQuery_QueryTransfers_ExtractsFromTraces(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	gock.New("http://rpc1.localhost").
		Post("").
		Filter(func(request *http.Request) bool {
			body := util.SafeReadBody(request)
			return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, "0x64") && strings.Contains(body, "true")
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0", "id": 1,
			"result": map[string]interface{}{
				"number": "0x64", "hash": "0xb001", "parentHash": "0xb000",
				"timestamp": "0x100", "gasUsed": "0x0", "gasLimit": "0x7a120",
				"transactions": []interface{}{},
			},
		})

	gock.New("http://rpc1.localhost").
		Post("").
		Filter(func(request *http.Request) bool {
			body := util.SafeReadBody(request)
			return strings.Contains(body, "trace_block") && strings.Contains(body, "0x64")
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0", "id": 1,
			"result": []interface{}{
				map[string]interface{}{
					"type": "call",
					"action": map[string]interface{}{
						"callType": "call",
						"from":     "0x0000000000000000000000000000000000000001",
						"to":       "0x0000000000000000000000000000000000000002",
						"value":    "0x1", "input": "0x", "gas": "0x5208",
					},
					"result":           map[string]interface{}{"output": "0x", "gasUsed": "0x5208"},
					"subtraces":        "0x0",
					"traceAddress":     []interface{}{},
					"transactionHash":  "0x0000000000000000000000000000000000000000000000000000000000000001",
					"transactionIndex": "0x0",
				},
			},
		})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ntw, _ := setupQueryTestNetwork(t, ctx, defaultQueryNetworkConfig())

	fakeReq := common.NewNormalizedRequest([]byte(`{
		"jsonrpc":"2.0","id":7,"method":"eth_queryTransfers",
		"params":[{
			"fromBlock":"0x64","toBlock":"0x64",
			"fields":{"transfers":["from","to","value"]}
		}]
	}`))

	resp, err := ntw.Forward(ctx, fakeReq)
	require.NoError(t, err)
	require.NotNil(t, resp)

	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)

	result := jrr.GetResultString()
	assert.Contains(t, result, "transfers")
	assert.Contains(t, result, "0x0000000000000000000000000000000000000001")
	assert.Contains(t, result, "0x0000000000000000000000000000000000000002")
}

func TestNetworkQuery_QueryLogs_DescOrderReversesResults(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 1)

	gock.New("http://rpc1.localhost").
		Post("").
		Filter(func(request *http.Request) bool {
			body := util.SafeReadBody(request)
			return strings.Contains(body, "eth_getLogs")
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0", "id": 1,
			"result": []interface{}{
				map[string]interface{}{
					"address": "0xtoken", "blockNumber": "0xc8", "blockHash": "0xb001",
					"transactionHash": "0xtxA", "transactionIndex": "0x0", "logIndex": "0x0",
					"topics": []interface{}{}, "data": "0xfirst",
				},
				map[string]interface{}{
					"address": "0xtoken", "blockNumber": "0xc9", "blockHash": "0xb002",
					"transactionHash": "0xtxB", "transactionIndex": "0x0", "logIndex": "0x0",
					"topics": []interface{}{}, "data": "0xsecond",
				},
			},
		})

	gock.New("http://rpc1.localhost").
		Post("").
		Persist().
		Filter(func(request *http.Request) bool {
			body := util.SafeReadBody(request)
			return strings.Contains(body, "eth_getBlockByNumber") &&
				(strings.Contains(body, "0xc8") || strings.Contains(body, "0xc9")) &&
				!strings.Contains(body, "latest") && !strings.Contains(body, "finalized")
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0", "id": 1,
			"result": map[string]interface{}{
				"number": "0xc9", "hash": "0xb002", "parentHash": "0xb001",
				"timestamp": "0x101", "gasUsed": "0x0", "gasLimit": "0x7a120",
				"transactions": []interface{}{},
			},
		})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ntw, _ := setupQueryTestNetwork(t, ctx, defaultQueryNetworkConfig())

	fakeReq := common.NewNormalizedRequest([]byte(`{
		"jsonrpc":"2.0","id":8,"method":"eth_queryLogs",
		"params":[{
			"fromBlock":"0xc8","toBlock":"0xc9","order":"desc",
			"fields":{"logs":["blockNumber","data"]}
		}]
	}`))

	resp, err := ntw.Forward(ctx, fakeReq)
	require.NoError(t, err)
	require.NotNil(t, resp)

	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)

	result := jrr.GetResultString()
	idxSecond := strings.Index(result, "0xsecond")
	idxFirst := strings.Index(result, "0xfirst")
	assert.Greater(t, idxFirst, idxSecond, "DESC order: log from block 0xc9 (data=0xsecond) should appear before log from block 0xc8 (data=0xfirst)")
}

func TestNetworkQuery_ShimFallsBackWhenNoNativeUpstream(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	gock.New("http://rpc1.localhost").
		Post("").
		Filter(func(request *http.Request) bool {
			body := util.SafeReadBody(request)
			return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, "0x64") && !strings.Contains(body, "latest") && !strings.Contains(body, "finalized")
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0", "id": 1,
			"result": map[string]interface{}{
				"number": "0x64", "hash": "0xaaa1", "parentHash": "0xaaa0",
				"timestamp": "0x100", "gasUsed": "0x0", "gasLimit": "0x7a120",
				"transactions": []interface{}{},
			},
		})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ntw, _ := setupQueryTestNetwork(t, ctx, defaultQueryNetworkConfig())

	fakeReq := common.NewNormalizedRequest([]byte(`{
		"jsonrpc":"2.0","id":9,"method":"eth_queryBlocks",
		"params":[{"fromBlock":"0x64","toBlock":"0x64","fields":{"blocks":["number","hash"]}}]
	}`))

	resp, err := ntw.Forward(ctx, fakeReq)
	require.NoError(t, err)
	require.NotNil(t, resp)

	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	assert.Contains(t, jrr.GetResultString(), "0x64")
}
