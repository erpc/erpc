package erpc

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"
)

func baseBatchConfig() *common.Config {
	return &common.Config{
		Server: &common.ServerConfig{IncludeErrorDetails: &common.TRUE},
		Projects: []*common.ProjectConfig{
			{
				Id: "test_project",
				Networks: []*common.NetworkConfig{
					{
						Architecture: common.ArchitectureEvm,
						Evm:          &common.EvmNetworkConfig{ChainId: 123},
					},
				},
				Upstreams: []*common.UpstreamConfig{
					{
						Type:     common.UpstreamTypeEvm,
						Endpoint: "http://rpc1.localhost",
						Evm:      &common.EvmUpstreamConfig{ChainId: 123},
					},
				},
			},
		},
		RateLimiters: &common.RateLimiterConfig{},
	}
}

func rateLimitConfig(budgetId string) *common.RateLimiterConfig {
	return &common.RateLimiterConfig{
		Budgets: []*common.RateLimitBudgetConfig{
			{
				Id: budgetId,
				Rules: []*common.RateLimitRuleConfig{
					{Method: "eth_call", MaxCount: 0, Period: common.RateLimitPeriodSecond},
				},
			},
		},
	}
}

func setupBatchHandler(t *testing.T, cfg *common.Config) (*HttpServer, *PreparedProject, context.Context, func()) {
	t.Helper()
	util.SetupMocksForEvmStatePoller()

	logger := log.Logger
	ctx, cancel := context.WithCancel(context.Background())

	ssr, err := data.NewSharedStateRegistry(ctx, &logger, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: "memory",
			Memory: &common.MemoryConnectorConfig{
				MaxItems:     100_000,
				MaxTotalSize: "1GB",
			},
		},
	})
	require.NoError(t, err)

	erpcInstance, err := NewERPC(ctx, &logger, ssr, nil, cfg)
	require.NoError(t, err)
	erpcInstance.Bootstrap(ctx)

	server, err := NewHttpServer(ctx, &logger, cfg.Server, cfg.HealthCheck, cfg.Admin, erpcInstance)
	require.NoError(t, err)

	project, err := erpcInstance.GetProject("test_project")
	require.NoError(t, err)

	cleanup := func() {
		cancel()
		util.AssertNoPendingMocks(t, 0)
		util.ResetGock()
	}

	return server, project, ctx, cleanup
}

func validBatchRequests(t *testing.T) []json.RawMessage {
	callObj := map[string]interface{}{
		"to":   "0x0000000000000000000000000000000000000001",
		"data": "0x",
	}
	return []json.RawMessage{
		buildEthCallRaw(t, 1, callObj, "latest"),
		buildEthCallRaw(t, 2, callObj, "latest"),
	}
}

func invalidBatchRequests(t *testing.T) []json.RawMessage {
	callObj := map[string]interface{}{
		"to":    "0x0000000000000000000000000000000000000001",
		"data":  "0x",
		"value": "0x1",
	}
	return []json.RawMessage{
		buildEthCallRaw(t, 1, callObj, "latest"),
		buildEthCallRaw(t, 2, callObj, "latest"),
	}
}

func buildEthCallRaw(t *testing.T, id interface{}, callObj map[string]interface{}, blockParam interface{}) json.RawMessage {
	t.Helper()
	params := []interface{}{callObj}
	if blockParam != nil {
		params = append(params, blockParam)
	}
	body := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "eth_call",
		"params":  params,
	}
	raw, err := common.SonicCfg.Marshal(body)
	require.NoError(t, err)
	return raw
}

func defaultBatchInfo() *ethCallBatchInfo {
	return &ethCallBatchInfo{networkId: "evm:123", blockRef: "latest", blockParam: "latest"}
}

