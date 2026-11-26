package policy

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/grafana/sobek"
)

// vm is one pre-warmed runtime with the operator function and stdlib wrap
// held in host-side Go state — not as JS globals. Operator code therefore
// cannot install setters on those slots to run outside the eval timeout.
// Runtimes are not goroutine-safe; the pool hands out one vm per eval and
// takes it back when done.
type vm struct {
	rt   *sobek.Runtime
	fn   sobek.Callable
	wrap sobek.Callable
}

const (
	// poolLowWater triggers asynchronous refill when idle VMs drop to this
	// many (feature §4.6). Kept as a constant so the hot path stays branchable.
	poolLowWater = 2
	// poolMaxSize caps total VMs (idle + in-flight). When hit, acquire waits
	// for a free VM (bounded by the caller's eval timeout) rather than
	// growing past the limit or bypassing eval.
	poolMaxSize = 64
	// poolAcquireWait is the short default wait when acquire is called
	// without an explicit budget (tests / internal drains).
	poolAcquireWait = 50 * time.Millisecond
)

// pool is a grow-on-demand set of pre-warmed VMs (feature §4.6). It pre-warms
// `size` VMs, asynchronously refills when idle drops to ≤ poolLowWater, and
// synchronically builds one more when acquire finds none free — never fail-
// closed for capacity. Discarded (poisoned) VMs are replaced asynchronously.
type pool struct {
	free  chan *vm
	build func() (*vm, error)
	size  int // pre-warm / refill target

	mu      sync.Mutex
	alive   int // idle + borrowed; includes in-flight builds counted after success
	refillN int32
}

func newPool(size int, build func() (*vm, error)) (*pool, error) {
	if size < 1 {
		size = 1
	}
	// Buffer for peak growth so release never blocks.
	p := &pool{free: make(chan *vm, poolMaxSize), build: build, size: size}
	for i := 0; i < size; i++ {
		v, err := build()
		if err != nil {
			return nil, err
		}
		p.alive++
		p.free <- v
	}
	return p, nil
}

// acquire returns a free VM. Prefers a warm idle VM; otherwise grows
// synchronously up to poolMaxSize. At the cap it waits for a free VM rather
// than growing past the limit or bypassing eval:
//   - wait < 0: unbounded wait (Evaluate with timeout <= 0)
//   - wait == 0: short poolAcquireWait (internal drains / tests)
//   - wait > 0: wait at most that long, then ErrEvalTimeout
func (p *pool) acquire(wait time.Duration) (*vm, error) {
	if v := p.tryIdle(); v != nil {
		p.maybeRefill()
		return v, nil
	}

	// Empty: grow on demand unless at the hard cap. Build off-lock in the
	// background so a cold build can't run synchronously past the caller's
	// wait budget (Copilot finding: sync p.build() exceeded evalTimeout). The
	// slot stays reserved until the build finishes.
	p.mu.Lock()
	canGrow := p.alive < poolMaxSize
	if canGrow {
		p.alive++ // reserve the slot before unlock so concurrent acquires don't overshoot
	}
	p.mu.Unlock()

	if canGrow {
		built := make(chan struct {
			v   *vm
			err error
		}, 1)
		go func() {
			v, err := p.build()
			built <- struct {
				v   *vm
				err error
			}{v, err}
		}()
		if wait < 0 {
			r := <-built
			if r.err != nil {
				p.mu.Lock()
				p.alive--
				p.mu.Unlock()
				return nil, r.err
			}
			return r.v, nil
		}
		if wait == 0 {
			wait = poolAcquireWait
		}
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case r := <-built:
			if r.err != nil {
				p.mu.Lock()
				p.alive--
				p.mu.Unlock()
				return nil, r.err
			}
			return r.v, nil
		case <-timer.C:
			// Build is still running in the background; the slot stays
			// reserved until it completes and the VM lands in the pool.
			go func() {
				r := <-built
				p.mu.Lock()
				if r.err != nil {
					p.alive--
				} else {
					select {
					case p.free <- r.v:
					default:
						p.alive--
					}
				}
				p.mu.Unlock()
			}()
			return nil, fmt.Errorf("%w: VM build exceeded acquire budget", ErrEvalTimeout)
		}
	}

	// At cap: wait for a free VM rather than growing past poolMaxSize.
	if wait < 0 {
		v := <-p.free
		p.maybeRefill()
		return v, nil
	}
	if wait == 0 {
		wait = poolAcquireWait
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case v := <-p.free:
		p.maybeRefill()
		return v, nil
	case <-timer.C:
		if v := p.tryIdle(); v != nil {
			return v, nil
		}
		return nil, fmt.Errorf("%w: VM pool at max size (%d) with no free VM", ErrEvalTimeout, poolMaxSize)
	}
}

