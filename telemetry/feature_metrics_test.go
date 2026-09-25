package telemetry

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var featureFamilies = []string{
	"erpc_cache_fill_total", "erpc_cache_fill_wait_seconds",
	"erpc_head_poll_lease_held", "erpc_head_poll_skipped_total",
	"erpc_head_cache_requests_total", "erpc_head_cache_fetches_total",
	"erpc_head_cache_reorgs_total", "erpc_head_cache_snapshots_published_total",
	"erpc_head_cache_leader_acquisitions_total", "erpc_head_cache_leader",
	"erpc_head_cache_head_block", "erpc_head_cache_bytes",
	"erpc_head_cache_snapshot_timestamp_seconds", "erpc_head_cache_subscribers",
	"erpc_head_cache_subscriber_closed_total",
	"erpc_ws_connections", "erpc_ws_subscriptions",
	"erpc_ws_notifications_total", "erpc_ws_connections_closed_total",
}

// Configure registers the feature families on the default registry and they
// appear on a real /metrics scrape; a drop customization removes them.
func TestFeatureMetrics_RegisteredAndScraped(t *testing.T) {
	reg := withFreshRegistry(t)
	if err := Configure(&Options{}); err != nil {
		t.Fatal(err)
	}
	CounterHandle(MetricCacheFillTotal, "p", "evm:1", "leader").Inc()
	ObserverHandle(MetricCacheFillWaitSeconds, "p", "evm:1", "follower_hit").Observe(0.02)
	MetricHeadPollLeaseHeld.WithLabelValues("p", "evm:1").Set(1)
	CounterHandle(MetricHeadPollSkippedTotal, "p", "evm:1").Inc()
	MetricWsConnections.WithLabelValues("p", "evm:1").Inc()
	MetricWsSubscriptions.WithLabelValues("p", "evm:1", "newHeads").Inc()
	CounterHandle(MetricWsNotificationsTotal, "p", "evm:1", "newHeads").Inc()
	CounterHandle(MetricWsClosedTotal, "p", "evm:1", "client").Inc()
	t.Cleanup(func() {
		MetricHeadPollLeaseHeld.Reset()
		MetricWsConnections.Reset()
		MetricWsSubscriptions.Reset()
	})

	srv := httptest.NewServer(promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	body := string(b)
	for _, l := range []string{
		`erpc_cache_fill_total{network="evm:1",outcome="leader",project="p"} 1`,
		`erpc_cache_fill_wait_seconds_count{network="evm:1",outcome="follower_hit",project="p"} 1`,
		`erpc_head_poll_lease_held{network="evm:1",project="p"} 1`,
		`erpc_head_poll_skipped_total{network="evm:1",project="p"} 1`,
		`erpc_ws_connections{network="evm:1",project="p"} 1`,
		`erpc_ws_subscriptions{kind="newHeads",network="evm:1",project="p"} 1`,
		`erpc_ws_notifications_total{kind="newHeads",network="evm:1",project="p"} 1`,
		`erpc_ws_connections_closed_total{network="evm:1",project="p",reason="client"} 1`,
	} {
		if !strings.Contains(body, l+"\n") {
			t.Errorf("scrape missing %q", l)
		}
	}
	registered := registeredFamilies(reg)
	for _, f := range featureFamilies {
		if _, ok := registered[f]; !ok {
			t.Errorf("%s not registered by Configure", f)
		}
	}
}

func TestFeatureMetrics_DropCustomization(t *testing.T) {
	reg := withFreshRegistry(t)
	if err := Configure(&Options{Customizations: []Customization{
		{Subject: "ws_*", Action: ActionDrop},
		{Subject: "head_cache_*", Action: ActionDrop},
	}}); err != nil {
		t.Fatal(err)
	}
	registered := registeredFamilies(reg)
	for _, f := range featureFamilies {
		_, ok := registered[f]
		dropped := strings.HasPrefix(f, "erpc_ws_") || strings.HasPrefix(f, "erpc_head_cache_")
		if ok == dropped {
			t.Errorf("%s registered=%v, want %v", f, ok, !dropped)
		}
	}
}
