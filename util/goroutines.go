package util

import "sync"

// Goroutines runs the goroutines of one owner whose ctx ends them, so the
// owner can wait until none runs. The zero value is ready.
type Goroutines struct {
	// Go only read-locks and starts fn after unlocking: a start that parks
	// (a GC assist in its allocation) never holds up the next one, and
	// every ws subscribe starts its tap here while it holds a slot.
	mu    sync.RWMutex
	ended bool
	wg    sync.WaitGroup
}

// Go runs fn on a new goroutine and reports true, or reports false once
// Wait began: the owner ended, so fn must not start.
func (g *Goroutines) Go(fn func()) bool {
	g.mu.RLock()
	if g.ended {
		g.mu.RUnlock()
		return false
	}
	g.wg.Add(1)
	g.mu.RUnlock()
	go func() {
		defer g.wg.Done()
		fn()
	}()
	return true
}

// Wait refuses new goroutines and waits for the running ones; call it
// after their ctx ended.
func (g *Goroutines) Wait() {
	g.mu.Lock()
	g.ended = true
	g.mu.Unlock()
	g.wg.Wait()
}
