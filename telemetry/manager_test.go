package telemetry

import (
	"fmt"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// withFreshRegistry points the manager at an empty registry and rewinds every
// definition to "not yet registered", so a test sees exactly what its own
// Configure call registers. The prior state is restored afterwards, since the
// definition index is package-global and shared with every other test.
func withFreshRegistry(t *testing.T) *prometheus.Registry {
	t.Helper()

	origRegisterer := prometheus.DefaultRegisterer
	reg := prometheus.NewRegistry()
	prometheus.DefaultRegisterer = reg

	registryMu.Lock()
	origExposure := exposure
	origRegistered := make([]prometheus.Registerer, len(definitions))
	for i, d := range definitions {
		origRegistered[i] = d.registeredWith
		d.registeredWith = nil
	}
	exposure = nil
	registryMu.Unlock()

	t.Cleanup(func() {
		prometheus.DefaultRegisterer = origRegisterer
		registryMu.Lock()
		exposure = origExposure
		for i, d := range definitions {
			d.registeredWith = origRegistered[i]
		}
		registryMu.Unlock()
		SetCounterLabelFilter(nil, nil)
		SetHistogramLabelFilter(nil, nil)
		ResetHandleCache()
	})
	return reg
}

func gatheredFamilies(t *testing.T, reg *prometheus.Registry) map[string]struct{} {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather() failed: %v", err)
	}
	out := make(map[string]struct{}, len(mfs))
	for _, mf := range mfs {
		out[mf.GetName()] = struct{}{}
	}
	return out
}

// registeredFamilies reports which families the manager put on the registry.
// Registration is what matters for exposure control, and a family with no
// observations yet registers without gathering anything, so this asks the
// definitions rather than the scrape output.
func registeredFamilies(reg prometheus.Registerer) map[string]struct{} {
	registryMu.Lock()
	defer registryMu.Unlock()
	out := make(map[string]struct{}, len(definitions))
	for _, d := range definitions {
		if d.registeredWith == reg {
			out[d.family] = struct{}{}
		}
	}
	return out
}

func TestKnownFamilies(t *testing.T) {
	families := KnownFamilies()
	if len(families) == 0 {
		t.Fatal("no metric families are defined")
	}

	seen := make(map[string]struct{}, len(families))
	for i, name := range families {
		if _, dup := seen[name]; dup {
			t.Errorf("family %q appears twice in KnownFamilies()", name)
		}
		seen[name] = struct{}{}
		if !strings.HasPrefix(name, metricNamespace) {
			t.Errorf("family %q does not carry the %q namespace prefix", name, metricNamespace)
		}
		if i > 0 && families[i-1] > name {
			t.Errorf("KnownFamilies() is not sorted: %q precedes %q", families[i-1], name)
		}
	}

	// Spot-check one family per kind, so a definition silently losing its
	// factory call is caught.
	for _, want := range []string{
		"erpc_upstream_request_total",            // labeled counter
		"erpc_upstream_request_duration_seconds", // labeled histogram
		"erpc_upstream_block_head_lag",           // gauge
		"erpc_selection_eval_duration_seconds",   // histogram with own buckets
		"erpc_network_data_unavailable_wait_seconds",
	} {
		if _, ok := seen[want]; !ok {
			t.Errorf("expected %q among the known families", want)
		}
	}
}

// The baseline: no exposure config registers every family the package defines.
func TestConfigure_NoExposureConfigRegistersEverything(t *testing.T) {
	reg := withFreshRegistry(t)
	if err := Configure(&Options{}); err != nil {
		t.Fatalf("Configure failed: %v", err)
	}

	registered := registeredFamilies(reg)
	if len(registered) != len(KnownFamilies()) {
		t.Errorf("registered %d families, expected all %d", len(registered), len(KnownFamilies()))
	}
	exposed, total := ExposedFamilyCount()
	if exposed != total {
		t.Errorf("ExposedFamilyCount() = (%d, %d), expected all families exposed", exposed, total)
	}
}

