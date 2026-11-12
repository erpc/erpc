package evm

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
)

// CacheHead is a network's view of the newest block its cache serves: the
// one its realtime policy for eth_getBlockByNumber("latest") reads from,
// counted only while that policy's age guard would serve it
// (realtimeAdmits): the bound is the policy's realtime TTL, never a second
// number that could disagree with it. One poller (Run) keeps it fresh for
// its two uses:
//
//   - Polls: each new fresh head is the block the cache's server holds.
//     Where a lane voter's endpoint names that server (the cache's own
//     upstream, Prism's voter), the network takes it as that voter's poll
//     of latest: the same request to the same server, which the voter's
//     own client would send. Else it proves its block exists
//     (ChainTracker.Sighted), so the block clock fetches it at once.
//   - Clamp, with evm.servedTip.followCacheHead: a network-wide read's
//     "latest" stops at it, so a client that reads "latest" and then asks
//     for that block's receipts, logs or body asks for a block the cache
//     already holds. The anchor assumes the connector that serves latest
//     blocks also serves the follow-up reads.
//
// A head moves the view only as its voter's poll, through intake's checks
// and the tracker's fork choice, as any vote; a clamp only lowers what a
// read takes. A cache that falls behind stops counting by itself.
type CacheHead struct {
	networkId string
	blockTime func() time.Duration
	policy    *data.CachePolicy
	reporter  data.CacheHeadReporter
	// heldSince is when Clamp started holding latest at the value already
	// served above a fresh cache head; nil while it is not holding. Monotonic,
	// so a clock step neither ends nor extends a hold.
	heldSince atomic.Pointer[time.Time]
}

// CacheHead returns networkId's cache head: the first realtime policy, on a
// head-aware connector and with a TTL, that serves eth_getBlockByNumber
// ("latest") for it. nil when there is none. blockTime is the network's
// estimated block time, which a block-time-relative TTL resolves against.
func (c *EvmJsonRpcCache) CacheHead(networkId string, blockTime func() time.Duration) *CacheHead {
	realtime := finalities{f: [2]common.DataFinalityState{common.DataFinalityStateRealtime}, n: 1}
	policies, err := c.findGetPolicies(networkId, "eth_getBlockByNumber", []interface{}{"latest", false}, realtime)
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
		return &CacheHead{networkId: networkId, blockTime: blockTime, policy: policy, reporter: reporter}
	}
	return nil
}

// Server is the host:port the cache reads the network from, "" unknown:
// an upstream whose endpoint names it is the cache's own upstream.
func (a *CacheHead) Server() string {
	return a.reporter.CacheServer(a.networkId)
}

// fresh is the cache's head while the policy's age guard would serve it.
func (a *CacheHead) fresh(now time.Time) (int64, bool) {
	number, timestamp, ok := a.reporter.CacheLatestBlock(a.networkId)
	return number, ok && realtimeAdmits(a.ttl(), timestamp, now)
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
func (a *CacheHead) Clamp(tip, served int64) int64 {
	if a == nil || tip <= 0 {
		return tip
	}
	now := time.Now()
	number, ok := a.fresh(now)
	if !ok || number >= tip {
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
	} else if now.Sub(*since) > a.ttl() {
		return tip
	}
	return min(tip, served)
}

func (a *CacheHead) endHold() {
	if a.heldSince.Load() != nil {
		a.heldSince.Store(nil)
	}
}

// Run refreshes the connector's head while demand reports a reader of the
// network's head, until ctx ends, and calls polled with each fresh head
// other than the last it reported: the block the connector fetched (its
// eth_getBlockByNumber("latest", false) result), when that fetch was
// sent, and its number. It refreshes every eighth of a block, or of the
// TTL when that is sooner, 50 ms to 1 s: at most 8 polls a block, so a
// head the network takes from the cache trails the cache by an eighth of
// a block at most, and the age polling adds stays a small share of the
// bound. Without a reader it fetches nothing and looks for one at the
// same pace: the connector's own head poll runs once a minute, so a
// realtime age guard and Clamp then read a stale head, and fail open. A
// fetch may take up to the TTL: one slower than that returns a head the
// bound no longer admits.
func (a *CacheHead) Run(ctx context.Context, demand func() bool, polled func(block []byte, sent time.Time, number int64)) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	var last int64
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		ttl := a.ttl()
		if demand() {
			fetchCtx, cancel := context.WithTimeout(ctx, ttl)
			block, sent := a.reporter.RefreshLatestBlock(fetchCtx, a.networkId)
			cancel()
			if number, ok := a.fresh(time.Now()); ok && block != nil && number != last {
				last = number
				polled(block, sent, number)
			}
		}
		timer.Reset(min(max(min(a.blockTime(), ttl)/8, 50*time.Millisecond), time.Second))
	}
}

func (a *CacheHead) ttl() time.Duration {
	return a.policy.ResolveTTL(a.blockTime(), defaultRealtimeColdStartTTL)
}
