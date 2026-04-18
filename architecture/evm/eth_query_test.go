package evm

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type queryTestNetwork struct {
	cfg       *common.NetworkConfig
	latest    int64
	finalized int64
	forwardFn func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error)
}

func (n *queryTestNetwork) Id() string                               { return "evm:1" }
func (n *queryTestNetwork) Label() string                            { return "evm:1" }
func (n *queryTestNetwork) ProjectId() string                        { return "test-project" }
func (n *queryTestNetwork) Architecture() common.NetworkArchitecture { return common.ArchitectureEvm }
func (n *queryTestNetwork) Config() *common.NetworkConfig            { return n.cfg }
func (n *queryTestNetwork) Logger() *zerolog.Logger {
	logger := zerolog.Nop()
	return &logger
}
func (n *queryTestNetwork) GetMethodMetrics(method string) common.TrackedMetrics { return nil }
func (n *queryTestNetwork) Forward(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	return n.forwardFn(ctx, req)
}
func (n *queryTestNetwork) GetFinality(ctx context.Context, req *common.NormalizedRequest, resp *common.NormalizedResponse) common.DataFinalityState {
	return common.DataFinalityStateFinalized
}
func (n *queryTestNetwork) EvmHighestLatestBlockNumber(ctx context.Context) int64 { return n.latest }
func (n *queryTestNetwork) EvmHighestFinalizedBlockNumber(ctx context.Context) int64 {
	return n.finalized
}
func (n *queryTestNetwork) EvmLeaderUpstream(ctx context.Context) common.Upstream { return nil }

type queryTestUpstream struct {
	supported bool
	cfg       *common.UpstreamConfig
}

func (u *queryTestUpstream) Id() string           { return "upstream-1" }
func (u *queryTestUpstream) VendorName() string   { return "test" }
func (u *queryTestUpstream) NetworkId() string    { return "evm:1" }
func (u *queryTestUpstream) NetworkLabel() string { return "evm:1" }
func (u *queryTestUpstream) Config() *common.UpstreamConfig {
	if u.cfg != nil {
		return u.cfg
	}
	return &common.UpstreamConfig{Id: "upstream-1"}
}
func (u *queryTestUpstream) Logger() *zerolog.Logger {
	logger := zerolog.Nop()
	return &logger
}
func (u *queryTestUpstream) Vendor() common.Vendor         { return nil }
func (u *queryTestUpstream) Tracker() common.HealthTracker { return nil }
func (u *queryTestUpstream) Forward(ctx context.Context, nq *common.NormalizedRequest, byPassMethodExclusion bool) (*common.NormalizedResponse, error) {
	return nil, nil
}
func (u *queryTestUpstream) Cordon(method string, reason string)   {}
func (u *queryTestUpstream) Uncordon(method string, reason string) {}
func (u *queryTestUpstream) IgnoreMethod(method string)            {}
func (u *queryTestUpstream) ShouldHandleMethod(method string) (bool, error) {
	return u.supported, nil
}

type queryTestConfigUpstream struct {
	cfg *common.UpstreamConfig
}

func (u *queryTestConfigUpstream) Id() string                     { return "upstream-config" }
func (u *queryTestConfigUpstream) VendorName() string             { return "test" }
func (u *queryTestConfigUpstream) NetworkId() string              { return "evm:1" }
func (u *queryTestConfigUpstream) NetworkLabel() string           { return "evm:1" }
func (u *queryTestConfigUpstream) Config() *common.UpstreamConfig { return u.cfg }
func (u *queryTestConfigUpstream) Logger() *zerolog.Logger {
	logger := zerolog.Nop()
	return &logger
}
func (u *queryTestConfigUpstream) Vendor() common.Vendor         { return nil }
func (u *queryTestConfigUpstream) Tracker() common.HealthTracker { return nil }
func (u *queryTestConfigUpstream) Forward(ctx context.Context, nq *common.NormalizedRequest, byPassMethodExclusion bool) (*common.NormalizedResponse, error) {
	return nil, nil
}
func (u *queryTestConfigUpstream) Cordon(method string, reason string)   {}
func (u *queryTestConfigUpstream) Uncordon(method string, reason string) {}
func (u *queryTestConfigUpstream) IgnoreMethod(method string)            {}

func TestParseQueryRequest_ResolvesCursorAndSelections(t *testing.T) {
	network := &queryTestNetwork{
		cfg: &common.NetworkConfig{
			Architecture: common.ArchitectureEvm,
			Evm:          &common.EvmNetworkConfig{},
		},
		latest:    120,
		finalized: 118,
	}
	qs := &common.EvmQueryShimConfig{
		DefaultLimit:  25,
		MaxLimit:      500,
		MaxBlockRange: 1000,
	}

	req := common.NewNormalizedRequest([]byte(`{
		"jsonrpc":"2.0",
		"id":1,
		"method":"eth_queryTransactions",
		"params":[{
			"fromBlock":"earliest",
			"toBlock":"latest",
			"order":"asc",
			"cursor":{"number":"0x2","hash":"0x01","parentHash":"0x00"},
			"filter":{"from":["0x0000000000000000000000000000000000000001"]},
			"fields":{"transactions":["hash","from"],"blocks":["number"]}
		}]
	}`))

	parsed, err := parseQueryRequest(context.Background(), network, qs, req)
	require.NoError(t, err)
	require.NotNil(t, parsed)

	assert.Equal(t, uint64(3), parsed.FromBlock)
	assert.Equal(t, uint64(120), parsed.ToBlock)
	assert.Equal(t, "asc", parsed.Order)
	assert.Equal(t, uint64(25), parsed.Limit)
	require.NotNil(t, parsed.Cursor)
	assert.Equal(t, uint64(2), parsed.Cursor.Number)
	require.NotNil(t, parsed.Filter)
	require.Len(t, parsed.Filter.FromAddresses, 1)
	assert.Equal(t, []string{"hash", "from"}, parsed.Fields.Transactions)
	assert.Equal(t, []string{"number"}, parsed.Fields.Blocks)
}

