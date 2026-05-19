package policy

import (
	"fmt"
	"sync"

	"github.com/erpc/erpc/common"
	"github.com/grafana/sobek"
)

// runtimePool guards a pool of *common.Runtime (sobek wrapper). Sobek
// runtimes are NOT goroutine-safe; the engine serializes access by
// checking out one runtime per eval and returning it when done. A single
// project usually needs no more than a handful of runtimes (one per
// concurrent in-flight tick).
//
// At engine construction time each runtime is primed with the shared
// installation: stdlib helpers, console, process.env. If the project
// was loaded from a `.ts` config, the user's whole compiled script is
// also evaluated in each primed runtime — that populates
// `globalThis.__erpcFns` with the real function values referenced by
// `SelectionPolicyConfig.EvalFunc` sentinels, and keeps any module-level
// helpers / imports the user wrote in scope of those functions.
//
// Per-tick state (the upstream array, ctx) is bound via Set() before
// running the program and cleared after to avoid leaking references
// between ticks.
type runtimePool struct {
	mu         sync.Mutex
	idle       []*common.Runtime
	primer     func(*common.Runtime) error
	userScript *sobek.Program
}

func newRuntimePool(primer func(*common.Runtime) error, userScript *sobek.Program) *runtimePool {
	return &runtimePool{primer: primer, userScript: userScript}
}

// acquire returns a runtime ready for use. The caller MUST call release()
// (typically via defer) when done.
func (p *runtimePool) acquire() (*common.Runtime, error) {
	p.mu.Lock()
	if n := len(p.idle); n > 0 {
		rt := p.idle[n-1]
		p.idle = p.idle[:n-1]
		p.mu.Unlock()
		return rt, nil
	}
	p.mu.Unlock()

	rt, err := common.NewRuntime()
	if err != nil {
		return nil, err
	}
	if p.primer != nil {
		if err := p.primer(rt); err != nil {
			return nil, err
		}
	}
	// Run the user's whole script — once per runtime, after the stdlib
	// primer. The script's tail walker (`tsLoaderWalker` in
	// common/config.go) discovers every function-typed leaf in the
	// default export and registers it on `globalThis.__erpcFns` so the
	// engine's eval step can look up by id. Helpers, imports, and
	// closure variables the user wrote stay live in this runtime's
	// module scope, so the registered functions can reference them.
	if p.userScript != nil {
		if _, err := rt.VM().RunProgram(p.userScript); err != nil {
			return nil, fmt.Errorf("evaluate user TS config in policy runtime: %w", err)
		}
	}
	return rt, nil
}

func (p *runtimePool) release(rt *common.Runtime) {
	if rt == nil {
		return
	}
	p.mu.Lock()
	p.idle = append(p.idle, rt)
	p.mu.Unlock()
}
