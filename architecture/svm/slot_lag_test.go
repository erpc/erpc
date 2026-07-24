package svm

import (
	"context"
	"reflect"
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
func (p *pollerAtSlot) ShredInsertSlot() int64            { return 0 }
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

// pollerAtSlots reports distinct latest vs finalized slots so tests can prove
// which of the two a filter compares against. Embeds pollerAtSlot: the
// embedded slot field is the finalized slot.
type pollerAtSlots struct {
	pollerAtSlot
	latest int64
}

func (p *pollerAtSlots) LatestSlot() int64 { return p.latest }

func upstreamAtSlots(id string, latest, finalized int64) common.Upstream {
	return &slotLagUpstream{id: id, poller: &pollerAtSlots{pollerAtSlot: pollerAtSlot{slot: finalized}, latest: latest}}
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

func TestReferenceFinalizedSlot(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		ups    []common.Upstream
		maxLag int64
		want   int64
	}{
		{name: "empty pool", ups: nil, maxLag: 100, want: 0},
		{name: "no reporters", ups: []common.Upstream{
			upstreamAt("zero-slot", 0),
			&slotLagUpstream{id: "no-poller"},
			&slotLagUpstream{id: "null-poller", poller: &pollerAtSlot{slot: 900, null: true}},
		}, maxLag: 100, want: 0},
		{name: "single reporter", ups: []common.Upstream{
			upstreamAt("only", 1234),
		}, maxLag: 100, want: 1234},
		{name: "tight pool uses pool max", ups: []common.Upstream{
			upstreamAt("a", 1000),
			upstreamAt("b", 1010),
			upstreamAt("c", 1050),
		}, maxLag: 100, want: 1050},
		{name: "single liar clamps to runner-up", ups: []common.Upstream{
			upstreamAt("honest-low", 1000),
			upstreamAt("honest-high", 1010),
			upstreamAt("liar", 50000),
		}, maxLag: 100, want: 1010},
		{name: "maxLag zero returns raw max", ups: []common.Upstream{
			upstreamAt("a", 1000),
			upstreamAt("liar", 50000),
		}, maxLag: 0, want: 50000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ReferenceFinalizedSlot(tc.ups, tc.maxLag); got != tc.want {
				t.Fatalf("ReferenceFinalizedSlot = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestReferenceFinalizedSlot_HonestPoolSurvivesLiar(t *testing.T) {
	t.Parallel()
	// End-to-end composition: a single upstream lying about its finalized slot
	// must not become the reference and shrink the pool to itself. The clamped
	// reference keeps every honest upstream in, and the liar stays too — the
	// lag filter only drops trailers, and being ahead of the reference is fine.
	ups := []common.Upstream{
		upstreamAt("honest-low", 1000),
		upstreamAt("honest-high", 1010),
		upstreamAt("liar", 50000),
	}
	ref := ReferenceFinalizedSlot(ups, 100)
	if ref != 1010 {
		t.Fatalf("reference must clamp to runner-up 1010, got %d", ref)
	}
	got := idsOf(FilterByFinalizedSlotLag(ups, 100, ref))
	want := []string{"honest-low", "honest-high", "liar"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("full pool must survive a single liar, got %v", got)
	}
}

func TestFilterByMinContextSlot(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		ups       []common.Upstream
		mcs       int64
		finalized bool
		want      []string
	}{
		{name: "mcs zero passes everything through", ups: []common.Upstream{
			upstreamAt("behind", 500),
			upstreamAt("ahead", 2000),
		}, mcs: 0, want: []string{"behind", "ahead"}},
		{name: "empty list passes through", ups: []common.Upstream{},
			mcs: 1000, want: []string{}},
		{name: "unknown state included, known-behind dropped", ups: []common.Upstream{
			&slotLagUpstream{id: "no-poller"},
			&slotLagUpstream{id: "null-poller", poller: &pollerAtSlot{slot: 500, null: true}},
			upstreamAt("zero-slot", 0),
			upstreamAt("behind", 500),
			upstreamAt("ahead", 2000),
		}, mcs: 1000, want: []string{"no-poller", "null-poller", "zero-slot", "ahead"}},
		{name: "finalized=false compares latest slot", ups: []common.Upstream{
			upstreamAtSlots("diverged", 1500, 900), // latest ahead, finalized behind
			upstreamAt("anchor", 2000),
		}, mcs: 1000, finalized: false, want: []string{"diverged", "anchor"}},
		{name: "finalized=true compares finalized slot", ups: []common.Upstream{
			upstreamAtSlots("diverged", 1500, 900),
			upstreamAt("anchor", 2000),
		}, mcs: 1000, finalized: true, want: []string{"anchor"}},
		{name: "all behind falls back to original list", ups: []common.Upstream{
			upstreamAt("a", 500),
			upstreamAt("b", 700),
		}, mcs: 1000, want: []string{"a", "b"}},
		{name: "mixed keeps only at-or-ahead", ups: []common.Upstream{
			upstreamAt("just-behind", 999),
			upstreamAt("exactly-at", 1000),
			upstreamAt("ahead", 1001),
		}, mcs: 1000, want: []string{"exactly-at", "ahead"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := idsOf(FilterByMinContextSlot(tc.ups, tc.mcs, tc.finalized))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("FilterByMinContextSlot = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMinContextSlotOf(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
		want int64
	}{
		{name: "number in options object",
			body: `{"jsonrpc":"2.0","id":1,"method":"getAccountInfo","params":["pubkey",{"minContextSlot":123}]}`,
			want: 123},
		{name: "string form",
			body: `{"jsonrpc":"2.0","id":1,"method":"getAccountInfo","params":["pubkey",{"minContextSlot":"123"}]}`,
			want: 123},
		{name: "absent",
			body: `{"jsonrpc":"2.0","id":1,"method":"getAccountInfo","params":["pubkey",{"commitment":"finalized"}]}`,
			want: 0},
		{name: "non-object params only",
			body: `{"jsonrpc":"2.0","id":1,"method":"getSlot","params":["pubkey","confirmed"]}`,
			want: 0},
		{name: "zero",
			body: `{"jsonrpc":"2.0","id":1,"method":"getAccountInfo","params":["pubkey",{"minContextSlot":0}]}`,
			want: 0},
		{name: "negative",
			body: `{"jsonrpc":"2.0","id":1,"method":"getAccountInfo","params":["pubkey",{"minContextSlot":-5}]}`,
			want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := common.NewNormalizedRequest([]byte(tc.body))
			if got := MinContextSlotOf(context.Background(), req); got != tc.want {
				t.Fatalf("MinContextSlotOf = %d, want %d", got, tc.want)
			}
		})
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
