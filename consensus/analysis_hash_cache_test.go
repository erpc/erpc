package consensus

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/failsafe-go/failsafe-go"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type testExecution struct {
	ctx context.Context
}

var _ failsafe.Execution[*common.NormalizedResponse] = testExecution{}

func (e testExecution) Context() context.Context {
	if e.ctx != nil {
		return e.ctx
	}
	return context.Background()
}

func (e testExecution) Attempts() int                          { return 1 }
func (e testExecution) Executions() int                        { return 1 }
func (e testExecution) Retries() int                           { return 0 }
func (e testExecution) Hedges() int                            { return 0 }
func (e testExecution) StartTime() time.Time                   { return time.Unix(0, 0) }
func (e testExecution) ElapsedTime() time.Duration             { return 0 }
func (e testExecution) LastResult() *common.NormalizedResponse { return nil }
func (e testExecution) LastError() error                       { return nil }
func (e testExecution) IsFirstAttempt() bool                   { return true }
func (e testExecution) IsRetry() bool                          { return false }
func (e testExecution) IsHedge() bool                          { return false }
func (e testExecution) AttemptStartTime() time.Time            { return time.Unix(0, 0) }
func (e testExecution) ElapsedAttemptTime() time.Duration      { return 0 }
func (e testExecution) IsCanceled() bool                       { return false }
func (e testExecution) Canceled() <-chan struct{}              { return nil }

