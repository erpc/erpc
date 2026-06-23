package evm

import (
	"context"
	"fmt"
	"sync"

	"github.com/erpc/erpc/architecture/evm/integrity"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
)

// defaultReorgWindow is how many blocks back from the tip the ChainView keeps a
// pin + header and tracks reorgs. Smallest-necessary by default; raise per network
// for deep-reorg chains (e.g. polygon 256) via integrity.reorgWindow.
const defaultReorgWindow = 32

// headerCacheSlack keeps a few extra headers beyond the pin window so concurrent
// forks near the tip don't evict a header we still need.
const headerCacheSlack = 2

// chainView is the data-integrity module's central, reorg-aware state for one
// network: a committed number→hash pin plus a content-addressed header cache,
// auto-populated from observed responses AND the module's own aux fetches (each
// block's header fetched once, then reused). It implements integrity.History and
// backs the resolver's CanonicalHeader. Isolated in-memory store — it does NOT use
// the shared cache DAL, so integrity works with no cache configured.
type chainView struct {
	mu          sync.RWMutex
	canonical   map[int64]string             // number → committed hash (the pin)
	headers     map[string]*integrity.Header // hash → header (immutable per hash)
	headerOrder []string                     // FIFO for header eviction
	tip         int64                        // highest number observed
	window      int
	network     common.Network

	flightMu sync.Mutex
	inflight map[string]*headerFlight
}

type headerFlight struct {
	wg     sync.WaitGroup
	header *integrity.Header
	ok     bool
}

func newChainView(n common.Network, window int) *chainView {
	if window <= 0 {
		window = defaultReorgWindow
	}
	return &chainView{
		canonical: make(map[int64]string),
		headers:   make(map[string]*integrity.Header),
		window:    window,
		network:   n,
		inflight:  make(map[string]*headerFlight),
	}
}

// HashAt implements integrity.History: the committed hash for a block number.
func (c *chainView) HashAt(number int64) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	h, ok := c.canonical[number]
	return h, ok
}

// observe records a block's number→hash + header. A changed hash for a number is a
// reorg: adopt the new fork and roll back its descendants (their pins re-populate
// as the new fork extends). Below tip−window, entries are evicted.
func (c *chainView) observe(number int64, hash string, header *integrity.Header) {
	if number < 0 || hash == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if header != nil {
		if _, seen := c.headers[hash]; !seen {
			c.headerOrder = append(c.headerOrder, hash)
		}
		c.headers[hash] = header
	}

	if prev, exists := c.canonical[number]; exists && prev != hash {
		// Reorg at `number`: drop now-stale descendants of the old fork.
		for k := range c.canonical {
			if k > number {
				delete(c.canonical, k)
			}
		}
	}
	c.canonical[number] = hash
	if number > c.tip {
		c.tip = number
	}
	c.evictLocked()
}

func (c *chainView) evictLocked() {
	lo := c.tip - int64(c.window)
	for k := range c.canonical {
		if k < lo {
			delete(c.canonical, k)
		}
	}
	max := c.window + headerCacheSlack
	for len(c.headerOrder) > max {
		h := c.headerOrder[0]
		c.headerOrder = c.headerOrder[1:]
		delete(c.headers, h)
	}
}

// headerByHash returns the header for a hash, fetching it once on a miss.
func (c *chainView) headerByHash(ctx context.Context, hash string) (*integrity.Header, bool) {
	c.mu.RLock()
	h, ok := c.headers[hash]
	c.mu.RUnlock()
	if ok {
		return h, true
	}
	return c.fetchOnce(ctx, hash, "eth_getBlockByHash", hash)
}

// headerByNumber returns the header for the committed hash of a number, resolving
// it once on a miss (and pinning whatever the trusted network path returns).
func (c *chainView) headerByNumber(ctx context.Context, number int64, blockRef string) (*integrity.Header, bool) {
	c.mu.RLock()
	if hash, ok := c.canonical[number]; ok {
		if h, ok2 := c.headers[hash]; ok2 {
			c.mu.RUnlock()
			return h, true
		}
	}
	c.mu.RUnlock()
	return c.fetchOnce(ctx, fmt.Sprintf("n:%d", number), "eth_getBlockByNumber", blockRef)
}

