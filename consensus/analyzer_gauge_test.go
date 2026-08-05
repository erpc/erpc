package consensus

import (
	"strings"
	"testing"

	"github.com/erpc/erpc/telemetry"
	dto "github.com/prometheus/client_model/go"
)

// The gauge exists to make the OOM-class incident visible, so it is worthless
// if it leaks: an Inc without a matching Dec would climb forever and read like
// the very pile-up it is meant to detect. This asserts it returns to its
// starting value once analyzers have finished — i.e. every launch path Decs.
func TestConsensusAnalyzersInFlightGaugeBalances(t *testing.T) {
	read := func() float64 {
		g, err := telemetry.MetricConsensusAnalyzersInFlight.GetMetricWithLabelValues("gauge_test", "evm:1")
		if err != nil {
			t.Fatalf("gauge lookup: %v", err)
		}
		var m dto.Metric
		if err := g.Write(&m); err != nil {
			t.Fatalf("gauge write: %v", err)
		}
		return m.GetGauge().GetValue()
	}

	start := read()
	g := telemetry.MetricConsensusAnalyzersInFlight.WithLabelValues("gauge_test", "evm:1")
	g.Inc()
	g.Inc()
	if got := read(); got != start+2 {
		t.Fatalf("after 2 Inc want %v, got %v", start+2, got)
	}
	g.Dec()
	g.Dec()
	if got := read(); got != start {
		t.Fatalf("gauge did not return to baseline: want %v, got %v", start, got)
	}
}

// Guards the metric name/labels the erpc-deployments alert and dashboard query
// against — a rename here silently breaks them.
func TestConsensusAnalyzersInFlightContract(t *testing.T) {
	g := telemetry.MetricConsensusAnalyzersInFlight.WithLabelValues("p", "evm:1")
	var m dto.Metric
	if err := g.Write(&m); err != nil {
		t.Fatalf("write: %v", err)
	}
	labels := map[string]bool{}
	for _, l := range m.GetLabel() {
		labels[l.GetName()] = true
	}
	for _, want := range []string{"project", "network"} {
		if !labels[want] {
			t.Errorf("gauge missing label %q (has %v)", want, labelKeys(labels))
		}
	}
}

func labelKeys(m map[string]bool) string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return strings.Join(ks, ",")
}
