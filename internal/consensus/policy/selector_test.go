package policy

import (
	"errors"
	"testing"
	"time"
)

// The reference eval function from feature.md §2. Used across tests so the
// suite exercises the shape operators actually write, not a toy.
const referenceEval = `(ctx) => {
    if (ctx.upstreams.anyPunished()) return "standard"
    if (ctx.upstreams.anyOperatorCordon()) return "standard"
    const internals = ctx.upstreams.withTag("type:internal")
    if (internals.allUnavailable())
      return ctx.user?.auth === "consensus-fallback" ? "fallback" : "standard"
    if (ctx.user?.auth === "consensus-waive") {
      const n = ctx.request.blockNumber
      if (n !== null && internals.canServeBlock(n).length === 0)
        return "historical"
      if (n === null)
        return "standard-waive-missing"
    }
    if (ctx.user?.auth === "consensus-dev") return "generous-dev"
    return "standard"
  }`

func mustCompile(t *testing.T, js string) *Selector {
	t.Helper()
	s, err := Compile(js, 0)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return s
}

func i64(n int64) *int64 { return &n }

func up(id string, tags []string, state string, classes []string, ba *BlockRange) Upstream {
	return Upstream{
		ID:                id,
		Tags:              tags,
		Health:            Health{State: state, CordonClasses: classes},
		BlockAvailability: ba,
	}
}

// --- Fallthrough / fail-closed cases first (repo design razor). ---

func TestEvaluate_EmptyEvalFunction(t *testing.T) {
	if _, err := Compile("   ", 0); err == nil {
		t.Fatal("expected error compiling empty eval function")
	}
}

func TestEvaluate_NotAFunction(t *testing.T) {
	if _, err := Compile(`"standard"`, 0); err == nil {
		t.Fatal("expected error: eval function is not callable")
	}
}

func TestEvaluate_SyntaxError(t *testing.T) {
	if _, err := Compile(`(ctx) => { this is not js `, 0); err == nil {
		t.Fatal("expected compile error on invalid JS")
	}
}

func TestCompile_SetterOnFnGlobalDoesNotHangStartup(t *testing.T) {
	// A construction expression that installs a setter on the old JS global
	// used to store the callable, then returns a function. rt.Set of that
	// global used to run the setter outside runProgramBounded and hang
	// startup. The callable is now host-side, so Compile must return quickly.
	done := make(chan error, 1)
	go func() {
		_, err := Compile(`(() => {
			Object.defineProperty(globalThis, "__erpcConsensusFn", {set() { while (true) {} }});
			return () => "standard"
		})()`, 50*time.Millisecond)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Compile: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Compile did not return — construction Set ran an operator setter")
	}
}

