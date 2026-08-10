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

// TestSvmCache_NeverCacheMethods_AreNotStoredOrRead is the regression guard for
// the hard never-cache rule. finalizedNetwork resolves finality to Finalized and
// newTestCache has a Finalized catch-all policy, so WITHOUT the method-level
// guard these effectful / freshness-critical methods would match and be cached.
// They must not be.
func TestSvmCache_NeverCacheMethods_AreNotStoredOrRead(t *testing.T) {
	t.Parallel()
	c := newTestCache(t)
	ctx := context.Background()

	for _, method := range []string{"sendTransaction", "getLatestBlockhash", "getSignatureStatuses"} {
		body := []byte(`{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":["x"]}`)
		req := common.NewNormalizedRequest(body)
		req.SetNetwork(finalizedNetwork{})
		jrr, _ := common.NewJsonRpcResponse(1, "should-not-be-cached", nil)
		resp := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)

		require.NoError(t, c.Set(ctx, req, resp), "%s Set", method)
		time.Sleep(ristrettoSettleDelay)

		req2 := common.NewNormalizedRequest(body)
		req2.SetNetwork(finalizedNetwork{})
		got, err := c.Get(ctx, req2)
		require.NoError(t, err, "%s Get", method)
		require.Nil(t, got, "%s must never be cached (neverCacheMethods)", method)
	}
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

// TestSvmCache_RequestKey_PreservesBase58Case is the regression guard for the
// cross-account leak: the shared req.CacheHash() lowercases every string param
// (correct EVM hex normalization), which aliased two DISTINCT Solana accounts
// whose base58 addresses differ only by letter case onto one cache key — so
// each could be served the other's balance/account data.
func TestSvmCache_RequestKey_PreservesBase58Case(t *testing.T) {
	t.Parallel()
	// Two well-formed base58 pubkeys differing only in the case of one letter.
	upper := common.NewJsonRpcRequest("getAccountInfo", []interface{}{"4Nd1mBQtrMJVYVfKf2PJy9NZUZdTAsp7D4xWLs4gDB4T"})
	lower := common.NewJsonRpcRequest("getAccountInfo", []interface{}{"4nd1mBQtrMJVYVfKf2PJy9NZUZdTAsp7D4xWLs4gDB4T"})

	upperKey, err := svmRequestKey(upper)
	require.NoError(t, err)
	lowerKey, err := svmRequestKey(lower)
	require.NoError(t, err)
	require.NotEqual(t, upperKey, lowerKey,
		"base58 pubkeys differing only by case must not share a cache key")

	// Same guard for transaction signatures, which are base58 too.
	sigA := common.NewJsonRpcRequest("getTransaction", []interface{}{"5VERv8NMvzbJMEkV8xnrLkEaWRtSz9CosKDYjCJjBRnbJLgp8uirBgmQpjKhoR4tjF3ZpRzrFmBV6UjKdiSZkQUW"})
	sigB := common.NewJsonRpcRequest("getTransaction", []interface{}{"5vERv8NMvzbJMEkV8xnrLkEaWRtSz9CosKDYjCJjBRnbJLgp8uirBgmQpjKhoR4tjF3ZpRzrFmBV6UjKdiSZkQUW"})
	keyA, err := svmRequestKey(sigA)
	require.NoError(t, err)
	keyB, err := svmRequestKey(sigB)
	require.NoError(t, err)
	require.NotEqual(t, keyA, keyB, "signatures differing only by case must not share a cache key")

	// And identical params still produce a stable key (cache hits still work).
	again, err := svmRequestKey(common.NewJsonRpcRequest("getAccountInfo", []interface{}{"4Nd1mBQtrMJVYVfKf2PJy9NZUZdTAsp7D4xWLs4gDB4T"}))
	require.NoError(t, err)
	require.Equal(t, upperKey, again, "identical params must produce an identical key")
}

