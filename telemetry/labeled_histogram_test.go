package telemetry

import (
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// scrapeMetricsOutput returns the full /metrics body and a per-metric line count.
func scrapeMetricsOutput(t *testing.T, reg *prometheus.Registry) (body string, linesByMetric map[string]int, totalLines int) {
	t.Helper()
	srv := httptest.NewServer(promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	defer srv.Close()
	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatalf("scrape failed: %v", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	body = string(b)
	linesByMetric = map[string]int{}
	for _, line := range strings.Split(body, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		totalLines++
		// metric_name{labels} value
		if i := strings.IndexAny(line, "{ "); i > 0 {
			linesByMetric[line[:i]]++
		}
	}
	return
}

// emitSynthetic drives the three filter-aware histograms with a fixed cross
// product so label-vs-bytes math is deterministic.
func emitSynthetic(users, networks, upstreams int) {
	for u := 0; u < users; u++ {
		user := fmt.Sprintf("user-%d", u)
		for n := 0; n < networks; n++ {
			network := fmt.Sprintf("net-%d", n)
			for up := 0; up < upstreams; up++ {
				upstream := fmt.Sprintf("ups-%d", up)
				MetricUpstreamRequestDuration.WithLabelValues(
					"standard", "vendorA", network, upstream, "eth_call", "none", "finalized", user,
				).Observe(0.123)
				MetricNetworkRequestDuration.WithLabelValues(
					"standard", network, "vendorA", upstream, "eth_call", "finalized", user,
				).Observe(0.200)
			}
			// getLogs histogram: network+user only (no upstream dim)
			MetricNetworkEvmGetLogsRangeRequested.WithLabelValues(
				"standard", network, "eth_getLogs", user, "finalized",
			).Observe(1000)
		}
	}
}

func runScenario(t *testing.T, name string, drop []string, overrides map[string][]string) (bytes int, lines int, perMetric map[string]int) {
	t.Helper()
	// Fresh registry so counts reflect only this run's emissions.
	reg := prometheus.NewRegistry()
	prometheus.DefaultRegisterer = reg
	SetHistogramLabelFilter(drop, overrides)
	if err := SetHistogramBuckets(""); err != nil {
		t.Fatalf("%s: SetHistogramBuckets: %v", name, err)
	}
	emitSynthetic(50, 10, 5) // 50 users × 10 networks × 5 upstreams = 2500 combos
	body, perMetric, lines := scrapeMetricsOutput(t, reg)
	return len(body), lines, perMetric
}

func TestHistogramLabelFilter_SizeAndCardinality(t *testing.T) {
	// Scenario: 50 users × 10 networks × 5 upstreams
	// Expected: dropping "user" should reduce upstream_request_duration and
	// network_request_duration series by ~50x (one row per user collapses to
	// one row total per (network, upstream, method, ...) tuple).

	baseBytes, baseLines, basePer := runScenario(t, "baseline", nil, nil)
	dropBytes, dropLines, dropPer := runScenario(t, "drop-user", []string{"user"}, nil)
	overrideBytes, overrideLines, overridePer := runScenario(t, "drop-user-keep-on-network", []string{"user"},
		map[string][]string{"network_request_duration_seconds": {"user"}})
	dropBothBytes, dropBothLines, dropBothPer := runScenario(t, "drop-user-and-composite", []string{"user", "composite"}, nil)

	reportMetrics := []string{
		"erpc_upstream_request_duration_seconds_bucket",
		"erpc_upstream_request_duration_seconds_count",
		"erpc_network_request_duration_seconds_bucket",
		"erpc_network_request_duration_seconds_count",
		"erpc_network_evm_get_logs_range_requested_bucket",
		"erpc_network_evm_get_logs_range_requested_count",
	}

	t.Logf("scenario                       | total lines | total bytes")
	t.Logf("-------------------------------+-------------+------------")
	t.Logf("baseline                       | %11d | %10d", baseLines, baseBytes)
	t.Logf("drop user                      | %11d | %10d  (-%d%%)", dropLines, dropBytes, int(100-100*float64(dropBytes)/float64(baseBytes)))
	t.Logf("drop user, keep on network_rd  | %11d | %10d  (-%d%%)", overrideLines, overrideBytes, int(100-100*float64(overrideBytes)/float64(baseBytes)))
	t.Logf("drop user + composite          | %11d | %10d  (-%d%%)", dropBothLines, dropBothBytes, int(100-100*float64(dropBothBytes)/float64(baseBytes)))
	t.Logf("")
	t.Logf("per-metric series counts (baseline → drop-user → drop+override → drop-both):")
	for _, m := range reportMetrics {
		t.Logf("  %-55s %6d → %6d → %6d → %6d", m, basePer[m], dropPer[m], overridePer[m], dropBothPer[m])
	}

	// Invariants that must hold for the feature to work.
	if dropBytes >= baseBytes {
		t.Fatalf("drop-user scenario produced %d bytes >= baseline %d", dropBytes, baseBytes)
	}
	upBase := basePer["erpc_upstream_request_duration_seconds_bucket"]
	upDrop := dropPer["erpc_upstream_request_duration_seconds_bucket"]
	if upDrop == 0 || upDrop >= upBase/10 {
		t.Fatalf("expected upstream_request_duration_bucket to shrink by >10x after dropping user; got baseline=%d drop=%d", upBase, upDrop)
	}

	// Override must preserve user on network_request_duration (cardinality stays).
	netOverride := overridePer["erpc_network_request_duration_seconds_bucket"]
	netDrop := dropPer["erpc_network_request_duration_seconds_bucket"]
	if netOverride <= netDrop {
		t.Fatalf("override should keep user on network_request_duration; override=%d drop=%d",
			netOverride, netDrop)
	}
	netBase := basePer["erpc_network_request_duration_seconds_bucket"]
	if netOverride != netBase {
		t.Fatalf("override should match baseline for network_request_duration; override=%d baseline=%d",
			netOverride, netBase)
	}
}

// emitAllHistograms hits every filter-aware histogram (all 13) so a filter
// change is observable across the full set, not just the three that carry
// a "user" label.
func emitAllHistograms(methods, networks int) {
	for m := 0; m < methods; m++ {
		method := fmt.Sprintf("m-%d", m)
		for n := 0; n < networks; n++ {
			network := fmt.Sprintf("net-%d", n)
			// 3 user-carrying histograms
			MetricUpstreamRequestDuration.WithLabelValues("standard", "vendorA", network, "up-1", method, "none", "finalized", "user-1").Observe(0.1)
			MetricNetworkRequestDuration.WithLabelValues("standard", network, "vendorA", "up-1", method, "finalized", "user-1").Observe(0.1)
			MetricNetworkEvmGetLogsRangeRequested.WithLabelValues("standard", network, method, "user-1", "finalized").Observe(100)
			// 10 historically-unfiltered histograms (now filter-aware after refactor)
			MetricNetworkHedgeDelaySeconds.WithLabelValues("standard", network, method, "finalized").Observe(0.05)
			MetricConsensusResponsesCollected.WithLabelValues("standard", network, method, "vA", "false", "finalized").Observe(3)
			MetricConsensusAgreementCount.WithLabelValues("standard", network, method, "finalized").Observe(2)
			MetricX402FacilitatorRequestDuration.WithLabelValues("standard", network, "facA", "verify", "ok").Observe(0.1)
			MetricConsensusDuration.WithLabelValues("standard", network, method, "ok", "finalized").Observe(0.1)
			MetricCacheSetSuccessDuration.WithLabelValues("standard", network, method, "conn", "pol", "60").Observe(0.01)
			MetricCacheSetErrorDuration.WithLabelValues("standard", network, method, "conn", "pol", "60", "err").Observe(0.01)
			MetricCacheGetSuccessHitDuration.WithLabelValues("standard", network, method, "conn", "pol", "60").Observe(0.01)
			MetricCacheGetSuccessMissDuration.WithLabelValues("standard", network, method, "conn", "pol", "60").Observe(0.01)
			MetricCacheGetErrorDuration.WithLabelValues("standard", network, method, "conn", "pol", "60", "err").Observe(0.01)
		}
	}
}

// TestHistogramLabelFilter_AllHistogramsObeyFilter verifies the refactor: a
// global drop on a shared label now affects every histogram, not only the
// three that previously used LabeledHistogram.
func TestHistogramLabelFilter_AllHistogramsObeyFilter(t *testing.T) {
	run := func(drop []string) map[string]int {
		reg := prometheus.NewRegistry()
		prometheus.DefaultRegisterer = reg
		SetHistogramLabelFilter(drop, nil)
		if err := SetHistogramBuckets(""); err != nil {
			t.Fatalf("SetHistogramBuckets: %v", err)
		}
		emitAllHistograms(5, 4) // 5 methods × 4 networks = 20 combos per histogram
		_, perMetric, _ := scrapeMetricsOutput(t, reg)
		return perMetric
	}

	baseline := run(nil)
	dropped := run([]string{"category"}) // "category" (= method) is present on most histograms

	// Every histogram that has the "category" label should shrink.
	// (x402_facilitator_request_duration_seconds has no "category" label — skip it.)
	withCategory := []string{
		"erpc_upstream_request_duration_seconds_bucket",
		"erpc_network_request_duration_seconds_bucket",
		"erpc_network_evm_get_logs_range_requested_bucket",
		"erpc_network_hedge_delay_seconds_bucket",
		"erpc_consensus_responses_collected_bucket",
		"erpc_consensus_agreement_count_bucket",
		"erpc_consensus_duration_seconds_bucket",
		"erpc_cache_set_success_duration_seconds_bucket",
		"erpc_cache_set_error_duration_seconds_bucket",
		"erpc_cache_get_success_hit_duration_seconds_bucket",
		"erpc_cache_get_success_miss_duration_seconds_bucket",
		"erpc_cache_get_error_duration_seconds_bucket",
	}

	t.Logf("metric                                                          | baseline | drop-category")
	t.Logf("----------------------------------------------------------------+----------+--------------")
	for _, m := range withCategory {
		t.Logf("  %-60s  | %8d | %13d", m, baseline[m], dropped[m])
		if dropped[m] >= baseline[m] {
			t.Errorf("%s: dropping 'category' did not reduce cardinality (baseline=%d drop=%d)",
				m, baseline[m], dropped[m])
		}
	}

	// x402 has no "category" label — it must be unaffected.
	const x402 = "erpc_x402_facilitator_request_duration_seconds_bucket"
	t.Logf("  %-60s  | %8d | %13d  (no 'category' label)", x402, baseline[x402], dropped[x402])
	if dropped[x402] != baseline[x402] {
		t.Errorf("%s: should be unaffected by dropping 'category'; baseline=%d drop=%d",
			x402, baseline[x402], dropped[x402])
	}
}
