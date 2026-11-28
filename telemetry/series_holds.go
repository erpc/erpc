package telemetry

import "sync"

// seriesHolds keeps a projected series alive while any full tuple holds it. A
// label projection maps many full-schema tuples onto one series, so releasing
// one idle tuple must not delete the series the others still write to. The
// count is per projected series; mu makes a hold and its child lookup atomic
// against the last release's delete, so no holder gets a child that is being
// deleted.
type seriesHolds struct {
	mu sync.Mutex
	n  map[string]int
}

// newSeriesHolds returns the holds for a Vec projected onto activeIdx, or nil
// when nothing is projected away: then every tuple is its own series and needs
// no count.
func newSeriesHolds(activeIdx []int, schema []string) *seriesHolds {
	if len(activeIdx) == len(schema) {
		return nil
	}
	return &seriesHolds{n: make(map[string]int)}
}

// acquireSeries adds one hold on the projected series active and returns its
// child from get.
func acquireSeries[T any](h *seriesHolds, active []string, get func(...string) T) T {
	key := labelsKey(active)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.n[key]++
	return get(active...)
}

// release drops one hold on the projected series active and deletes it through
// del once no hold remains. Returns del's result, or false while holds remain.
func (h *seriesHolds) release(active []string, del func(...string) bool) bool {
	key := labelsKey(active)
	h.mu.Lock()
	defer h.mu.Unlock()
	if n := h.n[key]; n > 1 {
		h.n[key] = n - 1
		return false
	}
	delete(h.n, key)
	return del(active...)
}

// reset runs resetVec, which removes every series, and drops every hold with
// them. A nil h has no holds to drop.
func (h *seriesHolds) reset(resetVec func()) {
	if h == nil {
		resetVec()
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	resetVec()
	clear(h.n)
}