// TestSvmCache_RequestKey_IsTypeAndStructureDelimited locks in that structurally
// different params cannot be concatenated into the same digest.
func TestSvmCache_RequestKey_IsTypeAndStructureDelimited(t *testing.T) {
	t.Parallel()
	keyOf := func(params ...interface{}) string {
		k, err := svmRequestKey(common.NewJsonRpcRequest("getAccountInfo", params))
		require.NoError(t, err)
		return k
	}

	distinct := map[string]string{
		`"abc"`:         keyOf("abc"),
		`["a","bc"]`:    keyOf([]interface{}{"a", "bc"}),
		`{"a":"bc"}`:    keyOf(map[string]interface{}{"a": "bc"}),
		`"a","bc"`:      keyOf("a", "bc"),
		`number 1`:      keyOf(float64(1)),
		`string "1"`:    keyOf("1"),
		`bool true`:     keyOf(true),
		`string "true"`: keyOf("true"),
	}
	seen := make(map[string]string, len(distinct))
	for label, key := range distinct {
		if other, dup := seen[key]; dup {
			t.Errorf("cache key collision between %s and %s", label, other)
		}
		seen[key] = label
	}

	// Map iteration order must not leak into the key.
	multi := map[string]interface{}{"z": 1.0, "a": 2.0, "m": 3.0, "b": 4.0}
	first := keyOf(multi)
	for range 20 {
		require.Equal(t, first, keyOf(multi), "key must be independent of map iteration order")
	}
}

// TestSvmCache_CaseDifferingAddressesDoNotShareCachedData walks the full
// Set→Get path: caching account A must never satisfy a lookup for account a.
func TestSvmCache_CaseDifferingAddressesDoNotShareCachedData(t *testing.T) {
	t.Parallel()
	c := newTestCache(t)
	ctx := context.Background()

	const upper = "4Nd1mBQtrMJVYVfKf2PJy9NZUZdTAsp7D4xWLs4gDB4T"
	const lower = "4nd1mBQtrMJVYVfKf2PJy9NZUZdTAsp7D4xWLs4gDB4T"

	setReq := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"getAccountInfo","params":["` + upper + `"]}`))
	setReq.SetNetwork(finalizedNetwork{})
	jrr, _ := common.NewJsonRpcResponse(1, map[string]interface{}{"lamports": 42}, nil)
	require.NoError(t, c.Set(ctx, setReq, common.NewNormalizedResponse().WithRequest(setReq).WithJsonRpcResponse(jrr)))
	time.Sleep(ristrettoSettleDelay)

	getReq := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":2,"method":"getAccountInfo","params":["` + lower + `"]}`))
	getReq.SetNetwork(finalizedNetwork{})
	got, err := c.Get(ctx, getReq)
	require.NoError(t, err)
	require.Nil(t, got, "a different account (case-differing base58) must not be served cached data")
}

