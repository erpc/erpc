package erpc

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	promUtil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStaticResponses verifies that per-network static responses short-circuit
// request handling: matching requests return the canned payload immediately
// and no upstream is contacted. Non-matching requests flow through normally.
func TestStaticResponses(t *testing.T) {
	t.Run("MatchingRequestServedWithoutContactingUpstream", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		var rpcCalls atomic.Int32
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getBlockByNumber")
			}).
			Persist().
			Reply(200).
			Map(func(r *http.Response) *http.Response { rpcCalls.Add(1); return r }).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error":   map[string]interface{}{"code": -32000, "message": "block not found"},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		stubResult := map[string]interface{}{
			"number": "0x0",
			"hash":   "0xaaaabbbbccccdddd",
		}
		netCfg := &common.NetworkConfig{
			Architecture: common.ArchitectureEvm,
			Evm:          &common.EvmNetworkConfig{ChainId: 123},
			StaticResponses: []*common.StaticResponseConfig{
				{
					Method: "eth_getBlockByNumber",
					Params: []interface{}{"0x0", false},
					Response: &common.StaticResponseBodyConfig{
						Result: stubResult,
					},
				},
			},
		}
		network := setupTestNetworkSimple(t, ctx, nil, netCfg)

		counter, counterErr := telemetry.MetricNetworkStaticResponseServedTotal.
			GetMetricWithLabelValues("test", network.Label(), "eth_getBlockByNumber")
		require.NoError(t, counterErr)
		before := promUtil.ToFloat64(counter)

		req := common.NewNormalizedRequest([]byte(
			`{"jsonrpc":"2.0","id":42,"method":"eth_getBlockByNumber","params":["0x0",false]}`,
		))

		resp, err := network.Forward(ctx, req)
		require.NoError(t, err)
		require.NotNil(t, resp)

		jrr, jrrErr := resp.JsonRpcResponse()
		require.NoError(t, jrrErr)

		// Inbound id must be echoed (not the stored id from the config).
		assert.Equal(t, float64(42), toJSONNumber(t, jrr.ID()))

		var decoded map[string]interface{}
		require.NoError(t, json.Unmarshal(jrr.GetResultBytes(), &decoded))
		assert.Equal(t, "0x0", decoded["number"])
		assert.Equal(t, "0xaaaabbbbccccdddd", decoded["hash"])

		assert.Equal(t, int32(0), rpcCalls.Load(), "upstream must not be contacted for matched static response")
		assert.Equal(t, before+1, promUtil.ToFloat64(counter), "static-response counter must increment on hit")
	})

	t.Run("NonMatchingParamsFallThroughToUpstream", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		var rpcCalls atomic.Int32
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, "0x1")
			}).
			Persist().
			Reply(200).
			Map(func(r *http.Response) *http.Response { rpcCalls.Add(1); return r }).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  map[string]interface{}{"number": "0x1"},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		netCfg := &common.NetworkConfig{
			Architecture: common.ArchitectureEvm,
			Evm:          &common.EvmNetworkConfig{ChainId: 123},
			StaticResponses: []*common.StaticResponseConfig{
				{
					Method:   "eth_getBlockByNumber",
					Params:   []interface{}{"0x0", false},
					Response: &common.StaticResponseBodyConfig{Result: map[string]interface{}{"number": "0x0"}},
				},
			},
		}
		network := setupTestNetworkSimple(t, ctx, nil, netCfg)

		req := common.NewNormalizedRequest([]byte(
			`{"jsonrpc":"2.0","id":7,"method":"eth_getBlockByNumber","params":["0x1",false]}`,
		))

		resp, err := network.Forward(ctx, req)
		require.NoError(t, err)
		require.NotNil(t, resp)

		jrr, jrrErr := resp.JsonRpcResponse()
		require.NoError(t, jrrErr)

		var decoded map[string]interface{}
		require.NoError(t, json.Unmarshal(jrr.GetResultBytes(), &decoded))
		assert.Equal(t, "0x1", decoded["number"])
		assert.GreaterOrEqual(t, rpcCalls.Load(), int32(1), "upstream must be contacted when params don't match a static entry")
	})

	t.Run("ErrorShapedStubReturnsJsonRpcError", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		netCfg := &common.NetworkConfig{
			Architecture: common.ArchitectureEvm,
			Evm:          &common.EvmNetworkConfig{ChainId: 123},
			StaticResponses: []*common.StaticResponseConfig{
				{
					Method: "eth_getBlockByNumber",
					Params: []interface{}{"0x0", false},
					Response: &common.StaticResponseBodyConfig{
						Error: &common.StaticResponseErrorConfig{
							Code:    -32000,
							Message: "block not found",
						},
					},
				},
			},
		}
		network := setupTestNetworkSimple(t, ctx, nil, netCfg)

		req := common.NewNormalizedRequest([]byte(
			`{"jsonrpc":"2.0","id":9,"method":"eth_getBlockByNumber","params":["0x0",false]}`,
		))

		resp, err := network.Forward(ctx, req)
		require.NoError(t, err)
		require.NotNil(t, resp)

		jrr, jrrErr := resp.JsonRpcResponse()
		require.NoError(t, jrrErr)
		require.NotNil(t, jrr.Error)
		assert.Equal(t, -32000, jrr.Error.Code)
		assert.Equal(t, "block not found", jrr.Error.Message)
		assert.Equal(t, float64(9), toJSONNumber(t, jrr.ID()))
	})

	t.Run("StringRequestIdEchoedOnStaticHit", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		netCfg := &common.NetworkConfig{
			Architecture: common.ArchitectureEvm,
			Evm:          &common.EvmNetworkConfig{ChainId: 123},
			StaticResponses: []*common.StaticResponseConfig{
				{
					Method: "eth_getBlockByNumber",
					Params: []interface{}{"0x0", false},
					Response: &common.StaticResponseBodyConfig{
						Result: map[string]interface{}{"number": "0x0"},
					},
				},
			},
		}
		network := setupTestNetworkSimple(t, ctx, nil, netCfg)

		req := common.NewNormalizedRequest([]byte(
			`{"jsonrpc":"2.0","id":"req-abc-123","method":"eth_getBlockByNumber","params":["0x0",false]}`,
		))

		resp, err := network.Forward(ctx, req)
		require.NoError(t, err)
		require.NotNil(t, resp)

		jrr, jrrErr := resp.JsonRpcResponse()
		require.NoError(t, jrrErr)
		assert.Equal(t, "req-abc-123", jrr.ID())
	})
}

// toJSONNumber normalizes a parsed JSON-RPC id value to a float64 for
// comparison. The request id in tests is always numeric.
func toJSONNumber(t *testing.T, id interface{}) float64 {
	t.Helper()
	switch v := id.(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case json.Number:
		f, err := v.Float64()
		require.NoError(t, err)
		return f
	}
	t.Fatalf("unexpected id type %T", id)
	return 0
}
