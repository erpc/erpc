package policy

import (
	"sync"

	"github.com/erpc/erpc/common"
)

// runtimePool guards a pool of *common.Runtime (sobek wrapper). Sobek
// runtimes are NOT goroutine-safe; the engine serializes access by
// checking out one runtime per eval and returning it when done. A single
// project usually needs no more than a handful of runtimes (one per
// concurrent in-flight tick).
//
// At engine construction time each runtime is primed with the shared
// installation: stdlib helpers, console, process.env. Per-tick state
// (the upstream array, ctx) is bound via Set() before running the program
// and cleared after to avoid leaking references between ticks.
type runtimePool struct {
	mu     sync.Mutex
	idle   []*common.Runtime
	primer func(*common.Runtime) error
}

func newRuntimePool(primer func(*common.Runtime) error) *runtimePool {
	return &runtimePool{primer: primer}
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
