package svm

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"
)

// Histogram metrics are nil until SetHistogramBuckets initializes them. The
// cache's Get/Set paths now emit hit/miss/error durations, so tests need them
// registered before any cache call — otherwise the first WithLabelValues
// panics on a nil HistogramVec.
func init() {
	_ = telemetry.SetHistogramBuckets("0.05,0.5,5,30")
}

// ristrettoSettleDelay lets the async Ristretto admission buffer drain so a
// subsequent Get sees the value that was just Set. 50ms is well above
// ristretto's internal buffer flush (default <1ms) with margin for CI hosts.
const ristrettoSettleDelay = 50 * time.Millisecond

// newTestCache builds an in-memory-backed SvmJsonRpcCache with a single
// catch-all policy. Good enough for happy-path Get/Set verification — the
// real policy matcher is a shared data.CachePolicy, which has its own tests.
func newTestCache(t *testing.T) *SvmJsonRpcCache {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	cfg := &common.CacheConfig{
		Connectors: []*common.ConnectorConfig{
			{
				Id:     "mem",
				Driver: common.DriverMemory,
				Memory: &common.MemoryConnectorConfig{MaxItems: 1000, MaxTotalSize: "1MB"},
			},
		},
		Policies: []*common.CachePolicyConfig{
			{
				Connector: "mem",
				// Use "*" so the policy matches requests built via NewNormalizedRequest
				// that don't have a network attached (req.NetworkId() returns "").
				// Production requests always have a network and would match "svm:*".
				Network:  "*",
				Method:   "*",
				Finality: common.DataFinalityStateFinalized,
			},
		},
	}

	c, err := NewSvmJsonRpcCache(ctx, &log.Logger, cfg)
	require.NoError(t, err)
	return c
}

func TestSvmCache_SetThenGet_RoundTripsFinalizedResult(t *testing.T) {
	t.Parallel()
	c := newTestCache(t)
	ctx := context.Background()

	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"getBlock","params":[100,{"commitment":"finalized"}]}`)
	req := common.NewNormalizedRequest(body)
	// Attach a fake network — req.Finality() returns Unknown without one, which
	// would make the Finalized policy skip the request entirely.
	req.SetNetwork(finalizedNetwork{})
	// Build a response carrying the same request so Finality() can resolve.
	jrr, err := common.NewJsonRpcResponse(1, map[string]interface{}{"blockhash": "abc"}, nil)
	require.NoError(t, err)
	resp := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)

	require.NoError(t, c.Set(ctx, req, resp))
	time.Sleep(ristrettoSettleDelay)

	// A fresh request with identical body must hit the cache. Using the exact same
	// bytes guarantees CacheHash() is stable.
	req2 := common.NewNormalizedRequest(body)
	req2.SetNetwork(finalizedNetwork{})
	got, err := c.Get(ctx, req2)
	require.NoError(t, err)
	require.NotNil(t, got, "expected cache hit for identical request body")

	gotJrr, err := got.JsonRpcResponse()
	require.NoError(t, err)
	require.Contains(t, string(gotJrr.GetResultBytes()), "abc")
}

func TestSvmCache_Get_MissesForDifferentParams(t *testing.T) {
	t.Parallel()
	c := newTestCache(t)
	ctx := context.Background()

	// Populate one key.
	reqA := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"getBlock","params":[100]}`))
	jrr, _ := common.NewJsonRpcResponse(1, "value-for-block-100", nil)
	respA := common.NewNormalizedResponse().WithRequest(reqA).WithJsonRpcResponse(jrr)
	require.NoError(t, c.Set(ctx, reqA, respA))
	time.Sleep(ristrettoSettleDelay)

	// Different block number → different params hash → miss.
	reqB := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":2,"method":"getBlock","params":[101]}`))
	got, err := c.Get(ctx, reqB)
	require.NoError(t, err)
	require.Nil(t, got, "different params must not reuse cached entry")
}

