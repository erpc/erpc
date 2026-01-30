package health

import (
	"math"
	"testing"

	"github.com/rs/zerolog/log"
)

// A small helper to check approximate equality,
// e.g. to confirm that ddsketch’s approximate percentile is within tolerance.
func approxEqual(got, want, tol float64) bool {
	return math.Abs(got-want) <= tol
}

func TestNewQuantileTracker(t *testing.T) {
	t.Parallel()
	qt := NewQuantileTracker(&log.Logger)

	if qt == nil {
		t.Fatal("Expected non-nil QuantileTracker")
	}

	// With no data, P90 should be 0
	if p90 := qt.GetQuantile(0.90).Seconds(); p90 != 0 {
		t.Errorf("Expected empty aggregator P90=0, got %f", p90)
	}
}

func TestAddAndQuantiles(t *testing.T) {
	t.Parallel()
	qt := NewQuantileTracker(&log.Logger)

	// Add a single value, check P90, P95, P99 — all should be 10
	qt.Add(10.0)
	if p90 := qt.GetQuantile(0.90).Seconds(); !approxEqual(p90, 10.0, 0.1) {
		t.Errorf("Expected P90 of single-value aggregator to be 10.0, got %f", p90)
	}
	if p95 := qt.GetQuantile(0.95).Seconds(); !approxEqual(p95, 10.0, 0.1) {
		t.Errorf("Expected P95 of single-value aggregator to be 10.0, got %f", p95)
	}
	if p99 := qt.GetQuantile(0.99).Seconds(); !approxEqual(p99, 10.0, 0.1) {
		t.Errorf("Expected P99 of single-value aggregator to be 10.0, got %f", p99)
	}

	// Add more values
	qt.Add(20.0)
	qt.Add(20.0)
	qt.Add(20.0)
	qt.Add(20.0)
	qt.Add(20.0)
	qt.Add(20.0)
	qt.Add(30.0)
	qt.Add(30.0)
	qt.Add(30.0)
	qt.Add(30.0)
	qt.Add(30.0)
	qt.Add(30.0)
	qt.Add(30.0)
	qt.Add(40.0)
	qt.Add(40.0)
	qt.Add(40.0)
	qt.Add(50.0)
	qt.Add(50.0)

	// We expect P90 ~ 40, P95 ~ 50, P99 ~ 50 in the exact set [10,20,30,40,50].
	// But DDSketch is approximate, so we allow some tolerance.
	p90 := qt.GetQuantile(0.90).Seconds()
	p95 := qt.GetQuantile(0.95).Seconds()
	p99 := qt.GetQuantile(0.99).Seconds()

	if !approxEqual(p90, 40.0, 5.0) { // ±5 tolerance
		t.Errorf("Expected P90 ~ 40.0, got %f", p90)
	}
	if !approxEqual(p95, 50.0, 5.0) { // ±5 tolerance
		t.Errorf("Expected P95 ~ 50.0, got %f", p95)
	}
	if !approxEqual(p99, 50.0, 5.0) { // ±5 tolerance
		t.Errorf("Expected P99 ~ 50.0, got %f", p99)
	}
}

func TestReset(t *testing.T) {
	t.Parallel()
	qt := NewQuantileTracker(&log.Logger)
	qt.Add(10.0)
	qt.Add(20.0)

	if p90 := qt.GetQuantile(0.90).Seconds(); p90 == 0.0 {
		t.Errorf("Expected aggregator to have data before reset, got P90=0")
	}

	// Now reset
	qt.Reset()

	if p90 := qt.GetQuantile(0.90).Seconds(); p90 != 0.0 {
		t.Errorf("Expected aggregator to be empty after reset, got P90=%f", p90)
	}
}

