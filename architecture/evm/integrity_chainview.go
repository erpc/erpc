package evm

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/erpc/erpc/architecture/evm/integrity"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"golang.org/x/time/rate"
)

// defaultReorgWindow is how many blocks back from the tip the ChainView keeps a
// pin + header/receipts and tracks reorgs. Smallest-necessary by default; raise per
// network for deep-reorg chains (e.g. polygon 256) via integrity.reorgWindow.
const defaultReorgWindow = 32

// cacheSlack keeps a few extra content entries beyond the pin window so concurrent
// forks near the tip don't evict something we still need.
const cacheSlack = 2

// reconfirmCooldown bounds how often one block number's pin is re-confirmed
// against a fresh canonical fetch: within the cooldown the current pin is
// trusted as-is. Keeps a fork-flapping tip from re-fetching per request while
// staying well under a block time, so the pin still adopts a reorg promptly.
const reconfirmCooldown = time.Second

// chainView is the data-integrity module's central, reorg-aware state for one
// network: a committed number→hash pin plus content-addressed header and receipts
// caches, auto-populated from observed responses AND the module's own aux fetches —
// each block's header/receipts fetched once, then reused (the "ad-hoc mini-indexer").
// It implements integrity.History and backs the resolver. Isolated in-memory store —
// it does NOT use the shared cache DAL, so integrity works with no cache configured.
type chainView struct {
	mu            sync.RWMutex
	canonical     map[int64]string               // number → committed hash (the pin)
	headers       map[string]*integrity.Header   // hash → header (immutable per hash)
	headerOrder   []string                       // FIFO for header eviction
	receipts      map[string][]integrity.Receipt // hash → canonical receipts (immutable)
	receiptsOrder []string                       // FIFO for receipts eviction
	tip           int64                          // highest number observed
	window        int
	network       common.Network

	// Group scoping: the integrity state + corroboration fetches are PER node group
	// (groups can differ in system-tx index conventions or tip lead) — numbering/tip only agree
	// within a group. selector is the use-upstream selector to pin force-fetches to
	// the group ("" = network-wide); group is the human-readable lane for metrics.
	selector  string
	group     string
	finalized func() int64 // best-effort finalized height for the aux finality label

	// budget caps the module's canonical force-fetches (integrity.budget) —
	// shared per NETWORK across group views. Nil = unlimited.
	budget *auxBudget

	// reconfirmedAt tracks when each number's pin was last re-confirmed against
	// a fresh canonical fetch (guarded by mu, evicted with the window).
	reconfirmedAt map[int64]time.Time

	flightMu  sync.Mutex
	hInflight map[string]*flight[*integrity.Header]
	rInflight map[string]*flight[[]integrity.Receipt]
}

// flight coalesces concurrent misses for one key into a single fetch (singleflight),
// so a block's header/receipts is fetched at most once even under hedging.
type flight[T any] struct {
	wg  sync.WaitGroup
	val T
	ok  bool
}

func doOnce[T any](mu *sync.Mutex, inflight map[string]*flight[T], key string, fn func() (T, bool)) (T, bool) {
	mu.Lock()
	if f, ok := inflight[key]; ok {
		mu.Unlock()
		f.wg.Wait()
		return f.val, f.ok
	}
	f := &flight[T]{}
	f.wg.Add(1)
	inflight[key] = f
	mu.Unlock()

	f.val, f.ok = fn()

	mu.Lock()
	delete(inflight, key)
	mu.Unlock()
	f.wg.Done()
	return f.val, f.ok
}