func TestUpstreamPreForwardEthQuery_PassthroughWhenNoShimEnabled(t *testing.T) {
	nq := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_queryBlocks","params":[{}]}`))
	network := &queryTestNetwork{
		cfg: &common.NetworkConfig{
			Architecture: common.ArchitectureEvm,
			Evm:          &common.EvmNetworkConfig{},
		},
		latest:    10,
		finalized: 10,
	}

	handled, resp, err := upstreamPreForward_eth_query(
		context.Background(),
		network,
		&queryTestConfigUpstream{
			cfg: &common.UpstreamConfig{
				Id:           "query-http-upstream",
				Endpoint:     "https://query-node.example",
				AllowMethods: []string{"eth_query*"},
			},
		},
		nq,
	)

	require.NoError(t, err)
	assert.False(t, handled)
	assert.Nil(t, resp)
}

func TestUpstreamPreForwardEthQuery_ShimsWhenShimEnabled(t *testing.T) {
	enabled := true
	network := &queryTestNetwork{
		cfg: &common.NetworkConfig{
			Architecture: common.ArchitectureEvm,
			Evm:          &common.EvmNetworkConfig{},
		},
		latest:    2,
		finalized: 2,
	}
	network.forwardFn = func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		jrq, err := req.JsonRpcRequest(ctx)
		require.NoError(t, err)
		require.Equal(t, "eth_getBlockByNumber", jrq.Method)

		blockRef, ok := jrq.Params[0].(string)
		require.True(t, ok)
		blockNumber, err := common.HexToUint64(blockRef)
		require.NoError(t, err)

		block := map[string]interface{}{
			"number":       fmt.Sprintf("0x%x", blockNumber),
			"hash":         fmt.Sprintf("0x%064x", blockNumber),
			"parentHash":   fmt.Sprintf("0x%064x", blockNumber-1),
			"timestamp":    "0x1",
			"transactions": []interface{}{},
		}
		jrr, err := common.NewJsonRpcResponse(req.ID(), block, nil)
		require.NoError(t, err)
		return common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr), nil
	}

	nq := common.NewNormalizedRequest([]byte(`{
		"jsonrpc":"2.0",
		"id":1,
		"method":"eth_queryBlocks",
		"params":[{
			"fromBlock":"0x1",
			"toBlock":"0x2",
			"fields":{"blocks":["number","hash"]}
		}]
	}`))

	handled, resp, err := upstreamPreForward_eth_query(context.Background(), network, &queryTestConfigUpstream{
		cfg: &common.UpstreamConfig{
			Id:       "shim-upstream",
			Endpoint: "https://rpc.example",
			Evm: &common.EvmUpstreamConfig{
				QueryShim: &common.EvmQueryShimConfig{
					Enabled:       &enabled,
					DefaultLimit:  100,
					MaxLimit:      1000,
					MaxBlockRange: 1000,
				},
			},
		},
	}, nq)
	require.NoError(t, err)
	require.True(t, handled)
	require.NotNil(t, resp)
}

func TestUpstreamPreForwardEthQuery_ShimsBlocks(t *testing.T) {
	enabled := true
	network := &queryTestNetwork{
		cfg: &common.NetworkConfig{
			Architecture: common.ArchitectureEvm,
			Evm:          &common.EvmNetworkConfig{},
		},
		latest:    2,
		finalized: 2,
	}
	network.forwardFn = func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		jrq, err := req.JsonRpcRequest(ctx)
		require.NoError(t, err)
		require.Equal(t, "eth_getBlockByNumber", jrq.Method)

		blockRef, ok := jrq.Params[0].(string)
		require.True(t, ok)
		blockNumber, err := common.HexToUint64(blockRef)
		require.NoError(t, err)

		block := map[string]interface{}{
			"number":       fmt.Sprintf("0x%x", blockNumber),
			"hash":         fmt.Sprintf("0x%064x", blockNumber),
			"parentHash":   fmt.Sprintf("0x%064x", blockNumber-1),
			"timestamp":    "0x1",
			"transactions": []interface{}{},
		}
		jrr, err := common.NewJsonRpcResponse(req.ID(), block, nil)
		require.NoError(t, err)
		return common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr), nil
	}

	nq := common.NewNormalizedRequest([]byte(`{
		"jsonrpc":"2.0",
		"id":1,
		"method":"eth_queryBlocks",
		"params":[{
			"fromBlock":"0x1",
			"toBlock":"0x2",
			"fields":{"blocks":["number","hash"]}
		}]
	}`))

	handled, resp, err := upstreamPreForward_eth_query(context.Background(), network, &queryTestConfigUpstream{
		cfg: &common.UpstreamConfig{
			Id: "shim-upstream",
			Evm: &common.EvmUpstreamConfig{
				QueryShim: &common.EvmQueryShimConfig{
					Enabled:       &enabled,
					DefaultLimit:  100,
					MaxLimit:      1000,
					MaxBlockRange: 1000,
				},
			},
		},
	}, nq)
	require.NoError(t, err)
	require.True(t, handled)
	require.NotNil(t, resp)

	jrr, err := resp.JsonRpcResponse(context.Background())
	require.NoError(t, err)
	require.NotNil(t, jrr)

	var payload map[string]interface{}
	require.NoError(t, common.SonicCfg.Unmarshal(jrr.GetResultBytes(), &payload))

	data, ok := payload["data"].(map[string]interface{})
	require.True(t, ok)
	blocks, ok := data["blocks"].([]interface{})
	require.True(t, ok)
	require.Len(t, blocks, 2)

	firstBlock, ok := blocks[0].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "0x1", firstBlock["number"])
	assert.Equal(t, fmt.Sprintf("0x%064x", 1), firstBlock["hash"])
	assert.Equal(t, fmt.Sprintf("0x%064x", 0), firstBlock["parentHash"])
	assert.Nil(t, payload["cursorBlock"])
}

