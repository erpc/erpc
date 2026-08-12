package evm

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/erpc/erpc/architecture/evm/integrity"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
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

	// reconfirmedAt tracks when each number's pin was last re-confirmed against
	// a fresh canonical fetch (guarded by mu, evicted with the window).
	reconfirmedAt map[int64]time.Time

	// followBase..followHead is the CONTIGUOUS, PARENT-LINKED segment this view
	// has verified block by block: every height in the range is held and every
	// adjacent pair satisfies chain[n].parentHash == chain[n-1].hash. Outside
	// that range the view still holds opportunistic pins learned from traffic,
	// which are individually trustworthy but say nothing about linkage.
	// Zero/zero when nothing has been followed yet. Guarded by mu.
	followBase int64
	followHead int64

	// follower drives this view forward block by block when enabled.
	follower *chainFollower

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

	defer func() {
		mu.Lock()
		delete(inflight, key)
		mu.Unlock()
		f.wg.Done()
	}()

	f.val, f.ok = fn()
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

// adoptFollowed records a block the follower verified and advances the
// contiguous segment to it.
func (c *chainView) adoptFollowed(n int64, h *integrity.Header) {
	if h == nil || h.Hash == "" {
		return
	}
	c.observe(n, h.Hash, h)
	c.mu.Lock()
	if c.followBase == 0 || n < c.followBase {
		c.followBase = n
	}
	if n > c.followHead {
		c.followHead = n
	}
	c.mu.Unlock()
}

// FollowedRange reports the contiguous parent-linked segment this view has
// verified, block by block. ok=false when nothing has been followed — callers
// must not treat a sparse, traffic-learned pin as part of a verified chain.
func (c *chainView) FollowedRange() (from, to int64, ok bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.followHead == 0 {
		return 0, 0, false
	}
	return c.followBase, c.followHead, true
}

// HeaderAt implements integrity.ChainSegment: the header committed at a height.
// Cache-only — a consecutive-header check must never trigger a fetch, and a
// miss simply means the check skips.
func (c *chainView) HeaderAt(number int64) (*integrity.Header, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	hash, ok := c.canonical[number]
	if !ok {
		return nil, false
	}
	h, ok := c.headers[hash]
	return h, ok
}

// reconcile resolves a block that does NOT link to the block we hold beneath
// it, the way an indexer resolves a reorg: walk back along the new block's
// ancestry until reaching a height where the branch and the followed chain
// agree — the common ancestor — then unwind everything above it and adopt the
// branch in order.
//
// Returns the reorg depth (how many blocks were replaced) and whether the walk
// succeeded. It fails, deliberately, when no common ancestor exists within the
// reorg window: at that point the branch is not a reorg of the chain we are
// following but an unrelated history, and adopting it would silently replace
// our view with an unverified one.
func (c *chainView) reconcile(ctx context.Context, h *integrity.Header) (int, bool) {
	if h == nil || h.Hash == "" || h.Number == "" {
		return 0, false
	}
	n, err := common.HexToInt64(h.Number)
	if err != nil || n < 0 {
		return 0, false
	}

	// Collected newest-first while walking back; adopted oldest-first at the end
	// so the segment is only ever extended through linked blocks.
	branch := []*integrity.Header{h}
	numbers := []int64{n}
	cur := h
	curNum := n

	// Snapshot the verified segment once: it bounds what counts as a reorg of
	// the chain we follow, as opposed to unrelated history.
	c.mu.RLock()
	followBase, followHead := c.followBase, c.followHead
	c.mu.RUnlock()

	for depth := 1; depth <= c.window; depth++ {
		parentNum := curNum - 1
		if parentNum < 0 {
			return 0, false
		}
		c.mu.RLock()
		held, ok := c.canonical[parentNum]
		c.mu.RUnlock()

		if ok && eqHexFold(held, cur.ParentHash) {
			// Common ancestor: the branch rejoins the chain we are following.
			c.applyReorg(parentNum, branch, numbers)
			return depth, true
		}
		if !ok {
			// Nothing held at that height. If we are FOLLOWING, the verified
			// segment is contiguous, so a walk that reaches below its base
			// without ever rejoining it has not found a reorg of our chain —
			// it is a different history. Anchoring there would swap the chain
			// we verified block by block for one we never checked, which is
			// precisely the failure this whole design exists to prevent.
			if followHead != 0 && parentNum < followBase {
				return 0, false
			}
			// Not following (sparse, traffic-learned pins): there is genuinely
			// nothing to contradict the branch, so anchor it here.
			c.applyReorg(parentNum, branch, numbers)
			return depth, true
		}

		// Still diverging: pull the parent and keep walking back. Fresh, because
		// this is exactly the tie-break the cache cannot be trusted for.
		parent, ok2 := c.resolveHeader(ctx, "eth_getBlockByHash", cur.ParentHash, true)
		if !ok2 || parent == nil || parent.Number == "" {
			return 0, false
		}
		pn, err := common.HexToInt64(parent.Number)
		if err != nil || pn != parentNum {
			return 0, false
		}
		branch = append(branch, parent)
		numbers = append(numbers, pn)
		cur = parent
		curNum = pn
	}
	return 0, false
}

