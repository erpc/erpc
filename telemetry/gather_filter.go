package telemetry

import (
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// FilteredGatherer drops unexposed families from a gather. eRPC families a
// customization drops are never registered, so this exists for the collectors
// the manager does not own — the go_/process_/promhttp_ families the default
// registry installs — which a "drop everything, keep these" policy should not
// silently keep. It filters after collection, so it shrinks the response but
// does not save collection cost.
type FilteredGatherer struct {
	gatherer prometheus.Gatherer
	policy   *MetricPolicy
}

func NewFilteredGatherer(g prometheus.Gatherer, p *MetricPolicy) *FilteredGatherer {
	return &FilteredGatherer{gatherer: g, policy: p}
}

func (fg *FilteredGatherer) Gather() ([]*dto.MetricFamily, error) {
	mfs, err := fg.gatherer.Gather()
	if !fg.policy.filtersExposure() {
		return mfs, err
	}
	kept := make([]*dto.MetricFamily, 0, len(mfs))
	for _, mf := range mfs {
		if fg.policy.Exposed(mf.GetName()) {
			kept = append(kept, mf)
		}
	}
	// Gather may return families alongside a partial-collection error; keep both.
	return kept, err
}
