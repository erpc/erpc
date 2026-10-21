package util

import "sync"

// Goroutines runs the goroutines of one owner whose ctx ends them, so the
// owner can wait until none runs. The zero value is ready.
type Goroutines struct {
	mu    sync.Mutex
	ended bool
	wg    sync.WaitGroup
}

// Go runs fn on a new goroutine and reports true, or reports false once
// Wait began: the owner ended, so fn must not start.
func (g *Goroutines) Go(fn func()) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.ended {
		return false
	}
	g.wg.Go(fn)
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
