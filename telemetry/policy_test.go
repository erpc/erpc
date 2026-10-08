package telemetry

import (
	"reflect"
	"strings"
	"testing"
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

func mustPolicy(t *testing.T, customizations ...Customization) *MetricPolicy {
	t.Helper()
	p, err := NewMetricPolicy(customizations, LegacyLabelConfig{})
	if err != nil {
		t.Fatalf("NewMetricPolicy(%v) failed: %v", customizations, err)
	}
	return p
}

func mustLegacyPolicy(t *testing.T, legacy LegacyLabelConfig, customizations ...Customization) *MetricPolicy {
	t.Helper()
	p, err := NewMetricPolicy(customizations, legacy)
	if err != nil {
		t.Fatalf("NewMetricPolicy(%v, %+v) failed: %v", customizations, legacy, err)
	}
	return p
}

// No customizations is the default deployment: every family stays exposed and
// the policy reports itself inactive so callers can skip it entirely.
func TestPolicy_EmptyExposesEverything(t *testing.T) {
	p := mustPolicy(t)
	if p.Active() {
		t.Error("policy with no rules should report Active() == false")
	}
	for _, name := range []string{"erpc_upstream_request_total", "go_goroutines", "anything_at_all"} {
		if !p.Exposed(name) {
			t.Errorf("expected %q exposed when nothing is customized", name)
		}
	}
}

// A nil policy is the "no metrics config" case and must behave like an empty one
// rather than dropping everything.
func TestPolicy_NilExposesEverything(t *testing.T) {
	var p *MetricPolicy
	if p.Active() {
		t.Error("nil policy should report Active() == false")
	}
	if !p.Exposed("erpc_upstream_request_total") {
		t.Error("nil policy should expose every family")
	}
	if got := p.UnmatchedSubjects([]string{"erpc_upstream_request_total"}); got != nil {
		t.Errorf("nil policy should report no unmatched subjects, got %v", got)
	}
	schema := []string{"project", "user"}
	if got := p.labelIndices("erpc_upstream_request_total", kindCounter, schema); !reflect.DeepEqual(got, []int{0, 1}) {
		t.Errorf("nil policy should keep every label, got indices %v", got)
	}
}

// An allowlist is written explicitly: drop everything, then keep what you want.
// There is no implicit mode switch, so reading a config never depends on which
// other keys are present.
func TestPolicy_DropAllThenKeep(t *testing.T) {
	p := mustPolicy(t,
		Customization{Subject: "*", Action: ActionDrop},
		Customization{Subject: "upstream_request_total", Action: ActionKeep},
		Customization{Subject: "erpc_network_request_duration_seconds", Action: ActionKeep},
	)
	if !p.Active() {
		t.Error("policy with rules should report Active() == true")
	}
	if !p.filtersExposure() {
		t.Error("policy with exposure actions should report filtersExposure() == true")
	}
	exposed := []string{"erpc_upstream_request_total", "erpc_network_request_duration_seconds"}
	dropped := []string{"erpc_upstream_request_errors_total", "go_goroutines", "erpc_upstream_request_total_extra"}
	for _, name := range exposed {
		if !p.Exposed(name) {
			t.Errorf("expected %q exposed by its keep rule", name)
		}
	}
	for _, name := range dropped {
		if p.Exposed(name) {
			t.Errorf("expected %q dropped by subject \"*\"", name)
		}
	}
}

// A trailing "*" addresses a whole subsystem, which is the only way to write a
// customization against families added by later releases.
func TestPolicy_PrefixSubject(t *testing.T) {
	p := mustPolicy(t, Customization{Subject: "consensus_*", Action: ActionDrop})
	dropped := []string{"erpc_consensus_total", "erpc_consensus_disputes_total", "erpc_consensus_"}
	kept := []string{"erpc_upstream_request_total", "erpc_consensu_total", "go_goroutines"}
	for _, name := range dropped {
		if p.Exposed(name) {
			t.Errorf("expected %q dropped by consensus_*", name)
		}
	}
	for _, name := range kept {
		if !p.Exposed(name) {
			t.Errorf("expected %q kept — outside consensus_*", name)
		}
	}
}

// Precedence is by specificity, not list order. This is the property that makes
// the config readable: an operator can append a carve-out without worrying about
// where in the list it lands.
func TestPolicy_SpecificityBeatsOrder(t *testing.T) {
	keepThenDrop := mustPolicy(t,
		Customization{Subject: "consensus_duration_seconds", Action: ActionKeep},
		Customization{Subject: "consensus_*", Action: ActionDrop},
	)
	dropThenKeep := mustPolicy(t,
		Customization{Subject: "consensus_*", Action: ActionDrop},
		Customization{Subject: "consensus_duration_seconds", Action: ActionKeep},
	)
	for _, p := range []*MetricPolicy{keepThenDrop, dropThenKeep} {
		if !p.Exposed("erpc_consensus_duration_seconds") {
			t.Error("the exact subject must win over the prefix regardless of order")
		}
		if p.Exposed("erpc_consensus_disputes_total") {
			t.Error("expected the rest of consensus_* dropped")
		}
	}
}

// A longer prefix is more specific than a shorter one, so nesting carve-outs
// works without exact names.
func TestPolicy_LongerPrefixWins(t *testing.T) {
	p := mustPolicy(t,
		Customization{Subject: "upstream_request_*", Action: ActionKeep},
		Customization{Subject: "upstream_*", Action: ActionDrop},
	)
	if !p.Exposed("erpc_upstream_request_total") {
		t.Error("expected upstream_request_* to win over upstream_*")
	}
	if p.Exposed("erpc_upstream_cordon_duration_seconds") {
		t.Error("expected upstream_* to drop families the longer prefix does not cover")
	}
}

// An entry with no action customizes labels or buckets only. It must not turn on
// the scrape-path filter, which exists solely to hide families.
func TestPolicy_ActionlessEntryDoesNotFilterExposure(t *testing.T) {
	p := mustPolicy(t, Customization{
		Subject: "upstream_request_total",
		Labels:  []LabelCustomization{{Subject: "user", Action: ActionDrop}},
	})
	if !p.Active() {
		t.Error("a label-only customization is still a customization")
	}
	if p.filtersExposure() {
		t.Error("a label-only customization must not enable exposure filtering")
	}
	if !p.Exposed("erpc_upstream_request_total") {
		t.Error("a label-only customization must leave the family exposed")
	}
}

func TestPolicy_LabelRulesByPrefixAndException(t *testing.T) {
	schema := []string{"project", "agent_name", "agent_version", "user"}
	p := mustPolicy(t, Customization{
		Subject: "upstream_request_total",
		Labels: []LabelCustomization{
			{Subject: "agent_*", Action: ActionDrop},
			{Subject: "agent_name", Action: ActionKeep},
		},
	})
	got := p.labelIndices("erpc_upstream_request_total", kindCounter, schema)
	want := []int{0, 1, 3} // project, agent_name, user
	if !reflect.DeepEqual(got, want) {
		t.Errorf("labelIndices = %v, want %v (agent_version dropped, agent_name spared)", got, want)
	}
	// Another family is untouched by an exact subject.
	if got := p.labelIndices("erpc_network_request_duration_seconds", kindHistogram, schema); !reflect.DeepEqual(got, []int{0, 1, 2, 3}) {
		t.Errorf("labelIndices for an unmatched family = %v, want every position", got)
	}
}

// Label rules follow the same specificity ordering as subjects, so the two
// spellings of the same intent agree.
func TestPolicy_LabelRuleSpecificityBeatsOrder(t *testing.T) {
	schema := []string{"agent_name", "agent_version"}
	reversed := mustPolicy(t, Customization{
		Subject: "*",
		Labels: []LabelCustomization{
			{Subject: "agent_name", Action: ActionKeep},
			{Subject: "agent_*", Action: ActionDrop},
		},
	})
	if got := reversed.labelIndices("erpc_upstream_request_total", kindCounter, schema); !reflect.DeepEqual(got, []int{0}) {
		t.Errorf("labelIndices = %v, want [0] — the exact label rule outranks the prefix", got)
	}
}

// A more specific subject's label rules override a broader one's, so a fleet-wide
// drop can be undone for a single metric a pipeline reads.
func TestPolicy_SpecificSubjectOverridesBroadLabelRule(t *testing.T) {
	schema := []string{"project", "user"}
	p := mustPolicy(t,
		Customization{Subject: "*", Labels: []LabelCustomization{{Subject: "user", Action: ActionDrop}}},
		Customization{Subject: "upstream_request_total", Labels: []LabelCustomization{{Subject: "user", Action: ActionKeep}}},
	)
	if got := p.labelIndices("erpc_upstream_request_total", kindCounter, schema); !reflect.DeepEqual(got, []int{0, 1}) {
		t.Errorf("labelIndices for the exempted family = %v, want [0 1]", got)
	}
	if got := p.labelIndices("erpc_consensus_total", kindCounter, schema); !reflect.DeepEqual(got, []int{0}) {
		t.Errorf("labelIndices for another family = %v, want [0]", got)
	}
}

// Buckets are per-family and the most specific subject wins, same as everything
// else.
func TestPolicy_BucketsFor(t *testing.T) {
	p := mustPolicy(t,
		Customization{Subject: "*", Buckets: []float64{1, 2, 3}},
		Customization{Subject: "network_request_duration_seconds", Buckets: []float64{0.05, 0.5, 5}},
	)
	if got, explicit := p.BucketsFor("erpc_network_request_duration_seconds"); !explicit || !reflect.DeepEqual(got, []float64{0.05, 0.5, 5}) {
		t.Errorf("BucketsFor(exact) = %v (explicit=%v), want [0.05 0.5 5]", got, explicit)
	}
	if got, explicit := p.BucketsFor("erpc_upstream_request_duration_seconds"); !explicit || !reflect.DeepEqual(got, []float64{1, 2, 3}) {
		t.Errorf("BucketsFor(wildcard) = %v (explicit=%v), want [1 2 3]", got, explicit)
	}
	if _, explicit := mustPolicy(t).BucketsFor("erpc_network_request_duration_seconds"); explicit {
		t.Error("an empty policy must not claim explicit buckets")
	}
}

// The deprecated knobs are kind-scoped where customizations are subject-scoped:
// counterDropLabels must not reach a histogram carrying the same label.
func TestPolicy_LegacyDropLabelsAreKindScoped(t *testing.T) {
	schema := []string{"project", "user"}
	p := mustLegacyPolicy(t, LegacyLabelConfig{CounterDropLabels: []string{"user"}})
	if got := p.labelIndices("erpc_upstream_request_total", kindCounter, schema); !reflect.DeepEqual(got, []int{0}) {
		t.Errorf("counter labelIndices = %v, want [0]", got)
	}
	if got := p.labelIndices("erpc_network_request_duration_seconds", kindHistogram, schema); !reflect.DeepEqual(got, []int{0, 1}) {
		t.Errorf("histogram labelIndices = %v, want [0 1] — counterDropLabels is counters only", got)
	}
}

func TestPolicy_LegacyOverridesReAddLabel(t *testing.T) {
	schema := []string{"project", "user"}
	p := mustLegacyPolicy(t, LegacyLabelConfig{
		HistogramDropLabels:     []string{"user"},
		HistogramLabelOverrides: map[string][]string{"network_request_duration_seconds": {"user"}},
	})
	if got := p.labelIndices("erpc_network_request_duration_seconds", kindHistogram, schema); !reflect.DeepEqual(got, []int{0, 1}) {
		t.Errorf("overridden histogram labelIndices = %v, want [0 1]", got)
	}
	if got := p.labelIndices("erpc_upstream_request_duration_seconds", kindHistogram, schema); !reflect.DeepEqual(got, []int{0}) {
		t.Errorf("other histogram labelIndices = %v, want [0]", got)
	}
}

// Migrating a legacy field to a customization has to take effect, so an equally
// broad customization outranks the desugared knob.
func TestPolicy_CustomizationBeatsLegacyAtEqualSpecificity(t *testing.T) {
	schema := []string{"project", "user"}
	p := mustLegacyPolicy(t,
		LegacyLabelConfig{CounterDropLabels: []string{"user"}},
		Customization{Subject: "*", Labels: []LabelCustomization{{Subject: "user", Action: ActionKeep}}},
	)
	if got := p.labelIndices("erpc_upstream_request_total", kindCounter, schema); !reflect.DeepEqual(got, []int{0, 1}) {
		t.Errorf("labelIndices = %v, want [0 1] — the customization overrides counterDropLabels", got)
	}
}

// hasLabelRules and namesExactly drive the startup warnings, which must fire for
// a hand-written exact subject and stay quiet for a sweeping one.
func TestPolicy_NamesExactlyAndHasLabelRules(t *testing.T) {
	p := mustPolicy(t,
		Customization{Subject: "upstream_request_total", Labels: []LabelCustomization{{Subject: "user", Action: ActionDrop}}},
		Customization{Subject: "consensus_*", Labels: []LabelCustomization{{Subject: "user", Action: ActionDrop}}},
	)
	if !p.namesExactly("erpc_upstream_request_total") {
		t.Error("expected the exact subject to be reported as naming its family")
	}
	if p.namesExactly("erpc_consensus_total") {
		t.Error("a prefix subject must not count as naming a family exactly")
	}
	if !p.hasLabelRules("erpc_consensus_total") {
		t.Error("expected the prefix subject's label rules to be seen")
	}
	if p.hasLabelRules("erpc_network_request_duration_seconds") {
		t.Error("expected no label rules for an unmatched family")
	}
	// Desugared legacy knobs have never warned and must not start now.
	legacy := mustLegacyPolicy(t, LegacyLabelConfig{CounterDropLabels: []string{"user"}})
	if legacy.hasLabelRules("erpc_upstream_request_total") {
		t.Error("legacy label knobs must not be reported as label rules")
	}
}

func TestNewMetricPolicy_ValidationErrors(t *testing.T) {
	cases := []struct {
		name           string
		customizations []Customization
		wantInError    string
	}{
		{
			name:           "empty subject",
			customizations: []Customization{{Subject: "  ", Action: ActionDrop}},
			wantInError:    "subject is required",
		},
		{
			name:           "star in the middle",
			customizations: []Customization{{Subject: "upstream_*_total", Action: ActionDrop}},
			wantInError:    "only use '*' as its final character",
		},
		{
			name:           "unknown action",
			customizations: []Customization{{Subject: "consensus_*", Action: "hide"}},
			wantInError:    "is not recognized",
		},
		{
			name: "duplicate subject",
			customizations: []Customization{
				{Subject: "upstream_request_total", Action: ActionDrop},
				{Subject: "erpc_upstream_request_total", Action: ActionKeep},
			},
			wantInError: "already customized by an earlier entry",
		},
		{
			name: "duplicate label subject",
			customizations: []Customization{{Subject: "consensus_*", Labels: []LabelCustomization{
				{Subject: "user", Action: ActionDrop},
				{Subject: "user", Action: ActionKeep},
			}}},
			wantInError: "is a duplicate",
		},
		{
			name:           "label without an action",
			customizations: []Customization{{Subject: "consensus_*", Labels: []LabelCustomization{{Subject: "user"}}}},
			wantInError:    "action is required",
		},
		{
			name:           "entry that does nothing",
			customizations: []Customization{{Subject: "consensus_*"}},
			wantInError:    "does nothing",
		},
		{
			name:           "buckets not increasing",
			customizations: []Customization{{Subject: "consensus_duration_seconds", Buckets: []float64{1, 1}}},
			wantInError:    "strictly increasing",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := NewMetricPolicy(c.customizations, LegacyLabelConfig{})
			if err == nil {
				t.Fatalf("expected an error for %v", c.customizations)
			}
			if !strings.Contains(err.Error(), c.wantInError) {
				t.Errorf("error %q does not contain %q", err.Error(), c.wantInError)
			}
		})
	}
}

