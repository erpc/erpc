package health

import (
	"sync"

	"github.com/DataDog/sketches-go/ddsketch"
	"github.com/bytedance/sonic"
)

type QuantileTracker struct {
	mu     sync.RWMutex
	sketch *ddsketch.DDSketch
}

func NewQuantileTracker() *QuantileTracker {
	// e.g. 1% relative accuracy
	sketch, _ := ddsketch.NewDefaultDDSketch(0.01)
	return &QuantileTracker{
		sketch: sketch,
	}
}

func (q *QuantileTracker) Add(value float64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.sketch.Add(value)
}

func (q *QuantileTracker) P90() float64 {
	return q.GetQuantile(0.90)
}

func (q *QuantileTracker) P95() float64 {
	return q.GetQuantile(0.95)
}

func (q *QuantileTracker) P99() float64 {
	return q.GetQuantile(0.99)
}

func (q *QuantileTracker) Reset() {
	q.mu.Lock()
	defer q.mu.Unlock()
	// Re-init the sketch
	q.sketch, _ = ddsketch.NewDefaultDDSketch(0.01)
}

func (q *QuantileTracker) MarshalJSON() ([]byte, error) {
	q.mu.RLock()
	defer q.mu.RUnlock()

	// If the sketch is empty, sketches-go returns 0 for quantiles
	p90, _ := q.sketch.GetValueAtQuantile(0.90)
	p95, _ := q.sketch.GetValueAtQuantile(0.95)
	p99, _ := q.sketch.GetValueAtQuantile(0.99)

	return sonic.Marshal(struct {
		P90 float64 `json:"p90"`
		P95 float64 `json:"p95"`
		P99 float64 `json:"p99"`
	}{
		P90: p90,
		P95: p95,
		P99: p99,
	})
}

func (q *QuantileTracker) GetQuantile(qtile float64) float64 {
	q.mu.RLock()
	defer q.mu.RUnlock()
	val, err := q.sketch.GetValueAtQuantile(qtile)
	if err != nil {
		// If there's no data, return 0
		return 0
	}
	return val
}
