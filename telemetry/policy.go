package telemetry

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// Actions a customization entry can take on a metric family or on one of its
// labels.
const (
	ActionKeep = "keep"
	ActionDrop = "drop"
)

// metricNamespace is the prefix every metric family eRPC defines carries.
// Customization subjects may omit it.
const metricNamespace = "erpc_"

// stockNamespaces are the prefixes of collectors eRPC does not define: the Go
// runtime, process, and promhttp collectors the default registry installs.
// Their families are addressed by full name, because there is no eRPC-prefixed
// name they could mean instead.
var stockNamespaces = []string{"go_", "process_", "promhttp_"}

// NormalizeMetricName resolves a configured metric name to the family name it
// refers to: eRPC families may be written with or without the "erpc_" prefix,
// stock collectors pass through by full name. Returns "" for a blank entry.
func NormalizeMetricName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if strings.HasPrefix(name, metricNamespace) {
		return name
	}
	for _, ns := range stockNamespaces {
		if strings.HasPrefix(name, ns) {
			return name
		}
	}
	return metricNamespace + name
}

// Customization is one entry of metrics.customizations: a subject selecting
// metric families, and what to do with them.
//
// telemetry cannot import common (common imports telemetry), so this mirrors
// common.MetricsCustomizationConfig and the config layer maps one onto the other.
type Customization struct {
	// Subject selects families by exact name, by trailing-prefix pattern
	// ("consensus_*"), or "*" for every family.
	Subject string

	// Action is ActionKeep, ActionDrop, or empty to leave exposure alone and
	// only customize labels or buckets.
	Action string

	// Labels projects the matched families' label sets.
	Labels []LabelCustomization

	// Buckets replaces the bucket boundaries of the matched histogram families,
	// overriding both metrics.histogramBuckets and whatever the definition
	// declares in code.
	Buckets []float64
}

// LabelCustomization keeps or drops one label, or a trailing-prefix group of
// them, on the families its parent Customization matched.
type LabelCustomization struct {
	Subject string
	Action  string
}

// LegacyLabelConfig carries the deprecated per-kind label knobs. They are
// desugared into ordinary rules so there is one projection implementation, not
// three.
type LegacyLabelConfig struct {
	HistogramDropLabels     []string
	HistogramLabelOverrides map[string][]string
	CounterDropLabels       []string
	CounterLabelOverrides   map[string][]string
}

// metricKind is which sort of collector a definition holds. It exists for the
// legacy knobs, which are kind-scoped (counterDropLabels means counters only)
// where customizations are subject-scoped.
type metricKind uint8

const (
	kindCounter metricKind = 1 << iota
	kindGauge
	kindHistogram

	kindAny = kindCounter | kindGauge | kindHistogram
)

// pattern is a compiled subject: an exact name, or a stem every match must
// start with. `raw` is kept verbatim so warnings quote what the operator wrote.
type pattern struct {
	raw    string
	match  string
	prefix bool
}

func (p pattern) matches(name string) bool {
	if p.prefix {
		return strings.HasPrefix(name, p.match)
	}
	return name == p.match
}

// specificity orders overlapping subjects. An exact name always outranks a
// prefix, and a longer prefix outranks a shorter one, so "*" is the weakest
// subject that can be written and an exact family name the strongest.
func (p pattern) specificity() int {
	if p.prefix {
		return len(p.match)
	}
	return math.MaxInt32 + len(p.match)
}

// rule is a compiled Customization. Legacy knobs compile to the same shape with
// `kinds` narrowed.
type rule struct {
	subject pattern
	kinds   metricKind
	action  string
	labels  []labelRule
	buckets []float64

	// legacy sinks desugared knobs below customizations of equal specificity,
	// so migrating a field to a customization entry takes effect.
	legacy bool
}

type labelRule struct {
	subject pattern
	keep    bool
}