// fetchOnce coalesces concurrent misses for the same key (singleflight) so a block
// is fetched at most once even under hedging, then observes the result.
func (c *chainView) fetchOnce(ctx context.Context, key, method, blockRef string) (*integrity.Header, bool) {
	c.flightMu.Lock()
	if f, ok := c.inflight[key]; ok {
		c.flightMu.Unlock()
		f.wg.Wait()
		return f.header, f.ok
	}
	f := &headerFlight{}
	f.wg.Add(1)
	c.inflight[key] = f
	c.flightMu.Unlock()

	f.header, f.ok = c.resolve(ctx, method, blockRef)

	c.flightMu.Lock()
	delete(c.inflight, key)
	c.flightMu.Unlock()
	f.wg.Done()
	return f.header, f.ok
}

// resolve force-fetches a header via the trusted network path (inheriting the
// network's configured failsafe/consensus) and feeds it back into the view.
func (c *chainView) resolve(ctx context.Context, method, blockRef string) (*integrity.Header, bool) {
	if c.network == nil {
		return nil, false
	}
	req := common.NewNormalizedRequest([]byte(fmt.Sprintf(
		`{"jsonrpc":"2.0","id":1,"method":"%s","params":["%s",false]}`, method, blockRef)))
	req.SetDirectives(&common.RequestDirectives{IsInternal: true})
	req.SetNetwork(c.network)

	resp, err := c.network.Forward(ctx, req)
	outcome := "error"
	if err == nil && resp != nil {
		outcome = "ok"
	}
	// Aux force-fetch (NOT part of the user request) — only on a ChainView miss,
	// so dedup keeps this rare. Network-scoped (the triggering upstream isn't known
	// at the shared store).
	telemetry.MetricIntegrityAuxRequest.WithLabelValues(
		c.network.ProjectId(), "", c.network.Label(), "", "canonical_header", outcome,
	).Inc()
	if err != nil || resp == nil {
		return nil, false
	}
	jrr, err := resp.JsonRpcResponse(ctx)
	if err != nil || jrr == nil {
		return nil, false
	}
	var h integrity.Header
	if common.SonicCfg.Unmarshal(jrr.GetResultBytes(), &h) != nil || h.Hash == "" {
		return nil, false
	}
	if n, err := common.HexToInt64(h.Number); err == nil {
		c.observe(n, h.Hash, &h)
	}
	return &h, true
}

var chainViewStore sync.Map // networkId -> *chainView

// networkChainView returns the per-network ChainView, creating it on first use
// with the configured reorg window. Returns nil only when the network is nil.
func networkChainView(n common.Network) *chainView {
	if n == nil {
		return nil
	}
	if v, ok := chainViewStore.Load(n.Id()); ok {
		return v.(*chainView)
	}
	window := defaultReorgWindow
	if cfg := n.Config(); cfg != nil && cfg.Integrity != nil && cfg.Integrity.ReorgWindow > 0 {
		window = cfg.Integrity.ReorgWindow
	}
	created := newChainView(n, window)
	actual, _ := chainViewStore.LoadOrStore(n.Id(), created)
	return actual.(*chainView)
}

func isBlockMethod(methodLower string) bool {
	return methodLower == "eth_getblockbynumber" || methodLower == "eth_getblockbyhash"
}

// observeBlockView records a validated block response into the ChainView so later
// requests link/anchor against it (continuity + receipt corroboration).
func observeBlockView(ctx context.Context, c *chainView, rs *common.NormalizedResponse) {
	if c == nil || rs == nil {
		return
	}
	jrr, err := rs.JsonRpcResponse(ctx)
	if err != nil || jrr == nil {
		return
	}
	var h integrity.Header
	if common.SonicCfg.Unmarshal(jrr.GetResultBytes(), &h) != nil || h.Hash == "" || h.Number == "" {
		return
	}
	if n, err := common.HexToInt64(h.Number); err == nil {
		c.observe(n, h.Hash, &h)
	}
}
