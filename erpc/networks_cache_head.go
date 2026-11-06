package erpc

import (
	"context"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/telemetry"
	"github.com/prometheus/client_golang/prometheus"
)

// cacheHeadAnchor caps a network's served "latest" at the newest block its
// read-through cache holds (evm.servedTip.cacheHeadMaxAge), so a client that
// reads "latest" and then asks for that block's receipts, logs or body asks
// for a block the cache already has.
//
// The gate is chain time, never poll age: the cache's head is used only while
// its own timestamp is at most maxAge behind the wall clock. A cache that falls
// behind therefore stops anchoring by itself, and the network serves latest as
// computed from its upstreams. The anchor only ever lowers latest: a cache
// ahead of the upstreams' pick changes nothing, so "latest" interpolation
// still names a block the upstreams have.
type cacheHeadAnchor struct {
	maxAge    time.Duration
	networkId string
	reporters []data.CacheHeadReporter
	outcomes  [cacheHeadOutcomes]prometheus.Counter
}

type cacheHeadOutcome int

const (
	cacheHeadAnchored cacheHeadOutcome = iota
	cacheHeadLevel
	cacheHeadStale
	cacheHeadAbsent
	cacheHeadOutcomes
)

var cacheHeadOutcomeLabels = [cacheHeadOutcomes]string{"anchored", "level", "stale", "absent"}

// newCacheHeadAnchor returns the network's anchor, or nil when the network
// sets no cacheHeadMaxAge.
func newCacheHeadAnchor(n *Network, cache *evm.EvmJsonRpcCache) *cacheHeadAnchor {
	if n.cfg == nil || n.cfg.Evm == nil || n.cfg.Evm.ServedTip == nil || n.cfg.Evm.ServedTip.CacheHeadMaxAge <= 0 {
		return nil
	}
	a := &cacheHeadAnchor{
		maxAge:    n.cfg.Evm.ServedTip.CacheHeadMaxAge.Duration(),
		networkId: n.networkId,
	}
	if cache != nil {
		a.reporters = cache.HeadReporters(n.networkId)
	}
	if len(a.reporters) == 0 {
		n.logger.Warn().Dur("cacheHeadMaxAge", a.maxAge).
			Msg("evm.servedTip.cacheHeadMaxAge is set but no head-aware cache connector covers this network; latest is never anchored")
	}
	for o, label := range cacheHeadOutcomeLabels {
		a.outcomes[o] = telemetry.MetricNetworkCacheHeadAnchorTotal.WithLabelValues(n.projectId, n.Label(), label)
	}
	return a
}

// pollInterval keeps the age a poll adds to the cache head at a tenth of the
// bound, within [50ms, 1s].
func (a *cacheHeadAnchor) pollInterval() time.Duration {
	return min(max(a.maxAge/10, 50*time.Millisecond), time.Second)
}

// run refreshes the cache connectors' head for this network every
// pollInterval until ctx ends. Without it the head is the one the connector's
// own minute-long poller saw, which no bound worth setting admits.
func (a *cacheHeadAnchor) run(ctx context.Context) {
	if len(a.reporters) == 0 {
		return
	}
	interval := a.pollInterval()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, r := range a.reporters {
				pollCtx, cancel := context.WithTimeout(ctx, interval)
				r.RefreshLatestBlock(pollCtx, a.networkId)
				cancel()
			}
		}
	}
}

// clamp returns the latest block to serve for the computed latest: the
// cache's fresh head when it is lower, latest otherwise. An unknown latest
// (<= 0) is returned as is: the anchor never makes a head up from nothing.
// Nil-safe, so a network without the option pays one comparison.
func (a *cacheHeadAnchor) clamp(latest int64) int64 {
	if a == nil || latest <= 0 {
		return latest
	}
	head, outcome := a.freshHead(time.Now())
	if outcome == cacheHeadLevel && head < latest {
		outcome = cacheHeadAnchored
		latest = head
	}
	a.outcomes[outcome].Inc()
	return latest
}

// freshHead returns the highest cache head at most maxAge old at now, with
// cacheHeadLevel; or why there is none: cacheHeadStale when a head is known
// but too old, cacheHeadAbsent when none is known.
func (a *cacheHeadAnchor) freshHead(now time.Time) (int64, cacheHeadOutcome) {
	var best int64
	outcome := cacheHeadAbsent
	nowMs := now.UnixMilli()
	for _, r := range a.reporters {
		h, ok := r.CacheLatestBlock(a.networkId)
		if !ok {
			continue
		}
		// A block stamped ahead of our clock (skew) reads as age zero.
		age := time.Duration(max(nowMs-h.Timestamp*1000, 0)) * time.Millisecond
		if age > a.maxAge {
			if outcome == cacheHeadAbsent {
				outcome = cacheHeadStale
			}
			continue
		}
		outcome = cacheHeadLevel
		best = max(best, h.Number)
	}
	return best, outcome
}
