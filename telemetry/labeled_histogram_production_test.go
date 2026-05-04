package telemetry

import (
	"testing"
)

// TestProductionFlow exercises the exact init sequence erpc.Init uses WITHOUT
// swapping the default registry. On main this panicked because package init
// pre-registered histograms with the empty filter, leaving dimHashesByName
// populated; the fix creates init-time wrappers without registering them and
// has SetHistogramBuckets do the one authoritative register.
func TestProductionFlow_SetFilterThenRegister(t *testing.T) {
	// Intentionally DO NOT reset prometheus.DefaultRegisterer — we want to
	// catch regressions where init() and SetHistogramBuckets collide in the
	// same registry, which is the production configuration.
	SetHistogramLabelFilter(
		[]string{"user", "composite"},
		map[string][]string{"network_request_duration_seconds": {"user"}},
	)
	if err := SetHistogramBuckets(""); err != nil {
		t.Fatalf("SetHistogramBuckets: %v", err)
	}
}
