package telemetry

import (
	"errors"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func TestNormalizeMetricName(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"upstream_request_total", "erpc_upstream_request_total"},
		{"erpc_upstream_request_total", "erpc_upstream_request_total"},
		{"  upstream_request_total  ", "erpc_upstream_request_total"},
		{"go_goroutines", "go_goroutines"},
		{"process_cpu_seconds_total", "process_cpu_seconds_total"},
		{"promhttp_metric_handler_requests_total", "promhttp_metric_handler_requests_total"},
		{"", ""},
		{"   ", ""},
		// Not a stock prefix and not erpc-prefixed: normalized like any eRPC name,
		// which is why third-party collectors outside go_/process_/promhttp_ are
		// not addressable by config.
		{"custom_thing_total", "erpc_custom_thing_total"},
	}
	for _, c := range cases {
		if got := NormalizeMetricName(c.in); got != c.want {
			t.Errorf("NormalizeMetricName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func mustFilter(t *testing.T, expose, drop []string) *MetricExposureFilter {
	t.Helper()
	f, err := NewMetricExposureFilter(expose, drop)
	if err != nil {
		t.Fatalf("NewMetricExposureFilter(%v, %v) failed: %v", expose, drop, err)
	}
	return f
}

// Both lists empty is the default deployment: every family stays exposed and
// the filter reports itself inactive so callers can skip it entirely.
func TestExposureFilter_EmptyExposesEverything(t *testing.T) {
	f := mustFilter(t, nil, nil)
	if f.Active() {
		t.Error("filter with no entries should report Active() == false")
	}
	for _, name := range []string{"erpc_upstream_request_total", "go_goroutines", "anything_at_all"} {
		if !f.Exposed(name) {
			t.Errorf("expected %q exposed when both lists are empty", name)
		}
	}
}

// A nil filter is the "no metrics config" case and must behave like an empty one
// rather than dropping everything.
func TestExposureFilter_NilExposesEverything(t *testing.T) {
	var f *MetricExposureFilter
	if f.Active() {
		t.Error("nil filter should report Active() == false")
	}
	if !f.Exposed("erpc_upstream_request_total") {
		t.Error("nil filter should expose every family")
	}
	if got := UnmatchedEntries(f, []string{"erpc_upstream_request_total"}); got != nil {
		t.Errorf("nil filter should report no unmatched entries, got %v", got)
	}
}

// An allowlist is exhaustive: anything not named is dropped, including the stock
// collectors, which is what makes exposeMetrics usable for a minimal scrape.
func TestExposureFilter_AllowlistExactOnly(t *testing.T) {
	f := mustFilter(t, []string{"upstream_request_total", "erpc_network_request_duration_seconds"}, nil)
	if !f.Active() {
		t.Error("filter with allowlist entries should report Active() == true")
	}
	exposed := []string{"erpc_upstream_request_total", "erpc_network_request_duration_seconds"}
	notExposed := []string{"erpc_upstream_request_errors_total", "go_goroutines", "erpc_upstream_request_total_extra"}
	for _, name := range exposed {
		if !f.Exposed(name) {
			t.Errorf("expected %q exposed by allowlist", name)
		}
	}
	for _, name := range notExposed {
		if f.Exposed(name) {
			t.Errorf("expected %q not exposed — absent from allowlist", name)
		}
	}
}

// A trailing "*" addresses a whole subsystem, which is the only way to write
// exposeMetrics/dropMetrics against families added by later releases.
func TestExposureFilter_PrefixEntries(t *testing.T) {
	f := mustFilter(t, nil, []string{"consensus_*"})
	dropped := []string{"erpc_consensus_total", "erpc_consensus_disputes_total", "erpc_consensus_"}
	kept := []string{"erpc_upstream_request_total", "erpc_consensu_total", "go_goroutines"}
	for _, name := range dropped {
		if f.Exposed(name) {
			t.Errorf("expected %q dropped by prefix entry consensus_*", name)
		}
	}
	for _, name := range kept {
		if !f.Exposed(name) {
			t.Errorf("expected %q kept — outside prefix consensus_*", name)
		}
	}
}

func TestExposureFilter_DenylistExactOnly(t *testing.T) {
	f := mustFilter(t, nil, []string{"upstream_request_total", "go_goroutines"})
	if f.Exposed("erpc_upstream_request_total") {
		t.Error("expected denylisted erpc_upstream_request_total to be dropped")
	}
	if f.Exposed("go_goroutines") {
		t.Error("expected denylisted go_goroutines to be dropped")
	}
	if !f.Exposed("erpc_network_request_duration_seconds") {
		t.Error("expected unlisted family to stay exposed under a denylist-only config")
	}
}

// Allowlist first, then denylist — so an operator can keep a subsystem and carve
// one noisy family out of it without enumerating the rest.
func TestExposureFilter_DenylistWinsOverAllowlist(t *testing.T) {
	f := mustFilter(t,
		[]string{"upstream_*", "network_request_duration_seconds"},
		[]string{"upstream_request_errors_total"},
	)
	if !f.Exposed("erpc_upstream_request_total") {
		t.Error("expected erpc_upstream_request_total exposed via upstream_*")
	}
	if f.Exposed("erpc_upstream_request_errors_total") {
		t.Error("expected the denylisted family dropped even though upstream_* allows it")
	}
	if !f.Exposed("erpc_network_request_duration_seconds") {
		t.Error("expected the exact allowlist entry exposed")
	}
	if f.Exposed("erpc_consensus_total") {
		t.Error("expected a family outside the allowlist dropped")
	}
}

func TestExposureFilter_MixedExactAndPrefixInOneList(t *testing.T) {
	f := mustFilter(t, []string{"consensus_*", "upstream_request_total"}, nil)
	for _, name := range []string{"erpc_consensus_disputes_total", "erpc_upstream_request_total"} {
		if !f.Exposed(name) {
			t.Errorf("expected %q exposed", name)
		}
	}
	if f.Exposed("erpc_upstream_request_errors_total") {
		t.Error("expected erpc_upstream_request_errors_total dropped")
	}
}

func TestNewMetricExposureFilter_ValidationErrors(t *testing.T) {
	cases := []struct {
		name        string
		expose      []string
		drop        []string
		wantInError string
	}{
		{
			name:        "empty entry in expose list",
			expose:      []string{"upstream_request_total", ""},
			wantInError: "metrics.exposeMetrics contains an empty entry",
		},
		{
			name:        "whitespace-only entry in drop list",
			drop:        []string{"   "},
			wantInError: "metrics.dropMetrics contains an empty entry",
		},
		{
			name:        "star in the middle",
			expose:      []string{"upstream_*_total"},
			wantInError: "only allowed as the final character",
		},
		{
			name:        "star at the start",
			drop:        []string{"*_total"},
			wantInError: "only allowed as the final character",
		},
		{
			name:        "bare star",
			expose:      []string{"*"},
			wantInError: `write "erpc_*"`,
		},
		{
			name:        "duplicate exact entries",
			expose:      []string{"upstream_request_total", "upstream_request_total"},
			wantInError: "is a duplicate",
		},
		{
			// The prefix is optional in config, so these two spellings are the
			// same entry and the second is a mistake worth reporting.
			name:        "duplicate across prefixed and unprefixed spelling",
			drop:        []string{"upstream_request_total", "erpc_upstream_request_total"},
			wantInError: "is a duplicate",
		},
		{
			name:        "duplicate prefix entries",
			drop:        []string{"consensus_*", "consensus_*"},
			wantInError: "is a duplicate",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := NewMetricExposureFilter(c.expose, c.drop)
			if err == nil {
				t.Fatalf("expected an error for expose=%v drop=%v", c.expose, c.drop)
			}
			if !strings.Contains(err.Error(), c.wantInError) {
				t.Errorf("error %q does not contain %q", err.Error(), c.wantInError)
			}
		})
	}
}

// An exact name and a prefix whose stem is that name are distinct entries, not a
// duplicate: "consensus_total" and "consensus_total*" mean different things.
func TestNewMetricExposureFilter_ExactAndPrefixAreNotDuplicates(t *testing.T) {
	if _, err := NewMetricExposureFilter([]string{"consensus_total", "consensus_total*"}, nil); err != nil {
		t.Fatalf("exact + same-stem prefix should be accepted, got: %v", err)
	}
}

// A typo'd entry silently does nothing, so startup needs to be able to point at
// it by the text the operator actually wrote.
func TestUnmatchedEntries(t *testing.T) {
	known := []string{
		"erpc_upstream_request_total",
		"erpc_network_request_duration_seconds",
		"erpc_consensus_disputes_total",
	}
	f := mustFilter(t,
		[]string{"upstream_request_total", "typo_metric_total", "consensus_*"},
		[]string{"network_request_duration_seconds", "nosuch_*", "go_goroutines"},
	)
	got := UnmatchedEntries(f, known)
	want := []string{"typo_metric_total", "nosuch_*"}
	if len(got) != len(want) {
		t.Fatalf("UnmatchedEntries() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("UnmatchedEntries()[%d] = %q, want %q (allowlist entries come first)", i, got[i], want[i])
		}
	}
}

func TestUnmatchedEntries_AllMatched(t *testing.T) {
	f := mustFilter(t, []string{"upstream_request_total"}, []string{"consensus_*"})
	if got := UnmatchedEntries(f, []string{"erpc_upstream_request_total", "erpc_consensus_total"}); got != nil {
		t.Errorf("expected no unmatched entries, got %v", got)
	}
}

type stubGatherer struct {
	families []*dto.MetricFamily
	err      error
}

func (s stubGatherer) Gather() ([]*dto.MetricFamily, error) { return s.families, s.err }

func familyNames(mfs []*dto.MetricFamily) []string {
	names := make([]string, len(mfs))
	for i, mf := range mfs {
		names[i] = mf.GetName()
	}
	return names
}

func stubFamilies(names ...string) []*dto.MetricFamily {
	mfs := make([]*dto.MetricFamily, len(names))
	for i, n := range names {
		name := n
		mfs[i] = &dto.MetricFamily{Name: &name}
	}
	return mfs
}

func TestFilteredGatherer_FiltersByFamilyName(t *testing.T) {
	g := stubGatherer{families: stubFamilies(
		"erpc_upstream_request_total",
		"erpc_consensus_total",
		"go_goroutines",
	)}
	fg := NewFilteredGatherer(g, mustFilter(t, nil, []string{"consensus_*", "go_goroutines"}))

	mfs, err := fg.Gather()
	if err != nil {
		t.Fatalf("Gather() failed: %v", err)
	}
	got := familyNames(mfs)
	if len(got) != 1 || got[0] != "erpc_upstream_request_total" {
		t.Errorf("Gather() returned %v, want [erpc_upstream_request_total]", got)
	}
}

// With no exposure config the gatherer must hand back exactly what the wrapped
// one produced — same slice, no per-scrape work.
func TestFilteredGatherer_PassthroughWhenInactive(t *testing.T) {
	families := stubFamilies("erpc_upstream_request_total", "go_goroutines")
	fg := NewFilteredGatherer(stubGatherer{families: families}, mustFilter(t, nil, nil))

	mfs, err := fg.Gather()
	if err != nil {
		t.Fatalf("Gather() failed: %v", err)
	}
	if len(mfs) != 2 {
		t.Fatalf("expected both families passed through, got %v", familyNames(mfs))
	}
}

func TestFilteredGatherer_NilFilterPassesThrough(t *testing.T) {
	families := stubFamilies("erpc_upstream_request_total")
	fg := NewFilteredGatherer(stubGatherer{families: families}, nil)
	mfs, err := fg.Gather()
	if err != nil {
		t.Fatalf("Gather() failed: %v", err)
	}
	if len(mfs) != 1 {
		t.Fatalf("expected the family passed through, got %v", familyNames(mfs))
	}
}

// Prometheus reports partial-collection failures as families plus a non-nil
// error; swallowing either half would hide a broken collector from the scrape.
func TestFilteredGatherer_PreservesPartialError(t *testing.T) {
	sentinel := errors.New("collector blew up")
	g := stubGatherer{families: stubFamilies("erpc_upstream_request_total", "erpc_consensus_total"), err: sentinel}
	fg := NewFilteredGatherer(g, mustFilter(t, nil, []string{"consensus_*"}))

	mfs, err := fg.Gather()
	if !errors.Is(err, sentinel) {
		t.Errorf("expected the wrapped gatherer's error to survive, got %v", err)
	}
	if got := familyNames(mfs); len(got) != 1 || got[0] != "erpc_upstream_request_total" {
		t.Errorf("expected the exposed family alongside the error, got %v", got)
	}
}

// Sanity check against a real registry rather than a stub, so the wiring Phase 2
// installs at the HTTP handler is known to work.
func TestFilteredGatherer_OverRealRegistry(t *testing.T) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(prometheus.NewCounter(prometheus.CounterOpts{Name: "erpc_kept_total"}))
	reg.MustRegister(prometheus.NewCounter(prometheus.CounterOpts{Name: "erpc_dropped_total"}))

	fg := NewFilteredGatherer(reg, mustFilter(t, []string{"kept_total"}, nil))
	mfs, err := fg.Gather()
	if err != nil {
		t.Fatalf("Gather() failed: %v", err)
	}
	if got := familyNames(mfs); len(got) != 1 || got[0] != "erpc_kept_total" {
		t.Errorf("Gather() returned %v, want [erpc_kept_total]", got)
	}
}