// TestSvmCache_CompressionRoundTrip proves the SVM path honors cache.compression
// the way the EVM path does — Solana getBlock payloads reach megabytes, so
// ignoring the (default-on) setting silently multiplies storage cost.
func TestSvmCache_CompressionRoundTrip(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	enabled := true
	c, err := NewSvmJsonRpcCache(ctx, &log.Logger, &common.CacheConfig{
		Connectors: []*common.ConnectorConfig{
			{Id: "mem", Driver: common.DriverMemory, Memory: &common.MemoryConnectorConfig{MaxItems: 1000, MaxTotalSize: "8MB"}},
		},
		Policies: []*common.CachePolicyConfig{
			{Connector: "mem", Network: "*", Method: "*", Finality: common.DataFinalityStateFinalized},
		},
		Compression: &common.CompressionConfig{Enabled: &enabled, ZstdLevel: "fastest", Threshold: 512},
	})
	require.NoError(t, err)

	// Highly compressible, comfortably over the threshold.
	blob := strings.Repeat("SolanaBlockPayload", 400)
	compressed := c.compress([]byte(blob))
	require.Less(t, len(compressed), len(blob), "large payloads must actually shrink")
	require.Equal(t, []byte{0x28, 0xB5, 0x2F, 0xFD}, compressed[:4], "stored bytes must carry the zstd magic")
	back, err := c.decompress(compressed)
	require.NoError(t, err)
	require.Equal(t, blob, string(back))

	// Sub-threshold payloads are stored verbatim and read back untouched.
	small := []byte(`{"lamports":1}`)
	require.Equal(t, small, c.compress(small))
	passthrough, err := c.decompress(small)
	require.NoError(t, err)
	require.Equal(t, small, passthrough)

	// End-to-end: a compressed entry must decode transparently on Get.
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"getBlock","params":[7,{"commitment":"finalized"}]}`)
	req := common.NewNormalizedRequest(body)
	req.SetNetwork(finalizedNetwork{})
	jrr, err := common.NewJsonRpcResponse(1, map[string]interface{}{"blockhash": blob}, nil)
	require.NoError(t, err)
	require.NoError(t, c.Set(ctx, req, common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)))
	time.Sleep(ristrettoSettleDelay)

	req2 := common.NewNormalizedRequest(body)
	req2.SetNetwork(finalizedNetwork{})
	got, err := c.Get(ctx, req2)
	require.NoError(t, err)
	require.NotNil(t, got, "compressed entry must be readable")
	gotJrr, err := got.JsonRpcResponse()
	require.NoError(t, err)
	require.Contains(t, string(gotJrr.GetResultBytes()), blob)
}

// finalizedNetwork stubs common.Network so req.Finality(ctx) resolves to
// Finalized in these tests — the real implementation lives in erpc/networks.go
// but we don't want the test to depend on constructing a full Network+registry.
type finalizedNetwork struct{}

func (finalizedNetwork) Id() string                               { return "svm:test" }
func (finalizedNetwork) Label() string                            { return "" }
func (finalizedNetwork) ProjectId() string                        { return "test" }
func (finalizedNetwork) Architecture() common.NetworkArchitecture { return common.ArchitectureSvm }
func (finalizedNetwork) Config() *common.NetworkConfig {
	return &common.NetworkConfig{Architecture: common.ArchitectureSvm}
}
func (finalizedNetwork) Logger() *zerolog.Logger                       { l := log.Logger; return &l }
func (finalizedNetwork) GetMethodMetrics(string) common.TrackedMetrics { return nil }
func (finalizedNetwork) SvmHighestLatestSlot(context.Context) int64    { return 0 }
func (finalizedNetwork) SvmHighestFinalizedSlot(context.Context) int64 { return 0 }
func (finalizedNetwork) SvmHighestIndexedSlot(context.Context) int64   { return 0 }
func (finalizedNetwork) Forward(context.Context, *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	return nil, nil
}
func (finalizedNetwork) GetFinality(context.Context, *common.NormalizedRequest, *common.NormalizedResponse) common.DataFinalityState {
	return common.DataFinalityStateFinalized
}

// TestRequestKey_PreservesBase58Case is the multiplexing counterpart to the
// cache-key case fix. It asserts BOTH halves of the reason RequestKey exists:
// the shared CacheHash collapses two distinct base58 accounts that differ only
// by letter case, and RequestKey keeps them apart. Any component that decides
// request identity on an SVM network (cache, in-flight multiplexer) must use
// the latter — a follower is handed the leader's response verbatim, so a
// collision serves one account's balance for another account's request.
func TestRequestKey_PreservesBase58Case(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Same pubkey shape, differing only in case — two DISTINCT valid addresses.
	lower := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"getBalance","params":["dRiFTyPePEHfLqZBUyHFLGVW3d5Fk1AmmZoRbCxDMDy"]}`))
	upper := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":2,"method":"getBalance","params":["DRiFTyPePEHfLqZBUyHFLGVW3d5Fk1AmmZoRbCxDMDy"]}`))

	// Premise: the shared hasher really does collide on these. If this ever
	// stops being true, RequestKey is no longer load-bearing and this whole
	// indirection can go away.
	lowerCacheHash, err := lower.CacheHash()
	require.NoError(t, err)
	upperCacheHash, err := upper.CacheHash()
	require.NoError(t, err)
	require.Equal(t, lowerCacheHash, upperCacheHash,
		"premise: common CacheHash is expected to collapse base58 case")

	lowerKey, err := RequestKey(ctx, lower)
	require.NoError(t, err)
	upperKey, err := RequestKey(ctx, upper)
	require.NoError(t, err)
	require.NotEqual(t, lowerKey, upperKey,
		"distinct base58 accounts must not share a request identity")

	// Identical requests must still agree, or multiplexing and caching stop
	// deduplicating anything.
	same := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":3,"method":"getBalance","params":["dRiFTyPePEHfLqZBUyHFLGVW3d5Fk1AmmZoRbCxDMDy"]}`))
	sameKey, err := RequestKey(ctx, same)
	require.NoError(t, err)
	require.Equal(t, lowerKey, sameKey, "identical params must produce one identity")

	// Method is part of the identity: same params, different method.
	otherMethod := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":4,"method":"getAccountInfo","params":["dRiFTyPePEHfLqZBUyHFLGVW3d5Fk1AmmZoRbCxDMDy"]}`))
	otherKey, err := RequestKey(ctx, otherMethod)
	require.NoError(t, err)
	require.NotEqual(t, lowerKey, otherKey, "method must be part of request identity")
}
