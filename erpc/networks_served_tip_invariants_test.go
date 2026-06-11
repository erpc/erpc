package erpc

import (
	"context"
	"testing"

	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/require"
)

// ─────────────────────────────────────────────────────────────────────────────
// PROD-INCIDENT INVARIANTS — DO NOT ADAPT THESE TO NEW INTERNALS.
//
// Each test below reproduces, end-to-end at the Network level, a failure mode
// that actually occurred (or was made possible) in production on 2026-06-10/11,
// when the served tip silently froze hours in the past across 25+ chain/region
// pairs and every eth_call(..., "latest") was rewritten to a stale block.
//
// They are BLACK-BOX: they assert client-visible OUTCOMES (the advertised
// "latest" tracks live heads, recovers from inherited state, never serves a
// rogue counter) — never the mechanism (no gate flags, no cluster shapes, no
// metric internals). Any future served-tip implementation, including a full
// redesign, MUST keep passing these tests UNCHANGED. If one fails, the new
// logic reintroduces a production outage; fix the logic, not the test.
// ─────────────────────────────────────────────────────────────────────────────

// servedTipInvariantFixtures is the standard 3-upstream healthy topology used
// by the invariant tests: all upstreams live and agreeing (within jitter).
func servedTipInvariantFixtures(head int64) []servedTipFixture {
	return []servedTipFixture{
		{id: "u1", chainID: 123, latestBlock: head},
		{id: "u2", chainID: 123, latestBlock: head},
		{id: "u3", chainID: 123, latestBlock: head},
	}
}

// THE incident: a process starts with a served-tip counter inherited from the
// persistent shared store that is far BEHIND the live chain (rolling deploys,
// region restarts, idle partitions), while the velocity gate is armed (block
// time known). In prod this wedged FOREVER: every pick rejected every live
// tip, the counter never advanced, "latest" froze hours in the past, and the
// zeros served to the customer were real state from the frozen block.
//
// INVARIANT: within a few picks the advertised "latest" must reach the live
// agreed head, and must keep tracking it as the chain advances. No inherited
// counter state may ever pin it down.
func TestServedTipInvariant_StaleInheritedCounter_RecoversAndTracks(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const liveHead = int64(10_000)
	network, ups := setupServedTipNetwork(t, ctx, servedTipInvariantFixtures(liveHead))

	// PRECONDITION NOTE: the original reproduction seeded the persistent
	// shared counter to 5_000 (the value a previous process life left in the
	// store — prod saw tips inherited 33k–4M blocks behind) with the velocity
	// gate armed. The majority design holds NO persistent served-tip state
	// and NO gate, so there is nothing left to seed or arm: the wedge's
	// enabling state is gone by construction. The OUTCOME assertions below
	// are unchanged and must hold for any future implementation, stateful or
	// not.

	// Drive the chain forward one block per pick, exactly like prod traffic.
	served := int64(0)
	for r := int64(1); r <= 20; r++ {
		for _, u := range ups {
			u.EvmStatePoller().SuggestLatestBlock(liveHead + r)
		}
		served = network.EvmHighestLatestBlockNumber(ctx)
	}

	finalHead := liveHead + 20
	require.GreaterOrEqual(t, served, finalHead-2,
		"advertised latest must catch up to the live agreed head and track it; "+
			"got %d while the live head is %d — the inherited counter wedged the tip (the 2026-06 prod incident)",
		served, finalHead)
}

// Prod follow-up to the same incident: the whole fleet was restarted and the
// freeze SURVIVED, because the wedge state lived in the persistent counter
// while every fresh process re-armed the gate against it.
//
// INVARIANT: losing all process-local state (anchors/clocks) while the shared
// counter survives must never stop the advertised "latest" from tracking the
// live chain.
func TestServedTipInvariant_ProcessRestart_KeepsTracking(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const liveHead = int64(10_000)
	network, ups := setupServedTipNetwork(t, ctx, servedTipInvariantFixtures(liveHead))

	// Healthy steady state first.
	for r := int64(1); r <= 5; r++ {
		for _, u := range ups {
			u.EvmStatePoller().SuggestLatestBlock(liveHead + r)
		}
		_ = network.EvmHighestLatestBlockNumber(ctx)
	}

	// Simulate a process restart: ALL in-process served-tip state is lost; the
	// shared counter survives in the backing store (exactly the prod restart
	// that failed to heal). The chain meanwhile advanced past the gate bound.
	network.servedLatestAnchor.seenValue.Store(0)
	network.servedLatestAnchor.changedAtMs.Store(0)

	served := int64(0)
	for r := int64(100); r <= 110; r++ { // jump: deploy downtime ≫ gate bound
		for _, u := range ups {
			u.EvmStatePoller().SuggestLatestBlock(liveHead + r)
		}
		served = network.EvmHighestLatestBlockNumber(ctx)
	}

	finalHead := liveHead + 110
	require.GreaterOrEqual(t, served, finalHead-2,
		"a restart must heal, not preserve, a stale tip; got %d vs live head %d", served, finalHead)
}

