package blockstore

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	"github.com/erpc/erpc/telemetry"
)

// Pull model. Without WebSocket subscribers nothing follows the chain in the
// background: the window is built from what clients already fetched. Every
// block a client reads by number (served from eRPC's cache or an upstream)
// and every log a client reads by block range is evidence of the canonical
// hash at that height at that time. Those headers are adopted here, linked to
// their neighbours by parent hash, and served while some linked descendant
// (or the header itself) was observed within MaxStaleness, or at any time once
// their height is at or below the network's finalized height (finalized
// heights cannot reorg). A conflicting observation at a held height is a
// reorg: the orphaned entries are dropped and everything unfinalized below
// loses its confirmation until it is observed again.
//
// Adoption never calls upstream. A height whose header is not held, or whose
// confirmation went stale, is a miss: the client's own request takes the
// normal path and its response is adopted.

type pullEntry struct {
	h *header
	// at is the last time this hash was observed as canonical at h.n.
	at time.Time
	// final is set once this hash was observed canonical (strong evidence)
	// while h.n was at or below the network's finalized height: it can no
	// longer reorg, so it is served without fresh confirmation or linkage.
	final bool
}

func (c *Cache) adoptMetric(kind PayloadKind) {
	telemetry.MetricBlockStoreAdoptTotal.WithLabelValues(c.opt.Scope.ProjectId, c.opt.Scope.NetworkId, string(kind)).Inc()
}

// latestKnown is the in-memory network head (no upstream call), or the
// highest adopted height when that is higher or the head is unknown.
func (c *Cache) latestKnown(ctx context.Context) int64 {
	latest := int64(-1)
	if c.opt.Latest != nil {
		latest = c.opt.Latest(ctx)
	}
	c.mu.RLock()
	top := c.pullTop
	c.mu.RUnlock()
	return maxI64(latest, top)
}

// inPullRange reports whether height n is recent enough to be held.
func (c *Cache) inPullRange(n, latest int64) bool {
	if n < 0 {
		return false
	}
	return latest < 0 || n > latest-c.opt.Depth
}

// confirmedLocked returns the latest observation time of the entry at n or
// of any header linked above it by parent hash.
func (c *Cache) confirmedLocked(n int64) time.Time {
	var t time.Time
	now := c.nowFn()
	for k := n; ; k++ {
		e := c.pull[k]
		if e == nil || (k > n && e.h.b.ParentHash != c.pull[k-1].h.b.Hash) {
			return t
		}
		if e.at.After(t) {
			t = e.at
		}
		if now.Sub(t) <= c.opt.MaxStaleness {
			return t
		}
	}
}

func (c *Cache) pullFreshLocked(n int64, hash string) *header {
	if n < 0 {
		var ok bool
		if n, ok = c.pullHashes[normHash(hash)]; !ok {
			return nil
		}
	}
	e := c.pull[n]
	if e == nil || (hash != "" && e.h.b.Hash != normHash(hash)) {
		return nil
	}
	if !e.final && c.nowFn().Sub(c.confirmedLocked(n)) > c.opt.MaxStaleness {
		return nil
	}
	return e.h
}

// finalizedHeight is the network's in-memory finalized height (no upstream
// call), or -1 when unknown.
func (c *Cache) finalizedHeight(ctx context.Context) int64 {
	if c.opt.Finalized == nil {
		return -1
	}
	if f := c.opt.Finalized(ctx); f > 0 {
		return f
	}
	return -1
}

// heldLocked reports whether hash is held at n in the followed or pulled view.
func (c *Cache) heldLocked(n int64, hash string) bool {
	if s := c.viewLocked(); s != nil && s.HashAt(n) == hash {
		return true
	}
	e := c.pull[n]
	return e != nil && e.h.b.Hash == hash
}

// keptHashLocked reports whether payloads for hash may stay cached.
func (c *Cache) keptHashLocked(hash string) bool {
	if c.headers[hash] != nil {
		return true
	}
	_, ok := c.pullHashes[hash]
	return ok
}

