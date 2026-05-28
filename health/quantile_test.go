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

// TestGetQuantiles_MatchesIndividualCalls — semantic equivalence of
// the batched API. Adding the same data must produce identical
// per-quantile results whether queried via 5× GetQuantile or one
// GetQuantiles call. This is the safety net for the policy-side
// optimization in `internal/policy/eval.go` that uses GetQuantiles
// to amortize the `mergedSnapshot` cost across all 5 percentiles.
func TestGetQuantiles_MatchesIndividualCalls(t *testing.T) {
	qt := NewQuantileTracker(&log.Logger)
	// Spread of values so quantile estimates differ at each bucket.
	for _, v := range []float64{
		0.001, 0.005, 0.010, 0.020, 0.030,
		0.050, 0.080, 0.120, 0.200, 0.350,
		0.500, 0.750, 1.000, 1.500, 2.500,
	} {
		qt.Add(v)
	}

	qs := []float64{0.50, 0.70, 0.90, 0.95, 0.99}
	batched := qt.GetQuantiles(qs)
	for i, q := range qs {
		individual := qt.GetQuantile(q)
		if batched[i] != individual {
			t.Errorf("GetQuantiles[%v]=%v but GetQuantile(%v)=%v — must match",
				q, batched[i], q, individual)
		}
	}
}

// TestGetQuantiles_EmptyInput — len(qtiles)==0 must return an empty
// slice and skip the merge entirely (the merge is the expensive
// part — we shouldn't pay for it when no quantiles are asked for).
func TestGetQuantiles_EmptyInput(t *testing.T) {
	qt := NewQuantileTracker(&log.Logger)
	qt.Add(0.5) // make sure there's data so empty-input != empty-sketch
	out := qt.GetQuantiles(nil)
	if len(out) != 0 {
		t.Errorf("GetQuantiles(nil) must return empty slice, got %v", out)
	}
	out = qt.GetQuantiles([]float64{})
	if len(out) != 0 {
		t.Errorf("GetQuantiles([]) must return empty slice, got %v", out)
	}
}

// TestGetQuantiles_EmptySketch — when no samples have been added,
// every requested quantile must come back as zero Duration (mirrors
// per-call GetQuantile's empty-sketch behavior).
func TestGetQuantiles_EmptySketch(t *testing.T) {
	qt := NewQuantileTracker(&log.Logger)
	out := qt.GetQuantiles([]float64{0.50, 0.90, 0.99})
	if len(out) != 3 {
		t.Fatalf("expected len 3, got %d", len(out))
	}
	for i, d := range out {
		if d != 0 {
			t.Errorf("empty sketch quantile[%d] must be 0, got %v", i, d)
		}
	}
}

// BenchmarkGetQuantiles_BatchedVsLoop — pins the policy-side speedup.
// Calling GetQuantile 5 times rebuilds the merged DDSketch 5 times;
// GetQuantiles merges once and reads 5. At the snapshotMetrics
// callsite (5 upstreams × 30 methods per tick) the batched form
// turns 750 merges into 150.
func BenchmarkGetQuantiles_BatchedVsLoop(b *testing.B) {
	logger := log.Logger
	qt := NewQuantileTracker(&logger)
	// Sample population sized to be representative of a ~1k-RPS
	// upstream over a 60s rolling window.
	for i := 0; i < 1000; i++ {
		qt.Add(0.001 + float64(i%200)*0.005)
	}

	qs := []float64{0.50, 0.70, 0.90, 0.95, 0.99}

	b.Run("Loop_5xGetQuantile", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			for _, q := range qs {
				_ = qt.GetQuantile(q)
			}
		}
	})

	b.Run("Batched_GetQuantiles", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = qt.GetQuantiles(qs)
		}
	})
}
