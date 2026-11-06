package evm

import (
	"context"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
)

// CacheHeadAnchor lowers a network's served tags to the newest block its cache
// serves (evm.servedTip.followCacheHead), so a client that reads "latest" and
// then asks for that block's receipts, logs or body asks for a block the cache
// already holds.
//
// The cache's head is the one its realtime policy for eth_getBlockByNumber
// ("latest") reads from, and it counts only while that policy's age guard
// would serve it (realtimeAdmits): the bound is the policy's realtime TTL,
// never a second number that could disagree with it. A cache that falls
// behind stops anchoring by itself, and tags are served as computed. The
// anchor assumes the connector that serves latest blocks also serves the
// follow-up reads.
type CacheHeadAnchor struct {
	networkId string
	blockTime func() time.Duration
	policy    *data.CachePolicy
	reporter  data.CacheHeadReporter
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

// Clamp returns tip lowered to the cache's head while the policy's age guard
// would serve that head, but never below served, the value last served: a
// cache that turns fresh again below a tip already served holds the tip there
// until it catches up, instead of stepping it back. It never raises tip, and
// an unknown tip (<= 0) stays unknown. Nil-safe: a network without the anchor
// pays one comparison.
func (a *CacheHeadAnchor) Clamp(tip, served int64) int64 {
	if a == nil || tip <= 0 {
		return tip
	}
	number, timestamp, ok := a.reporter.CacheLatestBlock(a.networkId)
	if !ok || number >= tip || !realtimeAdmits(a.ttl(), timestamp, time.Now()) {
		return tip
	}
	return min(tip, max(number, served))
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
