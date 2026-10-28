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

// TestProductionFlow_ConsensusPolicyEvalDurationIsRegistered guards against the
// class of bug where a histogram is declared but never reaches /metrics.
// consensus_policy_eval_duration_seconds must register through the production
// Configure/SetHistogramBuckets path like its sibling consensus_duration_seconds.
func TestProductionFlow_ConsensusPolicyEvalDurationIsRegistered(t *testing.T) {
	reg := withFreshRegistry(t)
	if err := SetHistogramBuckets(""); err != nil {
		t.Fatalf("SetHistogramBuckets: %v", err)
	}

	MetricConsensusPolicyEvalDuration.WithLabelValues("proj-1", "net-1").Observe(0.01)

	families := gatheredFamilies(t, reg)
	if _, ok := families["erpc_consensus_policy_eval_duration_seconds"]; !ok {
		t.Fatalf("erpc_consensus_policy_eval_duration_seconds not exported after SetHistogramBuckets "+
			"— histogram declared but never registered (scraped families: %v)", families)
	}
}
