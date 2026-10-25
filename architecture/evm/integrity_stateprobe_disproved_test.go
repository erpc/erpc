package evm

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/architecture/evm/integrity"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fleetNetwork is the minimal network shape the prober needs in tests: it can
// enumerate its upstreams, answer the metric-label lookups, expose a config
// (for the observeOnly gate), and forward probe requests to a scripted handler.
type fleetNetwork struct {
	common.Network
	ups     []common.Upstream
	cfg     *common.NetworkConfig
	latest  int64 // the network-wide Head's Latest
	forward func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error)
}

func (s *fleetNetwork) EvmAllUpstreams(ctx context.Context) []common.Upstream { return s.ups }
func (s *fleetNetwork) ProjectId() string                                     { return "test" }
func (s *fleetNetwork) Label() string                                         { return "testnet" }
func (s *fleetNetwork) Config() *common.NetworkConfig                         { return s.cfg }
func (s *fleetNetwork) Forward(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	return s.forward(ctx, req)
}
func (s *fleetNetwork) EvmHead(context.Context, *common.NormalizedRequest) common.EvmHead {
	return common.EvmHead{Latest: s.latest}
}
func (s *fleetNetwork) EvmLaneHead(string, string) common.EvmHead {
	return common.EvmHead{Latest: s.latest}
}
func (s *fleetNetwork) EvmRefreshHead(context.Context, bool) {}

func fakeTrackerOf(t *testing.T, u common.Upstream) *common.FakeHealthTracker {
	t.Helper()
	ht, ok := u.Tracker().(*common.FakeHealthTracker)
	require.True(t, ok)
	return ht
}

// Probe disproof is the FOURTH witness to the existing misbehavior ledger
// (consensus disputes, integrity deterministic rejects, wrong-empty responses
// are the other three). The streak is the INTERNAL debounce between one
// unlucky probe and real evidence:
//
//	cannot be probed  -> unproven  -> no evidence, records nothing, ever
//	answers wrongly,  -> DISPROVED -> each further mismatch is one recorded
//	  sustainedly                     misbehavior, for selection policies
//	                                  (misbehaviorRateAbove) to act on
//
// Collapsing those two is what let a node that returned pin_ignored on 202 of
// 202 shadow probes keep a clean score while answering state calls wrongly.
func TestDisprovedStreak_RecordsMisbehavior(t *testing.T) {
	p := &stateProber{network: &fleetNetwork{}}

	t.Run("below the threshold nothing is recorded", func(t *testing.T) {
		u := common.NewFakeUpstream("flaky")
		for i := 0; i < stateProbeDisprovedStreak-1; i++ {
			p.noteDisproved(u)
		}
		assert.Equal(t, 0, fakeTrackerOf(t, u).MisbehaviorCount,
			"a probe landing across a reorg must not incriminate an upstream")
	})

	t.Run("crossing the threshold records, and keeps recording per mismatch", func(t *testing.T) {
		u := common.NewFakeUpstream("wrong-height")
		for i := 0; i < stateProbeDisprovedStreak; i++ {
			p.noteDisproved(u)
		}
		ht := fakeTrackerOf(t, u)
		assert.Equal(t, 1, ht.MisbehaviorCount)
		p.noteDisproved(u)
		p.noteDisproved(u)
		assert.Equal(t, 3, ht.MisbehaviorCount,
			"evidence must keep accruing while the node keeps failing probes, or the rolling rate decays while the defect persists")
		assert.Equal(t, "eth_call", ht.LastMisbehaviorMethod,
			"the context probe IS a pinned eth_call; that is the traffic a wrong-height execution context corrupts")
		assert.Equal(t, common.DataFinalityStateUnfinalized, ht.LastMisbehaviorFinality,
			"probes pin the followed (not yet finalized) head")
	})

	t.Run("one good probe resets the debounce", func(t *testing.T) {
		u := common.NewFakeUpstream("recovers")
		for i := 0; i < stateProbeDisprovedStreak+2; i++ {
			p.noteDisproved(u)
		}
		ht := fakeTrackerOf(t, u)
		require.Equal(t, 3, ht.MisbehaviorCount)
		p.clearDisproved(u.Id())
		for i := 0; i < stateProbeDisprovedStreak-1; i++ {
			p.noteDisproved(u)
		}
		assert.Equal(t, 3, ht.MisbehaviorCount,
			"after recovery the full debounce applies again before anything is recorded")
	})

	t.Run("the streak must be consecutive, not cumulative", func(t *testing.T) {
		u := common.NewFakeUpstream("intermittent")
		for i := 0; i < stateProbeDisprovedStreak*3; i++ {
			p.noteDisproved(u)
			p.clearDisproved(u.Id())
		}
		assert.Equal(t, 0, fakeTrackerOf(t, u).MisbehaviorCount,
			"an upstream that mismatches occasionally but recovers is unreliable, not disproven")
	})
}