func (c *Cache) heightOfLocked(hash string) int64 {
	if h := c.headers[hash]; h != nil {
		return h.n
	}
	if n, ok := c.pullHashes[hash]; ok {
		return n
	}
	return -1
}

func (c *Cache) dropPayloadsLocked(hash string) {
	if c.keptHashLocked(hash) {
		return
	}
	if raw, ok := c.bodies[hash]; ok {
		c.payloadBytes -= int64(len(raw))
		delete(c.bodies, hash)
	}
	if raw, ok := c.logs[hash]; ok {
		c.payloadBytes -= int64(len(raw))
		delete(c.logs, hash)
	}
}

func (c *Cache) deletePullLocked(n int64) {
	e := c.pull[n]
	if e == nil {
		return
	}
	delete(c.pull, n)
	delete(c.pullHashes, e.h.b.Hash)
	c.dropPayloadsLocked(e.h.b.Hash)
}

// dropPullFromLocked removes every held entry at or above height from.
func (c *Cache) dropPullFromLocked(from int64) {
	for n := range c.pull {
		if n >= from {
			c.deletePullLocked(n)
		}
	}
	c.pullTop = -1
	for n := range c.pull {
		if n > c.pullTop {
			c.pullTop = n
		}
	}
}

// unconfirmBelowLocked fails closed after a reorg: entries below n keep their
// headers (immutable by hash) but must be relinked to a fresh observation
// before they are served again.
func (c *Cache) unconfirmBelowLocked(n int64) {
	for k, e := range c.pull {
		if k < n {
			e.at = time.Time{}
		}
	}
}

// adoptLocked records h as observed canonical at `at`. It returns whether h
// is now held and whether it was newly inserted. Older evidence that
// conflicts with a held neighbour is rejected; newer evidence replaces the
// conflicting entries (a reorg). Weak evidence (a response replayed from
// eRPC's cache, which may predate a reorg) never replaces anything and never
// confirms freshness: it is inserted unconfirmed, only where it conflicts
// with nothing held, and is served once a fresh linked descendant confirms it.
func (c *Cache) adoptLocked(h *header, at time.Time, latest, finalized int64, weak bool) (held, inserted bool) {
	top := maxI64(c.pullTop, latest)
	if !c.inPullRange(h.n, top) {
		return false, false
	}
	if weak {
		at = time.Time{}
	}
	if c.snap != nil && !weak {
		if fh := c.snap.HashAt(h.n); fh != "" && fh != h.b.Hash {
			// The followed window disagrees with an upstream observation:
			// re-verify its tip on the next tick.
			c.suspect.Store(true)
			c.Kick()
		}
	}
	final := !weak && finalized >= 0 && h.n <= finalized
	e := c.pull[h.n]
	if e != nil && e.h.b.Hash == h.b.Hash {
		if at.After(e.at) {
			e.at = at
		}
		e.final = e.final || final
		return true, false
	}
	parent, child := c.pull[h.n-1], c.pull[h.n+1]
	parentConflict := parent != nil && parent.h.b.Hash != h.b.ParentHash
	childConflict := child != nil && child.h.b.ParentHash != h.b.Hash
	if weak && (e != nil || parentConflict || childConflict) {
		return false, false
	}
	if (e != nil && at.Before(e.at)) || (parentConflict && at.Before(parent.at)) || (childConflict && at.Before(child.at)) {
		return false, false
	}
	reorg := false
	if e != nil || childConflict {
		// Everything at and above h.n descended from the replaced entry.
		c.dropPullFromLocked(h.n)
		reorg = e != nil
	}
	if parentConflict {
		c.deletePullLocked(h.n - 1)
		c.unconfirmBelowLocked(h.n - 1)
		reorg = true
	} else if e != nil {
		c.unconfirmBelowLocked(h.n)
	}
	if reorg {
		c.Stats.Reorgs.Add(1)
	}
	c.pull[h.n] = &pullEntry{h: h, at: at, final: final}
	c.pullHashes[h.b.Hash] = h.n
	if h.n > c.pullTop {
		c.pullTop = h.n
	}
	for n := range c.pull {
		if n <= c.pullTop-c.opt.Depth {
			c.deletePullLocked(n)
		}
	}
	return true, true
}