func TestEvaluate_PriorEvalSetterCannotHangBind(t *testing.T) {
	// A prior eval that defines a setter on the old per-request JS global
	// used to hang the next request's rt.Set of that global, outside the
	// timed RunProgram. Context is now passed as a Go call argument.
	s := mustCompile(t, `(ctx) => {
		Object.defineProperty(globalThis, "__erpcConsensusCtx", {set() { while (true) {} }});
		return "standard"
	}`)
	if _, err := s.Evaluate(EvalContext{Request: Request{Method: "eth_call"}}, 50*time.Millisecond); err != nil {
		t.Fatalf("first Evaluate: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := s.Evaluate(EvalContext{Request: Request{Method: "eth_call"}}, 50*time.Millisecond)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("second Evaluate: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Evaluate did not return — context bind ran an operator setter")
	}
}

func TestCompile_BoundsRunawayConstructionEval(t *testing.T) {
	// A syntactically valid expression that executes an infinite loop before
	// yielding the callable. Without a construction bound this hangs pool build
	// (and startup) forever; with buildTimeout it must fail loudly instead.
	done := make(chan error, 1)
	go func() {
		_, err := Compile(`(() => { while (true) {} })()`, 20*time.Millisecond)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected a bounded construction eval to fail, got nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Compile did not return — construction eval was not bounded")
	}
}

// The eval runs in a bare sandbox: env / process.env / console must be absent,
// so an operator function cannot read host-process secrets through the VM.
func TestEvaluate_SandboxOmitsHostEnv(t *testing.T) {
	t.Setenv("ERPC_POLICY_SANDBOX_SECRET", "leak-me")
	s := mustCompile(t, `(ctx) => (typeof process === "undefined" && typeof env === "undefined" && typeof console === "undefined") ? "sandboxed" : "leaked"`)
	name, err := s.Evaluate(EvalContext{Request: Request{Method: "eth_getBalance"}}, time.Second)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if name != "sandboxed" {
		t.Fatalf("eval saw host globals (env/process/console): got %q, want %q", name, "sandboxed")
	}
}

func TestEvaluate_NilUser(t *testing.T) {
	s := mustCompile(t, referenceEval)
	// No user, healthy internals present → default standard, no throw on ctx.user?.
	ctx := EvalContext{
		Request:   Request{Method: "eth_getBalance", BlockNumber: i64(100), Network: "evm:1"},
		User:      nil,
		Upstreams: []Upstream{up("i1", []string{"type:internal"}, StateHealthy, nil, nil)},
	}
	name, err := s.Evaluate(ctx, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if name != "standard" {
		t.Fatalf("nil user: got %q, want standard", name)
	}
}

func TestEvaluate_EmptyUpstreams(t *testing.T) {
	s := mustCompile(t, referenceEval)
	// allUnavailable() is false on an empty set, so no fallback selection.
	ctx := EvalContext{
		Request:   Request{Method: "eth_getBalance", BlockNumber: i64(100)},
		User:      &User{Auth: "consensus-fallback"},
		Upstreams: nil,
	}
	name, err := s.Evaluate(ctx, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if name != "standard" {
		t.Fatalf("empty upstreams: got %q, want standard", name)
	}
}

func TestEvaluate_NullReturnIsDefault(t *testing.T) {
	s := mustCompile(t, `(ctx) => null`)
	name, err := s.Evaluate(EvalContext{}, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if name != "" {
		t.Fatalf("null return: got %q, want empty", name)
	}
}

func TestEvaluate_UndefinedReturnIsDefault(t *testing.T) {
	s := mustCompile(t, `(ctx) => { }`)
	name, err := s.Evaluate(EvalContext{}, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if name != "" {
		t.Fatalf("undefined return: got %q, want empty", name)
	}
}

func TestEvaluate_Throw(t *testing.T) {
	s := mustCompile(t, `(ctx) => { throw new Error("boom") }`)
	_, err := s.Evaluate(EvalContext{}, 50*time.Millisecond)
	if !errors.Is(err, ErrEvalThrew) {
		t.Fatalf("throw: got %v, want ErrEvalThrew", err)
	}
}

// A throw that mutates Array.prototype before failing must not poison a later
// pooled eval into selecting a relaxed policy.
func TestEvaluate_ThrowDoesNotPoisonNextEval(t *testing.T) {
	s := mustCompile(t, `(ctx) => {
		if (ctx.request.method === "poison") {
			Array.prototype.filter = function () { return this; };
			throw new Error("after mutate");
		}
		const internals = ctx.upstreams.withTag("type:internal");
		return internals.length === 1 ? "standard" : "fallback";
	}`)
	_, err := s.Evaluate(EvalContext{Request: Request{Method: "poison"}}, 50*time.Millisecond)
	if !errors.Is(err, ErrEvalThrew) {
		t.Fatalf("poison throw: got %v, want ErrEvalThrew", err)
	}
	name, err := s.Evaluate(EvalContext{
		Request:   Request{Method: "eth_call"},
		Upstreams: []Upstream{up("i1", []string{"type:internal"}, StateHealthy, nil, nil)},
	}, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("post-poison Evaluate: %v", err)
	}
	if name != "standard" {
		t.Fatalf("post-poison got %q, want standard (filter must still work)", name)
	}
}

func TestEvaluate_NonStringReturn(t *testing.T) {
	s := mustCompile(t, `(ctx) => 42`)
	_, err := s.Evaluate(EvalContext{}, 50*time.Millisecond)
	if !errors.Is(err, ErrEvalThrew) {
		t.Fatalf("non-string return: got %v, want ErrEvalThrew", err)
	}
}

func TestEvaluate_Timeout(t *testing.T) {
	s := mustCompile(t, `(ctx) => { while (true) {} }`)
	start := time.Now()
	_, err := s.Evaluate(EvalContext{}, 20*time.Millisecond)
	if !errors.Is(err, ErrEvalTimeout) {
		t.Fatalf("timeout: got %v, want ErrEvalTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("timeout took too long: %v (eval not interrupted?)", elapsed)
	}
}

func TestEvaluate_TimeoutSelfHeals(t *testing.T) {
	s := mustCompile(t, `(ctx) => { if (ctx.request.method === "loop") { while (true) {} } return "standard" }`)
	// Poison every VM in the pool via timeout, discarding each.
	for i := 0; i < poolSize; i++ {
		if _, err := s.Evaluate(EvalContext{Request: Request{Method: "loop"}}, 20*time.Millisecond); !errors.Is(err, ErrEvalTimeout) {
			t.Fatalf("iter %d: got %v, want ErrEvalTimeout", i, err)
		}
	}
	// The pool rebuilds discarded VMs asynchronously and grows on demand; a
	// normal eval succeeds once a replacement lands (or a new VM is built).
	deadline := time.Now().Add(2 * time.Second)
	for {
		name, err := s.Evaluate(EvalContext{Request: Request{Method: "ok"}}, 50*time.Millisecond)
		if err == nil {
			if name != "standard" {
				t.Fatalf("after heal: got %q, want standard", name)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pool never healed after timeouts: last err %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestEvaluate_PoolGrowsOnDemand(t *testing.T) {
	// A blocking eval holds a VM until we release it; borrowing all poolSize
	// VMs then issuing another Evaluate must grow the pool rather than fail
	// closed (feature §4.6 — no pool_exhausted bypass).
	release := make(chan struct{})
	entered := make(chan struct{}, poolSize+1)
	s := mustCompile(t, `(ctx) => { block(); return "standard" }`)
	for i := 0; i < poolSize; i++ {
		v, err := s.pool.acquire(0)
		if err != nil {
			t.Fatalf("prime acquire %d: %v", i, err)
		}
		_ = v.runtime().Set("block", func() {
			entered <- struct{}{}
			<-release
		})
		s.pool.release(v)
	}

	for i := 0; i < poolSize; i++ {
		go func() { _, _ = s.Evaluate(EvalContext{}, 0) }()
	}
	for i := 0; i < poolSize; i++ {
		<-entered
	}

	// Install block on any newly grown VM by wrapping via a fresh compile path
	// is hard; instead verify Evaluate still returns successfully under load by
	// using a non-blocking eval that forces grow-on-demand.
	s2 := mustCompile(t, `(ctx) => "grown"`)
	// Drain s2's pre-warmed pool without blocking, then force grow.
	held := make([]*vm, 0, poolSize)
	for i := 0; i < poolSize; i++ {
		v, err := s2.pool.acquire(0)
		if err != nil {
			t.Fatalf("drain acquire %d: %v", i, err)
		}
		held = append(held, v)
	}
	name, err := s2.Evaluate(EvalContext{}, 50*time.Millisecond)
	for _, v := range held {
		s2.pool.release(v)
	}
	close(release)
	if err != nil {
		t.Fatalf("grow-on-demand Evaluate: %v", err)
	}
	if name != "grown" {
		t.Fatalf("grow-on-demand name = %q, want grown", name)
	}
}

// --- Happy-path name resolution against the reference eval. ---

func TestEvaluate_ReferenceMatrix(t *testing.T) {
	s := mustCompile(t, referenceEval)

	internalHealthy := func() Upstream { return up("i1", []string{"type:internal"}, StateHealthy, nil, nil) }
	externalHealthy := func(id string) Upstream { return up(id, []string{"type:external"}, StateHealthy, nil, nil) }

	tests := []struct {
		name string
		ctx  EvalContext
		want string
	}{
		{
			name: "default standard when internals healthy, no roles",
			ctx: EvalContext{
				Request:   Request{BlockNumber: i64(100)},
				Upstreams: []Upstream{internalHealthy(), externalHealthy("e1")},
			},
			want: "standard",
		},
		{
			name: "punished internal pins to standard even with fallback role and internals down",
			ctx: EvalContext{
				Request: Request{BlockNumber: i64(100)},
				User:    &User{Auth: "consensus-fallback"},
				// Selection already dropped the punished node from candidates.
				Upstreams: []Upstream{
					externalHealthy("e1"),
				},
				PunishedOrOperator: []Upstream{
					up("i1", []string{"type:internal"}, StateCordoned, []string{CordonPunishment}, nil),
				},
			},
			want: "standard",
		},
		{
			name: "operator cordon pins to standard",
			ctx: EvalContext{
				Request: Request{BlockNumber: i64(100)},
				User:    &User{Auth: "consensus-fallback"},
				Upstreams: []Upstream{
					externalHealthy("e1"),
				},
				PunishedOrOperator: []Upstream{
					up("i1", []string{"type:internal"}, StateCordoned, []string{CordonOperator}, nil),
				},
			},
			want: "standard",
		},
		{
			name: "punished node absent from candidates still pins (removeCordoned case)",
			ctx: EvalContext{
				Request: Request{BlockNumber: i64(500)},
				User:    &User{Auth: "consensus-waive"},
				// Without PunishedOrOperator this would pick historical
				// (empty internals → canServeBlock length 0).
				Upstreams: []Upstream{
					externalHealthy("e1"),
				},
				PunishedOrOperator: []Upstream{
					up("i1", []string{"type:internal"}, StateCordoned, []string{CordonPunishment}, nil),
				},
			},
			want: "standard",
		},
		{
			name: "availability-cordoned internals + authorized role → fallback",
			ctx: EvalContext{
				Request: Request{BlockNumber: i64(100)},
				User:    &User{Auth: "consensus-fallback"},
				Upstreams: []Upstream{
					up("i1", []string{"type:internal"}, StateCordoned, []string{CordonAvailability}, nil),
					externalHealthy("e1"),
				},
			},
			want: "fallback",
		},
		{
			name: "unhealthy internals + unauthorized → standard (never silent downgrade)",
			ctx: EvalContext{
				Request: Request{BlockNumber: i64(100)},
				Upstreams: []Upstream{
					up("i1", []string{"type:internal"}, StateUnhealthy, nil, nil),
					externalHealthy("e1"),
				},
			},
			want: "standard",
		},
		{
			name: "historical role + known-old block internals cannot serve → historical",
			ctx: EvalContext{
				Request: Request{BlockNumber: i64(500)},
				User:    &User{Auth: "consensus-waive"},
				Upstreams: []Upstream{
					up("i1", []string{"type:internal"}, StateHealthy, nil, &BlockRange{Lower: i64(1000), Upper: nil}),
					externalHealthy("e1"),
				},
			},
			want: "historical",
		},
		{
			name: "historical role + null block → standard-waive-missing",
			ctx: EvalContext{
				Request:   Request{Method: "eth_getTransactionByHash", BlockNumber: nil},
				User:      &User{Auth: "consensus-waive"},
				Upstreams: []Upstream{internalHealthy(), externalHealthy("e1")},
			},
			want: "standard-waive-missing",
		},
		{
			name: "historical role + in-range block → standard (internals can serve)",
			ctx: EvalContext{
				Request: Request{BlockNumber: i64(1500)},
				User:    &User{Auth: "consensus-waive"},
				Upstreams: []Upstream{
					up("i1", []string{"type:internal"}, StateHealthy, nil, &BlockRange{Lower: i64(1000), Upper: nil}),
					externalHealthy("e1"),
				},
			},
			want: "standard",
		},
		{
			name: "dev role → generous-dev",
			ctx: EvalContext{
				Request:   Request{BlockNumber: i64(100)},
				User:      &User{Auth: "consensus-dev"},
				Upstreams: []Upstream{internalHealthy(), externalHealthy("e1")},
			},
			want: "generous-dev",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.Evaluate(tc.ctx, 50*time.Millisecond)
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// A closure that counts calls must not carry that count onto the next
// request. The pool used to release a successful VM; the ninth call then
// reused one of the eight pre-warmed realms and returned "later".
func TestEvaluate_FreshRealmEachCall(t *testing.T) {
	s := mustCompile(t, `(() => { let n = 0; return () => { n++; return n === 1 ? "first" : "later" } })()`)
	for i := 0; i < poolSize+2; i++ {
		name, err := s.Evaluate(EvalContext{}, time.Second)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if name != "first" {
			t.Fatalf("call %d returned %q, want first", i, name)
		}
	}
}

func TestEvaluate_Concurrent(t *testing.T) {
	s := mustCompile(t, referenceEval)
	ctx := EvalContext{
		Request:   Request{BlockNumber: i64(100)},
		Upstreams: []Upstream{up("i1", []string{"type:internal"}, StateHealthy, nil, nil)},
	}
	const workers = 32
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		go func() {
			for j := 0; j < 50; j++ {
				// Generous timeout: under 32-way contention the eval itself is
				// fine, but acquire may briefly wait/grow (§4.6) before the
				// timed call starts.
				name, err := s.Evaluate(ctx, 500*time.Millisecond)
				if err != nil {
					errs <- err
					return
				}
				if name != "standard" {
					errs <- errors.New("unexpected name " + name)
					return
				}
			}
			errs <- nil
		}()
	}
	for i := 0; i < workers; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent eval: %v", err)
		}
	}
}
