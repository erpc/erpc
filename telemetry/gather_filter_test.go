package telemetry

import (
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

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
	fg := NewFilteredGatherer(g, mustPolicy(t,
		Customization{Subject: "consensus_*", Action: ActionDrop},
		Customization{Subject: "go_goroutines", Action: ActionDrop},
	))

	mfs, err := fg.Gather()
	if err != nil {
		t.Fatalf("Gather() failed: %v", err)
	}
	got := familyNames(mfs)
	if len(got) != 1 || got[0] != "erpc_upstream_request_total" {
		t.Errorf("Gather() returned %v, want [erpc_upstream_request_total]", got)
	}
}

// With no exposure rules the gatherer must hand back exactly what the wrapped one
// produced — same slice, no per-scrape work. A label-only customization is the
// interesting case: it is Active, but nothing about it can hide a family.
func TestFilteredGatherer_PassthroughWhenNothingIsDropped(t *testing.T) {
	families := stubFamilies("erpc_upstream_request_total", "go_goroutines")
	labelsOnly := mustPolicy(t, Customization{
		Subject: "*",
		Labels:  []LabelCustomization{{Subject: "user", Action: ActionDrop}},
	})
	for name, p := range map[string]*MetricPolicy{"empty": mustPolicy(t), "labels only": labelsOnly} {
		fg := NewFilteredGatherer(stubGatherer{families: families}, p)
		mfs, err := fg.Gather()
		if err != nil {
			t.Fatalf("%s: Gather() failed: %v", name, err)
		}
		if len(mfs) != 2 {
			t.Errorf("%s: expected both families passed through, got %v", name, familyNames(mfs))
		}
	}
}

func TestFilteredGatherer_NilPolicyPassesThrough(t *testing.T) {
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
	fg := NewFilteredGatherer(g, mustPolicy(t, Customization{Subject: "consensus_*", Action: ActionDrop}))

	mfs, err := fg.Gather()
	if !errors.Is(err, sentinel) {
		t.Errorf("expected the wrapped gatherer's error to survive, got %v", err)
	}
	if got := familyNames(mfs); len(got) != 1 || got[0] != "erpc_upstream_request_total" {
		t.Errorf("expected the exposed family alongside the error, got %v", got)
	}
}

// Sanity check against a real registry rather than a stub, so the wiring the
// metrics handler installs is known to work.
func TestFilteredGatherer_OverRealRegistry(t *testing.T) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(prometheus.NewCounter(prometheus.CounterOpts{Name: "erpc_kept_total"}))
	reg.MustRegister(prometheus.NewCounter(prometheus.CounterOpts{Name: "erpc_dropped_total"}))

	fg := NewFilteredGatherer(reg, mustPolicy(t,
		Customization{Subject: "*", Action: ActionDrop},
		Customization{Subject: "kept_total", Action: ActionKeep},
	))
	mfs, err := fg.Gather()
	if err != nil {
		t.Fatalf("Gather() failed: %v", err)
	}
	if got := familyNames(mfs); len(got) != 1 || got[0] != "erpc_kept_total" {
		t.Errorf("Gather() returned %v, want [erpc_kept_total]", got)
	}
}