// An allowlist is the headline: exactly the named families reach the registry,
// and nothing else costs a series.
func TestConfigure_ExposeMetricsRegistersOnlyListed(t *testing.T) {
	reg := withFreshRegistry(t)
	err := Configure(&Options{
		ExposeMetrics: []string{"upstream_request_total", "network_request_duration_seconds"},
	})
	if err != nil {
		t.Fatalf("Configure failed: %v", err)
	}

	registered := registeredFamilies(reg)
	want := []string{"erpc_upstream_request_total", "erpc_network_request_duration_seconds"}
	if len(registered) != len(want) {
		t.Errorf("registered %d families, want %d: %v", len(registered), len(want), registered)
	}
	for _, name := range want {
		if _, ok := registered[name]; !ok {
			t.Errorf("expected %q registered", name)
		}
	}
	if _, ok := registered["erpc_upstream_request_errors_total"]; ok {
		t.Error("erpc_upstream_request_errors_total is outside the allowlist and must not be registered")
	}

	// An unexposed family stays usable — call sites keep their pointer and are
	// not expected to check exposure — its increments just go nowhere.
	MetricUpstreamErrorTotal.WithLabelValues("p", "v", "n", "u", "c", "e", "s", "x", "f", "usr", "a").Inc()
	if _, ok := gatheredFamilies(t, reg)["erpc_upstream_request_errors_total"]; ok {
		t.Error("an unexposed family must not appear in the scrape even after being incremented")
	}
}

func TestConfigure_DropMetricsRemovesListed(t *testing.T) {
	reg := withFreshRegistry(t)
	if err := Configure(&Options{DropMetrics: []string{"consensus_*", "upstream_request_total"}}); err != nil {
		t.Fatalf("Configure failed: %v", err)
	}

	registered := registeredFamilies(reg)
	if _, ok := registered["erpc_upstream_request_total"]; ok {
		t.Error("erpc_upstream_request_total is denylisted and must not be registered")
	}
	for name := range registered {
		if strings.HasPrefix(name, "erpc_consensus_") {
			t.Errorf("%q matches the denylisted consensus_* prefix and must not be registered", name)
		}
	}
	if _, ok := registered["erpc_network_request_duration_seconds"]; !ok {
		t.Error("an unlisted family must stay registered under a denylist-only config")
	}
}

// dropMetrics is applied after exposeMetrics, so an operator can keep a
// subsystem and carve one family out of it.
func TestConfigure_DropWinsOverExpose(t *testing.T) {
	reg := withFreshRegistry(t)
	err := Configure(&Options{
		ExposeMetrics: []string{"consensus_*"},
		DropMetrics:   []string{"consensus_duration_seconds"},
	})
	if err != nil {
		t.Fatalf("Configure failed: %v", err)
	}

	registered := registeredFamilies(reg)
	if len(registered) == 0 {
		t.Fatal("expected the consensus_* families registered")
	}
	if _, ok := registered["erpc_consensus_duration_seconds"]; ok {
		t.Error("erpc_consensus_duration_seconds is denylisted and must not be registered")
	}
	if _, ok := registered["erpc_consensus_total"]; !ok {
		t.Error("expected erpc_consensus_total registered via consensus_*")
	}
}

// A bad exposure entry is a config error: nothing is registered, so the caller
// cannot end up serving a half-applied filter.
func TestConfigure_InvalidExposureRegistersNothing(t *testing.T) {
	reg := withFreshRegistry(t)
	err := Configure(&Options{ExposeMetrics: []string{"upstream_*_total"}})
	if err == nil {
		t.Fatal("expected an error for a mid-string '*'")
	}
	if got := registeredFamilies(reg); len(got) != 0 {
		t.Errorf("expected nothing registered after a config error, got %d families", len(got))
	}
}

// An unparseable bucket list is reported but must not stop startup: the defaults
// apply and every family is still registered.
func TestConfigure_InvalidBucketsFallsBackToDefaults(t *testing.T) {
	reg := withFreshRegistry(t)
	err := Configure(&Options{HistogramBuckets: "0.1,not-a-number"})
	if err == nil {
		t.Fatal("expected an error for an unparseable histogramBuckets value")
	}
	if _, ok := registeredFamilies(reg)["erpc_upstream_request_duration_seconds"]; !ok {
		t.Error("registration must proceed with the default buckets despite the parse error")
	}
}

// Histograms that omit Buckets take them from config; those that declare their
// own keep them.
func TestConfigure_HistogramBucketsApplyOnlyWhereUndeclared(t *testing.T) {
	reg := withFreshRegistry(t)
	if err := Configure(&Options{HistogramBuckets: "0.25,2.5"}); err != nil {
		t.Fatalf("Configure failed: %v", err)
	}

	MetricNetworkRequestDuration.WithLabelValues("p", "n", "v", "u", "eth_call", "finalized", "usr").Observe(1)
	MetricNetworkHedgeDelaySeconds.WithLabelValues("p", "n", "eth_call", "finalized").Observe(1)

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather() failed: %v", err)
	}
	bounds := map[string][]float64{}
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			if h := m.GetHistogram(); h != nil {
				for _, b := range h.GetBucket() {
					bounds[mf.GetName()] = append(bounds[mf.GetName()], b.GetUpperBound())
				}
			}
		}
	}

	if got := bounds["erpc_network_request_duration_seconds"]; len(got) != 2 || got[0] != 0.25 || got[1] != 2.5 {
		t.Errorf("expected the configured buckets [0.25 2.5] on network_request_duration_seconds, got %v", got)
	}
	// network_hedge_delay_seconds declares its own buckets, which config must
	// not override.
	if got := bounds["erpc_network_hedge_delay_seconds"]; len(got) == 2 {
		t.Errorf("network_hedge_delay_seconds must keep its declared buckets, got %v", got)
	}
}

