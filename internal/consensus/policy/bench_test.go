package policy

import (
	"strconv"
	"testing"
	"time"
)

// benchUpstreams builds a mixed internal/external set of n upstreams with
// realistic tags, health, and block ranges so the eval exercises every helper.
func benchUpstreams(n int) []Upstream {
	ups := make([]Upstream, 0, n)
	for i := 0; i < n; i++ {
		id := "u" + strconv.Itoa(i)
		if i%3 == 0 {
			ups = append(ups, up(id, []string{"type:internal"}, StateHealthy, nil, &BlockRange{Lower: i64(1000), Upper: nil}))
		} else {
			ups = append(ups, up(id, []string{"type:external"}, StateHealthy, nil, nil))
		}
	}
	return ups
}

func benchCtx(n int) EvalContext {
	return EvalContext{
		Request:   Request{Method: "eth_getBalance", BlockNumber: i64(5_000), Network: "evm:1"},
		User:      &User{Auth: "consensus-waive"},
		Upstreams: benchUpstreams(n),
	}
}

// Steady-state eval of the §2 reference function on a pre-warmed VM at a range
// of upstream counts. The reported ns/op is the per-request selector cost to
// compare against upstream RTT (the ≪ 1ms Phase 6 gate).
func benchSteadyState(b *testing.B, n int) {
	s, err := Compile(referenceEval, 0)
	if err != nil {
		b.Fatal(err)
	}
	ctx := benchCtx(n)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Evaluate(ctx, 50*time.Millisecond); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEvaluate_Reference_5Upstreams(b *testing.B)  { benchSteadyState(b, 5) }
func BenchmarkEvaluate_Reference_20Upstreams(b *testing.B) { benchSteadyState(b, 20) }
func BenchmarkEvaluate_Reference_50Upstreams(b *testing.B) { benchSteadyState(b, 50) }

// Context binding alone: a trivial eval isolates the per-request fixed cost of
// marshalling + parsing + wrapping the EvalContext from the eval logic itself.
func BenchmarkEvaluate_CtxBindingOnly(b *testing.B) {
	s, err := Compile(`(ctx) => "standard"`, 0)
	if err != nil {
		b.Fatal(err)
	}
	ctx := benchCtx(20)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Evaluate(ctx, 50*time.Millisecond); err != nil {
			b.Fatal(err)
		}
	}
}

// Cold VM: building a runtime, installing the stdlib, and evaluating the
// function for the first time — the cost the pre-warm pool amortizes away.
func BenchmarkEvaluate_ColdVM(b *testing.B) {
	fnProg, err := compileFn(referenceEval, 0)
	if err != nil {
		b.Fatal(err)
	}
	ctx := benchCtx(20)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		v, err := fnProg.build()
		if err != nil {
			b.Fatal(err)
		}
		// size 0 disables async refill; alive counts the one-shot VM so
		// Evaluate does not drive alive negative or spawn unreachable builds.
		p := &pool{free: make(chan *vm, 1), build: fnProg.build, size: 0, alive: 1}
		p.free <- v
		sel := &Selector{pool: p}
		if _, err := sel.Evaluate(ctx, 50*time.Millisecond); err != nil {
			b.Fatal(err)
		}
	}
}

// Warm VM: repeated evals on a pooled VM (the production steady state).
func BenchmarkEvaluate_WarmVM(b *testing.B) { benchSteadyState(b, 20) }

// Concurrent throughput across the pool. Pool-exhaustion fail-closed is an
// expected outcome under burst, not an error, so it does not fail the bench.
func BenchmarkEvaluate_Concurrent(b *testing.B) {
	s, err := Compile(referenceEval, 0)
	if err != nil {
		b.Fatal(err)
	}
	ctx := benchCtx(20)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := s.Evaluate(ctx, 50*time.Millisecond); err != nil {
				b.Fatal(err)
			}
		}
	})
}