// Under integrity observeOnly, rejections never reach misbehavior scoring (they
// are suppressed before they happen), so probe disproof must stay consistent
// and record nothing either — observeOnly is documented ABSOLUTE over every
// integrity effect.
func TestDisprovedStreak_ObserveOnlyRecordsNothing(t *testing.T) {
	p := &stateProber{network: &fleetNetwork{cfg: &common.NetworkConfig{
		Integrity: &common.IntegrityConfig{IntegritySettings: common.IntegritySettings{ObserveOnly: util.BoolPtr(true)}},
	}}}
	u := common.NewFakeUpstream("observed")
	for i := 0; i < stateProbeDisprovedStreak*2; i++ {
		p.noteDisproved(u)
	}
	assert.Equal(t, 0, fakeTrackerOf(t, u).MisbehaviorCount,
		"an observeOnly network must never see an integrity-driven scoring effect")
}

// A WRONG canary for a chain must degrade symmetrically. The measured
// precedent: on a Nitro chain block.number is the L1 height, so before the
// per-architecture override existed the standard Multicall3 probe mismatched
// on every honest upstream at once — the all-upstream signature that means the
// probe is wrong, not the nodes. This test pins the safety property that makes
// that mistake survivable: when every node answers the canary with the same
// wrong height, every upstream accrues IDENTICAL evidence (and no proven
// head), so the fleet's RELATIVE ranking is unchanged and a rate-based policy
// has nothing to reorder.
func TestStateProber_WrongCanaryFailsSymmetrically(t *testing.T) {
	const head = int64(1000)
	ups := []common.Upstream{
		common.NewFakeUpstream("upstream-a"),
		common.NewFakeUpstream("upstream-b"),
		common.NewFakeUpstream("upstream-c"),
	}
	net := &fleetNetwork{ups: ups, cfg: &common.NetworkConfig{}}
	net.forward = func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		jrq, _ := req.JsonRpcRequest()
		switch jrq.Method {
		case "eth_call":
			// Every node answers the same height that is NOT the pin — the
			// signature of a canary that is wrong for the chain, not of any
			// one node being wrong.
			ret := fmt.Sprintf(`"0x%064x"`, head+12345)
			return common.NewNormalizedResponse().WithJsonRpcResponse(
				common.MustNewJsonRpcResponseFromBytes([]byte(`"1"`), []byte(ret), nil)), nil
		}
		return nil, fmt.Errorf("method not found")
	}

	v := newChainTracker(context.Background(), net, 32, 0, "", "", "", nil, newFetchScheduler(context.Background(), net))
	voteChain(t, v, "a", "e", uint64(head), uint64(head), testHash("f", uint64(head-1)))
	p := &stateProber{
		network: net, view: v, interval: 0,
		ctxProbe:  integrity.ChainStateContextProbe(0),
		lastProbe: map[string]time.Time{}, work: make(chan int64, 1),
	}

	rounds := stateProbeDisprovedStreak + 2
	for i := 0; i < rounds; i++ {
		p.probeAll(context.Background(), head)
	}

	want := rounds - stateProbeDisprovedStreak + 1 // one record per round past the debounce
	for _, u := range ups {
		assert.Equal(t, want, fakeTrackerOf(t, u).MisbehaviorCount,
			"%s: symmetric failure must produce identical evidence on every upstream", u.Id())
		assert.EqualValues(t, 0, u.(*common.FakeUpstream).EvmStateProvenBlock(),
			"%s: nothing proves under a wrong canary", u.Id())
	}
}