func runHandle(t *testing.T, ctx context.Context, server *HttpServer, project *PreparedProject, batchInfo *ethCallBatchInfo, requests []json.RawMessage, headers http.Header) (bool, []interface{}) {
	t.Helper()
	responses := make([]interface{}, len(requests))
	startedAt := time.Now()
	req := httptest.NewRequest("POST", "http://localhost", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	if headers != nil {
		req.Header = headers
	}

	handled := server.handleEthCallBatchAggregation(
		ctx,
		&startedAt,
		req,
		project,
		log.Logger,
		batchInfo,
		requests,
		req.Header,
		req.URL.Query(),
		responses,
	)

	return handled, responses
}

type batchNetworkForward func(context.Context, *Network, *common.NormalizedRequest) (*common.NormalizedResponse, error)
type batchProjectForward func(context.Context, *PreparedProject, *Network, *common.NormalizedRequest) (*common.NormalizedResponse, error)
type batchNewJsonRpcResponse func(id interface{}, result interface{}, rpcError *common.ErrJsonRpcExceptionExternal) (*common.JsonRpcResponse, error)

func withBatchStubs(t *testing.T, network batchNetworkForward, project batchProjectForward, newResp batchNewJsonRpcResponse) {
	t.Helper()
	origNetwork := forwardBatchNetwork
	origProject := forwardBatchProject
	origNew := newBatchJsonRpcResponse
	if network != nil {
		forwardBatchNetwork = network
	}
	if project != nil {
		forwardBatchProject = project
	}
	if newResp != nil {
		newBatchJsonRpcResponse = newResp
	}
	t.Cleanup(func() {
		forwardBatchNetwork = origNetwork
		forwardBatchProject = origProject
		newBatchJsonRpcResponse = origNew
	})
}

func fallbackResponse(t *testing.T, req *common.NormalizedRequest) *common.NormalizedResponse {
	t.Helper()
	jrr := mustJsonRpcResponse(t, req.ID(), "0xfeed", nil)
	return common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
}

func mustJsonRpcResponse(t *testing.T, id interface{}, result interface{}, rpcErr *common.ErrJsonRpcExceptionExternal) *common.JsonRpcResponse {
	t.Helper()
	jrr, err := common.NewJsonRpcResponse(id, result, rpcErr)
	require.NoError(t, err)
	return jrr
}

func encodeAggregate3Results(results []evm.Multicall3Result) string {
	encoded := encodeAggregate3ResultsBytes(results)
	return "0x" + hex.EncodeToString(encoded)
}

func encodeAggregate3ResultsBytes(results []evm.Multicall3Result) []byte {
	// Offsets are relative to start of array content (after length word),
	// so the offset table size is just N*32 (not including the length word)
	headSize := 32 * len(results)
	offsets := make([]uint64, len(results))
	elems := make([][]byte, len(results))
	cur := uint64(headSize)

	for i, res := range results {
		elems[i] = encodeAggregate3ResultElement(res)
		offsets[i] = cur
		cur += uint64(len(elems[i]))
	}

	array := make([]byte, 0, int(cur))
	array = append(array, encodeUint64(uint64(len(results)))...)
	for _, off := range offsets {
		array = append(array, encodeUint64(off)...)
	}
	for _, elem := range elems {
		array = append(array, elem...)
	}

	out := make([]byte, 0, 32+len(array))
	out = append(out, encodeUint64(32)...)
	out = append(out, array...)
	return out
}

func encodeAggregate3ResultElement(result evm.Multicall3Result) []byte {
	head := make([]byte, 0, 64)
	head = append(head, encodeBool(result.Success)...)
	head = append(head, encodeUint64(64)...)
	tail := encodeBytes(result.ReturnData)
	return append(head, tail...)
}

func encodeUint64(value uint64) []byte {
	out := make([]byte, 32)
	out[24] = byte(value >> 56)
	out[25] = byte(value >> 48)
	out[26] = byte(value >> 40)
	out[27] = byte(value >> 32)
	out[28] = byte(value >> 24)
	out[29] = byte(value >> 16)
	out[30] = byte(value >> 8)
	out[31] = byte(value)
	return out
}

func encodeBool(value bool) []byte {
	out := make([]byte, 32)
	if value {
		out[31] = 1
	}
	return out
}

func encodeBytes(data []byte) []byte {
	out := make([]byte, 0, 32+len(data)+32)
	out = append(out, encodeUint64(uint64(len(data)))...)
	out = append(out, data...)
	pad := (32 - (len(data) % 32)) % 32
	if pad > 0 {
		out = append(out, make([]byte, pad)...)
	}
	return out
}

func setupBatchHandlerWithCache(t *testing.T, cfg *common.Config) (*HttpServer, *PreparedProject, *Network, context.Context, func()) {
	t.Helper()
	server, project, ctx, cleanup := setupBatchHandler(t, cfg)

	network, err := project.GetNetwork(ctx, "evm:123")
	require.NoError(t, err)

	return server, project, network, ctx, cleanup
}

func createCachedResponse(t *testing.T, req *common.NormalizedRequest, result string) *common.NormalizedResponse {
	t.Helper()
	jrr, err := common.NewJsonRpcResponse(req.ID(), result, nil)
	require.NoError(t, err)
	resp := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
	resp.SetFromCache(true)
	return resp
}
