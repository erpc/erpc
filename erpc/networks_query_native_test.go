package erpc

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// JSON-RPC eth_query* requests to upstreams that serve MIP-16 themselves
// (evm.nativeQueryMethods): sent unchanged, with the native QueryService upstreams
// and the query shim as the fallback.

const qnLogsParams = `[{"fromBlock":"0x64","toBlock":"0x66","target":"0x1","filter":{"address":"` + qtToken + `","topics":[["` + qtTopic + `"]]},` +
	`"fields":{"logs":["blockNumber","logIndex","data","transactionHash"],"transactions":["hash","status"]}}]`

// qnPage is the page the native upstream answers with. Its data differs from
// the mocked chain, so a test can tell which path answered.
func qnPage() map[string]interface{} {
	header := func(n uint64) map[string]interface{} {
		return map[string]interface{}{"number": fmt.Sprintf("0x%x", n), "hash": qtHash(n), "parentHash": qtHash(n - 1)}
	}
	return map[string]interface{}{
		"data": map[string]interface{}{
			"logs": []interface{}{map[string]interface{}{
				"blockNumber": "0x64", "logIndex": "0x0", "data": "0xaa", "transactionHash": qtTxHash(0x64, 0),
			}},
		},
		"fromBlock":   header(0x64),
		"toBlock":     header(0x66),
		"cursorBlock": header(0x64),
	}
}

// mockNativeQuery answers eth_queryLogs on rpc1 with reply, and counts the
// eth_queryLogs and eth_getLogs requests rpc1 receives. The last
// eth_queryLogs body is stored in body.
func mockNativeQuery(reply map[string]interface{}, queries, getLogs *atomic.Int32, body *atomic.Value) {
	gock.New("http://rpc1.localhost").Post("").Persist().
		Filter(func(r *http.Request) bool {
			b := util.SafeReadBody(r)
			if !strings.Contains(b, "eth_queryLogs") {
				return false
			}
			queries.Add(1)
			body.Store(b)
			return true
		}).
		Reply(200).JSON(reply)
	gock.New("http://rpc1.localhost").Post("").Persist().
		Filter(func(r *http.Request) bool {
			if strings.Contains(util.SafeReadBody(r), "eth_getLogs") {
				getLogs.Add(1)
			}
			return false
		}).
		Reply(200).JSON(qtReply(nil))
}

var qnMethods = []string{"eth_queryBlocks", "eth_queryTransactions", "eth_queryLogs"}

func TestNetworkQuery_JsonRpcNative(t *testing.T) {
	t.Run("the request goes to the upstream unchanged and its page is returned", func(t *testing.T) {
		util.ResetGock()
		t.Cleanup(util.ResetGock)
		util.SetupMocksForEvmStatePoller()
		var queries, getLogs atomic.Int32
		var body atomic.Value
		mockNativeQuery(qtReply(qnPage()), &queries, &getLogs, &body)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ntw := setupQueryTestNetworkWith(t, ctx, func(cfg *common.UpstreamConfig) {
			cfg.Evm.NativeQueryMethods = qnMethods
		})

		result, code := queryJson(t, ctx, ntw, "eth_queryLogs", qnLogsParams)
		require.Zero(t, code)
		assert.Equal(t, []interface{}{"0xaa"}, qtField(qtData(result, "logs"), "data"))
		assert.Equal(t, "0x64", result["cursorBlock"].(map[string]interface{})["number"])
		assert.Equal(t, int32(1), queries.Load())
		assert.Zero(t, getLogs.Load(), "the shim must not run")
		sent := body.Load().(string)
		assert.Contains(t, sent, qtToken, "the filter is sent as given")
		assert.Contains(t, sent, `"target":"0x1"`)
	})

	t.Run("an upstream error falls back to the query shim", func(t *testing.T) {
		util.ResetGock()
		t.Cleanup(util.ResetGock)
		util.SetupMocksForEvmStatePoller()
		var queries, getLogs atomic.Int32
		var body atomic.Value
		mockNativeQuery(map[string]interface{}{"jsonrpc": "2.0", "id": 1,
			"error": map[string]interface{}{"code": -32004, "message": "method not supported yet"}}, &queries, &getLogs, &body)
		mockQueryChain(0)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ntw := setupQueryTestNetworkWith(t, ctx, func(cfg *common.UpstreamConfig) {
			cfg.Evm.NativeQueryMethods = qnMethods
			cfg.Evm.QueryShim = &common.EvmQueryShimConfig{Enabled: util.BoolPtr(true)}
		})

		result, code := queryJson(t, ctx, ntw, "eth_queryLogs", qnLogsParams)
		require.Zero(t, code)
		assert.Equal(t, []interface{}{"0x01", "0x02"}, qtField(qtData(result, "logs"), "data"), "the shim answers from the chain")
		assert.GreaterOrEqual(t, queries.Load(), int32(1))
		assert.Positive(t, getLogs.Load())
	})

	t.Run("without a fallback the upstream's error is returned", func(t *testing.T) {
		util.ResetGock()
		t.Cleanup(util.ResetGock)
		util.SetupMocksForEvmStatePoller()
		var queries, getLogs atomic.Int32
		var body atomic.Value
		mockNativeQuery(map[string]interface{}{"jsonrpc": "2.0", "id": 1,
			"error": map[string]interface{}{"code": -32000, "message": "upstream query failed"}}, &queries, &getLogs, &body)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ntw := setupQueryTestNetworkWith(t, ctx, func(cfg *common.UpstreamConfig) {
			cfg.Evm.NativeQueryMethods = qnMethods
		})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_queryLogs","params":` + qnLogsParams + `}`))
		_, err := ntw.Forward(ctx, req)
		require.Error(t, err)
		var jre *common.ErrJsonRpcExceptionInternal
		require.ErrorAs(t, common.TranslateToJsonRpcException(err), &jre)
		assert.NotEqual(t, common.JsonRpcErrorNumber(-32004), jre.NormalizedCode(), "the upstream's error, not the executor's -32004: %v", err)
		assert.Contains(t, err.Error(), "upstream query failed")
		assert.GreaterOrEqual(t, queries.Load(), int32(1))
	})

	t.Run("an upstream that ignores the method is not asked", func(t *testing.T) {
		util.ResetGock()
		t.Cleanup(util.ResetGock)
		util.SetupMocksForEvmStatePoller()
		var queries, getLogs atomic.Int32
		var body atomic.Value
		mockNativeQuery(qtReply(qnPage()), &queries, &getLogs, &body)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ntw := setupQueryTestNetworkWith(t, ctx, func(cfg *common.UpstreamConfig) {
			cfg.Evm.NativeQueryMethods = qnMethods
			cfg.IgnoreMethods = []string{"eth_queryLogs"}
		})

		_, code := queryJson(t, ctx, ntw, "eth_queryLogs", qnLogsParams)
		assert.Equal(t, -32004, code)
		assert.Zero(t, queries.Load())
	})

	t.Run("only listed methods are sent natively", func(t *testing.T) {
		util.ResetGock()
		t.Cleanup(util.ResetGock)
		util.SetupMocksForEvmStatePoller()
		var queries, getLogs atomic.Int32
		var body atomic.Value
		mockNativeQuery(qtReply(qnPage()), &queries, &getLogs, &body)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ntw := setupQueryTestNetworkWith(t, ctx, func(cfg *common.UpstreamConfig) {
			cfg.Evm.NativeQueryMethods = []string{"eth_queryBlocks"}
		})

		_, code := queryJson(t, ctx, ntw, "eth_queryLogs", qnLogsParams)
		assert.Equal(t, -32004, code)
		assert.Zero(t, queries.Load())
	})
}