func TestUpstreamPreForwardEthQuery_SkipsSubRequests(t *testing.T) {
	enabled := true
	nq := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_queryBlocks","params":[{}]}`))
	nq.SetParentRequestId(123)

	handled, resp, err := upstreamPreForward_eth_query(context.Background(), &queryTestNetwork{
		cfg:       newQueryTestConfig(),
		latest:    10,
		finalized: 9,
	}, &queryTestConfigUpstream{
		cfg: &common.UpstreamConfig{
			Id: "shim-upstream",
			Evm: &common.EvmUpstreamConfig{
				QueryShim: &common.EvmQueryShimConfig{Enabled: &enabled},
			},
		},
	}, nq)
	require.NoError(t, err)
	assert.False(t, handled)
	assert.Nil(t, resp)
}

func TestIsQueryShimMethodAllowed(t *testing.T) {
	enabled := true
	t.Run("NilConfig", func(t *testing.T) {
		assert.False(t, isQueryShimMethodAllowed(nil, "eth_queryBlocks"))
	})
	t.Run("EmptyAllowedMethods_AllowsAll", func(t *testing.T) {
		qs := &common.EvmQueryShimConfig{Enabled: &enabled}
		assert.True(t, isQueryShimMethodAllowed(qs, "eth_queryBlocks"))
		assert.True(t, isQueryShimMethodAllowed(qs, "eth_queryLogs"))
	})
	t.Run("ExplicitAllowedMethods", func(t *testing.T) {
		qs := &common.EvmQueryShimConfig{Enabled: &enabled, AllowedMethods: []string{"eth_queryLogs"}}
		assert.True(t, isQueryShimMethodAllowed(qs, "eth_queryLogs"))
		assert.False(t, isQueryShimMethodAllowed(qs, "eth_queryBlocks"))
	})
	t.Run("WildcardAllowedMethods", func(t *testing.T) {
		qs := &common.EvmQueryShimConfig{Enabled: &enabled, AllowedMethods: []string{"eth_query*"}}
		assert.True(t, isQueryShimMethodAllowed(qs, "eth_queryBlocks"))
		assert.True(t, isQueryShimMethodAllowed(qs, "eth_queryTransactions"))
	})
}

func TestUpstreamPreForwardEthQuery_ShimsWhenUpstreamHasQueryShimConfig(t *testing.T) {
	enabled := true
	network := &queryTestNetwork{
		cfg: &common.NetworkConfig{
			Architecture: common.ArchitectureEvm,
			Evm:          &common.EvmNetworkConfig{},
		},
		latest:    2,
		finalized: 2,
	}
	network.forwardFn = func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		jrq, err := req.JsonRpcRequest(ctx)
		require.NoError(t, err)
		require.Equal(t, "eth_getBlockByNumber", jrq.Method)

		blockRef, ok := jrq.Params[0].(string)
		require.True(t, ok)
		blockNumber, err := common.HexToUint64(blockRef)
		require.NoError(t, err)

		block := map[string]interface{}{
			"number":       fmt.Sprintf("0x%x", blockNumber),
			"hash":         fmt.Sprintf("0x%064x", blockNumber),
			"parentHash":   fmt.Sprintf("0x%064x", blockNumber-1),
			"timestamp":    "0x1",
			"transactions": []interface{}{},
		}
		jrr, err := common.NewJsonRpcResponse(req.ID(), block, nil)
		require.NoError(t, err)
		return common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr), nil
	}

	nq := common.NewNormalizedRequest([]byte(`{
		"jsonrpc":"2.0",
		"id":1,
		"method":"eth_queryBlocks",
		"params":[{
			"fromBlock":"0x1",
			"toBlock":"0x2",
			"fields":{"blocks":["number","hash"]}
		}]
	}`))

	handled, resp, err := upstreamPreForward_eth_query(context.Background(), network, &queryTestConfigUpstream{
		cfg: &common.UpstreamConfig{
			Id:       "http-upstream",
			Endpoint: "https://rpc.example",
			Evm: &common.EvmUpstreamConfig{
				QueryShim: &common.EvmQueryShimConfig{
					Enabled:       &enabled,
					DefaultLimit:  100,
					MaxLimit:      1000,
					MaxBlockRange: 1000,
				},
			},
		},
	}, nq)
	require.NoError(t, err)
	require.True(t, handled)
	require.NotNil(t, resp)
}