// Configure(nil) is the "config has no metrics section" case: histograms become
// scrapeable, but counter label sets stay open so a later Configure carrying
// counterDropLabels can still apply them.
func TestConfigure_NilOptionsRegistersHistogramsOnly(t *testing.T) {
	reg := withFreshRegistry(t)
	if err := Configure(nil); err != nil {
		t.Fatalf("Configure(nil) failed: %v", err)
	}

	registered := registeredFamilies(reg)
	if _, ok := registered["erpc_upstream_request_duration_seconds"]; !ok {
		t.Error("expected histograms registered by Configure(nil)")
	}
	if _, ok := registered["erpc_upstream_request_total"]; ok {
		t.Error("counters must stay unregistered so a later Configure can apply counterDropLabels")
	}
	if _, ok := registered["erpc_upstream_block_head_lag"]; ok {
		t.Error("gauges must stay unregistered until a Configure with a metrics section runs")
	}
}

func TestSetHistogramBuckets_LeavesCountersUnregistered(t *testing.T) {
	reg := withFreshRegistry(t)
	if err := SetHistogramBuckets("0.5,1"); err != nil {
		t.Fatalf("SetHistogramBuckets failed: %v", err)
	}

	registered := registeredFamilies(reg)
	if _, ok := registered["erpc_network_request_duration_seconds"]; !ok {
		t.Error("expected histograms registered")
	}
	if _, ok := registered["erpc_upstream_request_total"]; ok {
		t.Error("SetHistogramBuckets must not register counters")
	}
	if _, ok := registered["erpc_selection_eval_duration_seconds"]; !ok {
		t.Error("expected histograms with their own buckets registered too")
	}
}

func TestSetHistogramBuckets_ReportsParseError(t *testing.T) {
	withFreshRegistry(t)
	if err := SetHistogramBuckets("nope"); err == nil {
		t.Error("expected an error for an unparseable bucket list")
	}
}

// counterDropLabels reaches the labeled counters through Configure, which is the
// step that both applies the filter and registers under it.
func TestConfigure_CounterDropLabelsApplyBeforeRegistration(t *testing.T) {
	reg := withFreshRegistry(t)
	err := Configure(&Options{
		CounterDropLabels:     []string{"user", "agent_name"},
		CounterLabelOverrides: map[string][]string{"upstream_request_total": {"user"}},
	})
	if err != nil {
		t.Fatalf("Configure failed: %v", err)
	}

	MetricUpstreamRequestTotal.WithLabelValues("p", "v", "n", "u", "c", "1", "x", "f", "usr", "agent").Inc()
	MetricUpstreamSkippedTotal.WithLabelValues("p", "v", "n", "u", "c", "f", "usr", "agent").Inc()

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather() failed: %v", err)
	}
	labelsOf := map[string]map[string]struct{}{}
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			set := map[string]struct{}{}
			for _, lp := range m.GetLabel() {
				set[lp.GetName()] = struct{}{}
			}
			labelsOf[mf.GetName()] = set
		}
	}

	req := labelsOf["erpc_upstream_request_total"]
	if req == nil {
		t.Fatal("erpc_upstream_request_total did not appear in the scrape")
	}
	if _, ok := req["user"]; !ok {
		t.Error("upstream_request_total has a keep override for user, so user must survive")
	}
	if _, ok := req["agent_name"]; ok {
		t.Error("agent_name is dropped globally and has no override on upstream_request_total")
	}

	skipped := labelsOf["erpc_upstream_request_skipped_total"]
	if skipped == nil {
		t.Fatal("erpc_upstream_request_skipped_total did not appear in the scrape")
	}
	for _, l := range []string{"user", "agent_name"} {
		if _, ok := skipped[l]; ok {
			t.Errorf("%q must be dropped from upstream_request_skipped_total", l)
		}
	}
}