// An exact name and a prefix whose stem is that name are distinct subjects, not a
// duplicate: "consensus_total" and "consensus_total*" mean different things.
func TestNewMetricPolicy_ExactAndPrefixAreNotDuplicates(t *testing.T) {
	_, err := NewMetricPolicy([]Customization{
		{Subject: "consensus_total", Action: ActionKeep},
		{Subject: "consensus_total*", Action: ActionDrop},
	}, LegacyLabelConfig{})
	if err != nil {
		t.Fatalf("exact + same-stem prefix should be accepted, got: %v", err)
	}
}

// A typo'd subject silently does nothing, so startup needs to be able to point at
// it by the text the operator actually wrote.
func TestPolicy_UnmatchedSubjects(t *testing.T) {
	known := []string{
		"erpc_upstream_request_total",
		"erpc_network_request_duration_seconds",
		"erpc_consensus_disputes_total",
	}
	p := mustLegacyPolicy(t,
		// Legacy knobs are validated by their own fields and must not appear here.
		LegacyLabelConfig{CounterLabelOverrides: map[string][]string{"typo_legacy_total": {"user"}}},
		Customization{Subject: "upstream_request_total", Action: ActionKeep},
		Customization{Subject: "typo_metric_total", Action: ActionKeep},
		Customization{Subject: "consensus_*", Action: ActionDrop},
		Customization{Subject: "nosuch_*", Action: ActionDrop},
		// Stock collectors are registered outside the manager, so they are never
		// in knownFamilies and warning about them would be a false alarm.
		Customization{Subject: "go_goroutines", Action: ActionDrop},
	)
	got := p.UnmatchedSubjects(known)
	want := []string{"nosuch_*", "typo_metric_total"} // broadest first
	if !reflect.DeepEqual(got, want) {
		t.Errorf("UnmatchedSubjects() = %v, want %v", got, want)
	}
}

func TestPolicy_UnmatchedSubjects_AllMatched(t *testing.T) {
	p := mustPolicy(t,
		Customization{Subject: "upstream_request_total", Action: ActionKeep},
		Customization{Subject: "consensus_*", Action: ActionDrop},
	)
	if got := p.UnmatchedSubjects([]string{"erpc_upstream_request_total", "erpc_consensus_total"}); got != nil {
		t.Errorf("expected no unmatched subjects, got %v", got)
	}
}