func TestResolveBlockTag(t *testing.T) {
	network := &queryTestNetwork{
		cfg:       newQueryTestConfig(),
		latest:    120,
		finalized: 118,
	}

	tests := []struct {
		name    string
		tag     string
		want    uint64
		wantErr bool
	}{
		{name: "DefaultToLatest", tag: "", want: 120},
		{name: "Earliest", tag: "earliest", want: 0},
		{name: "Latest", tag: "latest", want: 120},
		{name: "Finalized", tag: "finalized", want: 118},
		{name: "SafeFallsBackToFinalized", tag: "safe", want: 118},
		{name: "Hex", tag: "0x2a", want: 42},
		{name: "PendingErrors", tag: "pending", wantErr: true},
		{name: "InvalidErrors", tag: "abc", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveBlockTag(context.Background(), network, tt.tag)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestParseQueryRequest_DescAndLimitErrors(t *testing.T) {
	t.Run("DescCursorDecrementsFromBlock", func(t *testing.T) {
		network := &queryTestNetwork{
			cfg: &common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm:          &common.EvmNetworkConfig{},
			},
			latest:    50,
			finalized: 45,
		}
		qs := &common.EvmQueryShimConfig{DefaultLimit: 10, MaxLimit: 100, MaxBlockRange: 100}
		req := common.NewNormalizedRequest([]byte(`{
			"jsonrpc":"2.0","id":1,"method":"eth_queryBlocks",
			"params":[{"fromBlock":"0x1","toBlock":"0xa","order":"desc","cursor":{"number":"0x5"}}]
		}`))

		parsed, err := parseQueryRequest(context.Background(), network, qs, req)
		require.NoError(t, err)
		assert.Equal(t, "desc", parsed.Order)
		assert.Equal(t, uint64(4), parsed.FromBlock)
		assert.Equal(t, uint64(1), parsed.ToBlock)
	})

	t.Run("LimitExceedsMax", func(t *testing.T) {
		network := &queryTestNetwork{
			cfg: &common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm:          &common.EvmNetworkConfig{},
			},
			latest:    10,
			finalized: 10,
		}
		qs := &common.EvmQueryShimConfig{DefaultLimit: 10, MaxLimit: 1, MaxBlockRange: 100}
		_ = qs
		req := common.NewNormalizedRequest([]byte(`{
			"jsonrpc":"2.0","id":1,"method":"eth_queryBlocks",
			"params":[{"fromBlock":"0x1","toBlock":"0x2","limit":"0x2"}]
		}`))

		_, err := parseQueryRequest(context.Background(), network, qs, req)
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeJsonRpcExceptionInternal))
	})

	t.Run("RangeExceedsMax", func(t *testing.T) {
		network := &queryTestNetwork{
			cfg: &common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm:          &common.EvmNetworkConfig{},
			},
			latest:    10,
			finalized: 10,
		}
		qs := &common.EvmQueryShimConfig{DefaultLimit: 10, MaxLimit: 10, MaxBlockRange: 1}
		req := common.NewNormalizedRequest([]byte(`{
			"jsonrpc":"2.0","id":1,"method":"eth_queryBlocks",
			"params":[{"fromBlock":"0x1","toBlock":"0x2"}]
		}`))

		_, err := parseQueryRequest(context.Background(), network, qs, req)
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeJsonRpcExceptionInternal))
	})
}

func TestForwardSubRequestAndFetchBlockRange(t *testing.T) {
	t.Run("ForwardSubRequestPropagatesParentIDAndWritesNull", func(t *testing.T) {
		network := &queryTestNetwork{
			cfg:       newQueryTestConfig(),
			latest:    3,
			finalized: 3,
		}
		network.forwardFn = func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			assert.Equal(t, 55, req.ParentRequestId())
			jrr, err := common.NewJsonRpcResponse(req.ID(), nil, nil)
			require.NoError(t, err)
			return common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr), nil
		}

		result, err := forwardSubRequest(context.Background(), network, 55, "", "eth_getBlockByNumber", []interface{}{"0x1", false})
		require.NoError(t, err)
		assert.Equal(t, []byte("null"), result)
	})

	t.Run("FetchBlockRangePreservesOrderAndSkipsNull", func(t *testing.T) {
		var mu sync.Mutex
		parentIDs := make([]interface{}, 0)
		network := &queryTestNetwork{
			cfg:       newQueryTestConfig(),
			latest:    3,
			finalized: 3,
		}
		network.forwardFn = func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			jrq, err := req.JsonRpcRequest(ctx)
			require.NoError(t, err)
			blockRef, _ := jrq.Params[0].(string)
			blockNumber, err := common.HexToUint64(blockRef)
			require.NoError(t, err)

			mu.Lock()
			parentIDs = append(parentIDs, req.ParentRequestId())
			mu.Unlock()

			var result interface{}
			switch blockNumber {
			case 3:
				result = makeBlockResult(3, nil)
			case 2:
				result = nil
			case 1:
				result = makeBlockResult(1, nil)
			}
			jrr, err := common.NewJsonRpcResponse(req.ID(), result, nil)
			require.NoError(t, err)
			return common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr), nil
		}

		results, err := fetchBlockRange(context.Background(), network, "parent-1", "", 3, 1, "desc", false, 2)
		require.NoError(t, err)
		require.Len(t, results, 2)
		first, err := blockMapFromRaw(results[0])
		require.NoError(t, err)
		second, err := blockMapFromRaw(results[1])
		require.NoError(t, err)
		assert.Equal(t, "0x3", first["number"])
		assert.Equal(t, "0x1", second["number"])
		assert.ElementsMatch(t, []interface{}{"parent-1", "parent-1", "parent-1"}, parentIDs)
	})
}

