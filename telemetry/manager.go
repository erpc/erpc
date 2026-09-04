package telemetry

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// ErrNothingRegistered marks a Configure failure that stopped registration
// before it began, separating the two very different outcomes Configure reports
// through one error: an unusable exposeMetrics/dropMetrics list leaves the
// process serving no eRPC metrics at all, while a malformed histogramBuckets
// value only substitutes the default buckets and registers everything. Callers
// that log the error use this to pick a severity.
var ErrNothingRegistered = errors.New("no metric families were registered")

// This file is the single place metric families are declared and registered.
//
// Every metric in this package is *defined* at package init and *registered*
// later, when Configure runs with the resolved metrics config. The split is
// forced by Prometheus: a registry freezes a family's label-set hash the first
// time it is registered and keeps it for the registry's lifetime (dimHashesByName
// survives Unregister), so anything that changes a label set — histogramDropLabels,
// counterDropLabels — has to be applied before the first registration. Exposure
// control needs the same window for a different reason: the only way to keep a
// family off /metrics entirely is to never register it.
//
// Practically that means: define with Define*, use the returned pointer freely
// from any call site, and let Configure decide what actually reaches the registry.

// Options is the resolved metrics configuration the manager needs. It mirrors the
// metrics section of the eRPC config; telemetry cannot import common (common
// imports telemetry), so erpc.Init maps one onto the other.
type Options struct {
	// HistogramBuckets is the comma-separated bucket list applied to every
	// histogram that does not declare its own. An unparseable value is reported
	// as an error and the defaults are used.
	HistogramBuckets string

	HistogramDropLabels     []string
	HistogramLabelOverrides map[string][]string
	CounterDropLabels       []string
	CounterLabelOverrides   map[string][]string

	// CounterIdleEvictionAfter overrides how long an idle counter handle is kept
	// before its series is released. nil leaves the default in place.
	CounterIdleEvictionAfter *time.Duration

	// ExposeMetrics and DropMetrics select which families are registered at all.
	// See MetricExposureFilter.
	ExposeMetrics []string
	DropMetrics   []string
}

// definition is one metric family the manager owns.
type definition struct {
	family    string
	collector prometheus.Collector

	// rebuild re-creates the underlying Vec under the current label filter and
	// resolved buckets. nil when no config can change the label set — plain
	// counters and gauges, and the histograms that declare their own buckets and
	// carry no filterable schema.
	rebuild func(buckets []float64)

	// histogram marks the families SetHistogramBuckets applies to.
	histogram bool

	// registeredWith is the registerer this family was registered with, nil until
	// then. Its label set is frozen from that point, so a later Configure leaves
	// it alone. Recording the registerer rather than a bool lets tests that swap
	// in a fresh DefaultRegisterer register under it again.
	registeredWith prometheus.Registerer
}

var (
	registryMu  sync.Mutex
	definitions []*definition
	byFamily    = map[string]*definition{}

	// exposure is the filter installed by the last Configure call. nil means no
	// exposure config has been applied, which exposes everything.
	exposure *MetricExposureFilter
)

// define records a family. Called from package-var initializers, so a duplicate
// name is a programming error that should fail loudly at startup rather than
// produce a silently half-registered family.
func define(d *definition) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := byFamily[d.family]; dup {
		panic(fmt.Sprintf("telemetry: metric family %q is defined twice", d.family))
	}
	byFamily[d.family] = d
	definitions = append(definitions, d)
}

// DefineCounter declares a counter family whose label set no config can change.
// Use it for counters whose cardinality is bounded by deployment topology
// (project × network × upstream and the like). Counters carrying
// caller-controlled labels — a user id, a client-supplied agent name — belong in
// DefineLabeledCounter so counterDropLabels can reach them.
func DefineCounter(opts prometheus.CounterOpts, labels []string) *prometheus.CounterVec {
	vec := prometheus.NewCounterVec(opts, labels)
	define(&definition{family: familyName(opts.Namespace, opts.Subsystem, opts.Name), collector: vec})
	return vec
}

