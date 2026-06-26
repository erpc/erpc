package common

import (
	"context"
	"testing"
)

// These benchmarks guard the matcher hot path: MatchMatchers runs on the
// failsafe-selection path for (potentially) every request, so MatchConfig
// must stay allocation-light and the pipeline must stay lock-free.

func benchReq(method string, finality DataFinalityState, params ...interface{}) *NormalizedRequest {
	r := NewNormalizedRequestFromJsonRpcRequest(&JsonRpcRequest{Method: method, Params: params})
	r.finality.Store(finality)
	return r
}

func BenchmarkMatchConfig_StarMethodOnly(b *testing.B) {
	cfg := &MatcherConfig{Method: "*"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = MatchConfig(cfg, "evm:1", "eth_call", nil, DataFinalityStateUnknown, false)
	}
}

func BenchmarkMatchConfig_PrefixWildcard(b *testing.B) {
	cfg := &MatcherConfig{Method: "eth_*"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = MatchConfig(cfg, "evm:1", "eth_getLogs", nil, DataFinalityStateUnknown, false)
	}
}

func BenchmarkMatchConfig_NetworkMethodFinality(b *testing.B) {
	cfg := &MatcherConfig{Network: "evm:*", Method: "eth_*", Finality: []DataFinalityState{DataFinalityStateRealtime}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = MatchConfig(cfg, "evm:1", "eth_blockNumber", nil, DataFinalityStateRealtime, false)
	}
}

func BenchmarkMatchConfig_Params(b *testing.B) {
	cfg := &MatcherConfig{Method: "eth_getBlockByNumber", Params: []interface{}{"latest"}}
	params := []interface{}{"latest", false}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = MatchConfig(cfg, "evm:1", "eth_getBlockByNumber", params, DataFinalityStateRealtime, false)
	}
}

func BenchmarkMatchMatchers_SingleStar(b *testing.B) {
	ctx := context.Background()
	ms := []*MatcherConfig{{Method: "*", Action: MatcherInclude}}
	req := benchReq("eth_call", DataFinalityStateUnknown)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = MatchMatchers(ctx, ms, req, nil)
	}
}

func BenchmarkMatchMatchers_ExcludeThenInclude(b *testing.B) {
	ctx := context.Background()
	ms := []*MatcherConfig{
		{Method: "*", Action: MatcherExclude},
		{Method: "eth_*", Action: MatcherInclude},
	}
	req := benchReq("eth_getLogs", DataFinalityStateUnknown)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = MatchMatchers(ctx, ms, req, nil)
	}
}

func BenchmarkMatchMatchers_Parallel(b *testing.B) {
	ctx := context.Background()
	ms := []*MatcherConfig{
		{Method: "eth_call", Action: MatcherInclude},
		{Method: "eth_getLogs", Action: MatcherInclude},
		{Method: "*", Action: MatcherExclude},
	}
	req := benchReq("eth_getLogs", DataFinalityStateUnknown)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = MatchMatchers(ctx, ms, req, nil)
		}
	})
}