func TestFilterProjectionAndDedupHelpers(t *testing.T) {
	tx := map[string]interface{}{
		"hash":             "0xaaa",
		"from":             "0x0000000000000000000000000000000000000001",
		"to":               "0x0000000000000000000000000000000000000002",
		"input":            "0x12345678deadbeef",
		"blockNumber":      "0x1",
		"blockHash":        "0xabc",
		"transactionIndex": "0x0",
	}
	trace := map[string]interface{}{
		"from":         tx["from"],
		"to":           tx["to"],
		"input":        tx["input"],
		"traceAddress": []interface{}{"0x0"},
	}
	transfer := map[string]interface{}{
		"from":         tx["from"],
		"to":           tx["to"],
		"traceAddress": []interface{}{},
	}
	filter := &QueryFilter{
		FromAddresses: parseByteSliceList([]interface{}{tx["from"]}),
		ToAddresses:   parseByteSliceList([]interface{}{tx["to"]}),
		Selectors:     parseByteSliceList([]interface{}{"0x12345678"}),
	}
	require.True(t, matchesTransactionFilter(tx, filter))
	require.True(t, matchesTraceFilter(trace, filter))
	topLevel := true
	require.True(t, matchesTransferFilter(transfer, &QueryFilter{
		FromAddresses: filter.FromAddresses,
		ToAddresses:   filter.ToAddresses,
		IsTopLevel:    &topLevel,
	}))

	projected := projectFields(tx, []string{"from"}, []string{"hash"})
	assert.Equal(t, map[string]interface{}{"from": tx["from"], "hash": tx["hash"]}, projected)

	deduped := deduplicateByKey([]map[string]interface{}{
		{"hash": "0x1", "foo": "a"},
		{"hash": "0x1", "foo": "b"},
		{"hash": "0x2", "foo": "c"},
	}, "hash")
	require.Len(t, deduped, 2)
	assert.Equal(t, "0x1", deduped[0]["hash"])
	assert.Equal(t, "0x2", deduped[1]["hash"])
}

func TestBuildQueryJsonRpcResponse_AllMethods(t *testing.T) {
	resp := &QueryResponse{
		Blocks:             []map[string]interface{}{{"hash": "0x1"}},
		Transactions:       []map[string]interface{}{{"hash": "0x2"}},
		Logs:               []map[string]interface{}{{"logIndex": "0x0"}},
		Traces:             []map[string]interface{}{{"traceType": "call"}},
		Transfers:          []map[string]interface{}{{"value": "0x1"}},
		ParentBlocks:       []map[string]interface{}{{"hash": "0x3"}},
		ParentTransactions: []map[string]interface{}{{"hash": "0x4"}},
		FromBlock:          &QueryCursorBlock{Number: 1},
		ToBlock:            &QueryCursorBlock{Number: 2},
		CursorBlock:        &QueryCursorBlock{Number: 3},
	}

	tests := []struct {
		method string
		key    string
	}{
		{method: "eth_queryBlocks", key: "blocks"},
		{method: "eth_queryTransactions", key: "transactions"},
		{method: "eth_queryLogs", key: "logs"},
		{method: "eth_queryTraces", key: "traces"},
		{method: "eth_queryTransfers", key: "transfers"},
	}

	for _, tt := range tests {
		t.Run(tt.method, func(t *testing.T) {
			payload := buildQueryJsonRpcResponse(tt.method, resp)
			data, ok := payload["data"].(map[string]interface{})
			require.True(t, ok)
			require.Contains(t, data, tt.key)
			cursor, ok := payload["cursorBlock"].(map[string]interface{})
			require.True(t, ok)
			assert.Equal(t, "0x3", cursor["number"])
		})
	}
}

func TestShimQueryBlocks_RespectsPagination(t *testing.T) {
	network := newRouterBackedQueryNetwork(t, func(ctx context.Context, req *common.NormalizedRequest, jrq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
		blockRef, _ := jrq.Params[0].(string)
		blockNumber, err := common.HexToUint64(blockRef)
		require.NoError(t, err)
		return jsonResultResponse(t, req, makeBlockResult(blockNumber, nil)), nil
	})

	resp, err := shimQueryBlocks(context.Background(), network, "parent", "", nil, &QueryRequest{
		Method:    "eth_queryBlocks",
		FromBlock: 1,
		ToBlock:   3,
		Order:     "asc",
		Limit:     2,
		Fields:    &QueryFieldSelection{Blocks: []string{"number"}},
	})
	require.NoError(t, err)
	require.Len(t, resp.Blocks, 2)
	assert.Equal(t, "0x1", resp.Blocks[0]["number"])
	assert.Equal(t, "0x2", resp.Blocks[1]["number"])
	require.NotNil(t, resp.CursorBlock)
	assert.Equal(t, uint64(2), resp.CursorBlock.Number)
}

func TestShimQueryTransactions_FiltersAndKeepsFirstBlockAligned(t *testing.T) {
	block1Tx1 := makeTransactionResult("0x111", 1, 0, "0x0000000000000000000000000000000000000001", "0x0000000000000000000000000000000000000002", "0x12345678aaaa")
	block1Tx2 := makeTransactionResult("0x112", 1, 1, "0x0000000000000000000000000000000000000001", "0x0000000000000000000000000000000000000002", "0x12345678bbbb")
	block2Tx1 := makeTransactionResult("0x221", 2, 0, "0x0000000000000000000000000000000000000001", "0x0000000000000000000000000000000000000002", "0x12345678cccc")

	network := newRouterBackedQueryNetwork(t, func(ctx context.Context, req *common.NormalizedRequest, jrq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
		require.Equal(t, "eth_getBlockByNumber", jrq.Method)
		blockRef, _ := jrq.Params[0].(string)
		blockNumber, err := common.HexToUint64(blockRef)
		require.NoError(t, err)
		switch blockNumber {
		case 1:
			return jsonResultResponse(t, req, makeBlockResult(1, []interface{}{block1Tx1, block1Tx2})), nil
		case 2:
			return jsonResultResponse(t, req, makeBlockResult(2, []interface{}{block2Tx1})), nil
		default:
			return jsonResultResponse(t, req, nil), nil
		}
	})

	resp, err := shimQueryTransactions(context.Background(), network, "parent", "", nil, &QueryRequest{
		Method:    "eth_queryTransactions",
		FromBlock: 1,
		ToBlock:   2,
		Order:     "asc",
		Limit:     1,
		Filter: &QueryFilter{
			FromAddresses: parseByteSliceList([]interface{}{"0x0000000000000000000000000000000000000001"}),
			Selectors:     parseByteSliceList([]interface{}{"0x12345678"}),
		},
		Fields: &QueryFieldSelection{
			Transactions: []string{"hash", "from"},
			Blocks:       []string{"number"},
		},
	})
	require.NoError(t, err)
	require.Len(t, resp.Transactions, 2)
	require.Len(t, resp.ParentBlocks, 1)
	require.NotNil(t, resp.CursorBlock)
	assert.Equal(t, uint64(1), resp.CursorBlock.Number)
}