// A vendor can answer every pinned call right and still resolve "latest" to old
// state (HyperEVM, 2026-10: one vendor froze 31,733 blocks back, another ran
// ~110 back and then split, 9 of 20 answers 9-15 blocks back, all with a
// current head). Where clients' "latest" eth_calls reach upstreams as the tag,
// the latest probe builds each upstream's tag-state head against the
// network's Latest: a stale answer makes it stale at once (a split fleet
// included), a healthy sibling within W of Latest stays fresh, and 3 fresh
// samples or a W without stale evidence readmit. No misbehavior is recorded:
// the gate acts on the state. Where eRPC sends a number instead (the default
// translateLatestTag, or the upstream's pinLatestTag), the tag is never
// probed.
func TestStateProber_StaleLatestTag(t *testing.T) {
	const head = int64(1000)
	rounds := stateProbeDisprovedStreak + 2
	floor := tagStateProbeFloor
	tagStateProbeFloor = 0
	t.Cleanup(func() { tagStateProbeFloor = floor })
	gauge := func(id string) float64 {
		return testutil.ToFloat64(telemetry.MetricUpstreamTagStateStale.WithLabelValues("test", "testnet", id))
	}
	run := func(t *testing.T, methods *common.MethodsConfig) (p *stateProber, ups map[string]*common.FakeUpstream, latestCalls map[string]int) {
		t.Helper()
		ups = map[string]*common.FakeUpstream{}
		var list []common.Upstream
		for _, id := range []string{"stale", "split", "healthy", "pinned-stale"} {
			u := common.NewFakeUpstream(id, common.WithEvmStatePoller(common.NewFakeEvmStatePoller(head, head-10))).(*common.FakeUpstream)
			ups[id] = u
			list = append(list, u)
		}
		ups["pinned-stale"].Config().Evm = &common.EvmUpstreamConfig{PinLatestTag: util.BoolPtr(true)}
		latestCalls = map[string]int{}
		var mu sync.Mutex
		net := &fleetNetwork{ups: list, latest: head, cfg: &common.NetworkConfig{Methods: methods}}
		net.forward = func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			jrq, _ := req.JsonRpcRequest()
			if jrq.Method != "eth_call" || jrq.Params[1] != "latest" || !req.Directives().SkipInterpolation {
				return nil, fmt.Errorf("unexpected %s %v", jrq.Method, jrq.Params)
			}
			id := req.Directives().UseUpstream
			mu.Lock()
			latestCalls[id]++
			odd := latestCalls[id]%2 == 1
			mu.Unlock()
			var executed int64
			switch {
			case id == "stale" || id == "pinned-stale":
				executed = head - 110
			case id == "split" && !odd:
				executed = head - 10
			case id == "healthy" && !odd:
				executed = head - 2 // within W of Latest at a 1s block time
			default:
				executed = head + 1
			}
			ret := fmt.Sprintf(`"0x%064x"`, executed)
			return common.NewNormalizedResponse().WithJsonRpcResponse(
				common.MustNewJsonRpcResponseFromBytes([]byte(`"1"`), []byte(ret), nil)), nil
		}
		v := newChainTracker(t.Context(), net, 32, 0, "", "", "", nil, newFetchScheduler(t.Context(), net))
		p = &stateProber{
			network: net, view: v,
			ctxProbe:  integrity.ChainStateContextProbe(999),
			lastProbe: map[string]time.Time{}, work: make(chan int64, 1),
		}
		for range rounds {
			p.latestChanged()
			p.probeTagStates(t.Context())
			require.Eventually(t, func() bool {
				p.tagMu.Lock()
				defer p.tagMu.Unlock()
				return len(p.tagInflight) == 0
			}, 5*time.Second, time.Millisecond)
		}
		return p, ups, latestCalls
	}

	t.Run("latest reaches vendors as the tag: stale state, never misbehavior", func(t *testing.T) {
		p, ups, latestCalls := run(t, &common.MethodsConfig{Definitions: map[string]*common.CacheMethodConfig{
			"eth_call": {TranslateLatestTag: util.BoolPtr(false)},
		}})
		now := time.Now()
		for id, want := range map[string]tagState{"stale": tagStale, "split": tagStale, "healthy": tagFresh, "pinned-stale": tagNoEvidence} {
			assert.Equal(t, want, p.tagHead(id, now).state, id)
			assert.Equal(t, 0, fakeTrackerOf(t, ups[id]).MisbehaviorCount, "%s: a stale latest is gated, not scored", id)
		}
		assert.Equal(t, rounds, latestCalls["stale"])
		assert.Equal(t, 1.0, gauge("stale"))
		assert.Equal(t, 1.0, gauge("split"), "a fleet half stale is stale: each stale answer resets its fresh streak")
		assert.Equal(t, 0.0, gauge("healthy"))
		assert.Equal(t, 0, latestCalls["pinned-stale"], "a pinLatestTag upstream never receives \"latest\"")

		for i := 1; i <= tagStateFreshStreak; i++ {
			p.recordTagSample("stale", head, head, time.Now())
			if i < tagStateFreshStreak {
				assert.Equal(t, tagStale, p.tagHead("stale", time.Now()).state, "%d fresh samples do not readmit", i)
			}
		}
		assert.Equal(t, tagFresh, p.tagHead("stale", time.Now()).state, "3 fresh samples in a row readmit")
		assert.Equal(t, 0.0, gauge("stale"))

		_, w := p.tagTiming()
		p.recordTagSample("split", head, head, time.Now())
		require.Equal(t, tagStale, p.tagHead("split", time.Now()).state)
		assert.Equal(t, tagNoEvidence, p.tagHead("split", time.Now().Add(w+time.Second)).state,
			"a W without stale evidence admits")
		assert.Equal(t, 0.0, gauge("split"))
	})

	t.Run("eRPC translates latest to a number: no latest probe at all", func(t *testing.T) {
		_, ups, latestCalls := run(t, nil)
		assert.Empty(t, latestCalls)
		for id, u := range ups {
			assert.Equal(t, 0, fakeTrackerOf(t, u).MisbehaviorCount, id)
		}
	})
}