func TestAllValuesEqual(t *testing.T) {
	t.Parallel()
	qt := NewQuantileTracker(&log.Logger)

	// Add 10 identical values
	for i := 0; i < 10; i++ {
		qt.Add(5.0)
	}

	// P90/P95/P99 should all be ~5
	p90 := qt.GetQuantile(0.90).Seconds()
	if !approxEqual(p90, 5.0, 0.01) {
		t.Errorf("Expected P90=5.0 for identical values, got %f", p90)
	}

	p95 := qt.GetQuantile(0.95).Seconds()
	if !approxEqual(p95, 5.0, 0.01) {
		t.Errorf("Expected P95=5.0 for identical values, got %f", p95)
	}

	p99 := qt.GetQuantile(0.99).Seconds()
	if !approxEqual(p99, 5.0, 0.01) {
		t.Errorf("Expected P99=5.0 for identical values, got %f", p99)
	}
}

func TestWideRange(t *testing.T) {
	t.Parallel()
	qt := NewQuantileTracker(&log.Logger)

	qt.Add(1.0)
	qt.Add(10.0)
	qt.Add(300.0)
	qt.Add(300.0)
	qt.Add(300.0)
	qt.Add(300.0)
	qt.Add(400.0)
	qt.Add(400.0)
	qt.Add(400.0)
	qt.Add(500.0)
	qt.Add(500.0)
	qt.Add(500.0)
	qt.Add(600.0)
	qt.Add(1000000.0)
	qt.Add(1000000.0)

	// In an exact percentile calculation, P90 might be ~600, P99 ~ ~1000000.
	// But we allow some tolerance for approximation.
	p90 := qt.GetQuantile(0.90).Seconds()
	if p90 < 500 || p90 > 700 {
		t.Errorf("Expected P90 ~ 600, got %f", p90)
	}

	p99 := qt.GetQuantile(0.99).Seconds()
	if p99 < 900000 || p99 > 1100000 {
		t.Errorf("Expected P99 ~ 1000000, got %f", p99)
	}
}

func TestGetQuantile_NaNGuard(t *testing.T) {
	t.Parallel()
	// Test that GetQuantile never returns NaN or Inf, even in edge cases
	qt := NewQuantileTracker(&log.Logger)

	// Test with empty tracker - should return 0, not NaN
	result := qt.GetQuantile(0.50)
	if math.IsNaN(result.Seconds()) || math.IsInf(result.Seconds(), 0) {
		t.Errorf("Empty tracker should not return NaN/Inf, got %v", result)
	}
	if result.Seconds() != 0 {
		t.Errorf("Empty tracker should return 0, got %v", result)
	}

	// Test with valid data
	qt.Add(1.0)
	qt.Add(2.0)
	qt.Add(3.0)

	// Test all common quantiles
	quantiles := []float64{0.50, 0.70, 0.90, 0.95, 0.99}
	for _, q := range quantiles {
		result := qt.GetQuantile(q)
		if math.IsNaN(result.Seconds()) {
			t.Errorf("GetQuantile(%v) should not return NaN", q)
		}
		if math.IsInf(result.Seconds(), 0) {
			t.Errorf("GetQuantile(%v) should not return Inf", q)
		}
		if result.Seconds() < 0 {
			t.Errorf("GetQuantile(%v) should not return negative value, got %v", q, result)
		}
	}

	// Test boundary quantiles
	boundaryQuantiles := []float64{0.0, 0.01, 0.999, 1.0}
	for _, q := range boundaryQuantiles {
		result := qt.GetQuantile(q)
		if math.IsNaN(result.Seconds()) {
			t.Errorf("GetQuantile(%v) should not return NaN at boundary", q)
		}
		if math.IsInf(result.Seconds(), 0) {
			t.Errorf("GetQuantile(%v) should not return Inf at boundary", q)
		}
	}

	// Test after reset
	qt.Reset()
	result = qt.GetQuantile(0.50)
	if math.IsNaN(result.Seconds()) || math.IsInf(result.Seconds(), 0) {
		t.Errorf("Reset tracker should not return NaN/Inf, got %v", result)
	}
}
