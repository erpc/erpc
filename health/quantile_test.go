package health

import (
	"math"
	"testing"
	"time"
)

func TestNewQuantileTracker(t *testing.T) {
	windowSize := 5 * time.Minute
	qt := NewQuantileTracker(windowSize)

	if qt.windowSize != windowSize {
		t.Errorf("Expected window size %v, got %v", windowSize, qt.windowSize)
	}

	if len(qt.values) != 0 {
		t.Errorf("Expected empty values slice, got length %d", len(qt.values))
	}
}

func TestAdd(t *testing.T) {
	qt := NewQuantileTracker(5 * time.Minute)

	// Test adding a single value
	qt.Add(10.0)
	if len(qt.values) != 1 {
		t.Errorf("Expected 1 value, got %d", len(qt.values))
	}

	// Test adding multiple values
	qt.Add(20.0)
	qt.Add(30.0)
	if len(qt.values) != 3 {
		t.Errorf("Expected 3 values, got %d", len(qt.values))
	}
}

func TestAddWithWindowExpiry(t *testing.T) {
	qt := NewQuantileTracker(5 * time.Minute)

	now := time.Now()
	qt.values = append(qt.values,
		struct {
			value     float64
			timestamp time.Time
		}{10.0, now.Add(-6 * time.Minute)},
		struct {
			value     float64
			timestamp time.Time
		}{20.0, now.Add(-4 * time.Minute)},
	)

	qt.Add(30.0) // This should trigger removal of expired value

	if len(qt.values) != 2 {
		t.Errorf("Expected 2 values after expiry, got %d", len(qt.values))
	}

	if qt.values[0].value != 20.0 {
		t.Errorf("Expected first value to be 20.0, got %f", qt.values[0].value)
	}
}

func TestP90(t *testing.T) {
	qt := NewQuantileTracker(5 * time.Minute)

	// Test P90 with empty tracker
	if p90 := qt.P90(); p90 != 0.0 {
		t.Errorf("Expected P90 of empty tracker to be 0.0, got %f", p90)
	}

	// Test P90 with one value
	qt.Add(10.0)
	if p90 := qt.P90(); p90 != 10.0 {
		t.Errorf("Expected P90 with one value to be 10.0, got %f", p90)
	}

	// Test P90 with multiple values
	qt.Add(20.0)
	qt.Add(30.0)
	qt.Add(40.0)
	qt.Add(50.0)
	expectedP90 := 40.0 // 90th percentile of [10, 20, 30, 40, 50]
	if p90 := qt.P90(); math.Abs(p90-expectedP90) > 0.001 {
		t.Errorf("Expected P90 to be %f, got %f", expectedP90, p90)
	}
}

func TestP90WithEqualValues(t *testing.T) {
	qt := NewQuantileTracker(5 * time.Minute)

	for i := 0; i < 10; i++ {
		qt.Add(5.0)
	}

	if p90 := qt.P90(); p90 != 5.0 {
		t.Errorf("Expected P90 with equal values to be 5.0, got %f", p90)
	}
}

func TestP90WithLargeRange(t *testing.T) {
	qt := NewQuantileTracker(5 * time.Minute)

	qt.Add(1.0)
	qt.Add(10.0)
	qt.Add(400.0)
	qt.Add(600.0)
	qt.Add(1000000.0)

	expectedP90 := 600.0 // 90th percentile of [1, 1000000]
	if p90 := qt.P90(); math.Abs(p90-expectedP90) > 0.001 {
		t.Errorf("Expected P90 with large range to be %f, got %f", expectedP90, p90)
	}
}

func TestP90WithSmallDifferences(t *testing.T) {
	qt := NewQuantileTracker(5 * time.Minute)

	qt.Add(1.0001)
	qt.Add(1.0002)
	qt.Add(1.0003)
	qt.Add(1.0004)
	qt.Add(1.0005)

	expectedP90 := 1.0004 // 90th percentile of [1.0001, 1.0002, 1.0003, 1.0004, 1.0005]
	if p90 := qt.P90(); math.Abs(p90-expectedP90) > 0.000001 {
		t.Errorf("Expected P90 with small differences to be %f, got %f", expectedP90, p90)
	}
}
