package evm

import (
	"testing"

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

// aSiblingProves is the guard that keeps this from repeating the Base failover
// outage: it must answer false when there is no one else, so the diversion
// never removes the last upstream able to serve a height.
func TestASiblingProvesRefusesWithoutAnEnumerableNetwork(t *testing.T) {
	p := &stateProber{}
	assert.False(t, p.aSiblingProves("u1", 100),
		"with no way to enumerate siblings the answer must be 'no alternative', which keeps the upstream serving")
}
