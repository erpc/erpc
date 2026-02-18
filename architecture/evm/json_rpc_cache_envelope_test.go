package evm

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"
)

type testNetworkCacheEnvelope struct {
	cfg *common.NetworkConfig
}

var _ common.Network = (*testNetworkCacheEnvelope)(nil)

func (n *testNetworkCacheEnvelope) Id() string { return "evm:8453" }
func (n *testNetworkCacheEnvelope) Label() string {
	return "evm:8453"
}
func (n *testNetworkCacheEnvelope) ProjectId() string { return "test" }
func (n *testNetworkCacheEnvelope) Architecture() common.NetworkArchitecture {
	return common.ArchitectureEvm
}
func (n *testNetworkCacheEnvelope) Config() *common.NetworkConfig { return n.cfg }
func (n *testNetworkCacheEnvelope) Logger() *zerolog.Logger       { return &log.Logger }
func (n *testNetworkCacheEnvelope) GetMethodMetrics(method string) common.TrackedMetrics {
	return nil
}
func (n *testNetworkCacheEnvelope) Cache() common.CacheDAL { return nil }
func (n *testNetworkCacheEnvelope) Forward(ctx context.Context, nq *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	panic("not used")
}
func (n *testNetworkCacheEnvelope) GetFinality(ctx context.Context, req *common.NormalizedRequest, resp *common.NormalizedResponse) common.DataFinalityState {
	return common.DataFinalityStateUnknown
}
func (n *testNetworkCacheEnvelope) EvmHighestLatestBlockNumber(ctx context.Context) int64 {
	return 0
}
func (n *testNetworkCacheEnvelope) EvmHighestFinalizedBlockNumber(ctx context.Context) int64 {
	return 0
}
func (n *testNetworkCacheEnvelope) EvmLeaderUpstream(ctx context.Context) common.Upstream { return nil }

func TestEvmJsonRpcCache_Envelope_RoundTrip(t *testing.T) {
	util.ConfigureTestLogger()

	ctx := context.Background()
	lg := log.Logger

	waitHit := func(t *testing.T, cache *EvmJsonRpcCache, req *common.NormalizedRequest) *common.NormalizedResponse {
		t.Helper()
		deadline := time.Now().Add(1 * time.Second)
		for time.Now().Before(deadline) {
			r, err := cache.Get(ctx, req)
			require.NoError(t, err)
			if r != nil {
				return r
			}
			time.Sleep(10 * time.Millisecond)
		}
		return nil
	}

	t.Run("uncompressed", func(t *testing.T) {
		cfg := &common.CacheConfig{
			Envelope: util.BoolPtr(true),
			Connectors: []*common.ConnectorConfig{
				{
					Id:     "mem",
					Driver: common.DriverMemory,
					Memory: &common.MemoryConnectorConfig{
						MaxItems:     10_000,
						MaxTotalSize: "64MiB",
					},
				},
			},
			Policies: []*common.CachePolicyConfig{
				{
					Network:   "*",
					Method:    "*",
					Finality:  common.DataFinalityStateUnknown,
					Empty:     common.CacheEmptyBehaviorAllow,
					Connector: "mem",
					TTL:       common.Duration(0),
				},
			},
		}

		cache, err := NewEvmJsonRpcCache(ctx, &lg, cfg)
		require.NoError(t, err)
		cache = cache.WithProjectId("p1")

		jrq := common.NewJsonRpcRequest("eth_getBlockByNumber", []interface{}{"0x1", false})
		req := common.NewNormalizedRequestFromJsonRpcRequest(jrq)
		req.SetNetwork(&testNetworkCacheEnvelope{cfg: &common.NetworkConfig{Evm: &common.EvmNetworkConfig{ChainId: 8453}}})

		want := []byte(`{"number":"0x1"}`)
		jrr, err := common.NewJsonRpcResponseFromBytes(nil, want, nil)
		require.NoError(t, err)
		_ = jrr.SetID(1)

		resp := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
		require.NoError(t, cache.Set(ctx, req, resp))

		gotResp := waitHit(t, cache, req)
		require.NotNil(t, gotResp)
		gotJrr, err := gotResp.JsonRpcResponse(ctx)
		require.NoError(t, err)
		require.Equal(t, want, gotJrr.GetResultBytes())
	})

	t.Run("compressed", func(t *testing.T) {
		cfg := &common.CacheConfig{
			Envelope: util.BoolPtr(true),
			Compression: &common.CompressionConfig{
				Enabled:   util.BoolPtr(true),
				Algorithm: "zstd",
				ZstdLevel: "fastest",
				Threshold: 1,
			},
			Connectors: []*common.ConnectorConfig{
				{
					Id:     "mem",
					Driver: common.DriverMemory,
					Memory: &common.MemoryConnectorConfig{
						MaxItems:     10_000,
						MaxTotalSize: "256MiB",
					},
				},
			},
			Policies: []*common.CachePolicyConfig{
				{
					Network:   "*",
					Method:    "*",
					Finality:  common.DataFinalityStateUnknown,
					Empty:     common.CacheEmptyBehaviorAllow,
					Connector: "mem",
					TTL:       common.Duration(0),
				},
			},
		}

		cache, err := NewEvmJsonRpcCache(ctx, &lg, cfg)
		require.NoError(t, err)
		cache = cache.WithProjectId("p1")

		jrq := common.NewJsonRpcRequest("eth_getBlockByNumber", []interface{}{"0x1", false})
		req := common.NewNormalizedRequestFromJsonRpcRequest(jrq)
		req.SetNetwork(&testNetworkCacheEnvelope{cfg: &common.NetworkConfig{Evm: &common.EvmNetworkConfig{ChainId: 8453}}})

		// Large JSON string to exercise the streaming decompression path.
		want := []byte(`{"data":"` + strings.Repeat("x", 4096) + `"}`)
		jrr, err := common.NewJsonRpcResponseFromBytes(nil, want, nil)
		require.NoError(t, err)
		_ = jrr.SetID(1)

		resp := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
		require.NoError(t, cache.Set(ctx, req, resp))

		gotResp := waitHit(t, cache, req)
		require.NotNil(t, gotResp)
		gotJrr, err := gotResp.JsonRpcResponse(ctx)
		require.NoError(t, err)

		var buf bytes.Buffer
		_, err = gotJrr.WriteResultTo(&buf, false)
		require.NoError(t, err)
		require.Equal(t, want, buf.Bytes())
	})
}