// observeHashLocked records a canonical (n, hash) seen without its header,
// e.g. in a log. A matching held entry is reconfirmed; a different held hash
// is dropped with its descendants, and everything below loses confirmation.
// Weak evidence is ignored.
func (c *Cache) observeHashLocked(n int64, hash string, at time.Time, finalized int64, weak bool) {
	if weak {
		return
	}
	hash = normHash(hash)
	if c.snap != nil {
		if fh := c.snap.HashAt(n); fh != "" && fh != hash {
			c.suspect.Store(true)
			c.Kick()
		}
	}
	e := c.pull[n]
	if e == nil {
		return
	}
	if e.h.b.Hash == hash {
		if at.After(e.at) {
			e.at = at
		}
		e.final = e.final || (finalized >= 0 && n <= finalized)
		return
	}
	if at.Before(e.at) {
		return
	}
	c.dropPullFromLocked(n)
	c.unconfirmBelowLocked(n)
	c.Stats.Reorgs.Add(1)
}

// adopt records a header observation and, when it was observed locally from
// an upstream (not weak), shares the header and its canonical index entry
// with the fleet.
func (c *Cache) adopt(ctx context.Context, h *header, at time.Time, local, weak bool) bool {
	latest := int64(-1)
	if c.opt.Latest != nil {
		latest = c.opt.Latest(ctx)
	}
	// Only a local observation made now proves the hash at a height that is
	// finalized now; a fleet index entry may predate finalization.
	finalized := int64(-1)
	if local {
		finalized = c.finalizedHeight(ctx)
	}
	c.mu.Lock()
	held, inserted := c.adoptLocked(h, at, latest, finalized, weak)
	share := false
	if held && local && !weak && c.canon != nil {
		last := c.sharedCanon[h.b.Hash]
		if inserted || c.nowFn().Sub(last) >= c.opt.MaxStaleness/2 {
			c.sharedCanon[h.b.Hash] = c.nowFn()
			share = true
		}
		for hash := range c.sharedCanon {
			if _, ok := c.pullHashes[hash]; !ok {
				delete(c.sharedCanon, hash)
			}
		}
	}
	c.mu.Unlock()
	if inserted {
		c.adoptMetric(PayloadHeader)
	}
	if share {
		if inserted && c.store != nil {
			if err := c.store.PutPayload(ctx, c.opt.Scope, PayloadHeader, h.b.Hash, h.raw, c.opt.RecordTTL); err != nil {
				c.logger.Debug().Err(err).Int64("number", h.n).Msg("failed to share adopted header")
			}
		}
		if err := c.canon.PutCanonical(ctx, c.opt.Scope, h.n, h.b.Hash, at, c.opt.RecordTTL); err != nil {
			c.logger.Debug().Err(err).Int64("number", h.n).Msg("failed to share canonical index entry")
		}
	}
	return held
}

// lookupHeader resolves a verified header for height n (or hash when n < 0):
// the followed window, then held adopted headers (fresh or finalized), then
// the fleet's canonical index. It never calls upstream.
func (c *Cache) lookupHeader(ctx context.Context, n int64, hash string) *header {
	if h := c.viewHeader(n, hash); h != nil {
		return h
	}
	c.mu.RLock()
	h := c.pullFreshLocked(n, hash)
	c.mu.RUnlock()
	if h != nil {
		return h
	}
	if c.fleetHeader(ctx, n, hash) {
		c.mu.RLock()
		h = c.pullFreshLocked(n, hash)
		c.mu.RUnlock()
	}
	return h
}