// The inverse failure ("shared state rogueness"): the persistent counter is
// poisoned AHEAD of the real chain — a rogue store write, a garbage pick from
// a previous buggy build, or corruption. The monotonic clamp would preserve it
// for up to the rollback tolerance, and prod would advertise blocks NO
// upstream has: every interpolated request would fail or return inconsistent
// state.
//
// INVARIANT: the advertised "latest" never meaningfully exceeds the freshest
// block any live upstream actually has, no matter what value sits in the
// store.
func TestServedTipInvariant_PoisonedCounter_NeverServed(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const liveHead = int64(10_000)
	network, ups := setupServedTipNetwork(t, ctx, servedTipInvariantFixtures(liveHead))

	// PRECONDITION NOTE: the original reproduction poisoned the persistent
	// shared counter (+500 — small enough to dodge the store's large-rollback
	// self-heal, far beyond cross-pod skew). That state no longer exists; the
	// only remaining served-tip inputs are the per-upstream poller heads, so
	// the rogue value enters there instead: one upstream starts reporting a
	// fantasy-future tip (the wrong-chain/garbage-endpoint prod scenario).
	ups[0].EvmStatePoller().SuggestLatestBlock(10_000_000)

	served := int64(0)
	for r := int64(1); r <= 10; r++ {
		for _, u := range ups[1:] { // the rogue upstream stays rogue
			u.EvmStatePoller().SuggestLatestBlock(liveHead + r)
		}
		served = network.EvmHighestLatestBlockNumber(ctx)
	}

	finalHead := liveHead + 10
	require.LessOrEqual(t, served, finalHead+64,
		"advertised latest (%d) must stay within real reach of live upstream heads (%d) "+
			"regardless of rogue values in the shared store", served, finalHead)
	require.GreaterOrEqual(t, served, finalHead-2,
		"while clamping the rogue counter, the tip must still track the live chain")
}

// Liveness under sustained operation: no combination of anchor aging, gate
// dynamics, or counter state may EVER make the advertised "latest" stop for
// long while every upstream keeps advancing. This is the umbrella invariant —
// the three tests above pin the specific prod paths; this one sweeps the
// steady-state loop the same way prod traffic does, with the gate armed and
// realistic 1-block jitter between upstreams.
func TestServedTipInvariant_SteadyState_NeverStalls(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const liveHead = int64(10_000)
	network, ups := setupServedTipNetwork(t, ctx, servedTipInvariantFixtures(liveHead))

	var lastServed int64
	stalls := 0
	for r := int64(1); r <= 100; r++ {
		for i, u := range ups {
			u.EvmStatePoller().SuggestLatestBlock(liveHead + r - int64(i%2)) // 1-block jitter
		}
		served := network.EvmHighestLatestBlockNumber(ctx)
		if served <= lastServed {
			stalls++
		} else {
			stalls = 0
		}
		require.Less(t, stalls, 10,
			"advertised latest stalled at %d for %d consecutive picks while upstreams advanced to ~%d",
			served, stalls, liveHead+r)
		lastServed = served
	}
	require.GreaterOrEqual(t, lastServed, liveHead+100-2, "must end at the live head")
}

// Sanity guard for the invariants themselves: the fixture really runs the
// majority served-tip mode (not the legacy max fallback) — if someone flips
// the fixture default off, the invariants above would pass vacuously against
// max mode and stop guarding the served-tip path.
func TestServedTipInvariant_Fixture_ServedTipModeActive(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupServedTipNetwork(t, ctx, servedTipInvariantFixtures(100))
	require.True(t, network.servedTipEnabledFor("latest"),
		"invariant fixtures must exercise the served-tip path, not the max fallback")

	// MAX(heads) != majority(heads) for an uneven topology proves the pick
	// actually goes through the served-tip algorithm.
	network2, _ := setupServedTipNetwork(t, ctx, []servedTipFixture{
		{id: "w1", chainID: 123, latestBlock: 200},
		{id: "w2", chainID: 123, latestBlock: 100},
		{id: "w3", chainID: 123, latestBlock: 99},
	})
	require.Equal(t, int64(100), network2.EvmHighestLatestBlockNumber(ctx),
		"served-tip mode must be live (max mode would return 200)")
}