func newChainView(n common.Network, window int, selector, group string, finalized func() int64) *chainView {
	if window <= 0 {
		window = defaultReorgWindow
	}
	return &chainView{
		canonical:     make(map[int64]string),
		headers:       make(map[string]*integrity.Header),
		receipts:      make(map[string][]integrity.Receipt),
		window:        window,
		network:       n,
		selector:      selector,
		group:         group,
		finalized:     finalized,
		reconfirmedAt: make(map[int64]time.Time),
		hInflight:     make(map[string]*flight[*integrity.Header]),
		rInflight:     make(map[string]*flight[[]integrity.Receipt]),
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
// reorg: adopt the new fork and roll back its descendants (their pins re-populate as
// the new fork extends). Below tip−window, entries are evicted.
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

// observeReceipts caches a block's canonical receipts by hash (immutable content).
func (c *chainView) observeReceipts(blockHash string, receipts []integrity.Receipt) {
	if blockHash == "" || receipts == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, seen := c.receipts[blockHash]; !seen {
		c.receiptsOrder = append(c.receiptsOrder, blockHash)
	}
	c.receipts[blockHash] = receipts
	c.evictLocked()
}

// ReconfirmPin implements integrity.PinReconfirmer: force-fetch the block by
// NUMBER via the trusted network path — bypassing the cached pin — so the pin
// adopts whatever fork the network currently serves (resolveHeader → observe,
// which also rolls back stale descendants). This is how a stale pin after a
// routine reorg gets unstuck: the engine calls it on a pin-anchored violation
// and re-runs the check against the refreshed pin. Singleflighted per number;
// within reconfirmCooldown the current pin is returned as-is (already fresh).
func (c *chainView) ReconfirmPin(ctx context.Context, number int64) (string, bool) {
	if number < 0 {
		return "", false
	}
	c.mu.RLock()
	t, recent := c.reconfirmedAt[number]
	pin, pinned := c.canonical[number]
	c.mu.RUnlock()
	if recent && pinned && time.Since(t) < reconfirmCooldown {
		return pin, true
	}
	h, ok := doOnce(&c.flightMu, c.hInflight, fmt.Sprintf("reconfirm:%d", number), func() (*integrity.Header, bool) {
		return c.resolveHeader(ctx, "eth_getBlockByNumber", fmt.Sprintf("0x%x", number))
	})
	if !ok || h == nil || h.Hash == "" {
		return "", false
	}
	c.mu.Lock()
	c.reconfirmedAt[number] = time.Now()
	c.mu.Unlock()
	return h.Hash, true
}

func (c *chainView) evictLocked() {
	lo := c.tip - int64(c.window)
	for k := range c.canonical {
		if k < lo {
			delete(c.canonical, k)
		}
	}
	for k := range c.reconfirmedAt {
		if k < lo {
			delete(c.reconfirmedAt, k)
		}
	}
	max := c.window + cacheSlack
	for len(c.headerOrder) > max {
		h := c.headerOrder[0]
		c.headerOrder = c.headerOrder[1:]
		delete(c.headers, h)
	}
	for len(c.receiptsOrder) > max {
		h := c.receiptsOrder[0]
		c.receiptsOrder = c.receiptsOrder[1:]
		delete(c.receipts, h)
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
	return doOnce(&c.flightMu, c.hInflight, hash, func() (*integrity.Header, bool) {
		return c.resolveHeader(ctx, "eth_getBlockByHash", hash)
	})
}

// headerByNumber returns the header for the committed hash of a number, resolving it
// once on a miss (and pinning whatever the trusted network path returns).
func (c *chainView) headerByNumber(ctx context.Context, number int64, blockRef string) (*integrity.Header, bool) {
	c.mu.RLock()
	if hash, ok := c.canonical[number]; ok {
		if h, ok2 := c.headers[hash]; ok2 {
			c.mu.RUnlock()
			return h, true
		}
	}
	c.mu.RUnlock()
	return doOnce(&c.flightMu, c.hInflight, fmt.Sprintf("n:%d", number), func() (*integrity.Header, bool) {
		return c.resolveHeader(ctx, "eth_getBlockByNumber", blockRef)
	})
}

// receiptsByHash returns a block's canonical receipts, fetching them once on a miss.
// Keyed by block hash (immutable) so the corroboration is reused across every receipt
// request in the same block — "block N's receipts fetched once".
func (c *chainView) receiptsByHash(ctx context.Context, blockHash string) ([]integrity.Receipt, bool) {
	c.mu.RLock()
	r, ok := c.receipts[blockHash]
	c.mu.RUnlock()
	if ok {
		return r, true
	}
	return doOnce(&c.flightMu, c.rInflight, blockHash, func() ([]integrity.Receipt, bool) {
		return c.resolveReceipts(ctx, blockHash)
	})
}

// fetchDirectives marks the force-fetch internal (no recursion into the engine) and
// pins it to the ChainView's node group, so a receipt from one group is only
// ever corroborated against same-group nodes. Empty selector = network-wide.
func (c *chainView) fetchDirectives() *common.RequestDirectives {
	d := &common.RequestDirectives{IsInternal: true}
	if c.selector != "" {
		d.UseUpstream = c.selector
	}
	return d
}

// finalityLabel classifies an aux-fetched block: finalized when its number is at or
// below the group's finalized height, unfinalized when above, unknown otherwise.
func (c *chainView) finalityLabel(number int64) string {
	if number < 0 || c.finalized == nil {
		return "unknown"
	}
	fin := c.finalized()
	if fin <= 0 {
		return "unknown"
	}
	if number <= fin {
		return "finalized"
	}
	return "unfinalized"
}

// auxBudget enforces integrity.budget over the module's canonical force-fetches:
// maxPerSecond (token bucket) + maxConcurrent (semaphore), shared per NETWORK
// across every group view so the cap is global. Acquisition is non-blocking —
// the fetches run inside the request path, so over budget the fetch degrades to
// a skip (the corroborating check no-ops) instead of queuing user latency.
type auxBudget struct {
	lim *rate.Limiter
	sem chan struct{}
}

func newAuxBudget(cfg *common.IntegrityBudgetConfig) *auxBudget {
	if cfg == nil || (cfg.MaxPerSecond <= 0 && cfg.MaxConcurrent <= 0) {
		return nil
	}
	b := &auxBudget{}
	if cfg.MaxPerSecond > 0 {
		b.lim = rate.NewLimiter(rate.Limit(cfg.MaxPerSecond), cfg.MaxPerSecond)
	}
	if cfg.MaxConcurrent > 0 {
		b.sem = make(chan struct{}, cfg.MaxConcurrent)
	}
	return b
}

// acquire reserves one fetch slot. ok=false means over budget (skip the fetch);
// on ok=true the caller must call release when the fetch completes.
func (b *auxBudget) acquire() (release func(), ok bool) {
	if b == nil {
		return func() {}, true
	}
	if b.sem != nil {
		select {
		case b.sem <- struct{}{}:
		default:
			return nil, false
		}
	}
	if b.lim != nil && !b.lim.Allow() {
		if b.sem != nil {
			<-b.sem
		}
		return nil, false
	}
	return func() {
		if b.sem != nil {
			<-b.sem
		}
	}, true
}

// auxBudgets holds the per-network shared budget (networkId → *auxBudget, nil
// when the network has no budget configured).
var auxBudgets sync.Map

func networkAuxBudget(n common.Network) *auxBudget {
	if n == nil {
		return nil
	}
	if v, ok := auxBudgets.Load(n.Id()); ok {
		b, _ := v.(*auxBudget)
		return b
	}
	var b *auxBudget
	if cfg := n.Config(); cfg != nil && cfg.Integrity != nil {
		b = newAuxBudget(cfg.Integrity.Budget)
	}
	actual, _ := auxBudgets.LoadOrStore(n.Id(), b)
	got, _ := actual.(*auxBudget)
	return got
}

// resolveHeader force-fetches a header via the trusted network path (group-scoped,
// inheriting the network's failsafe/consensus) and feeds it back into the view.
func (c *chainView) resolveHeader(ctx context.Context, method, blockRef string) (*integrity.Header, bool) {
	if c.network == nil {
		return nil, false
	}
	release, ok := c.budget.acquire()
	if !ok {
		c.emitAux("canonical_header", method, "unknown", "throttled")
		return nil, false
	}
	defer release()
	req := common.NewNormalizedRequest([]byte(fmt.Sprintf(
		`{"jsonrpc":"2.0","id":1,"method":"%s","params":["%s",false]}`, method, blockRef)))
	req.SetDirectives(c.fetchDirectives())
	req.SetNetwork(c.network)

	resp, err := c.network.Forward(ctx, req)
	var h *integrity.Header
	num := int64(-1)
	if err == nil && resp != nil {
		if jrr, jerr := resp.JsonRpcResponse(ctx); jerr == nil && jrr != nil {
			var hh integrity.Header
			if common.SonicCfg.Unmarshal(jrr.GetResultBytes(), &hh) == nil && hh.Hash != "" {
				h = &hh
				num, _ = common.HexToInt64(hh.Number)
			}
		}
	}
	c.emitAux("canonical_header", method, c.finalityLabel(num), auxOutcome(h != nil))
	if h == nil {
		return nil, false
	}
	if num >= 0 {
		c.observe(num, h.Hash, h)
	}
	return h, true
}

// resolveReceipts force-fetches a block's receipts BY HASH (immutable — no reorg
// race), group-scoped, and caches them.
func (c *chainView) resolveReceipts(ctx context.Context, blockHash string) ([]integrity.Receipt, bool) {
	if c.network == nil {
		return nil, false
	}
	release, ok := c.budget.acquire()
	if !ok {
		c.emitAux("canonical_receipts", "eth_getBlockReceipts", "unknown", "throttled")
		return nil, false
	}
	defer release()
	req := common.NewNormalizedRequest([]byte(fmt.Sprintf(
		`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":["%s"]}`, blockHash)))
	req.SetDirectives(c.fetchDirectives())
	req.SetNetwork(c.network)

	resp, err := c.network.Forward(ctx, req)
	var receipts []integrity.Receipt
	num := int64(-1)
	got := false
	if err == nil && resp != nil {
		if jrr, jerr := resp.JsonRpcResponse(ctx); jerr == nil && jrr != nil {
			if common.SonicCfg.Unmarshal(jrr.GetResultBytes(), &receipts) == nil {
				got = true
				if len(receipts) > 0 {
					num, _ = common.HexToInt64(receipts[0].BlockNumber)
				}
			}
		}
	}
	c.emitAux("canonical_receipts", "eth_getBlockReceipts", c.finalityLabel(num), auxOutcome(got))
	if !got {
		return nil, false
	}
	c.observeReceipts(blockHash, receipts)
	return receipts, true
}

// emitAux records an auxiliary (force-fetch) request — NOT part of a user request —
// only on a ChainView miss, so dedup keeps it rare. Labeled with the node group, the
// actual method sent, the target block's finality, and the outcome
// (ok | error | throttled — throttled = denied by integrity.budget, no fetch sent).
func (c *chainView) emitAux(kind, method, finality, outcome string) {
	telemetry.MetricIntegrityAuxRequest.WithLabelValues(
		c.network.ProjectId(), "", c.network.Label(), "", c.group, kind, method, finality, outcome,
	).Inc()
}

func auxOutcome(ok bool) string {
	if ok {
		return "ok"
	}
	return "error"
}

var chainViewStore sync.Map // "networkId\x00groupKey" -> *chainView

// groupChainView returns the ChainView for a network + node GROUP, deriving the group
// from the request's use-upstream selector via the SAME mechanism as latest-block
// tracking (Network.EvmUpstreamGroupForSelector → partitionKeyFor). A selector that
// doesn't carve out a real sub-group (or "") yields the network-wide view — today's
// behavior. Per-group isolation is what stops cross-group
// cross-talk: numbering and tip only agree within a group.
func groupChainView(ctx context.Context, n common.Network, selector string) *chainView {
	if n == nil {
		return nil
	}
	var groupKey, group, fetchSelector string
	if selector != "" {
		if gn, ok := n.(interface {
			EvmUpstreamGroupForSelector(context.Context, string) (string, string)
		}); ok {
			if k, lane := gn.EvmUpstreamGroupForSelector(ctx, selector); k != "" {
				groupKey, group, fetchSelector = k, lane, selector
			}
		}
	}
	storeKey := n.Id() + "\x00" + groupKey
	if v, ok := chainViewStore.Load(storeKey); ok {
		return v.(*chainView)
	}
	window := defaultReorgWindow
	if cfg := n.Config(); cfg != nil && cfg.Integrity != nil && cfg.Integrity.ReorgWindow > 0 {
		window = cfg.Integrity.ReorgWindow
	}
	created := newChainView(n, window, fetchSelector, group, networkFinalized(n))
	created.budget = networkAuxBudget(n) // shared per network, across group views
	actual, _ := chainViewStore.LoadOrStore(storeKey, created)
	return actual.(*chainView)
}

// networkFinalized returns a best-effort finalized-height getter for the aux finality
// label, or nil when the network can't report one (→ finality "unknown").
func networkFinalized(n common.Network) func() int64 {
	if fn, ok := n.(interface {
		EvmHighestFinalizedBlockNumber(context.Context) int64
	}); ok {
		return func() int64 { return fn.EvmHighestFinalizedBlockNumber(context.Background()) }
	}
	return nil
}

func isBlockMethod(methodLower string) bool {
	return methodLower == "eth_getblockbynumber" || methodLower == "eth_getblockbyhash"
}

// isAnchoredNarrowMethod reports methods whose response carries a single block's
// {number, hash} we can pin (receipts/tx) — used to feed the pin from narrow traffic.
func isAnchoredNarrowMethod(methodLower string) bool {
	switch methodLower {
	case "eth_gettransactionreceipt", "eth_getblockreceipts", "eth_gettransactionbyhash":
		return true
	}
	return false
}

type blockAnchorLite struct {
	BlockNumber string `json:"blockNumber"`
	BlockHash   string `json:"blockHash"`
}

// observeBlockView records a validated block response into the ChainView (pin +
// header) so later requests link/anchor against it.
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

// observeNarrowView feeds the pin from a narrow response (receipts/tx) using the
// serving upstream's finalized height.
func observeNarrowView(ctx context.Context, c *chainView, u common.Upstream, rs *common.NormalizedResponse) {
	if c == nil || rs == nil {
		return
	}
	eu, ok := u.(common.EvmUpstream)
	if !ok {
		return
	}
	jrr, err := rs.JsonRpcResponse(ctx)
	if err != nil || jrr == nil {
		return
	}
	c.observeNarrowAnchors(eu.EvmEffectiveFinalizedBlock(), jrr.GetResultBytes())
}

// observeNarrowAnchors pins the number→hash from a narrow response's block anchor(s),
// but ONLY for FINALIZED blocks (number <= fin). A single narrow response shouldn't
// get to redefine the canonical block for N at a jittery sub-second tip (that would
// reintroduce thrash); once N is finalized the answer is settled, so pinning it is
// safe and gives cross-receipt consistency even for blocks no getBlock pulled. The
// hash isn't fetched, only pinned. fin<=0 (finality unknown) → no-op.
func (c *chainView) observeNarrowAnchors(fin int64, result []byte) {
	if c == nil || fin <= 0 || len(result) == 0 {
		return
	}
	pinIfFinal := func(numHex, hash string) {
		if hash == "" || numHex == "" {
			return
		}
		if n, err := common.HexToInt64(numHex); err == nil && n >= 0 && n <= fin {
			c.observe(n, hash, nil)
		}
	}

	// Response may be a single object (receipt/tx) or an array (block receipts).
	var arr []blockAnchorLite
	if common.SonicCfg.Unmarshal(result, &arr) == nil && len(arr) > 0 {
		for i := range arr {
			pinIfFinal(arr[i].BlockNumber, arr[i].BlockHash)
		}
		return
	}
	var one blockAnchorLite
	if common.SonicCfg.Unmarshal(result, &one) == nil {
		pinIfFinal(one.BlockNumber, one.BlockHash)
	}
}