func TestShimQueryLogs_HydratesParentsAndDeduplicates(t *testing.T) {
	log1 := makeLogResult(1, 0, 0, "0xaaa", "0x00000000000000000000000000000000000000aa")
	log2 := makeLogResult(1, 1, 0, "0xaaa", "0x00000000000000000000000000000000000000aa")
	tx := makeTransactionResult("0xaaa", 1, 0, "0x0000000000000000000000000000000000000001", "0x00000000000000000000000000000000000000aa", "0x12345678")

	network := newRouterBackedQueryNetwork(t, func(ctx context.Context, req *common.NormalizedRequest, jrq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
		switch jrq.Method {
		case "eth_getLogs":
			return jsonResultResponse(t, req, []interface{}{log1, log2}), nil
		case "eth_getBlockByNumber":
			return jsonResultResponse(t, req, makeBlockResult(1, []interface{}{tx})), nil
		case "eth_getTransactionByHash":
			return jsonResultResponse(t, req, tx), nil
		default:
			return nil, fmt.Errorf("unexpected method %s", jrq.Method)
		}
	})

	resp, err := shimQueryLogs(context.Background(), network, "parent", "", nil, &QueryRequest{
		Method:    "eth_queryLogs",
		FromBlock: 1,
		ToBlock:   2,
		Order:     "asc",
		Limit:     1,
		Fields: &QueryFieldSelection{
			Logs:         []string{"logIndex"},
			Transactions: []string{"hash"},
			Blocks:       []string{"number"},
		},
	})
	require.NoError(t, err)
	require.Len(t, resp.Logs, 2)
	require.Len(t, resp.ParentTransactions, 1)
	require.Len(t, resp.ParentBlocks, 1)
	assert.Nil(t, resp.CursorBlock)
}

func TestShimQueryLogs_DescUsesAscendingEthGetLogsRange(t *testing.T) {
	log1 := makeLogResult(2, 0, 0, "0xaaa", "0x00000000000000000000000000000000000000aa")
	log2 := makeLogResult(5, 0, 0, "0xbbb", "0x00000000000000000000000000000000000000bb")

	network := newRouterBackedQueryNetwork(t, func(ctx context.Context, req *common.NormalizedRequest, jrq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
		switch jrq.Method {
		case "eth_getLogs":
			filter, ok := jrq.Params[0].(map[string]interface{})
			require.True(t, ok)
			assert.Equal(t, "0x2", filter["fromBlock"])
			assert.Equal(t, "0x5", filter["toBlock"])
			return jsonResultResponse(t, req, []interface{}{log1, log2}), nil
		case "eth_getBlockByNumber":
			blockRef, _ := jrq.Params[0].(string)
			blockNumber, err := common.HexToUint64(blockRef)
			require.NoError(t, err)
			return jsonResultResponse(t, req, makeBlockResult(blockNumber, nil)), nil
		default:
			return nil, fmt.Errorf("unexpected method %s", jrq.Method)
		}
	})

	resp, err := shimQueryLogs(context.Background(), network, "parent", "", nil, &QueryRequest{
		Method:    "eth_queryLogs",
		FromBlock: 5,
		ToBlock:   2,
		Order:     "desc",
		Limit:     10,
		Fields:    &QueryFieldSelection{Logs: []string{"blockNumber", "logIndex"}},
	})
	require.NoError(t, err)
	require.Len(t, resp.Logs, 2)
	assert.Equal(t, "0x5", resp.Logs[0]["blockNumber"])
	assert.Equal(t, "0x2", resp.Logs[1]["blockNumber"])
}

