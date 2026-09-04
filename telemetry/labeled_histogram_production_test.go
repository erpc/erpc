package telemetry

import (
	"testing"
)

// TestProductionFlow exercises the exact init sequence erpc.Init uses WITHOUT
// swapping the default registry. On main this panicked because package init
// pre-registered histograms with the empty policy, leaving dimHashesByName
// populated; the fix creates init-time wrappers without registering them and
// has Configure/SetHistogramBuckets do the one authoritative register.
func TestProductionFlow_SetPolicyThenRegister(t *testing.T) {
	// Intentionally DO NOT reset prometheus.DefaultRegisterer — we want to
	// catch regressions where init() and SetHistogramBuckets collide in the
	// same registry, which is the production configuration.
	origPolicy := currentPolicy()
	t.Cleanup(func() { setPolicy(origPolicy) })
	setPolicy(mustPolicy(t,
		Customization{Subject: "*", Labels: []LabelCustomization{
			{Subject: "user", Action: ActionDrop},
			{Subject: "composite", Action: ActionDrop},
		}},
		Customization{Subject: "network_request_duration_seconds", Labels: []LabelCustomization{
			{Subject: "user", Action: ActionKeep},
		}},
	))
	if err := SetHistogramBuckets(""); err != nil {
		t.Fatalf("SetHistogramBuckets: %v", err)
	}
}