// MetricPolicy resolves what the metrics config says about a given family:
// whether it is exposed, which of its labels survive, and which buckets it uses.
//
// Overlapping subjects are resolved by specificity, not by list order: an exact
// family name beats a prefix, a longer prefix beats a shorter one, and equally
// specific subjects break to the one written later. That is what makes
// "drop consensus_*, keep consensus_duration_seconds" mean the same thing in
// either order — the operator's intent does not depend on where they typed it.
type MetricPolicy struct {
	// rules run broadest first, so a later match overrides an earlier one.
	rules []rule

	// exposure records whether any rule takes an exposure action, which decides
	// whether the scrape path needs filtering at all.
	exposure bool
}

// NewMetricPolicy compiles the metrics customizations and the deprecated label
// knobs into one policy. A malformed entry is an error rather than something
// skipped: a customization that silently does nothing keeps or drops families
// the operator did not ask for, which is worse than a failed config load.
func NewMetricPolicy(customizations []Customization, legacy LegacyLabelConfig) (*MetricPolicy, error) {
	rules, err := compileCustomizations(customizations)
	if err != nil {
		return nil, err
	}
	rules = append(rules, compileLegacyLabels(legacy)...)

	p := &MetricPolicy{rules: rules}
	for _, r := range p.rules {
		if r.action != "" {
			p.exposure = true
		}
	}

	// Broadest first, and legacy before customizations on a tie. sort.SliceStable
	// keeps the configured order among subjects that are equally specific.
	sort.SliceStable(p.rules, func(i, j int) bool {
		si, sj := p.rules[i].subject.specificity(), p.rules[j].subject.specificity()
		if si != sj {
			return si < sj
		}
		return p.rules[i].legacy && !p.rules[j].legacy
	})
	return p, nil
}

func compileCustomizations(customizations []Customization) ([]rule, error) {
	rules := make([]rule, 0, len(customizations))
	seen := make(map[string]struct{}, len(customizations))
	for i, c := range customizations {
		field := fmt.Sprintf("metrics.customizations[%d]", i)

		subject, err := parseFamilySubject(field, c.Subject)
		if err != nil {
			return nil, err
		}
		key := subject.match
		if subject.prefix {
			key += "*"
		}
		if _, dup := seen[key]; dup {
			return nil, fmt.Errorf("%s: subject %q is already customized by an earlier entry; merge them", field, subject.raw)
		}
		seen[key] = struct{}{}

		action, err := parseAction(field, c.Action, true)
		if err != nil {
			return nil, err
		}
		labels, err := compileLabelRules(field, c.Labels)
		if err != nil {
			return nil, err
		}
		if err := validateBuckets(field, c.Buckets); err != nil {
			return nil, err
		}
		if action == "" && len(labels) == 0 && len(c.Buckets) == 0 {
			return nil, fmt.Errorf("%s: subject %q has no action, labels or buckets, so it does nothing", field, subject.raw)
		}

		rules = append(rules, rule{
			subject: subject,
			kinds:   kindAny,
			action:  action,
			labels:  labels,
			buckets: c.Buckets,
		})
	}
	return rules, nil
}

func compileLabelRules(field string, labels []LabelCustomization) ([]labelRule, error) {
	out := make([]labelRule, 0, len(labels))
	seen := make(map[string]struct{}, len(labels))
	for i, l := range labels {
		lf := fmt.Sprintf("%s.labels[%d]", field, i)
		subject, err := parseLabelSubject(lf, l.Subject)
		if err != nil {
			return nil, err
		}
		key := subject.match
		if subject.prefix {
			key += "*"
		}
		if _, dup := seen[key]; dup {
			return nil, fmt.Errorf("%s: label subject %q is a duplicate of an earlier entry", lf, subject.raw)
		}
		seen[key] = struct{}{}

		// A label entry without an action says nothing about the label.
		action, err := parseAction(lf, l.Action, false)
		if err != nil {
			return nil, err
		}
		out = append(out, labelRule{subject: subject, keep: action == ActionKeep})
	}
	// Broadest first, so "agent_*: drop" plus "agent_name: keep" resolves the
	// same way regardless of the order they are written in.
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].subject.specificity() < out[j].subject.specificity()
	})
	return out, nil
}

