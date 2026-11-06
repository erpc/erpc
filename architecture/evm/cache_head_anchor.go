package evm

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
)

// CacheHeadAnchor lowers the "latest" a network's reads take to the newest
// block its cache serves (evm.servedTip.followCacheHead), so a client that
// reads "latest" and then asks for that block's receipts, logs or body asks
// for a block the cache already holds.
//
// The cache's head is the one its realtime policy for eth_getBlockByNumber
// ("latest") reads from, and it counts only while that policy's age guard
// would serve it (realtimeAdmits): the bound is the policy's realtime TTL,
// never a second number that could disagree with it. A cache that falls
// behind stops anchoring by itself, and reads take the tracker's head. The
// anchor assumes the connector that serves latest blocks also serves the
// follow-up reads.
//
// It is a read-side clamp only: it feeds no chain tracker, pin, hash vote,
// state poller, stream or reorg decision.
type CacheHeadAnchor struct {
	networkId string
	blockTime func() time.Duration
	policy    *data.CachePolicy
	reporter  data.CacheHeadReporter
	// heldSince is when Clamp started holding latest at the value already
	// served above a fresh cache head; nil while it is not holding. Monotonic,
	// so a clock step neither ends nor extends a hold.
	heldSince atomic.Pointer[time.Time]
}

// HeadAnchor returns the anchor for networkId: the first realtime policy, on a
// head-aware connector and with a TTL, that serves eth_getBlockByNumber
// ("latest") for it. nil when there is none. blockTime is the network's
// estimated block time, which a block-time-relative TTL resolves against.
func (c *EvmJsonRpcCache) HeadAnchor(networkId string, blockTime func() time.Duration) *CacheHeadAnchor {
	policies, err := c.findGetPolicies(networkId, "eth_getBlockByNumber", []interface{}{"latest", false}, common.DataFinalityStateRealtime)
	if err != nil {
		return nil
	}
	for _, policy := range policies {
		reporter, ok := policy.GetConnector().(data.CacheHeadReporter)
		// A realtime policy without a TTL serves a block of any age, and a
		// network that followed it would follow a stalled cache forever.
		if !ok || policy.ResolveTTL(0, defaultRealtimeColdStartTTL) <= 0 {
			continue
		}
		return &CacheHeadAnchor{networkId: networkId, blockTime: blockTime, policy: policy, reporter: reporter}
	}
	return nil
}

// Clamp returns the latest block to serve for tip, the computed latest, given
// served, the latest last served: the cache's head while the policy's age
// guard would serve it and it is below tip. Never below served, so latest
// never steps back: a fresh head below served holds latest at served, for at
// most one TTL. A cache that does not catch up within its own bound, one that
// reports another chain say, then stops anchoring until it goes stale or
// reaches the served value, so it cannot freeze latest. It never raises tip,
// and an unknown tip (<= 0) stays unknown. Nil-safe: a network without the
// anchor pays one comparison.
func (a *CacheHeadAnchor) Clamp(tip, served int64) int64 {
	if a == nil || tip <= 0 {
		return tip
	}
	now := time.Now()
	ttl := a.ttl()
	number, timestamp, ok := a.reporter.CacheLatestBlock(a.networkId)
	if !ok || number >= tip || !realtimeAdmits(ttl, timestamp, now) {
		a.endHold()
		return tip
	}
	if number >= served {
		a.endHold()
		return number
	}
	since := a.heldSince.Load()
	if since == nil {
		// Its own variable, so only a hold that starts allocates.
		start := now
		a.heldSince.CompareAndSwap(nil, &start)
	} else if now.Sub(*since) > ttl {
		return tip
	}
	return min(tip, served)
}

func (a *CacheHeadAnchor) endHold() {
	if a.heldSince.Load() != nil {
		a.heldSince.Store(nil)
	}
}

// Run refreshes the connector's head every eighth of the TTL (50ms to 1s)
// until ctx ends, so the age polling adds stays a small share of the bound.
// The connector's own head poll runs once a minute, which no realtime TTL
// admits. A fetch may take up to the TTL: one slower than that returns a head
// the bound no longer admits.
func (a *CacheHeadAnchor) Run(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		ttl := a.ttl()
		fetchCtx, cancel := context.WithTimeout(ctx, ttl)
		a.reporter.RefreshLatestBlock(fetchCtx, a.networkId)
		cancel()
		timer.Reset(min(max(ttl/8, 50*time.Millisecond), time.Second))
	}
}

func (a *CacheHeadAnchor) ttl() time.Duration {
	return a.policy.ResolveTTL(a.blockTime(), defaultRealtimeColdStartTTL)
}
