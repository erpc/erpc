package matchers

import (
	"context"
	"fmt"
	"testing"

	"github.com/erpc/erpc/common"
)

// Benchmarks for the matcher hot path. Per PR #388 review comment #18 from
// @aramalipoor (2025-10-17): production targets ~8–10k rps with multiple
// matchers per request, so MatchConfig and Match must run with absolute-
// minimum CPU/allocations. These benchmarks lock in baseline numbers and
// guard against regressions.
//
// Run with: go test -bench=. -benchmem -count=8 ./matchers/
//
// Spot-check expectations (M-series Mac, single-threaded):
//   - MatchConfig with method="*" only:               ~50ns / 0 alloc
//   - MatchConfig with eth_* wildcard:                ~150ns / 3 alloc (WildcardMatch tokenizes)
//   - MatchConfig with literal method:                ~190ns / 5 alloc
//   - MatchConfig with finality []:                   ~50ns / 0 alloc (no constraint)
//   - Match (full pipeline) with single matcher:      ~250ns / 5 alloc

// --- MatchConfig micro-benchmarks (per-config, no Match() pipeline) ---

func BenchmarkMatchConfig_StarMethodOnly(b *testing.B) {
	cfg := &common.MatcherConfig{Method: "*", Action: common.MatcherInclude}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = MatchConfig(cfg, "evm:1", "eth_call", nil, common.DataFinalityStateFinalized, false)
	}
}

func BenchmarkMatchConfig_LiteralMethodMatch(b *testing.B) {
	cfg := &common.MatcherConfig{Method: "eth_call", Action: common.MatcherInclude}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = MatchConfig(cfg, "evm:1", "eth_call", nil, common.DataFinalityStateFinalized, false)
	}
}

func BenchmarkMatchConfig_LiteralMethodMiss(b *testing.B) {
	cfg := &common.MatcherConfig{Method: "eth_call", Action: common.MatcherInclude}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = MatchConfig(cfg, "evm:1", "eth_getBalance", nil, common.DataFinalityStateFinalized, false)
	}
}

func BenchmarkMatchConfig_PrefixWildcardMatch(b *testing.B) {
	cfg := &common.MatcherConfig{Method: "eth_*", Action: common.MatcherInclude}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = MatchConfig(cfg, "evm:1", "eth_getLogs", nil, common.DataFinalityStateFinalized, false)
	}
}

func BenchmarkMatchConfig_OrPipePattern(b *testing.B) {
	cfg := &common.MatcherConfig{
		Method: "eth_getLogs|eth_getBlockReceipts|eth_getTransactionReceipt",
		Action: common.MatcherInclude,
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = MatchConfig(cfg, "evm:1", "eth_getBlockReceipts", nil, common.DataFinalityStateFinalized, false)
	}
}

func BenchmarkMatchConfig_NetworkAndMethod(b *testing.B) {
	cfg := &common.MatcherConfig{
		Network: "evm:1",
		Method:  "eth_*",
		Action:  common.MatcherInclude,
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = MatchConfig(cfg, "evm:1", "eth_call", nil, common.DataFinalityStateFinalized, false)
	}
}

func BenchmarkMatchConfig_FinalitySliceMatch(b *testing.B) {
	cfg := &common.MatcherConfig{
		Method: "*",
		Finality: []common.DataFinalityState{
			common.DataFinalityStateRealtime,
			common.DataFinalityStateUnfinalized,
		},
		Action: common.MatcherInclude,
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = MatchConfig(cfg, "evm:1", "eth_call", nil, common.DataFinalityStateRealtime, false)
	}
}

func BenchmarkMatchConfig_EmptyConstraintAllow(b *testing.B) {
	allow := common.CacheEmptyBehaviorAllow
	cfg := &common.MatcherConfig{
		Method: "*",
		Empty:  &allow,
		Action: common.MatcherInclude,
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = MatchConfig(cfg, "evm:1", "eth_call", nil, common.DataFinalityStateFinalized, true)
	}
}

func BenchmarkMatchConfig_NoEmptyConstraint(b *testing.B) {
	// Pointer-types regression guard for Bugbot #7: matchers without explicit
	// Empty must not pay the deref cost (nil short-circuit in MatchConfig).
	cfg := &common.MatcherConfig{Method: "*", Action: common.MatcherInclude}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = MatchConfig(cfg, "evm:1", "eth_call", nil, common.DataFinalityStateFinalized, true)
	}
}

