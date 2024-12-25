package health

import (
	"sync"
	"time"

	"github.com/DataDog/sketches-go/ddsketch"
	"github.com/bytedance/sonic"
	"github.com/rs/zerolog/log"
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
	err := q.sketch.Add(value)
	if err != nil {
		log.Warn().Err(err).Float64("value", value).Msg("failed to add value to quantile tracker")
	}
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

	return sonic.Marshal(struct {
		P50 float64 `json:"p50"`
		P70 float64 `json:"p70"`
		P90 float64 `json:"p90"`
		P95 float64 `json:"p95"`
		P99 float64 `json:"p99"`
	}{
		P50: q.GetQuantile(0.50).Seconds(),
		P70: q.GetQuantile(0.70).Seconds(),
		P90: q.GetQuantile(0.90).Seconds(),
		P95: q.GetQuantile(0.95).Seconds(),
		P99: q.GetQuantile(0.99).Seconds(),
	})
}

func (q *QuantileTracker) GetQuantile(qtile float64) time.Duration {
	q.mu.Lock()
	defer q.mu.Unlock()
	seconds, err := q.sketch.GetValueAtQuantile(qtile)
	if err != nil {
		// If there's no data, return 0
		return 0
	}
	return time.Duration(seconds * float64(time.Second))
}