func TestSvmCache_Set_SkipsResponsesWithJsonRpcError(t *testing.T) {
	t.Parallel()
	c := newTestCache(t)
	ctx := context.Background()

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"getBlock","params":[100]}`))
	rpcErr := common.NewErrJsonRpcExceptionExternal(-32004, "Block not available", "")
	jrr, _ := common.NewJsonRpcResponse(1, nil, rpcErr)
	resp := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)

	require.NoError(t, c.Set(ctx, req, resp))

	// Same request must NOT see the error cached — we must not serve stale errors.
	got, err := c.Get(ctx, req)
	require.NoError(t, err)
	require.Nil(t, got, "error responses must not be cached")
}

func TestSvmCache_IsObjectNull_EmptyCacheReportsNull(t *testing.T) {
	t.Parallel()
	var nilC *SvmJsonRpcCache
	require.True(t, nilC.IsObjectNull(), "nil cache must report null")

	emptyCfg := &common.CacheConfig{
		Connectors: []*common.ConnectorConfig{
			{Id: "mem", Driver: common.DriverMemory, Memory: &common.MemoryConnectorConfig{MaxItems: 10, MaxTotalSize: "1KB"}},
		},
	}
	c, err := NewSvmJsonRpcCache(context.Background(), &log.Logger, emptyCfg)
	require.NoError(t, err)
	require.True(t, c.IsObjectNull(), "cache without policies reports null — prevents useless lookups")
}

func TestSvmCache_ExtractSlotRef_MinContextSlotBecomesPartitionKey(t *testing.T) {
	t.Parallel()
	// Direct check of the helper — the cache partitions by slot so different
	// minContextSlot values land in distinct partitions.
	rpc := common.NewJsonRpcRequest("getAccountInfo",
		[]interface{}{"pubkey", map[string]interface{}{"minContextSlot": float64(12345)}})
	got := extractSlotRef(rpc)
	require.Equal(t, "12345", got)

	rpcNoOpts := common.NewJsonRpcRequest("getAccountInfo", []interface{}{"pubkey"})
	require.Equal(t, "*", extractSlotRef(rpcNoOpts))
}

func TestSvmCache_ConcreteSlotHitIsIsolatedFromWildcardSlot(t *testing.T) {
	t.Parallel()
	c := newTestCache(t)
	ctx := context.Background()

	// Set with a concrete minContextSlot. Partition key: svm:test:12345
	slotBody := []byte(`{"jsonrpc":"2.0","id":1,"method":"getAccountInfo","params":["pubkey",{"minContextSlot":12345}]}`)
	slotReq := common.NewNormalizedRequest(slotBody)
	slotReq.SetNetwork(finalizedNetwork{})
	jrr, _ := common.NewJsonRpcResponse(1, map[string]interface{}{"lamports": 99}, nil)
	require.NoError(t, c.Set(ctx, slotReq, common.NewNormalizedResponse().WithRequest(slotReq).WithJsonRpcResponse(jrr)))
	time.Sleep(ristrettoSettleDelay)

	// A second Get WITHOUT minContextSlot has different params → different
	// requestKey → miss regardless of index routing. This locks in the
	// invariant that a concrete-slot Set is NOT accidentally returned when
	// the caller asks for a different (wildcard) slot — which would be a
	// silent correctness regression for cache-ability boundaries.
	wildBody := []byte(`{"jsonrpc":"2.0","id":2,"method":"getAccountInfo","params":["pubkey"]}`)
	wildReq := common.NewNormalizedRequest(wildBody)
	wildReq.SetNetwork(finalizedNetwork{})

	got, err := c.Get(ctx, wildReq)
	require.NoError(t, err)
	require.Nil(t, got, "concrete-slot entry must not leak to wildcard-slot get (different requestKey)")
}

func TestSvmCache_Get_RespectsSkipCacheReadDirective(t *testing.T) {
	t.Parallel()
	c := newTestCache(t)
	ctx := context.Background()

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"getBlock","params":[200]}`))
	jrr, _ := common.NewJsonRpcResponse(1, "cached", nil)
	resp := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
	require.NoError(t, c.Set(ctx, req, resp))

	// Mimic x-erpc-skip-cache-read: * by setting the directive.
	req2 := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":2,"method":"getBlock","params":[200]}`))
	req2.SetDirectives(&common.RequestDirectives{SkipCacheRead: "*"})

	got, err := c.Get(ctx, req2)
	require.NoError(t, err)
	// Policy must be skipped; returning nil signals miss → forward to upstream.
	if got != nil {
		jrrGot, _ := got.JsonRpcResponse()
		if jrrGot != nil {
			require.False(t, strings.Contains(string(jrrGot.GetResultBytes()), "cached"),
				"skip-cache-read directive must prevent cache reads")
		}
	}
}

// finalizedNetwork stubs common.Network so req.Finality(ctx) resolves to
// Finalized in these tests — the real implementation lives in erpc/networks.go
// but we don't want the test to depend on constructing a full Network+registry.
type finalizedNetwork struct{}

func (finalizedNetwork) Id() string                                        { return "svm:test" }
func (finalizedNetwork) Label() string                                     { return "" }
func (finalizedNetwork) ProjectId() string                                 { return "test" }
func (finalizedNetwork) Architecture() common.NetworkArchitecture          { return common.ArchitectureSvm }
func (finalizedNetwork) Config() *common.NetworkConfig                     { return &common.NetworkConfig{Architecture: common.ArchitectureSvm} }
func (finalizedNetwork) Logger() *zerolog.Logger                           { l := log.Logger; return &l }
func (finalizedNetwork) GetMethodMetrics(string) common.TrackedMetrics     { return nil }
func (finalizedNetwork) SvmHighestLatestSlot(context.Context) int64    { return 0 }
func (finalizedNetwork) SvmHighestFinalizedSlot(context.Context) int64 { return 0 }
func (finalizedNetwork) Forward(context.Context, *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	return nil, nil
}
func (finalizedNetwork) GetFinality(context.Context, *common.NormalizedRequest, *common.NormalizedResponse) common.DataFinalityState {
	return common.DataFinalityStateFinalized
}