func newTestExecution(method string) failsafe.Execution[*common.NormalizedResponse] {
	req := common.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"%s","params":[]}`, method)))
	ctx := context.WithValue(context.Background(), common.RequestContextKey, req)
	return testExecution{ctx: ctx}
}

func newTestResponse(t *testing.T, result any) *common.NormalizedResponse {
	t.Helper()
	jrr, err := common.NewJsonRpcResponse(1, result, nil)
	require.NoError(t, err)
	return common.NewNormalizedResponse().WithJsonRpcResponse(jrr)
}

// classifyAndHashResponseLegacy mirrors classifyAndHashResponse behavior before cache short-circuit.
func classifyAndHashResponseLegacy(r *execResult, exec failsafe.Execution[*common.NormalizedResponse], cfg *config) {
	if r.Err != nil {
		if isConsensusValidError(r.Err) || isAgreedUponError(r.Err) {
			r.CachedResponseType = ResponseTypeConsensusError
		} else {
			r.CachedResponseType = ResponseTypeInfrastructureError
		}
		r.CachedHash, _ = resultOrErrorToHash(r, exec, cfg)
		if r.CachedHash == "" {
			r.CachedHash = "error:generic"
		}
		r.CachedResponseSize = 0
		return
	}

	jr := resultToJsonRpcResponse(r.Result, exec)
	if jr == nil {
		r.CachedResponseType = ResponseTypeInfrastructureError
		r.CachedHash = "error:generic"
		return
	}

	if r.Result != nil && r.Result.IsResultEmptyish(exec.Context()) {
		r.CachedResponseType = ResponseTypeEmpty
	} else {
		r.CachedResponseType = ResponseTypeNonEmpty
	}

	if size, err := jr.Size(exec.Context()); err == nil {
		r.CachedResponseSize = size
	}

	r.CachedHash, _ = resultOrErrorToHash(r, exec, cfg)
	if r.CachedHash == "" {
		r.CachedResponseType = ResponseTypeInfrastructureError
		r.CachedHash = "error:generic"
	}
}

func TestClassifyAndHashResponse_CacheMatchesLegacy(t *testing.T) {
	t.Parallel()

	const method = "eth_getBlockByNumber"
	exec := newTestExecution(method)
	cfg := &config{}

	resp := newTestResponse(t, map[string]any{
		"number":           "0x1234",
		"transactionsRoot": "0xabc",
		"transactions":     []any{"0x1", "0x2", "0x3"},
	})

	legacy := &execResult{Result: resp}
	classifyAndHashResponseLegacy(legacy, exec, cfg)

	cached := &execResult{Result: resp}
	classifyAndHashResponse(cached, exec, cfg)
	classifyAndHashResponse(cached, exec, cfg) // second pass must reuse cached values

	assert.Equal(t, legacy.CachedHash, cached.CachedHash)
	assert.Equal(t, legacy.CachedResponseType, cached.CachedResponseType)
	assert.Equal(t, legacy.CachedResponseSize, cached.CachedResponseSize)
}

func TestClassifyAndHashResponse_CacheMatchesLegacy_WithIgnoredFields(t *testing.T) {
	t.Parallel()

	const method = "eth_getLogs"
	exec := newTestExecution(method)
	cfg := &config{
		ignoreFields: map[string][]string{
			method: {"logs.*.blockTimestamp", "logs.*.logIndex"},
		},
	}

	resp1 := newTestResponse(t, map[string]any{
		"logs": []any{
			map[string]any{"address": "0xaaa", "blockTimestamp": "0x1", "logIndex": "0x1", "data": "0xabc"},
			map[string]any{"address": "0xbbb", "blockTimestamp": "0x2", "logIndex": "0x2", "data": "0xdef"},
		},
	})
	resp2 := newTestResponse(t, map[string]any{
		"logs": []any{
			map[string]any{"address": "0xaaa", "blockTimestamp": "0x99", "logIndex": "0x99", "data": "0xabc"},
			map[string]any{"address": "0xbbb", "blockTimestamp": "0x88", "logIndex": "0x88", "data": "0xdef"},
		},
	})

	legacy1 := &execResult{Result: resp1}
	legacy2 := &execResult{Result: resp2}
	classifyAndHashResponseLegacy(legacy1, exec, cfg)
	classifyAndHashResponseLegacy(legacy2, exec, cfg)
	require.Equal(t, legacy1.CachedHash, legacy2.CachedHash, "legacy behavior should match when ignored fields differ")

	cached1 := &execResult{Result: resp1}
	cached2 := &execResult{Result: resp2}
	classifyAndHashResponse(cached1, exec, cfg)
	classifyAndHashResponse(cached2, exec, cfg)
	classifyAndHashResponse(cached1, exec, cfg)
	classifyAndHashResponse(cached2, exec, cfg)

	assert.Equal(t, legacy1.CachedHash, cached1.CachedHash)
	assert.Equal(t, legacy2.CachedHash, cached2.CachedHash)
	assert.Equal(t, cached1.CachedHash, cached2.CachedHash)

	lg := zerolog.Nop()
	analysis := newConsensusAnalysis(&lg, exec, cfg, []*execResult{cached1, cached2})
	require.Len(t, analysis.groups, 1)
	for _, group := range analysis.groups {
		assert.Equal(t, 2, group.Count)
	}
}

func BenchmarkClassifyAndHashResponse_HashCacheReuse(b *testing.B) {
	const method = "eth_getLogs"
	exec := newTestExecution(method)
	cfg := &config{
		ignoreFields: map[string][]string{
			method: {"logs.*.blockTimestamp", "logs.*.logIndex", "logs.*.transactionIndex"},
		},
	}
	resp := newTestResponseForBench(b, map[string]any{
		"logs": []any{
			map[string]any{
				"address":          "0xaaa",
				"blockTimestamp":   "0x111",
				"logIndex":         "0x1",
				"transactionIndex": "0x1",
				"topics":           []any{"0xa", "0xb", "0xc"},
				"data":             "0xabc",
			},
			map[string]any{
				"address":          "0xbbb",
				"blockTimestamp":   "0x222",
				"logIndex":         "0x2",
				"transactionIndex": "0x2",
				"topics":           []any{"0xd", "0xe", "0xf"},
				"data":             "0xdef",
			},
		},
	})

	b.Run("legacy_like_recompute_per_pass", func(b *testing.B) {
		r := &execResult{Result: resp}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			r.CachedHash = ""
			r.CachedResponseType = ResponseTypeNonEmpty
			r.CachedResponseSize = 0
			classifyAndHashResponse(r, exec, cfg)
		}
	})

	b.Run("cached_reuse_per_pass", func(b *testing.B) {
		r := &execResult{Result: resp}
		classifyAndHashResponse(r, exec, cfg)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			classifyAndHashResponse(r, exec, cfg)
		}
	})
}

func newTestResponseForBench(b *testing.B, result any) *common.NormalizedResponse {
	b.Helper()
	jrr, err := common.NewJsonRpcResponse(1, result, nil)
	if err != nil {
		b.Fatalf("failed to build json-rpc response: %v", err)
	}
	return common.NewNormalizedResponse().WithJsonRpcResponse(jrr)
}
