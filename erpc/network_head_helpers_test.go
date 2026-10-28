package erpc

import (
	"context"

	"github.com/erpc/erpc/common"
)

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
// finalized-floor memory, as in a process that has evaluated nothing yet:
// the head loop already evaluated the partial ballots a fixture's setup
// pushes, head by head.
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
