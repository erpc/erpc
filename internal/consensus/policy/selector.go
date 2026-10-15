package policy

import (
	_ "embed"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/grafana/sobek"
)

//go:embed stdlib.js
var stdlibSource string

// poolSize is the number of pre-warmed VMs. Bounded, and the same order of
// magnitude as the selection-policy engine. The eval runs once per request
// before fan-out; 8 concurrent evals in flight is already well beyond the
// per-request eval cost.
const poolSize = 8

// Selector is a compiled consensus-policy eval function plus its VM pool. Safe
// for concurrent use.
type Selector struct {
	pool *pool
}

// compiled is the compiled programs plus the per-VM build closure, shared by
// Compile and by the cold-VM benchmark.
type compiled struct {
	build func() (*vm, error)
}

// newSandboxRuntime builds a bare sobek VM for a consensus-policy eval. Unlike
// common.NewRuntime it deliberately does NOT expose env / process.env / console:
// the operator function must see only the bounded EvalContext + stdlib surface
// this package owns (see the package doc), never the host process environment.
// The JSON field-name mapper is kept so struct-shaped values (should the bridge
// ever pass one) map to their json tags, matching the shared runtime.
func newSandboxRuntime() *sobek.Runtime {
	rt := sobek.New()
	rt.SetFieldNameMapper(sobek.TagFieldNameMapper("json", true))
	return rt
}

func compileFn(evalFunction string, buildTimeout time.Duration) (*compiled, error) {
	if strings.TrimSpace(evalFunction) == "" {
		return nil, fmt.Errorf("consensus policy: empty eval function")
	}

	fnProg, err := sobek.Compile("consensus-policy.js", "("+evalFunction+")", false)
	if err != nil {
		return nil, fmt.Errorf("consensus policy: compile eval function: %w", err)
	}

	build := func() (*vm, error) {
		rt := newSandboxRuntime()
		if _, err := rt.RunString(stdlibSource); err != nil {
			return nil, fmt.Errorf("consensus policy: install stdlib: %w", err)
		}
		// Freeze builtins the stdlib relies on so a successful eval that
		// mutates Array.prototype (etc.) cannot poison a later pooled caller.
		// Throw paths discard the VM entirely (see Evaluate).
		if _, err := rt.RunString(`(function () {
			var f = Object.freeze;
			f(Object.prototype);
			f(Array.prototype);
			f(Function.prototype);
			f(String.prototype);
			f(Number.prototype);
			f(Boolean.prototype);
			f(Math);
			f(JSON);
		})()`); err != nil {
			return nil, fmt.Errorf("consensus policy: freeze sandbox builtins: %w", err)
		}
		// Evaluating the operator's function EXPRESSION must be bounded. A
		// legitimate `(ctx) => …` yields a callable instantly, but a config that
		// executes code before yielding — e.g. `(() => { while (true) {} })()` —
		// is valid JS that would hang pool construction (and thus startup)
		// forever, since Evaluate's per-request timeout never covers this build.
		// Bound it with the operator's own configured eval budget and fail the
		// config loudly instead.
		fnVal, err := runProgramBounded(rt, fnProg, buildTimeout)
		if err != nil {
			return nil, fmt.Errorf("consensus policy: evaluate eval function: %w", err)
		}
		fn, ok := sobek.AssertFunction(fnVal)
		if !ok {
			return nil, fmt.Errorf("consensus policy: evalFunction is not a function")
		}
		wrap, ok := sobek.AssertFunction(rt.Get("__erpcWrapConsensusCtx"))
		if !ok {
			return nil, fmt.Errorf("consensus policy: stdlib wrap missing")
		}
		// The callable lives in Go, not on a JS global. Binding it via rt.Set
		// would run any operator-installed setter for that name *outside*
		// runProgramBounded, hanging startup. Same for the per-request context.
		return &vm{rt: rt, fn: fn, wrap: wrap}, nil
	}
	return &compiled{build: build}, nil
}

// runProgramBounded runs prog on rt, aborting after timeout via the VM
// interrupt when timeout > 0. timeout <= 0 disables the cap (mirroring
// Evaluate). Used to bound the one-time construction eval of the operator
// function expression so a runaway expression cannot hang startup.
func runProgramBounded(rt *sobek.Runtime, prog *sobek.Program, timeout time.Duration) (sobek.Value, error) {
	if timeout <= 0 {
		return rt.RunProgram(prog)
	}
	var (
		val  sobek.Value
		err  error
		done = make(chan struct{})
	)
	go func() {
		defer close(done)
		val, err = rt.RunProgram(prog)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return val, err
	case <-timer.C:
		rt.Interrupt(ErrEvalTimeout)
		return nil, ErrEvalTimeout
	}
}