func TestShimQueryTraces_UsesTraceBlockAndDebugFallback(t *testing.T) {
	t.Run("TraceBlock", func(t *testing.T) {
		tx := makeTransactionResult("0xaaa", 1, 0, "0x0000000000000000000000000000000000000001", "0x0000000000000000000000000000000000000002", "0x12345678")
		network := newRouterBackedQueryNetwork(t, func(ctx context.Context, req *common.NormalizedRequest, jrq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
			switch jrq.Method {
			case "eth_getBlockByNumber":
				return jsonResultResponse(t, req, makeBlockResult(1, []interface{}{tx})), nil
			case "trace_block":
				return jsonResultResponse(t, req, []interface{}{
					map[string]interface{}{
						"type": "call",
						"action": map[string]interface{}{
							"from":     tx["from"],
							"to":       tx["to"],
							"input":    tx["input"],
							"value":    "0x1",
							"gas":      "0x5208",
							"callType": "call",
						},
						"result": map[string]interface{}{
							"gasUsed": "0x5208",
							"output":  "0x",
						},
						"traceAddress":        []interface{}{},
						"subtraces":           0,
						"transactionHash":     tx["hash"],
						"transactionIndex":    "0x0",
						"transactionPosition": "0x0",
					},
				}), nil
			case "eth_getTransactionByHash":
				return jsonResultResponse(t, req, tx), nil
			default:
				return nil, fmt.Errorf("unexpected method %s", jrq.Method)
			}
		})

		resp, err := shimQueryTraces(context.Background(), network, "parent", "", nil, &QueryRequest{
			Method:    "eth_queryTraces",
			FromBlock: 1,
			ToBlock:   1,
			Order:     "asc",
			Limit:     10,
			Fields: &QueryFieldSelection{
				Traces:       []string{"traceType", "transactionHash"},
				Transactions: []string{"hash"},
				Blocks:       []string{"number"},
			},
		})
		require.NoError(t, err)
		require.Len(t, resp.Traces, 1)
		require.Len(t, resp.ParentTransactions, 1)
		require.Len(t, resp.ParentBlocks, 1)
		assert.Equal(t, "call", resp.Traces[0]["traceType"])
	})

	t.Run("DebugFallback", func(t *testing.T) {
		tx := makeTransactionResult("0xbbb", 1, 0, "0x0000000000000000000000000000000000000001", "0x0000000000000000000000000000000000000002", "0x12345678")
		network := newRouterBackedQueryNetwork(t, func(ctx context.Context, req *common.NormalizedRequest, jrq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
			switch jrq.Method {
			case "eth_getBlockByNumber":
				return jsonResultResponse(t, req, makeBlockResult(1, []interface{}{tx})), nil
			case "trace_block":
				return nil, common.NewErrEndpointUnsupported(errors.New("method not found"))
			case "debug_traceBlockByNumber":
				return jsonResultResponse(t, req, map[string]interface{}{
					"type":    "CALL",
					"from":    tx["from"],
					"to":      tx["to"],
					"input":   tx["input"],
					"output":  "0x",
					"gas":     "0x5208",
					"gasUsed": "0x5208",
					"value":   "0x1",
				}), nil
			default:
				return nil, fmt.Errorf("unexpected method %s", jrq.Method)
			}
		})

		resp, err := shimQueryTraces(context.Background(), network, "parent", "", nil, &QueryRequest{
			Method:    "eth_queryTraces",
			FromBlock: 1,
			ToBlock:   1,
			Order:     "asc",
			Limit:     10,
			Filter: &QueryFilter{
				Selectors: parseByteSliceList([]interface{}{"0x12345678"}),
			},
			Fields: &QueryFieldSelection{Traces: true},
		})
		require.NoError(t, err)
		require.Len(t, resp.Traces, 1)
		assert.Equal(t, "0x0bbb", resp.Traces[0]["transactionHash"])
	})
}

func TestShimQueryTraces_ErrorsWhenNoTraceMethodsSupported(t *testing.T) {
	tx := makeTransactionResult("0xccc", 1, 0, "0x0000000000000000000000000000000000000001", "0x0000000000000000000000000000000000000002", "0x12345678")
	network := newRouterBackedQueryNetwork(t, func(ctx context.Context, req *common.NormalizedRequest, jrq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
		switch jrq.Method {
		case "eth_getBlockByNumber":
			return jsonResultResponse(t, req, makeBlockResult(1, []interface{}{tx})), nil
		case "trace_block", "debug_traceBlockByNumber":
			return nil, common.NewErrEndpointUnsupported(errors.New("method not found"))
		default:
			return nil, fmt.Errorf("unexpected method %s", jrq.Method)
		}
	})

	_, err := shimQueryTraces(context.Background(), network, "parent", "", nil, &QueryRequest{
		Method:    "eth_queryTraces",
		FromBlock: 1,
		ToBlock:   1,
		Order:     "asc",
		Limit:     10,
		Fields:    &QueryFieldSelection{Traces: true},
	})
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointUnsupported))
}

func TestShimQueryTransfers_ExtractsTopLevelTransfers(t *testing.T) {
	tx := makeTransactionResult("0xddd", 1, 0, "0x0000000000000000000000000000000000000001", "0x0000000000000000000000000000000000000002", "0x12345678")
	network := newRouterBackedQueryNetwork(t, func(ctx context.Context, req *common.NormalizedRequest, jrq *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
		switch jrq.Method {
		case "eth_getBlockByNumber":
			return jsonResultResponse(t, req, makeBlockResult(1, []interface{}{tx})), nil
		case "trace_block":
			return jsonResultResponse(t, req, []interface{}{
				map[string]interface{}{
					"type": "call",
					"action": map[string]interface{}{
						"from":     tx["from"],
						"to":       tx["to"],
						"input":    tx["input"],
						"value":    "0x5",
						"gas":      "0x5208",
						"callType": "call",
					},
					"result":              map[string]interface{}{"gasUsed": "0x5208", "output": "0x"},
					"traceAddress":        []interface{}{},
					"subtraces":           1,
					"transactionHash":     tx["hash"],
					"transactionPosition": "0x0",
				},
				map[string]interface{}{
					"type": "call",
					"action": map[string]interface{}{
						"from":     tx["from"],
						"to":       tx["to"],
						"input":    "0x",
						"value":    "0x1",
						"gas":      "0x5208",
						"callType": "call",
					},
					"result":              map[string]interface{}{"gasUsed": "0x5208", "output": "0x"},
					"traceAddress":        []interface{}{"0x0"},
					"subtraces":           0,
					"transactionHash":     tx["hash"],
					"transactionPosition": "0x0",
				},
			}), nil
		case "eth_getTransactionByHash":
			return jsonResultResponse(t, req, tx), nil
		default:
			return nil, fmt.Errorf("unexpected method %s", jrq.Method)
		}
	})
	topLevel := true
	resp, err := shimQueryTransfers(context.Background(), network, "parent", "", nil, &QueryRequest{
		Method:    "eth_queryTransfers",
		FromBlock: 1,
		ToBlock:   1,
		Order:     "asc",
		Limit:     10,
		Filter:    &QueryFilter{IsTopLevel: &topLevel},
		Fields: &QueryFieldSelection{
			Transfers:    []string{"value"},
			Transactions: []string{"hash"},
			Blocks:       []string{"number"},
		},
	})
	require.NoError(t, err)
	require.Len(t, resp.Transfers, 1)
	assert.Equal(t, "0x5", resp.Transfers[0]["value"])
	require.Len(t, resp.ParentTransactions, 1)
	require.Len(t, resp.ParentBlocks, 1)
}

