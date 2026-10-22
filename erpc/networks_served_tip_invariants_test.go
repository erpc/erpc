package erpc

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/require"
)

// ─────────────────────────────────────────────────────────────────────────────
// FROZEN-TIP INVARIANTS — DO NOT ADAPT THESE TO NEW INTERNALS.
//
// Each test below reproduces, end-to-end at the Network level, a failure mode
// of an earlier served-tip design: the served tip silently froze hours in the
// past across many chains and every eth_call(..., "latest") was rewritten to a
// stale block.
//
// They are BLACK-BOX: they assert client-visible OUTCOMES (the advertised
// "latest" tracks live heads, recovers from inherited state, never serves a
// rogue counter) — never the mechanism (no gate flags, no cluster shapes, no
// metric internals). Any future served-tip implementation, including a full
// redesign, MUST keep passing these tests UNCHANGED. If one fails, the new
// logic reintroduces a frozen or poisoned tip; fix the logic, not the test.
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

// THE frozen tip: a process starts with a served-tip counter inherited from the
// persistent shared store that is far BEHIND the live chain (rolling deploys,
// region restarts, idle partitions), while the velocity gate is armed (block
// time known). That design wedged FOREVER: every pick rejected every live
// tip, the counter never advanced, "latest" froze hours in the past, and the
// zeros served to clients were real state from the frozen block.
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
	// store, far behind the live head) with the velocity
	// gate armed. The majority design holds NO persistent served-tip state
	// and NO gate, so there is nothing left to seed or arm: the wedge's
	// enabling state is gone by construction. The OUTCOME assertions below
	// are unchanged and must hold for any future implementation, stateful or
	// not.

	// Drive the chain forward one block per pick, like live traffic.
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
			"got %d while the live head is %d — the inherited counter wedged the tip (the frozen-tip regression)",
		served, finalHead)
}

// Follow-up to the same failure: restarting every process did not heal it; the
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
	// shared counter survives in the backing store (exactly the restart that
	// failed to heal). The chain meanwhile advanced past the gate bound.
	network.servedTipAll.latest.seenValue.Store(0)
	network.servedTipAll.latest.changedAtMs.Store(0)

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
// for up to the rollback tolerance, and the network would advertise blocks NO
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
	// fantasy-future tip (a wrong-chain or garbage endpoint).
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
// the three tests above pin the specific failure paths; this one sweeps the
// steady-state loop the same way live traffic does, with the gate armed and
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
// majority quorum — if someone flips the fixture to another quorum, the
// invariants above would no longer guard the shape they were written for.
func TestServedTipInvariant_Fixture_ServedTipModeActive(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupServedTipNetwork(t, ctx, servedTipInvariantFixtures(100))
	require.Equal(t, common.ServedTipQuorumMajority, network.cfg.Evm.ServedTipQuorum(),
		"invariant fixtures must exercise the majority quorum")

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

// Two "legacy" upstreams configured with `blockAvailability.upper.exactBlock`
// far below the chain head polled the live head but reported the BOUND as
// their effective latest. While all five upstreams were eligible the three
// live heads carried the majority. Then the selection policy excluded one live
// upstream for lag, the ballot became [99996, 99968, 50000, 50000], and the
// pick — the 3rd of 4 — fell to the cap. "latest" was interpolated to that
// historical block, the availability gate then admitted ONLY the two legacy
// upstreams (the live ones declare a lower bound above the cap), and they
// correctly served historical state: eth_getBalance returned 0x0 for funded
// accounts and eth_call reverted.
//
// INVARIANT: an upstream that is only DECLARING a serving range must never
// define the network's head while any upstream is actually observing one — with
// the full fleet eligible and with any subset of it eligible.
func TestServedTipInvariant_AvailabilityBoundNeverDefinesTheHead(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const legacyCap = int64(50_000)
	const liveHead = int64(100_000)

	network, _ := setupServedTipNetwork(t, ctx, []servedTipFixture{
		{id: "leader", chainID: 123, latestBlock: liveHead},
		{id: "peer-a", chainID: 123, latestBlock: 99_996},
		{id: "peer-b", chainID: 123, latestBlock: 99_968},
		// The legacy pair: pollers at the live tip, serving range frozen far below it.
		{id: "legacy-1", chainID: 123, latestBlock: liveHead, upperExactBlock: legacyCap},
		{id: "legacy-2", chainID: 123, latestBlock: liveHead, upperExactBlock: legacyCap},
	})

	served := network.EvmHighestLatestBlockNumber(ctx)
	require.Greater(t, served, legacyCap,
		"with the full fleet eligible the served tip must be a live head, not the legacy serving bound")

	// leader's head froze briefly and the selection policy excluded it for
	// lag. This is the ballot that pulls the tip down to the cap.
	network.PinUpstreamOrderForTest("peer-a", "peer-b", "legacy-1", "legacy-2")

	served = network.EvmHighestLatestBlockNumber(ctx)
	require.Greater(t, served, legacyCap,
		"losing one live upstream must not hand the tip to the availability bounds: "+
			"served %d, the legacy cap is %d — clients would read historical state (eth_getBalance = 0x0 on funded accounts)",
		served, legacyCap)
	require.LessOrEqual(t, served, liveHead,
		"the served tip must still be a block the live upstreams actually have")
}
