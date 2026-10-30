package jsonrpc

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"
)

// Histogram metrics are nil until SetHistogramBuckets initializes them; the
// cache's Get/Set paths emit hit/miss durations, so register them first.
func init() {
	_ = telemetry.SetHistogramBuckets("0.05,0.5,5,30")
}

// ristrettoSettleDelay lets the async Ristretto admission buffer drain so a
// subsequent Get sees the value that was just Set.
const ristrettoSettleDelay = 50 * time.Millisecond

// newTestCache builds an in-memory-backed GenericJsonRpcCache with ONE policy:
// method starknet_getBlockWithTxHashes at finality unknown — the shape an
// operator writes for a protocol eRPC knows nothing about.
func newTestCache(t *testing.T) *GenericJsonRpcCache {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	cfg := &common.CacheConfig{
		Connectors: []*common.ConnectorConfig{{
			Id:     "mem",
			Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{MaxItems: 1000, MaxTotalSize: "1MB"},
		}},
		Policies: []*common.CachePolicyConfig{{
			Connector: "mem",
			Network:   "*",
			Method:    "starknet_getBlockWithTxHashes",
			Finality:  common.DataFinalityStateUnknown,
		}},
	}
	c, err := NewGenericJsonRpcCache(ctx, &log.Logger, cfg)
	require.NoError(t, err)
	return c
}

func newReq(body string) *common.NormalizedRequest {
	req := common.NewNormalizedRequest([]byte(body))
	req.SetNetwork(genericNetwork{})
	return req
}

func TestGenericCache_SetThenGet_HitsOnMatchingPolicy(t *testing.T) {
	t.Parallel()
	c := newTestCache(t)
	ctx := context.Background()

	body := `{"jsonrpc":"2.0","id":1,"method":"starknet_getBlockWithTxHashes","params":[{"block_number":100}]}`
	req := newReq(body)
	jrr, err := common.NewJsonRpcResponse(1, map[string]interface{}{"block_hash": "0xAbC"}, nil)
	require.NoError(t, err)
	resp := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)

	require.NoError(t, c.Set(ctx, req, resp))
	time.Sleep(ristrettoSettleDelay)

	got, err := c.Get(ctx, newReq(body))
	require.NoError(t, err)
	require.NotNil(t, got, "second Get for an identical request must hit")
	gotJrr, err := got.JsonRpcResponse()
	require.NoError(t, err)
	require.Contains(t, string(gotJrr.GetResultBytes()), "0xAbC")
	require.True(t, got.FromCache())
}

func TestGenericCache_UnmatchedMethod_IsAMiss(t *testing.T) {
	t.Parallel()
	c := newTestCache(t)
	ctx := context.Background()

	body := `{"jsonrpc":"2.0","id":1,"method":"starknet_blockNumber","params":[]}`
	req := newReq(body)
	jrr, err := common.NewJsonRpcResponse(1, 100, nil)
	require.NoError(t, err)
	require.NoError(t, c.Set(ctx, req, common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)))
	time.Sleep(ristrettoSettleDelay)

	got, err := c.Get(ctx, newReq(body))
	require.NoError(t, err)
	require.Nil(t, got, "a method no policy matches must never be served from cache")
}

func TestGenericCache_ErrorResponse_IsNotStored(t *testing.T) {
	t.Parallel()
	c := newTestCache(t)
	ctx := context.Background()

	body := `{"jsonrpc":"2.0","id":1,"method":"starknet_getBlockWithTxHashes","params":[{"block_number":7}]}`
	req := newReq(body)
	jrr := common.MustNewJsonRpcResponse(1, nil, common.NewErrJsonRpcExceptionExternal(24, "Block not found", ""))
	require.NoError(t, c.Set(ctx, req, common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)))
	time.Sleep(ristrettoSettleDelay)

	got, err := c.Get(ctx, newReq(body))
	require.NoError(t, err)
	require.Nil(t, got, "a JSON-RPC error body must never be cached")
}

func TestGenericCache_RequestKey_PreservesCase(t *testing.T) {
	t.Parallel()
	c := newTestCache(t)
	ctx := context.Background()

	upper := `{"jsonrpc":"2.0","id":1,"method":"starknet_getBlockWithTxHashes","params":[{"block_hash":"0xABC"}]}`
	lower := `{"jsonrpc":"2.0","id":1,"method":"starknet_getBlockWithTxHashes","params":[{"block_hash":"0xabc"}]}`
	req := newReq(upper)
	jrr, err := common.NewJsonRpcResponse(1, map[string]interface{}{"block_hash": "0xABC"}, nil)
	require.NoError(t, err)
	require.NoError(t, c.Set(ctx, req, common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)))
	time.Sleep(ristrettoSettleDelay)

	got, err := c.Get(ctx, newReq(lower))
	require.NoError(t, err)
	require.Nil(t, got, "params differing only by case must not share a cache entry")
}

// genericNetwork is the minimal common.Network a jsonrpc request needs: no
// methods config, so finality is unknown for every method.
type genericNetwork struct{}

func (genericNetwork) Id() string                               { return "jsonrpc:starknet" }
func (genericNetwork) Label() string                            { return "" }
func (genericNetwork) ProjectId() string                        { return "test" }
func (genericNetwork) Architecture() common.NetworkArchitecture { return common.ArchitectureJsonRpc }
func (genericNetwork) Config() *common.NetworkConfig {
	return &common.NetworkConfig{Architecture: common.ArchitectureJsonRpc, Generic: &common.GenericNetworkConfig{Slug: "starknet"}}
}
func (genericNetwork) Logger() *zerolog.Logger                       { l := log.Logger; return &l }
func (genericNetwork) GetMethodMetrics(string) common.TrackedMetrics { return nil }
func (genericNetwork) Forward(context.Context, *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	return nil, nil
}
func (genericNetwork) GetFinality(context.Context, *common.NormalizedRequest, *common.NormalizedResponse) common.DataFinalityState {
	return common.DataFinalityStateUnknown
}