func TestParseQueryRequest_RejectsLimitAboveMaxWithoutNarrowing(t *testing.T) {
	network := &queryTestNetwork{
		cfg: &common.NetworkConfig{
			Architecture: common.ArchitectureEvm,
			Evm:          &common.EvmNetworkConfig{},
		},
		latest:    120,
		finalized: 118,
	}
	qs := &common.EvmQueryShimConfig{DefaultLimit: 25, MaxLimit: 500, MaxBlockRange: 1000}

	req := common.NewNormalizedRequest([]byte(`{
		"jsonrpc":"2.0",
		"id":1,
		"method":"eth_queryBlocks",
		"params":[{"fromBlock":"0x1","toBlock":"0x2","limit":"0xffffffffffffffff"}]
	}`))

	parsed, err := parseQueryRequest(context.Background(), network, qs, req)
	require.Nil(t, parsed)
	require.ErrorContains(t, err, "max limit")
}

func TestProtoTraceFromJSON_RejectsUint32Overflow(t *testing.T) {
	base := map[string]interface{}{
		"traceType":        "call",
		"callType":         "call",
		"from":             "0x0000000000000000000000000000000000000001",
		"to":               "0x0000000000000000000000000000000000000002",
		"value":            "0x0",
		"input":            "0x",
		"output":           "0x",
		"gas":              "0x5208",
		"gasUsed":          "0x5208",
		"subtraces":        "0x0",
		"traceAddress":     []interface{}{},
		"transactionHash":  "0x01",
		"transactionIndex": "0x0",
		"blockNumber":      "0x1",
		"blockHash":        "0x02",
	}

	tests := []struct {
		name   string
		field  string
		mutate func(map[string]interface{})
	}{
		{
			name:  "subtraces",
			field: "subtraces",
			mutate: func(trace map[string]interface{}) {
				trace["subtraces"] = "0x100000000"
			},
		},
		{
			name:  "transactionIndex",
			field: "transactionIndex",
			mutate: func(trace map[string]interface{}) {
				trace["transactionIndex"] = "0x100000000"
			},
		},
		{
			name:  "traceAddress",
			field: "traceAddress",
			mutate: func(trace map[string]interface{}) {
				trace["traceAddress"] = []interface{}{"0x100000000"}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			trace := map[string]interface{}{}
			for key, value := range base {
				trace[key] = value
			}
			tt.mutate(trace)

			parsed, err := protoTraceFromJSON(trace)
			require.Nil(t, parsed)
			require.ErrorContains(t, err, tt.field)
		})
	}
}

func newQueryTestConfig() *common.NetworkConfig {
	return &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{},
	}
}

func newRouterBackedQueryNetwork(
	t *testing.T,
	router func(ctx context.Context, req *common.NormalizedRequest, jrq *common.JsonRpcRequest) (*common.NormalizedResponse, error),
) *queryTestNetwork {
	t.Helper()
	return &queryTestNetwork{
		cfg:       newQueryTestConfig(),
		latest:    10,
		finalized: 9,
		forwardFn: func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			jrq, err := req.JsonRpcRequest(ctx)
			require.NoError(t, err)
			return router(ctx, req, jrq)
		},
	}
}

func jsonResultResponse(t *testing.T, req *common.NormalizedRequest, result interface{}) *common.NormalizedResponse {
	t.Helper()
	jrr, err := common.NewJsonRpcResponse(req.ID(), result, nil)
	require.NoError(t, err)
	return common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
}

func makeBlockResult(number uint64, txs []interface{}) map[string]interface{} {
	return map[string]interface{}{
		"number":       fmt.Sprintf("0x%x", number),
		"hash":         fmt.Sprintf("0x%064x", number),
		"parentHash":   fmt.Sprintf("0x%064x", number-1),
		"timestamp":    "0x64",
		"transactions": txs,
	}
}

func makeTransactionResult(hash string, blockNumber uint64, txIndex uint64, from, to, input string) map[string]interface{} {
	return map[string]interface{}{
		"hash":             hash,
		"nonce":            "0x0",
		"from":             from,
		"to":               to,
		"value":            "0x0",
		"input":            input,
		"type":             "0x2",
		"gas":              "0x5208",
		"gasPrice":         "0x1",
		"blockNumber":      fmt.Sprintf("0x%x", blockNumber),
		"blockHash":        fmt.Sprintf("0x%064x", blockNumber),
		"transactionIndex": fmt.Sprintf("0x%x", txIndex),
	}
}

func makeLogResult(blockNumber uint64, logIndex uint64, txIndex uint64, txHash, address string) map[string]interface{} {
	return map[string]interface{}{
		"address":          address,
		"topics":           []interface{}{"0xddf252ad"},
		"data":             "0x",
		"blockNumber":      fmt.Sprintf("0x%x", blockNumber),
		"blockHash":        fmt.Sprintf("0x%064x", blockNumber),
		"transactionHash":  txHash,
		"transactionIndex": fmt.Sprintf("0x%x", txIndex),
		"logIndex":         fmt.Sprintf("0x%x", logIndex),
	}
}