// fleetHeader adopts the header another replica observed at n (or for hash),
// keeping that replica's observation time. No upstream call.
func (c *Cache) fleetHeader(ctx context.Context, n int64, hash string) bool {
	if c.canon == nil || c.store == nil {
		return false
	}
	var raw json.RawMessage
	if n < 0 {
		if !isHexOfLen(hash, 64) {
			return false
		}
		var err error
		if raw, err = c.store.GetPayload(ctx, c.opt.Scope, PayloadHeader, normHash(hash)); err != nil || len(raw) == 0 {
			return false
		}
		_, got, err := parseBlockHeader(raw)
		if err != nil {
			return false
		}
		n = got
	}
	latest := c.latestKnown(ctx)
	if latest < 0 || !c.inPullRange(n, latest) || n > latest+1 {
		return false
	}
	ch, at, err := c.canon.GetCanonical(ctx, c.opt.Scope, n)
	if err != nil || ch == "" || (hash != "" && normHash(ch) != normHash(hash)) {
		return false
	}
	if at.After(c.nowFn()) {
		at = c.nowFn()
	}
	if c.nowFn().Sub(at) > c.opt.MaxStaleness {
		return false
	}
	if raw == nil {
		if raw, err = c.store.GetPayload(ctx, c.opt.Scope, PayloadHeader, normHash(ch)); err != nil || len(raw) == 0 {
			return false
		}
	}
	h, err := parseHeader(raw, n)
	if err != nil || h.b.Hash != normHash(ch) {
		c.Stats.Rejected.Add(1)
		return false
	}
	return c.adopt(ctx, h, at, false, false)
}

// hashOnlyHeader renders a full block as eth_getBlockByNumber(n, false).
func hashOnlyHeader(raw json.RawMessage) (json.RawMessage, error) {
	return (&BlockRecord{Block: raw}).BlockJSON(false)
}

// AdoptBlock adopts a block result served to a client through the normal
// path (eRPC's cache or an upstream). canonical is true when the request
// selected the block by number or tag, so the result is evidence of the
// canonical hash at its height; a by-hash result only completes the body of
// a header already held. full marks an eth_getBlockByNumber(n, true) result.
// fromCache marks weak evidence (see adoptLocked).
func (c *Cache) AdoptBlock(ctx context.Context, raw json.RawMessage, full, canonical, fromCache bool) {
	if c == nil || c.opt.MaxBlockSize > 0 && int64(len(raw)) > c.opt.MaxBlockSize {
		return
	}
	b, n, err := parseBlockHeader(raw)
	if err != nil {
		return
	}
	_, isFull, err := txHashesOf(b)
	if err != nil || isFull != full && len(b.Transactions) > 0 {
		return
	}
	hraw := raw
	if isFull && len(b.Transactions) > 0 {
		if hraw, err = hashOnlyHeader(raw); err != nil {
			return
		}
	}
	h, err := parseHeader(hraw, n)
	if err != nil {
		return
	}
	if canonical {
		if !c.adopt(ctx, h, c.nowFn(), true, fromCache) {
			return
		}
	} else {
		c.mu.RLock()
		held := c.heldLocked(h.n, h.b.Hash)
		c.mu.RUnlock()
		if !held {
			return
		}
		if held := c.lookupHeader(ctx, h.n, ""); held == nil || held.b.Hash != h.b.Hash {
			return
		}
	}
	if !full {
		return
	}
	held := c.lookupHeader(ctx, h.n, "")
	if held == nil || held.b.Hash != h.b.Hash || c.localPayload(PayloadBlock, h.b.Hash) != nil {
		return
	}
	c.adoptPayload(ctx, PayloadBlock, held, raw)
}

// adoptPayload validates a client-served body or log list against the held
// header, shares it with the fleet and keeps it locally.
func (c *Cache) adoptPayload(ctx context.Context, kind PayloadKind, h *header, raw json.RawMessage) bool {
	if c.validatePayload(kind, h, raw) != nil {
		c.Stats.Rejected.Add(1)
		return false
	}
	raw = append(json.RawMessage(nil), raw...)
	if c.store != nil {
		if err := c.store.PutPayload(ctx, c.opt.Scope, kind, h.b.Hash, raw, c.opt.RecordTTL); err != nil {
			c.logger.Debug().Err(err).Int64("number", h.n).Str("kind", string(kind)).Msg("failed to share adopted payload")
		}
	}
	c.keepPayload(kind, h, raw)
	c.adoptMetric(kind)
	return true
}

