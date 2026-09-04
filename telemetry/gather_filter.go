package telemetry

import (
	"fmt"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// metricNamespace is the prefix every metric family eRPC defines carries. Config
// entries omit it — the same convention histogramLabelOverrides /
// counterLabelOverrides already use for metric names.
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

// exposureEntry is one configured list item, resolved. `raw` is kept verbatim so
// warnings quote what the operator actually wrote rather than the normalized
// form.
type exposureEntry struct {
	raw    string
	match  string // normalized family name, or the prefix stem when prefix is true
	prefix bool
}

// MetricExposureFilter decides which metric families are exposed, from the
// operator's metrics.exposeMetrics (allowlist) and metrics.dropMetrics
// (denylist). Entries are exact family names or trailing-prefix subsystem
// patterns ("consensus_*"); the allowlist is applied first and the denylist
// wins on overlap. Both empty means expose everything.
type MetricExposureFilter struct {
	allow []exposureEntry
	drop  []exposureEntry

	// Exact names are indexed for the per-scrape FilteredGatherer path;
	// prefixes stay a slice because they need a scan either way.
	allowExact  map[string]struct{}
	allowPrefix []string
	dropExact   map[string]struct{}
	dropPrefix  []string
}

// NewMetricExposureFilter builds the filter from the two config lists. Entries
// are rejected when blank, when they use "*" anywhere but as the final
// character, or when they duplicate another entry in the same list after
// normalization — a filter that silently ignores a malformed entry would drop
// or keep families the operator did not ask for.
func NewMetricExposureFilter(expose, drop []string) (*MetricExposureFilter, error) {
	allowEntries, err := parseExposureEntries("metrics.exposeMetrics", expose)
	if err != nil {
		return nil, err
	}
	dropEntries, err := parseExposureEntries("metrics.dropMetrics", drop)
	if err != nil {
		return nil, err
	}

	f := &MetricExposureFilter{
		allow:      allowEntries,
		drop:       dropEntries,
		allowExact: make(map[string]struct{}, len(allowEntries)),
		dropExact:  make(map[string]struct{}, len(dropEntries)),
	}
	for _, e := range allowEntries {
		if e.prefix {
			f.allowPrefix = append(f.allowPrefix, e.match)
		} else {
			f.allowExact[e.match] = struct{}{}
		}
	}
	for _, e := range dropEntries {
		if e.prefix {
			f.dropPrefix = append(f.dropPrefix, e.match)
		} else {
			f.dropExact[e.match] = struct{}{}
		}
	}
	return f, nil
}

func parseExposureEntries(field string, entries []string) ([]exposureEntry, error) {
	out := make([]exposureEntry, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, raw := range entries {
		e := strings.TrimSpace(raw)
		if e == "" {
			return nil, fmt.Errorf("%s contains an empty entry", field)
		}
		if i := strings.IndexByte(e, '*'); i >= 0 && i != len(e)-1 {
			return nil, fmt.Errorf("%s entry %q: '*' is only allowed as the final character, as a subsystem prefix like \"consensus_*\"", field, e)
		}
		if e == "*" {
			return nil, fmt.Errorf("%s entry %q has an empty prefix; write \"erpc_*\" to mean every eRPC family", field, e)
		}

		entry := exposureEntry{raw: e, prefix: strings.HasSuffix(e, "*")}
		entry.match = NormalizeMetricName(strings.TrimSuffix(e, "*"))

		key := entry.match
		if entry.prefix {
			key += "*"
		}
		if _, dup := seen[key]; dup {
			return nil, fmt.Errorf("%s entry %q is a duplicate of an earlier entry", field, e)
		}
		seen[key] = struct{}{}
		out = append(out, entry)
	}
	return out, nil
}

// Exposed reports whether the family should be registered and served. `family`
// is a resolved family name (with the erpc_ prefix for eRPC metrics). A nil
// filter exposes everything.
func (f *MetricExposureFilter) Exposed(family string) bool {
	if f == nil {
		return true
	}
	if len(f.allowExact) > 0 || len(f.allowPrefix) > 0 {
		if !matchesFamily(f.allowExact, f.allowPrefix, family) {
			return false
		}
	}
	return !matchesFamily(f.dropExact, f.dropPrefix, family)
}

func matchesFamily(exact map[string]struct{}, prefixes []string, family string) bool {
	if _, ok := exact[family]; ok {
		return true
	}
	for _, p := range prefixes {
		if strings.HasPrefix(family, p) {
			return true
		}
	}
	return false
}

// Active reports whether either list carries an entry — used to skip filtering
// work, and to decide whether startup logs anything about exposure at all.
func (f *MetricExposureFilter) Active() bool {
	return f != nil && (len(f.allow) > 0 || len(f.drop) > 0)
}

// UnmatchedEntries returns the configured entries that match none of
// knownFamilies, verbatim as written, allowlist before denylist. A typo'd name
// or subsystem prefix silently does nothing, so this drives a startup warning.
//
// Entries naming a stock collector are skipped: those families are registered
// outside the manager, so they never appear in knownFamilies and warning about
// them would be a false alarm.
func UnmatchedEntries(f *MetricExposureFilter, knownFamilies []string) []string {
	if f == nil {
		return nil
	}
	var unmatched []string
	for _, e := range append(append([]exposureEntry{}, f.allow...), f.drop...) {
		if isStockName(e.match) {
			continue
		}
		if !entryMatchesAny(e, knownFamilies) {
			unmatched = append(unmatched, e.raw)
		}
	}
	return unmatched
}

func entryMatchesAny(e exposureEntry, families []string) bool {
	for _, name := range families {
		if e.prefix {
			if strings.HasPrefix(name, e.match) {
				return true
			}
			continue
		}
		if name == e.match {
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

// FilteredGatherer applies an exposure filter to gathered families. Unexposed
// eRPC families are never registered, so this exists for the collectors the
// manager does not own — the go_/process_/promhttp_ families the default
// registry installs — which an exposeMetrics allowlist should not silently keep.
// It filters after collection, so it shrinks the response but does not save
// collection cost.
type FilteredGatherer struct {
	gatherer prometheus.Gatherer
	filter   *MetricExposureFilter
}

func NewFilteredGatherer(g prometheus.Gatherer, f *MetricExposureFilter) *FilteredGatherer {
	return &FilteredGatherer{gatherer: g, filter: f}
}

func (fg *FilteredGatherer) Gather() ([]*dto.MetricFamily, error) {
	mfs, err := fg.gatherer.Gather()
	if !fg.filter.Active() {
		return mfs, err
	}
	kept := make([]*dto.MetricFamily, 0, len(mfs))
	for _, mf := range mfs {
		if fg.filter.Exposed(mf.GetName()) {
			kept = append(kept, mf)
		}
	}
	// Gather may return families alongside a partial-collection error; keep both.
	return kept, err
}
