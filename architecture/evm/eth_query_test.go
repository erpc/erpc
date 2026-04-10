package evm

import (
	"context"
	"fmt"
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

func (n *queryTestNetwork) Id() string { return "evm:1" }
func (n *queryTestNetwork) Label() string { return "evm:1" }
func (n *queryTestNetwork) ProjectId() string { return "test-project" }
func (n *queryTestNetwork) Architecture() common.NetworkArchitecture { return common.ArchitectureEvm }
func (n *queryTestNetwork) Config() *common.NetworkConfig { return n.cfg }
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
func (n *queryTestNetwork) EvmHighestFinalizedBlockNumber(ctx context.Context) int64 { return n.finalized }
func (n *queryTestNetwork) EvmLeaderUpstream(ctx context.Context) common.Upstream { return nil }

type queryTestUpstream struct {
	supported bool
}

func (u *queryTestUpstream) Id() string { return "upstream-1" }
func (u *queryTestUpstream) VendorName() string { return "test" }
func (u *queryTestUpstream) NetworkId() string { return "evm:1" }
func (u *queryTestUpstream) NetworkLabel() string { return "evm:1" }
func (u *queryTestUpstream) Config() *common.UpstreamConfig { return &common.UpstreamConfig{Id: "upstream-1"} }
func (u *queryTestUpstream) Logger() *zerolog.Logger {
	logger := zerolog.Nop()
	return &logger
}
func (u *queryTestUpstream) Vendor() common.Vendor { return nil }
func (u *queryTestUpstream) Tracker() common.HealthTracker { return nil }
func (u *queryTestUpstream) Forward(ctx context.Context, nq *common.NormalizedRequest, byPassMethodExclusion bool) (*common.NormalizedResponse, error) {
	return nil, nil
}
func (u *queryTestUpstream) Cordon(method string, reason string) {}
func (u *queryTestUpstream) Uncordon(method string, reason string) {}
func (u *queryTestUpstream) IgnoreMethod(method string) {}
func (u *queryTestUpstream) ShouldHandleMethod(method string) (bool, error) {
	return u.supported, nil
}

func TestParseQueryRequest_ResolvesCursorAndSelections(t *testing.T) {
	network := &queryTestNetwork{
		cfg: &common.NetworkConfig{
			Architecture: common.ArchitectureEvm,
			Evm: &common.EvmNetworkConfig{
				QueryShimDefaultLimit: 25,
				QueryShimMaxLimit:     500,
				QueryShimMaxBlockRange: 1000,
			},
		},
		latest:    120,
		finalized: 118,
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

	parsed, err := parseQueryRequest(context.Background(), network, req)
	require.NoError(t, err)
	require.NotNil(t, parsed)

	assert.Equal(t, uint64(3), parsed.FromBlock)
	assert.Equal(t, uint64(120), parsed.ToBlock)
	assert.Equal(t, "asc", parsed.Order)
	assert.Equal(t, 25, parsed.Limit)
	require.NotNil(t, parsed.Cursor)
	assert.Equal(t, uint64(2), parsed.Cursor.Number)
	require.NotNil(t, parsed.Filter)
	require.Len(t, parsed.Filter.FromAddresses, 1)
	assert.Equal(t, []string{"hash", "from"}, parsed.Fields.Transactions)
	assert.Equal(t, []string{"number"}, parsed.Fields.Blocks)
}

func TestNetworkPreForwardEthQuery_PassthroughWhenAnyUpstreamSupportsMethod(t *testing.T) {
	nq := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_queryBlocks","params":[{}]}`))
	network := &queryTestNetwork{
		cfg: &common.NetworkConfig{
			Architecture: common.ArchitectureEvm,
			Evm:          &common.EvmNetworkConfig{},
		},
		latest:    10,
		finalized: 10,
		forwardFn: func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return nil, fmt.Errorf("should not be called")
		},
	}

	handled, resp, err := networkPreForward_eth_query(
		context.Background(),
		network,
		[]common.Upstream{&queryTestUpstream{supported: true}},
		nq,
	)

	require.NoError(t, err)
	assert.False(t, handled)
	assert.Nil(t, resp)
}

func TestNetworkPreForwardEthQuery_ShimsBlocks(t *testing.T) {
	network := &queryTestNetwork{
		cfg: &common.NetworkConfig{
			Architecture: common.ArchitectureEvm,
			Evm: &common.EvmNetworkConfig{
				QueryShimDefaultLimit: 100,
				QueryShimMaxLimit:     1000,
				QueryShimMaxBlockRange: 1000,
			},
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
			"number":     fmt.Sprintf("0x%x", blockNumber),
			"hash":       fmt.Sprintf("0x%064x", blockNumber),
			"parentHash": fmt.Sprintf("0x%064x", blockNumber-1),
			"timestamp":  "0x1",
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

	handled, resp, err := networkPreForward_eth_query(context.Background(), network, nil, nq)
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

func TestFlattenGethCallTrace(t *testing.T) {
	flat := flattenGethCallTrace(map[string]interface{}{
		"type": "CALL",
		"calls": []interface{}{
			map[string]interface{}{
				"type": "CALL",
				"calls": []interface{}{
					map[string]interface{}{"type": "CREATE"},
				},
			},
		},
	}, nil)

	require.Len(t, flat, 3)
	assert.Empty(t, flat[0]["traceAddress"])
	assert.Equal(t, []interface{}{"0x0"}, flat[1]["traceAddress"])
	assert.Equal(t, []interface{}{"0x0", "0x0"}, flat[2]["traceAddress"])
}