// ObserveLogs adopts an eth_getLogs result served to a client for a block
// range. Every log's (blockNumber, blockHash) is canonical evidence. When the
// request was unfiltered over the explicit range [from, to], each height's
// list is complete and is adopted as that block's logs once validated against
// its held header. A height whose header is not held is skipped: adoption
// never fetches. from < 0 means the range is unknown (tags): observation
// only. fromCache marks weak evidence: it reconfirms but never replaces held
// hashes.
func (c *Cache) ObserveLogs(ctx context.Context, raw json.RawMessage, from, to int64, unfiltered, fromCache bool) {
	if c == nil {
		return
	}
	var logs []rawLog
	if err := json.Unmarshal(bytes.TrimSpace(raw), &logs); err != nil {
		return
	}
	now := c.nowFn()
	finalized := c.finalizedHeight(ctx)
	seen := map[int64]string{}
	for i := range logs {
		if logs[i].Removed {
			return
		}
		n, err := parseHexInt(logs[i].BlockNumber)
		if err != nil || !isHexOfLen(logs[i].BlockHash, 64) {
			return
		}
		if prev, ok := seen[n]; ok && prev != normHash(logs[i].BlockHash) {
			return
		}
		seen[n] = normHash(logs[i].BlockHash)
	}
	c.mu.Lock()
	for n, hash := range seen {
		c.observeHashLocked(n, hash, now, finalized, fromCache)
	}
	c.mu.Unlock()
	if !unfiltered || from < 0 || to < from || to-from >= c.opt.MaxLogsRange {
		return
	}
	entries, removed, err := SplitRangeLogs(raw, from, to)
	if err != nil || removed {
		return
	}
	c.adoptHeldLogs(ctx, entries)
}

// adoptHeldLogs adopts complete per-height lists for heights whose header is
// held, validated against that header. No upstream call.
func (c *Cache) adoptHeldLogs(ctx context.Context, entries []*BlockLogs) {
	for _, e := range entries {
		if e == nil {
			continue
		}
		h := c.lookupHeader(ctx, e.Number, "")
		if h == nil || (e.Hash != "" && normHash(e.Hash) != h.b.Hash) || c.localPayload(PayloadLogs, h.b.Hash) != nil {
			continue
		}
		c.adoptPayload(ctx, PayloadLogs, h, e.Logs)
	}
}

// AdoptLogsByHash adopts an unfiltered eth_getLogs{blockHash} result for a
// hash held in the window.
func (c *Cache) AdoptLogsByHash(ctx context.Context, hash string, raw json.RawMessage) {
	if c == nil {
		return
	}
	h := c.lookupHeader(ctx, -1, hash)
	if h == nil || c.localPayload(PayloadLogs, h.b.Hash) != nil {
		return
	}
	c.adoptPayload(ctx, PayloadLogs, h, raw)
}

// seedWindowFromPull returns the linked adopted chain ending at the highest
// adopted height, when that height was observed within MaxStaleness, so
// header following that starts (a subscriber appeared) extends what clients
// already fetched instead of refetching it.
func (c *Cache) seedWindowFromPull() ([]*header, int64) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	top := c.pull[c.pullTop]
	if top == nil || c.nowFn().Sub(top.at) > c.opt.MaxStaleness {
		return nil, -1
	}
	chain := []*header{top.h}
	for k := c.pullTop - 1; ; k-- {
		e := c.pull[k]
		if e == nil || e.h.b.Hash != chain[0].b.ParentHash || int64(len(chain)) >= c.opt.Depth {
			break
		}
		chain = append([]*header{e.h}, chain...)
	}
	return chain, c.pullTop
}
