package erpc

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/upstream"
)

// voteHead is u's poll answering block of the one test chain (tipHead
// "a"): a hash vote, as every real poll and push is, and it moves u's
// counter. A number alone never moves the view (design: one live head).
// A major jump is held while u's chain identity is checked (spec §5): as
// u's next poll would, it answers the same block again until taken. It
// returns once every tracker took the vote, as polls a block apart find
// it: a test's next read is then a read of the next block.
func voteHead(t testing.TB, n *Network, u *upstream.Upstream, block int64) {
	t.Helper()
	h := polledHead(t, "a", uint64(block))
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		n.intake.Polled(u.Id(), h, nil, evm.HeaderPolled, time.Now())
		if u.EvmStatePoller().LatestBlock() >= block {
			evm.SyncTrackersForTest(n.appCtx, n)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("intake never took %s's head %d", u.Id(), block)
		}
	}
}

// voteTip votes the head of the upstream whose poller is highest, at that
// head: the one upstream at the tip polled it with its hash.
func voteTip(t testing.TB, ctx context.Context, n *Network) {
	t.Helper()
	var tip *upstream.Upstream
	for _, u := range n.upstreamsRegistry.GetNetworkUpstreams(ctx, n.networkId) {
		if tip == nil || u.EvmStatePoller().LatestBlock() > tip.EvmStatePoller().LatestBlock() {
			tip = u
		}
	}
	voteHead(t, n, tip, tip.EvmStatePoller().LatestBlock())
}

// servedHead computes every lane's Head, as a head move or the 1s timer
// does, and returns the Head of the use-upstream selector of the request
// bound to ctx; a selector with no lane gets its stateless pick.
func servedHead(n *Network, ctx context.Context) common.EvmHead {
	n.refreshHeads()
	return *n.headFor(requestSelector(ctx))
}

// laneCeilingOf is the network-wide lane's ceiling and whether it is known.
func laneCeilingOf(n *Network) (int64, bool) {
	n.refreshHeads()
	c := n.servedTipAll.published().Ceiling
	return c, c > 0
}

// freshHeadMemory wipes the network-wide lane's guard, watchdog and
// finalized-floor memory, the shared floor included, as in a cluster that
// has evaluated nothing yet: the head loop already evaluated the partial
// ballots a fixture's setup pushes, head by head. A max-only counter cannot
// be lowered, so the network stops sharing; only laneTip of this lane reads
// it, under l.mu.
func freshHeadMemory(n *Network) {
	l := &n.servedTipAll
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, a := range []*servedTipAnchor{&l.latest, &l.finalized} {
		a.seenValue.Store(0)
		a.changedAtMs.Store(0)
		a.lastGoodValue.Store(0)
		a.lastGoodAt.Store(nil)
		a.guardState.Store(servedTipGuardHealthy)
	}
	l.finalizedFloor = 0
	n.servedFinalizedShared = nil
	l.head.Store(nil)
}

// highestOf is the first upstream of ups with the highest head: the Leader
// a Head of exactly ups would elect.
func highestOf(ups []common.Upstream) common.Upstream {
	var leader common.Upstream
	var top int64
	for _, u := range ups {
		if h := evmTipHead(u); h > top {
			leader, top = u, h
		}
	}
	return leader
}