// compileLegacyLabels turns the deprecated per-kind knobs into rules. A drop
// list becomes an every-family rule narrowed to one kind; an overrides entry
// becomes an exact-subject rule keeping the listed labels.
func compileLegacyLabels(l LegacyLabelConfig) []rule {
	var rules []rule

	dropAll := func(kind metricKind, labels []string) {
		var lrs []labelRule
		for _, name := range labels {
			if name = strings.TrimSpace(name); name != "" {
				lrs = append(lrs, labelRule{subject: pattern{raw: name, match: name}})
			}
		}
		if len(lrs) > 0 {
			rules = append(rules, rule{subject: pattern{raw: "*", prefix: true}, kinds: kind, labels: lrs, legacy: true})
		}
	}
	keepFor := func(kind metricKind, overrides map[string][]string) {
		// Sorted so the compiled policy does not depend on map iteration order.
		metrics := make([]string, 0, len(overrides))
		for name := range overrides {
			if name = strings.TrimSpace(name); name != "" {
				metrics = append(metrics, name)
			}
		}
		sort.Strings(metrics)
		for _, name := range metrics {
			var lrs []labelRule
			for _, label := range overrides[name] {
				if label = strings.TrimSpace(label); label != "" {
					lrs = append(lrs, labelRule{subject: pattern{raw: label, match: label}, keep: true})
				}
			}
			if len(lrs) > 0 {
				rules = append(rules, rule{
					subject: pattern{raw: name, match: NormalizeMetricName(name)},
					kinds:   kind,
					labels:  lrs,
					legacy:  true,
				})
			}
		}
	}

	dropAll(kindHistogram, l.HistogramDropLabels)
	dropAll(kindCounter, l.CounterDropLabels)
	keepFor(kindHistogram, l.HistogramLabelOverrides)
	keepFor(kindCounter, l.CounterLabelOverrides)
	return rules
}

func parseFamilySubject(field, raw string) (pattern, error) {
	p, err := parseSubject(field, raw)
	if err != nil {
		return p, err
	}
	p.match = NormalizeMetricName(p.match)
	return p, nil
}

// parseLabelSubject matches label names, which carry no namespace prefix.
func parseLabelSubject(field, raw string) (pattern, error) {
	return parseSubject(field, raw)
}

func parseSubject(field, raw string) (pattern, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return pattern{}, fmt.Errorf("%s: subject is required", field)
	}
	if i := strings.IndexByte(s, '*'); i >= 0 && i != len(s)-1 {
		return pattern{}, fmt.Errorf("%s: subject %q may only use '*' as its final character, as a prefix like \"consensus_*\"", field, s)
	}
	return pattern{raw: s, match: strings.TrimSuffix(s, "*"), prefix: strings.HasSuffix(s, "*")}, nil
}

func parseAction(field, raw string, optional bool) (string, error) {
	a := strings.ToLower(strings.TrimSpace(raw))
	switch a {
	case ActionKeep, ActionDrop:
		return a, nil
	case "":
		if optional {
			return "", nil
		}
		return "", fmt.Errorf("%s: action is required and must be %q or %q", field, ActionKeep, ActionDrop)
	default:
		return "", fmt.Errorf("%s: action %q is not recognized; use %q or %q", field, raw, ActionKeep, ActionDrop)
	}
}

// validateBuckets rejects what prometheus.NewHistogramVec would panic on.
func validateBuckets(field string, buckets []float64) error {
	for i, b := range buckets {
		if math.IsNaN(b) {
			return fmt.Errorf("%s: buckets[%d] is not a number", field, i)
		}
		if i > 0 && b <= buckets[i-1] {
			return fmt.Errorf("%s: buckets must be strictly increasing, but %v does not exceed %v", field, b, buckets[i-1])
		}
	}
	return nil
}