// DefineGauge declares a gauge family. Gauges are point-in-time values with no
// label projection: dropping a gauge label would collapse distinct series onto
// one that then reports whichever writer wrote last, which is a wrong number
// rather than a coarser one.
func DefineGauge(opts prometheus.GaugeOpts, labels []string) *prometheus.GaugeVec {
	vec := prometheus.NewGaugeVec(opts, labels)
	define(&definition{family: familyName(opts.Namespace, opts.Subsystem, opts.Name), collector: vec})
	return vec
}

// DefineHistogram declares a histogram family with fixed labels and its own
// buckets. Histograms that should follow metrics.histogramBuckets, or whose
// labels should follow histogramDropLabels, use DefineLabeledHistogram.
func DefineHistogram(opts prometheus.HistogramOpts, labels []string) *prometheus.HistogramVec {
	if len(opts.Buckets) == 0 {
		panic(fmt.Sprintf("telemetry: histogram %q declares no buckets; use DefineLabeledHistogram to take the buckets from config", opts.Name))
	}
	vec := prometheus.NewHistogramVec(opts, labels)
	define(&definition{
		family:    familyName(opts.Namespace, opts.Subsystem, opts.Name),
		collector: vec,
		histogram: true,
	})
	return vec
}

// DefineLabeledCounter declares a counter whose label set is projected through
// counterDropLabels / counterLabelOverrides. `schema` is the canonical, full
// label list; call sites always pass values for all of it, and the wrapper
// forwards only the retained positions.
func DefineLabeledCounter(opts prometheus.CounterOpts, schema []string) *LabeledCounter {
	lc := newLabeledCounterUnregistered(opts, schema)
	define(&definition{
		family:    familyName(opts.Namespace, opts.Subsystem, opts.Name),
		collector: lc,
		rebuild:   func([]float64) { lc.rebuildInPlace() },
	})
	return lc
}

// DefineLabeledHistogram declares a histogram whose label set is projected
// through histogramDropLabels / histogramLabelOverrides. Leaving opts.Buckets
// empty takes the buckets from metrics.histogramBuckets; set them explicitly
// only when the global latency buckets resolve this metric's range poorly.
func DefineLabeledHistogram(opts prometheus.HistogramOpts, schema []string) *LabeledHistogram {
	lh := NewLabeledHistogram(opts, schema)
	define(&definition{
		family:    familyName(opts.Namespace, opts.Subsystem, opts.Name),
		collector: lh,
		histogram: true,
		rebuild:   lh.rebuildInPlace,
	})
	return lh
}

func familyName(namespace, subsystem, name string) string {
	return prometheus.BuildFQName(namespace, subsystem, name)
}

// Configure applies the resolved metrics config and registers every exposed
// family with prometheus.DefaultRegisterer. Call it once, from erpc.Init, before
// the metrics server starts serving.
//
// A nil `o` means the config carries no metrics section: only histograms are
// registered, with the default buckets, exactly as before. Counters stay
// unregistered so that a later Configure carrying counterDropLabels can still
// apply them — processes that never scrape do not need those collectors on the
// default registry.
//
// Returns an error for an invalid exposeMetrics/dropMetrics entry, wrapped in
// ErrNothingRegistered because nothing is registered in that case, and for an
// unparseable histogramBuckets value, unwrapped because the defaults are applied
// and registration proceeds. The caller can surface either without failing
// startup over bucket syntax, and tell the outage apart from the typo.
func Configure(o *Options) error {
	if o == nil {
		return SetHistogramBuckets("")
	}

	filter, err := NewMetricExposureFilter(o.ExposeMetrics, o.DropMetrics)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrNothingRegistered, err)
	}
	buckets, bucketErr := ParseHistogramBuckets(o.HistogramBuckets)
	if bucketErr != nil {
		buckets = DefaultHistogramBuckets
		// The exposure errors name their field; this one is a bare strconv
		// message, and the caller logs both through the same line.
		bucketErr = fmt.Errorf("metrics.histogramBuckets: %w", bucketErr)
	}

	// The label filters must be installed before anything is rebuilt, since the
	// rebuild is what reads them.
	SetHistogramLabelFilter(o.HistogramDropLabels, o.HistogramLabelOverrides)
	SetCounterLabelFilter(o.CounterDropLabels, o.CounterLabelOverrides)
	if o.CounterIdleEvictionAfter != nil {
		SetCounterIdleEvictionAfter(*o.CounterIdleEvictionAfter)
	}

	registryMu.Lock()
	exposure = filter
	registryMu.Unlock()

	apply(buckets, false)
	return bucketErr
}

