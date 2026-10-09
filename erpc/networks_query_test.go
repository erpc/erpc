package erpc

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bytedance/sonic"
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

// The query shim end to end: JSON-RPC eth_query* requests through
// Network.Forward, answered from eth_getBlockByNumber, eth_getBlockReceipts,
// eth_getLogs and debug_traceBlockByNumber sub-requests that go to a mocked
// JSON-RPC upstream through the network. Native QueryService upstreams and the
// gRPC transport are in query_executor_test.go.

const (
	qtAddrA   = "0x00000000000000000000000000000000000000aa"
	qtAddrB   = "0x00000000000000000000000000000000000000bb"
	qtAddrC   = "0x00000000000000000000000000000000000000cc"
	qtMiner   = "0x0000000000000000000000000000000000000111"
	qtToken   = "0x0000000000000000000000000000000000000777"
	qtTopic   = "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"
	qtSig     = "0x1111111111111111111111111111111111111111111111111111111111111111"
	qtFirstNo = 0x64
	qtLastNo  = 0x69
	// qtReorgNo is on another fork: its parentHash is not the hash of the
	// block before it, so a page must not cross from 0x67 into it.
	qtReorgNo = 0x68
)

func qtHash(n uint64) string      { return fmt.Sprintf("0x%064x", n) }
func qtTxHash(n, i uint64) string { return fmt.Sprintf("0x%062x%02x", n, i+0xa0) }

// qtBlock is block n of the mocked chain: two transactions, index 0 from A to
// B calling selector 0xa9059cbb, index 1 from C creating a contract.
func qtBlock(n uint64) map[string]interface{} {
	txs := []interface{}{
		map[string]interface{}{
			"hash": qtTxHash(n, 0), "nonce": "0x1", "from": qtAddrA, "to": qtAddrB, "value": "0x10",
			"input": "0xa9059cbb0000", "gas": "0x5208", "gasPrice": "0x1", "type": "0x0", "chainId": "0x7b",
			"v": "0x11a", "r": qtSig, "s": qtSig, "transactionIndex": "0x0",
			"blockNumber": fmt.Sprintf("0x%x", n), "blockHash": qtHash(n),
		},
		map[string]interface{}{
			"hash": qtTxHash(n, 1), "nonce": "0x2", "from": qtAddrC, "to": nil, "value": "0x0",
			"input": "0x60", "gas": "0x9000", "gasPrice": "0x1", "type": "0x0", "chainId": "0x7b",
			"v": "0x11a", "r": qtSig, "s": qtSig, "transactionIndex": "0x1",
			"blockNumber": fmt.Sprintf("0x%x", n), "blockHash": qtHash(n),
		},
	}
	return map[string]interface{}{
		"number": fmt.Sprintf("0x%x", n), "hash": qtHash(n), "parentHash": qtParentHash(n),
		"timestamp": fmt.Sprintf("0x%x", 1000+n), "gasLimit": "0x1000000", "gasUsed": "0x10000",
		"miner": qtMiner, "logsBloom": "0x00", "transactionsRoot": qtHash(1), "stateRoot": qtHash(2),
		"receiptsRoot": qtHash(3), "sha3Uncles": qtHash(4), "extraData": "0x", "size": "0x100",
		"nonce": "0x0000000000000000", "mixHash": qtHash(5), "baseFeePerGas": "0x1", "difficulty": "0x0",
		"totalDifficulty": "0x0", "transactions": txs,
	}
}

func qtParentHash(n uint64) string {
	if n == qtReorgNo {
		return qtHash(0xdead)
	}
	return qtHash(n - 1)
}

func qtReceipts(n uint64) []interface{} {
	out := make([]interface{}, 2)
	for i := range uint64(2) {
		status := "0x1"
		var contract interface{}
		if i == 1 {
			contract = qtAddrB
			if n == 0x65 {
				status = "0x0"
			}
		}
		out[i] = map[string]interface{}{
			"transactionHash": qtTxHash(n, i), "transactionIndex": fmt.Sprintf("0x%x", i),
			"blockNumber": fmt.Sprintf("0x%x", n), "blockHash": qtHash(n), "from": qtAddrA,
			"status": status, "gasUsed": "0x5208", "cumulativeGasUsed": fmt.Sprintf("0x%x", 0x5208*(i+1)),
			"effectiveGasPrice": "0x1", "logsBloom": "0x00", "logs": []interface{}{}, "contractAddress": contract,
			"type": "0x0",
		}
	}
	return out
}

