package svm

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
)

// pollerAtSlot is a minimal SvmStatePoller used for slot-lag filter tests.
// It reports a fixed finalized slot and stubs out everything else.
type pollerAtSlot struct {
	slot int64
	null bool
}

func (p *pollerAtSlot) Bootstrap(context.Context) error   { return nil }
func (p *pollerAtSlot) IsObjectNull() bool                { return p.null }
func (p *pollerAtSlot) Poll(context.Context) error        { return nil }
func (p *pollerAtSlot) LatestSlot() int64                 { return p.slot }
func (p *pollerAtSlot) FinalizedSlot() int64              { return p.slot }
func (p *pollerAtSlot) MaxShredInsertSlotLag() int64      { return 0 }
func (p *pollerAtSlot) IsHealthy() bool                   { return true }
func (p *pollerAtSlot) SuggestLatestSlot(int64)           {}
func (p *pollerAtSlot) SuggestFinalizedSlot(int64)        {}
func (p *pollerAtSlot) SetDebounceInterval(time.Duration) {}

// slotLagUpstream is a per-id SvmUpstream stub — svmUpstreamStub in
// hooks_test.go has a fixed id so it can't be reused for multi-upstream
// lag-filter tests.
type slotLagUpstream struct {
	id     string
	poller common.SvmStatePoller
}

func (s *slotLagUpstream) Id() string           { return s.id }
func (s *slotLagUpstream) VendorName() string   { return "" }
func (s *slotLagUpstream) NetworkId() string    { return "svm:mainnet-beta" }
func (s *slotLagUpstream) NetworkLabel() string { return "" }
func (s *slotLagUpstream) Config() *common.UpstreamConfig {
	return &common.UpstreamConfig{Id: s.id, Type: common.UpstreamTypeSvm}
}
func (s *slotLagUpstream) Logger() *zerolog.Logger       { l := zerolog.Nop(); return &l }
func (s *slotLagUpstream) Vendor() common.Vendor         { return nil }
func (s *slotLagUpstream) Tracker() common.HealthTracker { return nil }
func (s *slotLagUpstream) Forward(context.Context, *common.NormalizedRequest, bool, bool) (*common.NormalizedResponse, error) {
	return nil, nil
}
func (s *slotLagUpstream) ShouldHandleMethod(string) (bool, error) { return true, nil }
func (s *slotLagUpstream) Cordon(string, string)                   {}
func (s *slotLagUpstream) Uncordon(string, string)                 {}
func (s *slotLagUpstream) IgnoreMethod(string)                     {}
func (s *slotLagUpstream) SvmStatePoller() common.SvmStatePoller   { return s.poller }

func upstreamAt(id string, slot int64) common.Upstream {
	return &slotLagUpstream{id: id, poller: &pollerAtSlot{slot: slot}}
}

func TestFilterByFinalizedSlotLag_ExcludesStaleUpstreams(t *testing.T) {
	t.Parallel()
	ups := []common.Upstream{
		upstreamAt("current", 1000),
		upstreamAt("stale", 800), // 200 slots behind → past maxLag=100
		upstreamAt("edge", 900),  // exactly at the lag limit → included
	}

	got := FilterByFinalizedSlotLag(ups, 100, 1000)

	gotIds := idsOf(got)
	if len(gotIds) != 2 {
		t.Fatalf("expected 2 upstreams (current + edge), got %v", gotIds)
	}
	if contains(gotIds, "stale") {
		t.Fatalf("stale upstream (slot 800, 200 behind) must be filtered out, got %v", gotIds)
	}
}

func TestFilterByFinalizedSlotLag_FallsBackWhenAllStale(t *testing.T) {
	t.Parallel()
	ups := []common.Upstream{
		upstreamAt("a", 100),
		upstreamAt("b", 200),
	}
	// Every upstream is >500 slots behind the reference. If we excluded them all
	// the request would deadlock — the filter's defensive fallback must return
	// the original list so the failsafe consensus layer decides what to do.
	got := FilterByFinalizedSlotLag(ups, 100, 1000)

	if len(got) != len(ups) {
		t.Fatalf("all-stale → pass-through expected, got %d of %d", len(got), len(ups))
	}
}

func TestFilterByFinalizedSlotLag_DisabledWhenMaxLagZero(t *testing.T) {
	t.Parallel()
	ups := []common.Upstream{
		upstreamAt("a", 500),
		upstreamAt("b", 1000),
	}
	got := FilterByFinalizedSlotLag(ups, 0, 1000)
	if len(got) != 2 {
		t.Fatalf("maxLag=0 disables filtering, got %d", len(got))
	}
}

func TestFilterByFinalizedSlotLag_IncludesUnreadyPollers(t *testing.T) {
	t.Parallel()
	// Upstream whose state poller hasn't yet received a slot (returns 0) must
	// pass through — excluding new upstreams would brick early bootstrap.
	ups := []common.Upstream{
		upstreamAt("warming-up", 0),
		upstreamAt("ready", 1000),
	}
	got := FilterByFinalizedSlotLag(ups, 100, 1000)
	if len(got) != 2 {
		t.Fatalf("warming-up upstream must not be filtered; got %d", len(got))
	}
}

func TestHighestFinalizedSlot_PicksMaxAcrossUpstreams(t *testing.T) {
	t.Parallel()
	ups := []common.Upstream{
		upstreamAt("a", 500),
		upstreamAt("b", 1200),
		upstreamAt("c", 900),
	}
	if got := HighestFinalizedSlot(ups); got != 1200 {
		t.Fatalf("expected 1200, got %d", got)
	}
}

func TestHighestFinalizedSlot_ZeroWhenNoSvmUpstreams(t *testing.T) {
	t.Parallel()
	// stubSvm (from error_normalizer_test.go) implements common.Upstream but
	// not common.SvmUpstream — no SvmStatePoller method.
	nonSvm := newSvmStub()
	if got := HighestFinalizedSlot([]common.Upstream{nonSvm}); got != 0 {
		t.Fatalf("non-svm upstreams must contribute 0, got %d", got)
	}
}

// ---- helpers ---------------------------------------------------------------

func idsOf(ups []common.Upstream) []string {
	ids := make([]string, len(ups))
	for i, u := range ups {
		ids[i] = u.Id()
	}
	return ids
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