// SetHistogramBuckets resolves the bucket list and registers the histogram
// families only, leaving counters and gauges unregistered.
//
// This narrower entry point exists for two callers: config validation, which
// wants the parse error and nothing else, and test setup, which needs histograms
// scrapeable without freezing the counter label sets that TestInit_CounterDropLabels_EndToEnd
// depends on being still open. Production goes through Configure.
func SetHistogramBuckets(bucketsStr string) error {
	buckets, err := ParseHistogramBuckets(bucketsStr)
	if err != nil {
		buckets = DefaultHistogramBuckets
	}
	apply(buckets, true)
	return err
}

// apply rebuilds and registers every exposed definition that is not yet
// registered with the current DefaultRegisterer.
func apply(buckets []float64, histogramsOnly bool) {
	registryMu.Lock()
	defer registryMu.Unlock()

	reg := prometheus.DefaultRegisterer
	for _, d := range definitions {
		if histogramsOnly && !d.histogram {
			continue
		}
		// Skipping before the rebuild is what makes exposure control work: an
		// unexposed family is never registered, so it costs no series and never
		// reaches /metrics.
		if !exposure.Exposed(d.family) {
			continue
		}
		// Already registered: its label set is frozen and rebuilding it now
		// would leave the registry describing a shape it no longer collects.
		if d.registeredWith == reg {
			continue
		}
		if d.rebuild != nil {
			d.rebuild(buckets)
		}
		register(reg, d.collector)
		d.registeredWith = reg
	}

	// The rebuilt Vecs are new objects; cached child handles point at the old
	// ones and would otherwise increment series that are no longer collected.
	ResetHandleCache()
}

// register adds the collector, tolerating a family that is already present (a
// repeat Configure, or a test that registered its own). Any other error —
// notably a label-set mismatch, which means a filter changed after the first
// registration — panics, because a process running with a metric it believes is
// registered and is not is worse than one that fails at startup.
func register(reg prometheus.Registerer, c prometheus.Collector) {
	if err := reg.Register(c); err != nil {
		if _, ok := err.(prometheus.AlreadyRegisteredError); ok {
			return
		}
		panic(err)
	}
}

// KnownFamilies returns every family name this package defines, sorted. Names
// are fully qualified (with the erpc_ prefix) and independent of what is
// currently registered.
func KnownFamilies() []string {
	registryMu.Lock()
	defer registryMu.Unlock()
	out := make([]string, 0, len(definitions))
	for _, d := range definitions {
		out = append(out, d.family)
	}
	sort.Strings(out)
	return out
}

// ExposedFamilyCount reports how many known families the installed exposure
// filter keeps, out of how many exist, for a startup log line.
func ExposedFamilyCount() (exposed, total int) {
	registryMu.Lock()
	defer registryMu.Unlock()
	for _, d := range definitions {
		if exposure.Exposed(d.family) {
			exposed++
		}
	}
	return exposed, len(definitions)
}

// UnmatchedExposureEntries returns the configured exposeMetrics/dropMetrics
// entries that match no known family, verbatim as written. An entry that matches
// nothing does nothing, so a typo is otherwise invisible — the caller turns this
// into a startup warning.
func UnmatchedExposureEntries() []string {
	registryMu.Lock()
	f := exposure
	registryMu.Unlock()
	return UnmatchedEntries(f, KnownFamilies())
}

// Gatherer wraps g so the exposure filter also applies to families the manager
// does not own — the stock go_/process_/promhttp_ collectors the default registry
// installs. eRPC's own unexposed families are already absent, having never been
// registered. Returns g unchanged when no exposure list is configured, so the
// scrape path is untouched in the default case.
func Gatherer(g prometheus.Gatherer) prometheus.Gatherer {
	registryMu.Lock()
	f := exposure
	registryMu.Unlock()
	if !f.Active() {
		return g
	}
	return NewFilteredGatherer(g, f)
}