// qtCallTrace is the callTracer result of a transaction: a successful CALL
// with one failed child and one child below the failed one.
func qtCallTrace(n, i uint64) map[string]interface{} {
	return map[string]interface{}{
		"txHash": qtTxHash(n, i),
		"result": map[string]interface{}{
			"type": "CALL", "from": qtAddrA, "to": qtAddrB, "value": "0x10", "gas": "0x9000", "gasUsed": "0x5000",
			"input": "0xa9059cbb0000", "output": "0x",
			"calls": []interface{}{
				map[string]interface{}{
					"type": "CALL", "from": qtAddrB, "to": qtAddrC, "value": "0x1", "gas": "0x100", "gasUsed": "0x100",
					"input": "0x", "error": "execution reverted",
					"calls": []interface{}{
						map[string]interface{}{"type": "CALL", "from": qtAddrC, "to": qtAddrA, "value": "0x1", "gas": "0x10", "gasUsed": "0x10", "input": "0x"},
					},
				},
				map[string]interface{}{"type": "STATICCALL", "from": qtAddrB, "to": qtToken, "value": "0x0", "gas": "0x100", "gasUsed": "0x10", "input": "0x70a08231", "output": "0x01"},
			},
		},
	}
}

func qtReply(result interface{}) map[string]interface{} {
	return map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": result}
}

// mockQueryChain serves the mocked chain from http://rpc1.localhost.
func mockQueryChain(blockDelay time.Duration) {
	for n := uint64(qtFirstNo - 1); n <= qtLastNo; n++ {
		ref := fmt.Sprintf(`"0x%x"`, n)
		gock.New("http://rpc1.localhost").Post("").Persist().
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, ref)
			}).
			Reply(200).Delay(blockDelay).JSON(qtReply(qtBlock(n)))
		gock.New("http://rpc1.localhost").Post("").Persist().
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getBlockReceipts") && strings.Contains(body, ref)
			}).
			Reply(200).JSON(qtReply(qtReceipts(n)))
		gock.New("http://rpc1.localhost").Post("").Persist().
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "debug_traceBlockByNumber") && strings.Contains(body, ref)
			}).
			Reply(200).JSON(qtReply([]interface{}{qtCallTrace(n, 0), qtCallTrace(n, 1)}))
	}
	gock.New("http://rpc1.localhost").Post("").Persist().
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "trace_block") }).
		Reply(200).JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "error": map[string]interface{}{"code": -32601, "message": "the method trace_block does not exist/is not available"}})
	gock.New("http://rpc1.localhost").Post("").Persist().
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			return strings.Contains(body, "eth_getLogs") && !strings.Contains(body, `"0x69"`)
		}).
		Reply(200).JSON(qtReply([]interface{}{
		map[string]interface{}{
			"address": qtToken, "topics": []interface{}{qtTopic}, "data": "0x01", "blockNumber": "0x64",
			"blockHash": qtHash(0x64), "transactionHash": qtTxHash(0x64, 0), "transactionIndex": "0x0", "logIndex": "0x0",
		},
		map[string]interface{}{
			"address": qtToken, "topics": []interface{}{qtTopic}, "data": "0x02", "blockNumber": "0x64",
			"blockHash": qtHash(0x64), "transactionHash": qtTxHash(0x64, 0), "transactionIndex": "0x0", "logIndex": "0x1",
		},
		map[string]interface{}{
			"address": qtToken, "topics": []interface{}{qtTopic}, "data": "0x03", "blockNumber": "0x66",
			"blockHash": qtHash(0x66), "transactionHash": qtTxHash(0x66, 1), "transactionIndex": "0x1", "logIndex": "0x0",
		},
	}))
	// The logs of a range that ends at 0x69: one log of block 0x69 that
	// carries the hash of another fork.
	gock.New("http://rpc1.localhost").Post("").Persist().
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			return strings.Contains(body, "eth_getLogs") && strings.Contains(body, `"0x69"`)
		}).
		Reply(200).JSON(qtReply([]interface{}{
		map[string]interface{}{
			"address": qtToken, "topics": []interface{}{qtTopic}, "data": "0x04", "blockNumber": "0x69",
			"blockHash": qtHash(0xbeef), "transactionHash": qtTxHash(0x69, 0), "transactionIndex": "0x0", "logIndex": "0x0",
		},
	}))
}