// Compile compiles the operator eval function and pre-warms the VM pool. The
// function source is the body an operator writes for customPolicy.evalFunction:
// an expression evaluating to (ctx) => string | null. buildTimeout bounds the
// one-time evaluation of that expression during pool construction (0 disables
// the bound); pass the operator's configured evalTimeout.
func Compile(evalFunction string, buildTimeout time.Duration) (*Selector, error) {
	c, err := compileFn(evalFunction, buildTimeout)
	if err != nil {
		return nil, err
	}
	p, err := newPool(poolSize, c.build)
	if err != nil {
		return nil, err
	}
	return &Selector{pool: p}, nil
}

// Evaluate runs the eval for one request and returns the selected policy name.
//
// An empty or null return is the empty string — the caller resolves that to the
// default policy, as it does an unknown name. Every failure (throw, non-string
// return, timeout) returns the empty name and a typed error so the caller can
// attribute the fail-closed reason. Lack of a free VM is never itself a
// fail-closed trigger while under poolMaxSize — the pool grows on demand
// (feature §4.6). At the hard cap, acquire waits against the same evalTimeout
// budget. timeout <= 0 disables the wall-clock cap.
func (s *Selector) Evaluate(ctx EvalContext, timeout time.Duration) (string, error) {
	deadline := time.Time{}
	acquireWait := time.Duration(-1) // unbounded at pool cap when timeout <= 0
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
		acquireWait = time.Until(deadline)
		if acquireWait <= 0 {
			return "", ErrEvalTimeout
		}
	}
	v, err := s.pool.acquire(acquireWait)
	if err != nil {
		return "", err
	}

	var (
		result  sobek.Value
		evalErr error
		done    = make(chan struct{})
	)
	go func() {
		defer close(done)
		// Wrap + call run inside the timed goroutine so neither step can hang
		// Evaluate after a prior eval mutated globals. The callable itself is
		// host-side (see compileFn), so there is no JS setter to trip.
		result, evalErr = v.call(ctx)
	}()

	if timeout > 0 {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			v.interrupt(ErrEvalTimeout)
			// The slot stays in alive until the goroutine exits. Discarding
			// now would let the pool build another runtime while this one is
			// still running, past poolMaxSize.
			discardWhenStopped(s.pool, done)
			return "", ErrEvalTimeout
		}
		timer := time.NewTimer(remaining)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
			// Interrupt is cooperative and does not preempt a native builtin
			// (regex, sort, JSON.stringify). Do not block the request on that
			// call. Discard only after done so the slot is counted until the
			// goroutine actually exits.
			v.interrupt(ErrEvalTimeout)
			discardWhenStopped(s.pool, done)
			return "", ErrEvalTimeout
		}
	} else {
		<-done
	}

	if evalErr != nil {
		// A throw can leave mutated builtins (e.g. Array.prototype.filter)
		// behind; releasing would poison the next eval. Discard and refill.
		s.pool.discard()
		return "", fmt.Errorf("%w: %v", ErrEvalThrew, evalErr)
	}

	name, err := resultName(result)
	if err != nil {
		s.pool.discard()
		return "", err
	}
	// A successful eval can still mutate globalThis or close over mutable
	// state. Reusing the VM would let a later request observe that. Discard
	// so the next caller gets a fresh realm; the pool refills asynchronously.
	s.pool.discard()
	return name, nil
}

// discardWhenStopped drops the VM after its eval goroutine exits. The request
// path must not wait: a native builtin ignores Interrupt and may run long.
// Holding the slot until then is what keeps alive <= poolMaxSize.
func discardWhenStopped(p *pool, done <-chan struct{}) {
	go func() {
		<-done
		p.discard()
	}()
}

// resultName interprets the eval return value. null/undefined → "" (default);
// a string is the policy name; anything else is a fail-closed error.
//
// Do not call Export() here: for non-primitive returns it recursively converts
// objects and can run Proxy/accessor traps after the eval timeout has already
// elapsed. ExportType() / String() stay on the typed value metadata.
func resultName(v sobek.Value) (string, error) {
	if v == nil || sobek.IsUndefined(v) || sobek.IsNull(v) {
		return "", nil
	}
	if et := v.ExportType(); et != nil && et.Kind() == reflect.String {
		return v.String(), nil
	}
	typeName := "unknown"
	if et := v.ExportType(); et != nil {
		typeName = et.String()
	}
	return "", fmt.Errorf("%w: eval returned %s, want string or null", ErrEvalThrew, typeName)
}