// applyReorg drops everything above the common ancestor and installs the branch
// (given newest-first) in ascending order.
func (c *chainView) applyReorg(ancestor int64, branch []*integrity.Header, numbers []int64) {
	c.mu.Lock()
	for k := range c.canonical {
		if k > ancestor {
			delete(c.canonical, k)
		}
	}
	c.mu.Unlock()

	for i := len(branch) - 1; i >= 0; i-- {
		c.observe(numbers[i], branch[i].Hash, branch[i])
	}

	c.mu.Lock()
	if c.followBase == 0 || ancestor+1 < c.followBase {
		c.followBase = ancestor + 1
	}
	// Everything above the ancestor was dropped above, so the branch head is
	// the new head unconditionally — it may move backwards on a deeper reorg.
	c.followHead = numbers[0]
	c.mu.Unlock()

	telemetry.MetricIntegrityReorgDepth.WithLabelValues(c.projectId(), c.networkLabel(), c.group).
		Observe(float64(len(branch)))
}

// eqHexFold compares two hex strings case-insensitively (upstreams differ on
// the case of hash payloads).
func eqHexFold(a, b string) bool { return strings.EqualFold(a, b) }

// observeHeader caches a block's header by hash WITHOUT touching the number→hash
// pin. This is the by-hash lookup path: the caller named the hash, so the
// response is not evidence about which hash is canonical at that height — it may
// legitimately be an orphan (fetching orphans by hash is how indexers unwind
// reorgs). Pinning from it would adopt the orphan as canonical and roll back the
// real fork's descendants. The header itself is immutable and content-addressed,
// so caching it by hash is always safe and still serves the by-hash consumers
// (receipt/root corroboration).
func (c *chainView) observeHeader(hash string, header *integrity.Header) {
	if hash == "" || header == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, seen := c.headers[hash]; !seen {
		c.headerOrder = append(c.headerOrder, hash)
	}
	c.headers[hash] = header
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
// and re-runs the check against the refreshed pin. Singleflighted per number.
//
// Returns verified=true ONLY when a fresh fetch actually resolved the pin. A
// re-confirmation within reconfirmCooldown is rate-limited, so it returns the
// current pin as PinRateLimited — handed back for context, but carrying NO new
// evidence. Reporting that as a confirmation made the cooldown window assert
// whatever the pin already held, so one bad pin hard-rejected every honest
// response for a full second (mainnet 25589196, 2026-07-22: 24 rejects across 3
// upstreams in ~700ms, 8 client errors, 0 saves — the pin was the non-canonical
// one and all three upstreams were right).
func (c *chainView) ReconfirmPin(ctx context.Context, number int64) (string, integrity.PinConfirmation) {
	if number < 0 {
		return "", integrity.PinUnverifiable
	}
	c.mu.RLock()
	t, recent := c.reconfirmedAt[number]
	pin, pinned := c.canonical[number]
	c.mu.RUnlock()
	if recent && pinned && time.Since(t) < reconfirmCooldown {
		return pin, integrity.PinRateLimited
	}
	h, ok := doOnce(&c.flightMu, c.hInflight, fmt.Sprintf("reconfirm:%d", number), func() (*integrity.Header, bool) {
		// fresh=true: this is the pin-vs-response tie-break, so it must reach
		// upstreams rather than replay the cached entry that seeded the pin.
		return c.resolveHeader(ctx, "eth_getBlockByNumber", fmt.Sprintf("0x%x", number), true)
	})
	if !ok || h == nil || h.Hash == "" {
		return "", integrity.PinUnverifiable
	}
	c.mu.Lock()
	c.reconfirmedAt[number] = time.Now()
	c.mu.Unlock()
	return h.Hash, integrity.PinFresh
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
	// The followed segment must not claim heights that were just evicted.
	// FollowedRange is a promise that EVERY height in it was verified block by
	// block, and the consecutive-header checks lean on exactly that promise to
	// decide whether a parent is genuinely the block before this one on one
	// chain. Left unmaintained, followBase keeps pointing at the bootstrap
	// height forever, so the range grows to cover heights whose pins were
	// re-learned from ordinary traffic — pins that may sit on another fork.
	// Those checks would then compare across a fork boundary and could reject
	// honest data, which is the one failure they exist to avoid.
	if c.followHead != 0 && c.followBase < lo {
		c.followBase = lo
	}
	if c.followBase > c.followHead {
		c.followBase, c.followHead = 0, 0
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
		return c.resolveHeader(ctx, "eth_getBlockByHash", hash, false)
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
		return c.resolveHeader(ctx, "eth_getBlockByNumber", blockRef, false)
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

// CachedReceipts implements integrity.ReceiptCache: a cache-only read of a
// block's canonical receipts by hash — never fetches. Opportunistic checks
// (eth_getLogs completeness) validate when the block is already warm and skip
// otherwise, adding zero upstream cost.
func (c *chainView) CachedReceipts(blockHash string) ([]integrity.Receipt, bool) {
	c.mu.RLock()
	r, ok := c.receipts[blockHash]
	c.mu.RUnlock()
	return r, ok
}

// fetchDirectives marks the force-fetch internal (no recursion into the engine) and
// pins it to the ChainView's node group, so a receipt from one group is only
// ever corroborated against same-group nodes. Empty selector = network-wide.
// fetchDirectives builds the directives for an aux force-fetch. fresh=true adds
// a cache-read bypass and MUST be used for anything that re-checks a disputed
// pin: the shared cache is where the disputed value came from, so reading it
// back is not corroboration, it is an echo. See ReconfirmPin.
func (c *chainView) fetchDirectives(fresh bool) *common.RequestDirectives {
	d := &common.RequestDirectives{IsInternal: true}
	if c.selector != "" {
		d.UseUpstream = c.selector
	}
	if fresh {
		d.SkipCacheRead = "true"
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

// resolveHeader force-fetches a header via the trusted network path (group-scoped,
// inheriting the network's failsafe/consensus) and feeds it back into the view.
// auxKind labels why a header fetch happened. The follower ingests EVERY block,
// so folding its fetches in with check-driven corroboration would swamp the
// metric and make the thing operators actually care about — what the checks
// cost — unmeasurable.
const (
	auxKindHeader = "canonical_header"
	auxKindFollow = "chain_follow"
)

func (c *chainView) resolveHeader(ctx context.Context, method, blockRef string, fresh bool) (*integrity.Header, bool) {
	return c.resolveHeaderKind(ctx, method, blockRef, fresh, auxKindHeader)
}

func (c *chainView) resolveHeaderKind(ctx context.Context, method, blockRef string, fresh bool, kind string) (*integrity.Header, bool) {
	if c.network == nil {
		return nil, false
	}
	req := common.NewNormalizedRequest([]byte(fmt.Sprintf(
		`{"jsonrpc":"2.0","id":1,"method":"%s","params":["%s",false]}`, method, blockRef)))
	req.SetDirectives(c.fetchDirectives(fresh))
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
	// Enforce the request anchor before trusting (and observing!) the answer:
	// a by-hash fetch must return THAT hash, a by-number fetch THAT number —
	// otherwise a node answering with a different block would poison the pin
	// and the header cache under the wrong key.
	if h != nil && !headerMatchesRef(h, num, method, blockRef) {
		h = nil
	}
	c.emitAux(kind, method, c.finalityLabel(num), h != nil)
	if h == nil {
		return nil, false
	}
	// Only a by-NUMBER answer states what the chain IS at a height, so only it
	// may move the pin. A by-hash fetch resolves a block the caller already
	// named, which may legitimately be an ORPHAN — corroborating a block a
	// client asked for by hash is how indexers unwind a reorg — so it
	// contributes its immutable header and nothing else. observeBlockView has
	// applied this rule to client responses since the by-hash scoping fix; the
	// aux path never did, which left the pin poisonable through the back door:
	// one orphan corroboration would pin the orphan at its height and then fail
	// continuity on every honest by-number response that followed.
	if method == "eth_getBlockByHash" {
		c.observeHeader(h.Hash, h)
	} else if num >= 0 {
		c.observe(num, h.Hash, h)
	}
	return h, true
}

// headerMatchesRef reports whether a fetched header is the block the request
// anchored on: same hash for a by-hash fetch, same number for a numeric
// by-number fetch (tags like "latest" have no anchor to enforce).
func headerMatchesRef(h *integrity.Header, num int64, method, blockRef string) bool {
	switch method {
	case "eth_getBlockByHash":
		return strings.EqualFold(h.Hash, blockRef)
	case "eth_getBlockByNumber":
		if want, err := common.HexToInt64(blockRef); err == nil {
			return num == want
		}
	}
	return true
}

// resolveReceipts force-fetches a block's receipts BY HASH (immutable — no reorg
// race), group-scoped, and caches them.
func (c *chainView) resolveReceipts(ctx context.Context, blockHash string) ([]integrity.Receipt, bool) {
	if c.network == nil {
		return nil, false
	}
	req := common.NewNormalizedRequest([]byte(fmt.Sprintf(
		`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":["%s"]}`, blockHash)))
	req.SetDirectives(c.fetchDirectives(false))
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
	// Enforce the hash anchor: a node may answer a by-hash receipts request
	// with a DIFFERENT block's receipts (still on a losing fork at that height,
	// or mishandling the hash param). Trusting it corrupts the corroboration
	// AND poisons the by-hash cache — every honest receipt for the real block
	// then rejects as "not found in canonical" (observed live: a group node
	// kept serving another fork's receipts for a reorged block, hours after
	// the reorg). A mismatched or empty answer is "canonical unavailable",
	// never evidence.
	if got && !receiptsMatchBlock(receipts, blockHash) {
		got = false
		receipts = nil
	}
	c.emitAux("canonical_receipts", "eth_getBlockReceipts", c.finalityLabel(num), got)
	if !got || len(receipts) == 0 {
		// Don't cache empty either: a tip-lagged [] cached under the hash would
		// permanently blind corroboration for that block.
		return nil, false
	}
	c.observeReceipts(blockHash, receipts)
	return receipts, true
}

// receiptsMatchBlock reports whether every receipt claims the expected block
// hash — the invariant a by-hash fetch is anchored on. Receipts without a
// blockHash field can't prove membership, so they fail the anchor too.
func receiptsMatchBlock(receipts []integrity.Receipt, blockHash string) bool {
	for i := range receipts {
		if !strings.EqualFold(receipts[i].BlockHash, blockHash) {
			return false
		}
	}
	return true
}

// emitAux records an auxiliary (force-fetch) request — NOT part of a user request —
// only on a ChainView miss, so dedup keeps it rare. Labeled with the node group, the
// actual method sent, and the target block's finality.
func (c *chainView) emitAux(kind, method, finality string, ok bool) {
	outcome := "error"
	if ok {
		outcome = "ok"
	}
	telemetry.MetricIntegrityAuxRequest.WithLabelValues(
		c.network.ProjectId(), "", c.network.Label(), "", c.group, kind, method, finality, outcome,
	).Inc()
}

// projectId / networkLabel are nil-safe metric label helpers (a view built for
// tests may carry no network).
func (c *chainView) projectId() string {
	if c.network == nil {
		return ""
	}
	return c.network.ProjectId()
}

func (c *chainView) networkLabel() string {
	if c.network == nil {
		return ""
	}
	return c.network.Label()
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
	actual, loaded := chainViewStore.LoadOrStore(storeKey, created)
	view := actual.(*chainView)
	// Start the follower exactly once per view, and only for the view that won
	// the LoadOrStore race — otherwise a concurrent miss would leave a second
	// follower fetching the same blocks.
	if !loaded {
		if cfg := n.Config(); cfg != nil && cfg.Integrity != nil && cfg.Integrity.Follow.IsEnabled() {
			view.follower = newChainFollower(view, networkLatest(n), cfg.Integrity.Follow)
			view.follower.start()
			// The state prober rides on the follower: its verified headers are
			// the only sound anchor for judging an upstream's state claims.
			if cfg.Integrity.StateProbe.IsEnabled() {
				startStateProber(n, view, cfg.Integrity.StateProbe)
			}
		}
	}
	return view
}

// networkLatest returns the network's current head-height getter, or nil when
// the network cannot report one (the follower then has nothing to follow).
func networkLatest(n common.Network) func(context.Context) int64 {
	if fn, ok := n.(interface {
		EvmHighestLatestBlockNumber(context.Context) int64
	}); ok {
		return fn.EvmHighestLatestBlockNumber
	}
	return nil
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
func observeBlockView(ctx context.Context, c *chainView, rs *common.NormalizedResponse, methodLower string) {
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
	// Only a by-NUMBER lookup answers "what is the chain at height N" — that is
	// the claim the pin records. A by-hash lookup answers "give me this exact
	// block", which may be an orphan, so it contributes the header only.
	if methodLower == "eth_getblockbyhash" {
		c.observeHeader(h.Hash, &h)
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

// chainView satisfies the optional History extensions the checks look for.
var (
	_ integrity.History      = (*chainView)(nil)
	_ integrity.ChainSegment = (*chainView)(nil)
)