// setupQueryTestNetwork builds a network of the upstreams ids (default rpc1),
// each at http://<id>.localhost, all enabling queryShim, selected in the
// order given.
func setupQueryTestNetwork(t *testing.T, ctx context.Context, queryShim *common.EvmQueryShimConfig, ids ...string) *Network {
	t.Helper()

	clr := clients.NewClientRegistry(&log.Logger, "prjA", nil, evm.NewJsonRpcErrorExtractor())
	rlr, err := upstream.NewRateLimitersRegistry(ctx, &common.RateLimiterConfig{Budgets: []*common.RateLimitBudgetConfig{}}, &log.Logger)
	require.NoError(t, err)
	mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)

	if len(ids) == 0 {
		ids = []string{"rpc1"}
	}
	ups := make([]*common.UpstreamConfig, len(ids))
	for i, id := range ids {
		ups[i] = &common.UpstreamConfig{
			Id:       id,
			Type:     common.UpstreamTypeEvm,
			Endpoint: "http://" + id + ".localhost",
			Evm:      &common.EvmUpstreamConfig{ChainId: 123, QueryShim: queryShim},
		}
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

	upr := upstream.NewUpstreamsRegistry(ctx, &log.Logger, "prjA", ups, ssr, rlr, vr, pr, nil, mt, nil)
	upr.Bootstrap(ctx)
	time.Sleep(100 * time.Millisecond)
	require.NoError(t, upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123)))

	for _, cfg := range ups {
		pup, err := upr.NewUpstream(cfg)
		require.NoError(t, err)
		require.NoError(t, pup.Bootstrap(ctx))
		cl, err := clr.GetOrCreateClient(ctx, pup)
		require.NoError(t, err)
		pup.Client = cl
	}

	ntw, err := NewNetwork(ctx, &log.Logger, "prjA", &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
		Failsafe:     []*common.FailsafeConfig{{Retry: &common.RetryPolicyConfig{MaxAttempts: 1}}},
	}, rlr, upr, mt, nil)
	require.NoError(t, err)
	ntw.Bootstrap(ctx)
	time.Sleep(100 * time.Millisecond)
	upr.OverrideOrderForTest(util.EvmNetworkId(123), ids...)
	// The mocked chain's head: the range checks need a known latest block.
	for _, u := range upr.GetNetworkUpstreams(ctx, util.EvmNetworkId(123)) {
		u.EvmStatePoller().SuggestLatestBlock(qtLastNo)
		u.EvmStatePoller().SuggestFinalizedBlock(qtLastNo)
	}
	return ntw
}

// queryJson sends a JSON-RPC eth_query* request through the network and
// returns its result, or the JSON-RPC error code the HTTP server renders.
func queryJson(t *testing.T, ctx context.Context, ntw *Network, method string, params string) (map[string]interface{}, int) {
	t.Helper()
	req := common.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":%s}`, method, params)))
	resp, err := ntw.Forward(ctx, req)
	if err != nil {
		translated := common.TranslateToJsonRpcException(err)
		jre, ok := translated.(*common.ErrJsonRpcExceptionInternal)
		require.True(t, ok, "error must render as a JSON-RPC error: %v", err)
		return nil, int(jre.NormalizedCode())
	}
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	var result map[string]interface{}
	require.NoError(t, sonic.Unmarshal(jrr.GetResultBytes(), &result))
	return result, 0
}

func qtData(result map[string]interface{}, key string) []map[string]interface{} {
	rows := result["data"].(map[string]interface{})[key].([]interface{})
	out := make([]map[string]interface{}, len(rows))
	for i, r := range rows {
		out[i] = r.(map[string]interface{})
	}
	return out
}

func qtField(rows []map[string]interface{}, key string) []interface{} {
	out := make([]interface{}, len(rows))
	for i, r := range rows {
		out[i] = r[key]
	}
	return out
}