// A second Configure on the same registry must not disturb what is already
// registered: Prometheus has frozen those label sets, and rebuilding under them
// would leave the registry describing a shape it no longer collects.
func TestConfigure_SecondCallLeavesRegisteredFamiliesAlone(t *testing.T) {
	reg := withFreshRegistry(t)
	if err := Configure(&Options{}); err != nil {
		t.Fatalf("first Configure failed: %v", err)
	}
	MetricUpstreamRequestTotal.WithLabelValues("p", "v", "n", "u", "c", "1", "x", "f", "usr", "agent").Inc()

	// A changed counter filter cannot be applied retroactively.
	if err := Configure(&Options{CounterDropLabels: []string{"user"}}); err != nil {
		t.Fatalf("second Configure failed: %v", err)
	}

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather() after a repeat Configure failed: %v", err)
	}
	var found bool
	for _, mf := range mfs {
		if mf.GetName() != "erpc_upstream_request_total" {
			continue
		}
		found = true
		for _, m := range mf.GetMetric() {
			var hasUser bool
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "user" {
					hasUser = true
				}
			}
			if !hasUser {
				t.Error("the already-registered label set must be preserved, including user")
			}
			if m.GetCounter().GetValue() != 1 {
				t.Errorf("expected the accumulated count preserved, got %v", m.GetCounter().GetValue())
			}
		}
	}
	if !found {
		t.Error("erpc_upstream_request_total disappeared after a repeat Configure")
	}
}

func TestUnmatchedExposureEntries(t *testing.T) {
	withFreshRegistry(t)
	err := Configure(&Options{
		ExposeMetrics: []string{"upstream_request_total", "no_such_metric_total"},
		DropMetrics:   []string{"consensus_*", "nosuch_*", "go_goroutines"},
	})
	if err != nil {
		t.Fatalf("Configure failed: %v", err)
	}

	got := UnmatchedExposureEntries()
	want := []string{"no_such_metric_total", "nosuch_*"}
	if len(got) != len(want) {
		t.Fatalf("UnmatchedExposureEntries() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("UnmatchedExposureEntries()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestExposedFamilyCount(t *testing.T) {
	withFreshRegistry(t)
	if err := Configure(&Options{ExposeMetrics: []string{"upstream_request_total"}}); err != nil {
		t.Fatalf("Configure failed: %v", err)
	}
	exposed, total := ExposedFamilyCount()
	if exposed != 1 {
		t.Errorf("expected 1 exposed family, got %d", exposed)
	}
	if total != len(KnownFamilies()) {
		t.Errorf("total = %d, want %d", total, len(KnownFamilies()))
	}
}

// Gatherer only wraps when there is something to filter, and when it does wrap it
// covers the stock collectors the manager never registers itself.
func TestGatherer(t *testing.T) {
	withFreshRegistry(t)
	// A pointer, so the identity check below compares pointers rather than a
	// struct holding an (incomparable) slice.
	stock := &stubGatherer{families: stubFamilies("go_goroutines", "erpc_upstream_request_total")}

	if err := Configure(&Options{}); err != nil {
		t.Fatalf("Configure failed: %v", err)
	}
	if got := Gatherer(stock); got != prometheus.Gatherer(stock) {
		t.Error("with no exposure list configured, Gatherer must hand back the gatherer unchanged")
	}

	if err := Configure(&Options{ExposeMetrics: []string{"upstream_request_total"}}); err != nil {
		t.Fatalf("Configure failed: %v", err)
	}
	got, err := Gatherer(stock).Gather()
	if err != nil {
		t.Fatalf("Gather() failed: %v", err)
	}
	if names := familyNames(got); len(names) != 1 || names[0] != "erpc_upstream_request_total" {
		t.Errorf("Gather() returned %v, want only erpc_upstream_request_total", names)
	}
}

// Two definitions sharing a family name would half-register and silently lose
// one of them, so it fails at startup instead.
func TestDefine_DuplicateFamilyPanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected a panic when a family name is defined twice")
		}
		if !strings.Contains(fmt.Sprint(r), "defined twice") {
			t.Errorf("unexpected panic: %v", r)
		}
	}()
	DefineCounter(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "upstream_request_total",
	}, []string{"project"})
}

// A histogram with no buckets would silently take Prometheus's defaults instead
// of the configured ones, which looks like config being ignored.
func TestDefineHistogram_RequiresExplicitBuckets(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected a panic for a plain histogram with no buckets")
		}
		if !strings.Contains(fmt.Sprint(r), "declares no buckets") {
			t.Errorf("unexpected panic: %v", r)
		}
	}()
	DefineHistogram(prometheus.HistogramOpts{
		Namespace: "erpc",
		Name:      "test_manager_bucketless_seconds",
	}, []string{"project"})
}