// gRPC QueryService and evm.nativeQueryMethods upstreams share one selection-policy
// order on JSON-RPC calls; gRPC streams use only the gRPC ones.
func TestQueryExecutor_NativeUpstreamOrder(t *testing.T) {
	grpcUps := newTestQueryUpstream(t, "grpc", &fakeGrpcBdsClient{queryClient: &fakeQueryServiceClient{}})
	jsonUps := newTestQueryUpstream(t, "json", nil)
	jsonUps.Config().Evm.NativeQueryMethods = qnMethods
	plainUps := newTestQueryUpstream(t, "plain", nil)
	ids := func(ups []common.Upstream) []string {
		out := make([]string, len(ups))
		for i, u := range ups {
			out[i] = u.Id()
		}
		return out
	}
	jsonReq := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_queryLogs","params":[{}]}`))

	qe := newTestQueryExecutor(t, "eth_queryLogs", jsonUps, plainUps, grpcUps)
	assert.Equal(t, []string{"grpc"}, ids(qe.nativeUpstreams(context.Background(), "eth_queryLogs")), "gRPC stream")
	qe.jsonReq = jsonReq
	assert.Equal(t, []string{"json", "grpc"}, ids(qe.nativeUpstreams(context.Background(), "eth_queryLogs")))

	qe = newTestQueryExecutor(t, "eth_queryLogs", grpcUps, plainUps, jsonUps)
	qe.jsonReq = jsonReq
	assert.Equal(t, []string{"grpc", "json"}, ids(qe.nativeUpstreams(context.Background(), "eth_queryLogs")), "a gRPC upstream ranked first stays first")
}

func TestEvmUpstreamConfig_ServesNativeQuery(t *testing.T) {
	var nilCfg *common.EvmUpstreamConfig
	assert.False(t, nilCfg.ServesNativeQuery("eth_queryLogs"))
	assert.False(t, (&common.EvmUpstreamConfig{}).ServesNativeQuery("eth_queryLogs"), "empty lists no method")
	cfg := &common.EvmUpstreamConfig{NativeQueryMethods: []string{"eth_queryLogs", "eth_queryBl*"}}
	assert.True(t, cfg.ServesNativeQuery("eth_queryLogs"))
	assert.True(t, cfg.ServesNativeQuery("eth_queryBlocks"), "wildcards match")
	assert.False(t, cfg.ServesNativeQuery("eth_queryTraces"))

	copied := cfg.Copy()
	copied.NativeQueryMethods[0] = "x"
	assert.Equal(t, "eth_queryLogs", cfg.NativeQueryMethods[0], "Copy is deep")
}
