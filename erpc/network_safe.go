package erpc

import (
	"context"
	"sync"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/telemetry"
)

// Head.Safe is demand-driven (streams spec §4): voters' safe tags are
// polled, and every lane computes Safe, only while a safe stream is open.

// openSafe registers a safe stream until release. The first one starts
// the safe loop; a stream that finds no Safe yet waits, at most W or until
// ctx ends, for a finalized and safe refresh of the voters, so its start
// is fixed on a computed Safe.
func (n *Network) openSafe(ctx context.Context) (release func()) {
	n.safeMu.Lock()
	if n.safeTaps.Add(1) == 1 {
		loop, stop := context.WithCancel(n.appCtx)
		n.safeStop = stop
		n.bg.Go(func() { n.safeLoop(loop) })
	}
	n.safeMu.Unlock()
	if n.headFor("").Safe == 0 {
		n.awaitSafeRefresh(ctx)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			n.safeMu.Lock()
			if n.safeTaps.Add(-1) == 0 {
				n.safeStop()
				n.safeStop = nil
			}
			n.safeMu.Unlock()
			n.headsChanged()
		})
	}
}

// safeLoop refreshes the voters' finalized and safe tags once per block
// time until ctx ends; each poller polls only past its finalized debounce.
func (n *Network) safeLoop(ctx context.Context) {
	for {
		n.awaitSafeRefresh(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(max(n.blockTimeOrCold(), 100*time.Millisecond)):
		}
	}
}

// awaitSafeRefresh refreshes the finalized and safe tags of every voter of
// the safe ballot and waits for them, at most W or until ctx ends, then
// computes the network-wide Head and wakes the other lanes.
func (n *Network) awaitSafeRefresh(ctx context.Context) {
	s := n.voters(n.safeSelector(""), voterQuery{kind: numberVote})
	if s.count() == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, health.FreshnessWindow(n.EvmBlockTime()))
	defer cancel()
	var wg sync.WaitGroup
	s.each(func(u common.Upstream, _ int64) bool {
		sp := u.(common.EvmUpstream).EvmStatePoller()
		wg.Go(func() { sp.RefreshFinalized(ctx) })
		wg.Go(func() { sp.RefreshSafe(ctx) })
		return true
	})
	wg.Wait()
	if n.appCtx.Err() == nil {
		n.refreshLaneRecovered(&n.servedTipAll)
		n.headsChanged()
	}
}

// safeSelector is the selector whose voters decide Safe for selector sel:
// evm.safeBlockSource when set, else sel.
func (n *Network) safeSelector(sel string) string {
	if n.cfg != nil && n.cfg.Evm != nil && n.cfg.Evm.SafeBlockSource != "" {
		return n.cfg.Evm.SafeBlockSource
	}
	return sel
}

// laneSafe is Safe for selector sel: the served-tip ballot over the safe
// tags of the voters with a finalized number, never below finalized nor
// above latest; finalized when no voter has a safe vote. l, when set,
// exports it as axis safe.
func (n *Network) laneSafe(l *servedTipLane, sel string, latest, finalized int64) int64 {
	var q common.ServedTipQuorum
	if n.cfg != nil && n.cfg.Evm != nil {
		q = n.cfg.Evm.ServedTipQuorum()
	}
	var buf [16]int64
	votes := buf[:0]
	n.voters(n.safeSelector(sel), voterQuery{kind: numberVote, finalized: true}).each(func(u common.Upstream, _ int64) bool {
		if sp := u.(common.EvmUpstream).EvmStatePoller(); sp != nil {
			if safe := sp.SafeBlock(); safe > 0 {
				votes = append(votes, safe)
			}
		}
		return true
	})
	safe := max(common.PickServedTip(votes, q).Tip, finalized)
	if latest > 0 {
		safe = min(safe, latest)
	}
	if l != nil && safe > 0 {
		n.observeServedTipMetrics("safe", l.label, safe, safe, -1)
	}
	return safe
}

// forgetSafeAxis removes lane l's safe series once Safe is no longer
// computed: the axis is exported only while it is (streams spec §12).
func (n *Network) forgetSafeAxis(l *servedTipLane) {
	telemetry.MetricNetworkServedTipBlockNumber.DeleteLabelValues(n.projectId, n.Label(), l.label, "safe")
	telemetry.MetricNetworkServedTipLagBlocks.DeleteLabelValues(n.projectId, n.Label(), l.label, "safe")
}