func (p *pool) tryIdle() *vm {
	select {
	case v := <-p.free:
		return v
	default:
		return nil
	}
}

// maybeRefill kicks a background top-up when idle count is at/below the
// low-water mark. Best-effort; never blocks the request path.
func (p *pool) maybeRefill() {
	if len(p.free) > poolLowWater {
		return
	}
	if !atomic.CompareAndSwapInt32(&p.refillN, 0, 1) {
		return
	}
	go func() {
		// Re-check demand AFTER clearing the ownership flag before exiting, so
		// a discard that ran while we were building (refillN==1) is not lost
		// (Copilot finding: lost-refill race left cap-waiters blocked).
		exiting := false
		defer func() {
			atomic.StoreInt32(&p.refillN, 0)
			p.mu.Lock()
			idle := len(p.free)
			need := p.size - idle
			atCap := p.alive >= poolMaxSize
			p.mu.Unlock()
			if !exiting && need > 0 && !atCap {
				p.maybeRefill()
			}
		}()
		for {
			p.mu.Lock()
			idle := len(p.free)
			need := p.size - idle
			atCap := p.alive >= poolMaxSize
			if need <= 0 || atCap {
				p.mu.Unlock()
				exiting = true
				return
			}
			// Reserve the slot before building. A synchronous acquire that
			// lands mid-build would otherwise reserve the same slot and the
			// refill would construct a runtime past poolMaxSize.
			p.alive++
			p.mu.Unlock()
			v, err := p.build()
			if err != nil {
				p.mu.Lock()
				p.alive--
				p.mu.Unlock()
				return
			}
			select {
			case p.free <- v:
			default:
				p.mu.Lock()
				p.alive--
				p.mu.Unlock()
				return
			}
		}
	}()
}

// release returns a healthy VM to the pool. If the idle buffer is already at
// the pre-warm target and we're over size, the VM is dropped so peak growth
// drains back toward the steady-state target.
func (p *pool) release(v *vm) {
	p.mu.Lock()
	idle := len(p.free)
	overTarget := p.alive > p.size && idle >= p.size
	if overTarget {
		p.alive--
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()

	select {
	case p.free <- v:
	default:
		p.mu.Lock()
		p.alive--
		p.mu.Unlock()
	}
}

// discard drops a poisoned VM. Replacement is routed through maybeRefill so
// a burst of timeout/poison discards shares one background refill goroutine
// and still respects poolMaxSize — never spawn an unbounded build per discard.
func (p *pool) discard() {
	p.mu.Lock()
	p.alive--
	p.mu.Unlock()
	p.maybeRefill()
}

func (v *vm) interrupt(reason any) { v.rt.Interrupt(reason) }

func (v *vm) runtime() *sobek.Runtime { return v.rt }

// call wraps the EvalContext and invokes the operator function. Both steps
// run on the caller (Evaluate's timed goroutine) so a runaway wrap or fn
// is interruptible.
func (v *vm) call(ctx EvalContext) (sobek.Value, error) {
	wrapped, err := v.wrap(sobek.Undefined(), v.rt.ToValue(ctx.toJS()))
	if err != nil {
		return nil, err
	}
	return v.fn(sobek.Undefined(), wrapped)
}
