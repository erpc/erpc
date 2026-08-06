package evm

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"

	"github.com/stretchr/testify/assert"
)

// The whole point of the disproved streak is to separate two things that both
// look like "this upstream has proven nothing":
//
//	cannot be probed  -> unproven -> keeps serving on its claimed head
//	answers wrongly   -> DISPROVEN -> diverted, if someone else can serve
//
// Collapsing them is what let a node that returned pin_ignored on 202 of 202
// shadow probes keep answering state calls.
func TestDisprovedStreak(t *testing.T) {
	p := &stateProber{}

	t.Run("an upstream nobody has probed is not disproved", func(t *testing.T) {
		assert.False(t, p.disproved("never-probed"))
	})

	t.Run("a short run of mismatches is not yet evidence", func(t *testing.T) {
		for i := 0; i < stateProbeDisprovedStreak-1; i++ {
			p.noteDisproved("flaky")
		}
		assert.False(t, p.disproved("flaky"),
			"a probe landing across a reorg must not be enough to divert traffic")
	})

	t.Run("a sustained run is", func(t *testing.T) {
		p.noteDisproved("flaky")
		assert.True(t, p.disproved("flaky"))
	})

	t.Run("one good probe clears it", func(t *testing.T) {
		p.clearDisproved("flaky")
		assert.False(t, p.disproved("flaky"),
			"an upstream that starts answering correctly must recover immediately")
	})

	t.Run("the streak must be consecutive, not cumulative", func(t *testing.T) {
		for i := 0; i < stateProbeDisprovedStreak*3; i++ {
			p.noteDisproved("intermittent")
			p.clearDisproved("intermittent")
		}
		assert.False(t, p.disproved("intermittent"),
			"an upstream that mismatches occasionally but recovers is unreliable, not disproven")
	})
}

// aSiblingCanServe is the guard that keeps this from repeating the Base
// failover outage: it must answer false when there is no one else, so the
// diversion never removes the last upstream able to serve a height.
func TestASiblingCanServeRefusesWithoutAnEnumerableNetwork(t *testing.T) {
	p := &stateProber{}
	assert.False(t, p.aSiblingCanServe(context.Background(), "u1", 100),
		"with no way to enumerate siblings the answer must be 'no alternative', which keeps the upstream serving")
}

// siblingNetwork is the minimal shape aSiblingCanServe needs: a network that
// can enumerate its upstreams.
type siblingNetwork struct {
	common.Network
	ups []common.Upstream
}

func (s *siblingNetwork) EvmAllUpstreams(ctx context.Context) []common.Upstream { return s.ups }

func provenUp(id string, proven int64) common.Upstream {
	u := common.NewFakeUpstream(id)
	if w, ok := u.(common.EvmStateProvenWriter); ok && proven > 0 {
		w.EvmSetStateProvenBlock(proven)
	}
	return u
}

// The guard has two tiers because insisting on PROOF at the exact height would
// leave the newest blocks permanently unprotected: the proven head lags the
// followed tip, while the defect this exists for is present at every depth
// (the real upstream ignored the pin identically at the tip and 5,000 blocks
// back). A sibling with no adverse evidence is still strictly better than one
// proven to answer at the wrong height.
func TestASiblingCanServeTiers(t *testing.T) {
	ctx := context.Background()
	const block = int64(1000)

	t.Run("a sibling proven at the height qualifies", func(t *testing.T) {
		p := &stateProber{network: &siblingNetwork{ups: []common.Upstream{
			provenUp("bad", 0), provenUp("good", 1200),
		}}}
		assert.True(t, p.aSiblingCanServe(ctx, "bad", block))
	})

	t.Run("an unproven but unincriminated sibling still qualifies", func(t *testing.T) {
		p := &stateProber{network: &siblingNetwork{ups: []common.Upstream{
			provenUp("bad", 0), provenUp("warming", 0),
		}}}
		assert.True(t, p.aSiblingCanServe(ctx, "bad", block),
			"otherwise the last ~15 blocks are never protected, which is where most state traffic lives")
	})

	t.Run("a DISPROVED sibling does not qualify", func(t *testing.T) {
		p := &stateProber{network: &siblingNetwork{ups: []common.Upstream{
			provenUp("bad", 0), provenUp("alsoBad", 0),
		}}}
		for i := 0; i < stateProbeDisprovedStreak; i++ {
			p.noteDisproved("alsoBad")
		}
		assert.False(t, p.aSiblingCanServe(ctx, "bad", block),
			"diverting onto another liar is not a correction")
	})

	t.Run("the upstream itself never counts as its own alternative", func(t *testing.T) {
		p := &stateProber{network: &siblingNetwork{ups: []common.Upstream{provenUp("bad", 5000)}}}
		assert.False(t, p.aSiblingCanServe(ctx, "bad", block),
			"a lone upstream must keep serving — excluding the last resort trades wrong data for an outage")
	})
}