// Exposed reports whether the family should be registered and served. `family`
// is a resolved family name (with the erpc_ prefix for eRPC metrics). A nil
// policy exposes everything.
func (p *MetricPolicy) Exposed(family string) bool {
	exposed := true
	if p == nil {
		return exposed
	}
	for _, r := range p.rules {
		if r.action == "" || !r.subject.matches(family) {
			continue
		}
		exposed = r.action == ActionKeep
	}
	return exposed
}

// labelIndices returns the positions of `schema` the policy retains for this
// family. Rules are consulted broadest first, so a more specific subject's label
// rules override a broader one's.
func (p *MetricPolicy) labelIndices(family string, kind metricKind, schema []string) []int {
	out := make([]int, 0, len(schema))
	for i, label := range schema {
		if p.keepsLabel(family, kind, label) {
			out = append(out, i)
		}
	}
	return out
}

func (p *MetricPolicy) keepsLabel(family string, kind metricKind, label string) bool {
	keep := true
	if p == nil {
		return keep
	}
	for _, r := range p.rules {
		if r.kinds&kind == 0 || !r.subject.matches(family) {
			continue
		}
		for _, lr := range r.labels {
			if lr.subject.matches(label) {
				keep = lr.keep
			}
		}
	}
	return keep
}

// BucketsFor returns the bucket boundaries an operator configured for this
// family, and whether any rule set them. An explicit override wins over both
// metrics.histogramBuckets and the buckets the definition declares in code.
func (p *MetricPolicy) BucketsFor(family string) ([]float64, bool) {
	var buckets []float64
	if p == nil {
		return nil, false
	}
	for _, r := range p.rules {
		if len(r.buckets) == 0 || !r.subject.matches(family) {
			continue
		}
		buckets = r.buckets
	}
	return buckets, buckets != nil
}

// hasLabelRules reports whether any rule tries to project this family's labels.
// Used to warn about rules aimed at a family that has no label projection.
func (p *MetricPolicy) hasLabelRules(family string) bool {
	if p == nil {
		return false
	}
	for _, r := range p.rules {
		if len(r.labels) > 0 && !r.legacy && r.subject.matches(family) {
			return true
		}
	}
	return false
}

// namesExactly reports whether an operator singled this family out by full name
// rather than sweeping it up in a prefix. Warnings about rules that cannot be
// honored are limited to those: a broad subject is expected to cover families
// that do not support every customization.
func (p *MetricPolicy) namesExactly(family string) bool {
	if p == nil {
		return false
	}
	for _, r := range p.rules {
		if !r.legacy && !r.subject.prefix && r.subject.match == family {
			return true
		}
	}
	return false
}

// Active reports whether the policy carries any rule, i.e. whether the metrics
// config customizes anything at all.
func (p *MetricPolicy) Active() bool {
	return p != nil && len(p.rules) > 0
}

// filtersExposure reports whether any rule can keep a family off /metrics,
// which is the only reason the scrape path needs a filtering gatherer.
func (p *MetricPolicy) filtersExposure() bool {
	return p != nil && p.exposure
}

// UnmatchedSubjects returns the configured subjects that match none of
// knownFamilies, verbatim as written. A subject that matches nothing does
// nothing, so a typo is otherwise invisible — the caller turns this into a
// startup warning.
//
// Subjects naming a stock collector are skipped: those families are registered
// outside the manager, so they never appear in knownFamilies and warning about
// them would be a false alarm. So are the desugared legacy knobs, which have
// never warned and are validated by their own fields.
func (p *MetricPolicy) UnmatchedSubjects(knownFamilies []string) []string {
	if p == nil {
		return nil
	}
	var unmatched []string
	for _, r := range p.rules {
		if r.legacy || isStockName(r.subject.match) {
			continue
		}
		if !matchesAny(r.subject, knownFamilies) {
			unmatched = append(unmatched, r.subject.raw)
		}
	}
	return unmatched
}

func matchesAny(p pattern, families []string) bool {
	for _, name := range families {
		if p.matches(name) {
			return true
		}
	}
	return false
}

func isStockName(name string) bool {
	for _, ns := range stockNamespaces {
		if strings.HasPrefix(name, ns) {
			return true
		}
	}
	return false
}