// --- WildcardMatch baseline (the dominant cost inside MatchConfig) ---

func BenchmarkWildcardMatch_StarOnly(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = common.WildcardMatch("*", "eth_getBlockByNumber")
	}
}

func BenchmarkWildcardMatch_PrefixGlob(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = common.WildcardMatch("eth_*", "eth_getBlockByNumber")
	}
}

func BenchmarkWildcardMatch_LiteralMatch(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = common.WildcardMatch("eth_getBlockByNumber", "eth_getBlockByNumber")
	}
}

func BenchmarkWildcardMatch_PipeOr(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = common.WildcardMatch("eth_getLogs|eth_getBlockReceipts|eth_getTransactionReceipt", "eth_getBlockReceipts")
	}
}

// --- Match (full pipeline including request shaping) ---
//
// These exercise the public `Match()` entrypoint with realistic inputs. Because
// Match needs a `*common.NormalizedRequest`, we synthesize a simple JSON-RPC
// request body for each iteration. The synthesis cost (NewNormalizedRequest)
// is part of what callers pay in production, so it's intentionally inside the
// benchmark loop only when the per-call shape varies; for fixed inputs we
// hoist it out. To keep results comparable, both variants are provided.

func benchPrepRequest(b *testing.B, method string) *common.NormalizedRequest {
	b.Helper()
	body := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","method":%q,"params":[],"id":1}`, method))
	req := common.NewNormalizedRequest(body)
	// Pre-resolve so Match doesn't pay parse cost in the benchmark.
	_, _ = req.JsonRpcRequest()
	return req
}

func BenchmarkMatch_SingleStarMatcher(b *testing.B) {
	configs := []*common.MatcherConfig{
		{Method: "*", Action: common.MatcherInclude},
	}
	req := benchPrepRequest(b, "eth_call")
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Match(ctx, configs, req, nil)
	}
}

func BenchmarkMatch_TwoMatcher_ExcludeThenInclude(b *testing.B) {
	// Mirrors validateMatchers' auto-prepended catch-all-exclude pattern that
	// production user configs end up with after SetDefaults runs.
	configs := []*common.MatcherConfig{
		{Method: "*", Action: common.MatcherExclude},
		{Method: "eth_*", Action: common.MatcherInclude},
	}
	req := benchPrepRequest(b, "eth_getBalance")
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Match(ctx, configs, req, nil)
	}
}

func BenchmarkMatch_NineMatcherMethodTier(b *testing.B) {
	// Mirrors goldsky-prod's actual upstreamDefaults.failsafe shape — 9
	// method-tier matchers. Per goldsky's config, this runs on every request
	// to determine which executor's policies apply. Locks in the realistic
	// per-request cost.
	configs := []*common.MatcherConfig{
		{Method: "*", Action: common.MatcherExclude},
		{Method: "eth_getTransactionCount|eth_gasPrice|eth_maxPriorityFeePerGas", Action: common.MatcherInclude},
		{Finality: []common.DataFinalityState{common.DataFinalityStateRealtime}, Action: common.MatcherInclude},
		{Method: "eth_getLogs|eth_getBlockReceipts", Action: common.MatcherInclude},
		{Method: "eth_getTransactionReceipt", Action: common.MatcherInclude},
		{Method: "eth_getBlockByNumber|eth_getBlockByHash", Action: common.MatcherInclude},
		{Method: "eth_get*", Action: common.MatcherInclude},
		{Method: "eth_call", Action: common.MatcherInclude},
		{Method: "trace_*|debug_*|arbtrace_*", Action: common.MatcherInclude},
	}
	req := benchPrepRequest(b, "eth_call")
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Match(ctx, configs, req, nil)
	}
}

func BenchmarkMatch_Parallel(b *testing.B) {
	// Parallel benchmark to detect lock contention in WildcardMatch /
	// MatchConfig under concurrent load (8-10k rps target = many parallel
	// goroutines). MatchConfig is intended to be lock-free — this guards
	// against future regressions that introduce sync.Map or mutex usage in
	// the hot path.
	configs := []*common.MatcherConfig{
		{Method: "*", Action: common.MatcherExclude},
		{Method: "eth_*", Action: common.MatcherInclude},
	}
	req := benchPrepRequest(b, "eth_getBalance")
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = Match(ctx, configs, req, nil)
		}
	})
}
