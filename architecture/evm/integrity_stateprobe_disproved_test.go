package evm

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/erpc/erpc/architecture/evm/integrity"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"

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
	forward func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error)
}

func (s *fleetNetwork) EvmAllUpstreams(ctx context.Context) []common.Upstream { return s.ups }
func (s *fleetNetwork) ProjectId() string                                     { return "test" }
func (s *fleetNetwork) Label() string                                         { return "testnet" }
func (s *fleetNetwork) Config() *common.NetworkConfig                         { return s.cfg }
func (s *fleetNetwork) Forward(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	return s.forward(ctx, req)
}

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

	v := newChainView(net, 32, "", "", nil)
	v.adoptFollowed(head, &integrity.Header{
		Number: fmt.Sprintf("0x%x", head), Hash: "0xhead", ParentHash: "0xpar",
	})
	p := &stateProber{
		network: net, view: v, interval: 0,
		ctxProbe:  integrity.ChainStateContextProbe(0),
		lastProbe: map[string]time.Time{}, work: make(chan int64, 1),
	}

	rounds := stateProbeDisprovedStreak + 2
	for i := 0; i < rounds; i++ {
		p.probeAll(head)
	}

	want := rounds - stateProbeDisprovedStreak + 1 // one record per round past the debounce
	for _, u := range ups {
		assert.Equal(t, want, fakeTrackerOf(t, u).MisbehaviorCount,
			"%s: symmetric failure must produce identical evidence on every upstream", u.Id())
		assert.EqualValues(t, 0, u.(*common.FakeUpstream).EvmStateProvenBlock(),
			"%s: nothing proves under a wrong canary", u.Id())
	}
}
