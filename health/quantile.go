package health

import (
	"encoding/json"
	"sort"
	"sync"
	"time"
)

type QuantileTracker struct {
	windowSize time.Duration
	values     []struct {
		value     float64
		timestamp time.Time
	}
	mu sync.RWMutex
}

func NewQuantileTracker(windowSize time.Duration) *QuantileTracker {
	return &QuantileTracker{
		windowSize: windowSize,
		values: make([]struct {
			value     float64
			timestamp time.Time
		}, 0),
	}
}

// Add adds a new value to the tracker, maintaining the time-based window
func (p *QuantileTracker) Add(value float64) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	p.values = append(p.values, struct {
		value     float64
		timestamp time.Time
	}{value, now})

	// Remove values outside the time window
	cutoff := now.Add(-p.windowSize)
	for len(p.values) > 0 && p.values[0].timestamp.Before(cutoff) {
		p.values = p.values[1:]
	}
}

// P90 calculates the 90th percentile of the current values in the time window
func (p *QuantileTracker) P90() float64 {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if len(p.values) == 0 {
		return 0.0 // or some default value or error
	}

	sortedValues := make([]float64, len(p.values))
	for i, v := range p.values {
		sortedValues[i] = v.value
	}
	sort.Float64s(sortedValues)

	index := int(float64(len(sortedValues)-1) * 0.9)
	return sortedValues[index]
}

func (p *QuantileTracker) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.values = make([]struct {
		value     float64
		timestamp time.Time
	}, 0)
}

func (p *QuantileTracker) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		P90 float64 `json:"p90"`
	}{
		P90: p.P90(),
	})
}