func TestNetworkQuery_Shim(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	mockQueryChain(0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ntw := setupQueryTestNetwork(t, ctx, &common.EvmQueryShimConfig{Enabled: util.BoolPtr(true)})

	t.Run("blocks page ends at target with real block references", func(t *testing.T) {
		result, code := queryJson(t, ctx, ntw, "eth_queryBlocks",
			`[{"fromBlock":"0x64","toBlock":"0x66","target":"0x2","fields":{"blocks":["number","miner"]},"filter":{"miner":[]}}]`)
		require.Zero(t, code)
		blocks := qtData(result, "blocks")
		assert.Equal(t, []interface{}{"0x64", "0x65"}, qtField(blocks, "number"))
		assert.Equal(t, map[string]interface{}{"number": "0x65", "miner": qtMiner}, blocks[1], "only selected fields")
		assert.Equal(t, map[string]interface{}{"number": "0x64", "hash": qtHash(0x64), "parentHash": qtHash(0x63)}, result["fromBlock"])
		assert.Equal(t, map[string]interface{}{"number": "0x66", "hash": qtHash(0x66), "parentHash": qtHash(0x65)}, result["toBlock"])
		assert.Equal(t, map[string]interface{}{"number": "0x65", "hash": qtHash(0x65), "parentHash": qtHash(0x64)}, result["cursorBlock"])

		// The next page resumes after cursorBlock and runs to toBlock.
		result, code = queryJson(t, ctx, ntw, "eth_queryBlocks",
			`[{"fromBlock":"0x66","toBlock":"0x66","target":"0x2","fields":{"blocks":["number"]}}]`)
		require.Zero(t, code)
		assert.Equal(t, []interface{}{"0x66"}, qtField(qtData(result, "blocks"), "number"))
		assert.Equal(t, "0x66", result["cursorBlock"].(map[string]interface{})["number"])
	})

	t.Run("transactions desc with receipt fields, selector filter and blocks relation", func(t *testing.T) {
		result, code := queryJson(t, ctx, ntw, "eth_queryTransactions",
			`[{"fromBlock":"0x66","toBlock":"0x64","order":"desc","filter":{"from":["`+qtAddrA+`","`+qtAddrC+`"]},
			  "fields":{"transactions":["blockNumber","transactionIndex","status","contractAddress"],"blocks":["number","timestamp"]}}]`)
		require.Zero(t, code)
		txs := qtData(result, "transactions")
		assert.Equal(t, []interface{}{"0x66", "0x66", "0x65", "0x65", "0x64", "0x64"}, qtField(txs, "blockNumber"))
		assert.Equal(t, []interface{}{"0x1", "0x0", "0x1", "0x0", "0x1", "0x0"}, qtField(txs, "transactionIndex"), "desc reverses within a block")
		assert.Equal(t, []interface{}{"0x1", "0x1", "0x0", "0x1", "0x1", "0x1"}, qtField(txs, "status"))
		assert.Equal(t, []interface{}{qtAddrB, nil, qtAddrB, nil, qtAddrB, nil}, qtField(txs, "contractAddress"))
		assert.Equal(t, []interface{}{"0x66", "0x65", "0x64"}, qtField(qtData(result, "blocks"), "number"))
		assert.Equal(t, "0x64", result["cursorBlock"].(map[string]interface{})["number"])

		result, code = queryJson(t, ctx, ntw, "eth_queryTransactions",
			`[{"fromBlock":"0x64","toBlock":"0x64","filter":{"selector":"0xa9059cbb"},"fields":{"transactions":["hash"]}}]`)
		require.Zero(t, code)
		assert.Equal(t, []interface{}{qtTxHash(0x64, 0)}, qtField(qtData(result, "transactions"), "hash"))
	})

	t.Run("logs with the transactions relation joined once", func(t *testing.T) {
		result, code := queryJson(t, ctx, ntw, "eth_queryLogs",
			`[{"fromBlock":"0x64","toBlock":"0x66","target":"0x1","filter":{"address":"`+qtToken+`","topics":[["`+qtTopic+`"]]},
			  "fields":{"logs":["blockNumber","logIndex","data","transactionHash"],"transactions":["hash","status"]}}]`)
		require.Zero(t, code)
		logs := qtData(result, "logs")
		assert.Equal(t, []interface{}{"0x01", "0x02"}, qtField(logs, "data"), "the page holds the whole first block")
		assert.Equal(t, []interface{}{qtTxHash(0x64, 0)}, qtField(qtData(result, "transactions"), "hash"))
		assert.Equal(t, "0x64", result["cursorBlock"].(map[string]interface{})["number"])
	})

	t.Run("traces fall back to debug_traceBlockByNumber and omit reverted frames", func(t *testing.T) {
		result, code := queryJson(t, ctx, ntw, "eth_queryTraces",
			`[{"fromBlock":"0x64","toBlock":"0x64","fields":{"traces":["transactionIndex","traceAddress","reverted","type"]}}]`)
		require.Zero(t, code)
		traces := qtData(result, "traces")
		require.Len(t, traces, 4)
		assert.Equal(t, []interface{}{[]interface{}{}, []interface{}{float64(1)}, []interface{}{}, []interface{}{float64(1)}}, qtField(traces, "traceAddress"))
		assert.Equal(t, []interface{}{false, false, false, false}, qtField(traces, "reverted"))

		result, code = queryJson(t, ctx, ntw, "eth_queryTraces",
			`[{"fromBlock":"0x64","toBlock":"0x64","order":"desc","filter":{"includeReverted":true},"fields":{"traces":["transactionIndex","traceAddress","reverted","error"]}}]`)
		require.Zero(t, code)
		traces = qtData(result, "traces")
		require.Len(t, traces, 8)
		assert.Equal(t, []interface{}{float64(1)}, traces[0]["traceAddress"], "desc reverses frames too")
		assert.Equal(t, []interface{}{float64(0), float64(0)}, traces[1]["traceAddress"])
		assert.Equal(t, true, traces[1]["reverted"], "a frame below a failed call is reverted")
		assert.Nil(t, traces[1]["error"])
		assert.Equal(t, "execution reverted", traces[2]["error"])
	})

	t.Run("transfers skip zero-value frames", func(t *testing.T) {
		result, code := queryJson(t, ctx, ntw, "eth_queryTransfers",
			`[{"fromBlock":"0x65","toBlock":"0x65","fields":{"transfers":["from","to","value","type"],"blocks":["number"]}}]`)
		require.Zero(t, code)
		transfers := qtData(result, "transfers")
		require.Len(t, transfers, 2)
		assert.Equal(t, []interface{}{"0x10", "0x10"}, qtField(transfers, "value"))
		assert.Equal(t, []interface{}{"0x65"}, qtField(qtData(result, "blocks"), "number"))
	})

	t.Run("errors carry the MIP-16 codes", func(t *testing.T) {
		for _, tc := range []struct {
			name, method, params string
			code                 int
		}{
			{"inverted asc range", "eth_queryBlocks", `[{"fromBlock":"0x66","toBlock":"0x64"}]`, -32602},
			{"inverted desc range", "eth_queryLogs", `[{"fromBlock":"0x64","toBlock":"0x66","order":"desc"}]`, -32602},
			{"params not an array", "eth_queryLogs", `{"fromBlock":"0x64"}`, -32602},
			{"unknown request key", "eth_queryTraces", `[{"fromBlock":"0x64","limit":"0x1"}]`, -32602},
			{"block above the chain head", "eth_queryTransactions", `[{"fromBlock":"0x64","toBlock":"0xffffffff"}]`, -32001},
		} {
			_, code := queryJson(t, ctx, ntw, tc.method, tc.params)
			assert.Equal(t, tc.code, code, tc.name)
		}
	})

	t.Run("a page never crosses a reorg", func(t *testing.T) {
		// 0x68's parentHash is not 0x67's hash: the page ends at 0x67 and the
		// next page starts on the other fork.
		result, code := queryJson(t, ctx, ntw, "eth_queryBlocks", `[{"fromBlock":"0x65","toBlock":"0x69","fields":{"blocks":["number"]}}]`)
		require.Zero(t, code)
		assert.Equal(t, []interface{}{"0x65", "0x66", "0x67"}, qtField(qtData(result, "blocks"), "number"))
		assert.Equal(t, map[string]interface{}{"number": "0x67", "hash": qtHash(0x67), "parentHash": qtHash(0x66)}, result["cursorBlock"])
		assert.Equal(t, "0x69", result["toBlock"].(map[string]interface{})["number"])

		// In desc order the same break ends the page at 0x68.
		result, code = queryJson(t, ctx, ntw, "eth_queryBlocks", `[{"fromBlock":"0x69","toBlock":"0x65","order":"desc","fields":{"blocks":["number"]}}]`)
		require.Zero(t, code)
		assert.Equal(t, []interface{}{"0x69", "0x68"}, qtField(qtData(result, "blocks"), "number"))

		// A log whose blockHash is not its block's header hash: the page ends
		// before that block.
		result, code = queryJson(t, ctx, ntw, "eth_queryLogs", `[{"fromBlock":"0x68","toBlock":"0x69","fields":{"logs":["blockNumber","data"]}}]`)
		require.Zero(t, code)
		assert.Empty(t, qtData(result, "logs"))
		assert.Equal(t, "0x68", result["cursorBlock"].(map[string]interface{})["number"])

		// A page whose first block is not consistent has no block-aligned page.
		_, code = queryJson(t, ctx, ntw, "eth_queryLogs", `[{"fromBlock":"0x69","toBlock":"0x69"}]`)
		assert.Equal(t, -32005, code)
	})

	t.Run("a lagging upstream's null never hides an available block", func(t *testing.T) {
		// rpc2 is selected first and lags: it answers null for 0x66, which
		// rpc1 has. The page asks the other upstream and completes.
		var lagged atomic.Int32
		for n := uint64(qtFirstNo - 1); n <= 0x66; n++ {
			ref := fmt.Sprintf(`"0x%x"`, n)
			var block interface{} = qtBlock(n)
			if n == 0x66 {
				block = nil
			}
			gock.New("http://rpc2.localhost").Post("").Persist().
				Filter(func(r *http.Request) bool {
					body := util.SafeReadBody(r)
					if !strings.Contains(body, "eth_getBlockByNumber") || !strings.Contains(body, ref) {
						return false
					}
					if block == nil {
						lagged.Add(1)
					}
					return true
				}).
				Reply(200).JSON(qtReply(block))
		}
		lagging := setupQueryTestNetwork(t, ctx, &common.EvmQueryShimConfig{Enabled: util.BoolPtr(true)}, "rpc2", "rpc1")

		result, code := queryJson(t, ctx, lagging, "eth_queryBlocks", `[{"fromBlock":"0x64","toBlock":"0x66","fields":{"blocks":["number","hash"]}}]`)
		require.Zero(t, code)
		assert.Equal(t, []interface{}{"0x64", "0x65", "0x66"}, qtField(qtData(result, "blocks"), "number"))
		assert.Equal(t, map[string]interface{}{"number": "0x66", "hash": qtHash(0x66), "parentHash": qtHash(0x65)}, result["cursorBlock"])
		assert.Positive(t, lagged.Load(), "rpc2 must have been asked for 0x66 and answered null")
	})
}

func TestNetworkQuery_ShimLimits(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	mockQueryChain(time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	t.Run("no upstream serves the method", func(t *testing.T) {
		ntw := setupQueryTestNetwork(t, ctx, &common.EvmQueryShimConfig{Enabled: util.BoolPtr(true), AllowedMethods: []string{"eth_queryLogs"}})
		_, code := queryJson(t, ctx, ntw, "eth_queryBlocks", `[{"fromBlock":"0x64","toBlock":"0x64"}]`)
		assert.Equal(t, -32004, code)
	})

	t.Run("budget ends before fromBlock completes", func(t *testing.T) {
		ntw := setupQueryTestNetwork(t, ctx, &common.EvmQueryShimConfig{Enabled: util.BoolPtr(true), MaxPageDuration: common.Duration(200 * time.Millisecond)})
		_, code := queryJson(t, ctx, ntw, "eth_queryBlocks", `[{"fromBlock":"0x64","toBlock":"0x66"}]`)
		assert.Equal(t, -32005, code)
	})
}
