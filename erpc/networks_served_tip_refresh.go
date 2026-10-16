package erpc

import (
	"time"

	"github.com/erpc/erpc/common"
)

// servedTipRefreshInterval is the longest the network-wide served tip goes
// without an evaluation.
const servedTipRefreshInterval = time.Second

// startServedTipRefresh evaluates an EVM network's network-wide served tip on
// both axes every servedTipRefreshInterval until the app stops.
//
// Only an evaluation writes the served-tip gauges, moves the advance-age
// anchor and refreshes the regression guard's memory. Without this loop a
// network that sees one request a minute exports a tip up to a minute old, and
// its guard measures against a pick that old. The loop evaluates exactly what
// an unscoped request would, so it adds no state of its own.
//
// Once per network: its bootstrap task can run again after it succeeded.
func (n *Network) startServedTipRefresh() {
	if n.cfg.Architecture != common.ArchitectureEvm {
		return
	}
	n.servedTipRefreshOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(servedTipRefreshInterval)
			defer ticker.Stop()
			for {
				select {
				case <-n.appCtx.Done():
					return
				case <-ticker.C:
					n.EvmHighestLatestBlockNumber(n.appCtx)
					n.EvmHighestFinalizedBlockNumber(n.appCtx)
				}
			}
		}()
	})
}
