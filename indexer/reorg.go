package indexer

import (
	"encoding/json"
	"sync"

	"github.com/erpc/erpc/common"
)

// DefaultCanonicalChainDepth is the default ring-buffer size per
// network. Sized to cover deep-but-realistic reorgs (e.g. up to
// ~1 minute of blocks on a 12-second-block chain) without pinning too
// much memory per network.
const DefaultCanonicalChainDepth = 256

// canonicalChain is the per-network ring-buffer tracker. It records the
// last N canonical heads observed, detects reorgs on each new head via
// parentHash continuity, and maintains a per-blockHash index of indexed
// logs so reorged-out logs can be re-emitted with Removed=true.
//
// Scope limits:
//   - Reorgs deeper than the ring depth are partially handled — the
//     common ancestor is "not found", so we evict only the immediate
//     head. Consumers that need stronger guarantees should use
//     upstream-level consensus policies.
//   - Gaps in observed heads (missed block between two observations)
//     do not synthesize reorg events — the tracker treats them as
//     forward progress.
type canonicalChain struct {
	maxDepth int

	mu sync.Mutex
	// ring is kept ordered by block number, oldest first. len <= maxDepth.
	ring []BlockRef
	// logs indexes delivered logs by block hash so we can re-emit them
	// with Removed=true when the block is reorged out. Values point to
	// the original StreamEvent payload (never mutated).
	logs map[string][]loggedLog
}

// loggedLog records enough detail to rebuild the IndexedEvent when a
// reorg invalidates the block.
type loggedLog struct {
	filterHash string
	networkID  string
	sourceID   string
	block      BlockRef
	payload    json.RawMessage
}

// newCanonicalChain constructs a tracker with the given ring depth.
// depth <= 0 falls back to DefaultCanonicalChainDepth.
func newCanonicalChain(depth int) *canonicalChain {
	if depth <= 0 {
		depth = DefaultCanonicalChainDepth
	}
	return &canonicalChain{
		maxDepth: depth,
		ring:     make([]BlockRef, 0, depth),
		logs:     make(map[string][]loggedLog),
	}
}

// observeHead pushes a new head onto the ring. Returns the set of blocks
// the ring evicted because the new head invalidated them — i.e. the
// reorged-out segment. Empty slice for the happy path.
//
// Semantics:
//
//   - Ring empty or new head extends the tip cleanly: push, no
//     evictions.
//   - new.num < tip.num: caller has already ingested a newer head, this
//     is stale — no-op, no evictions.
//   - new.parentHash matches some ring entry: evict everything after
//     that entry, push new — this is the reorg path.
//   - new.parentHash unknown in the ring AND new.num <= tip.num:
//     same-height or shallow reorg we can't fully walk; evict anything
//     with num >= new.num and push. Best-effort.
//   - Gap (new.num > tip.num+1): push without evictions, since we have
//     no knowledge of the skipped blocks.
func (c *canonicalChain) observeHead(b BlockRef) (evicted []BlockRef) {
	if b.Hash == "" || b.Number == 0 {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.ring) == 0 {
		c.pushLocked(b)
		return nil
	}

	// parentHash continuity drives the primary decision: if we can find
	// the incoming block's parent in the ring, we know exactly where the
	// chain diverges and can evict everything after that point. This
	// handles both the clean-extension case (parent == tip) and
	// reorg-to-common-ancestor cases (parent deeper in ring).
	if b.ParentHash != "" {
		for i := len(c.ring) - 1; i >= 0; i-- {
			if c.ring[i].Hash == b.ParentHash {
				if i < len(c.ring)-1 {
					evicted = append(evicted, c.ring[i+1:]...)
				}
				c.ring = c.ring[:i+1]
				c.pushLocked(b)
				return evicted
			}
		}
	}

	// No parent match. Disambiguate: is this a stale/dup head or a
	// genuine reorg whose ancestor fell out of our window?
	tip := c.ring[len(c.ring)-1]
	if b.Number < tip.Number {
		// Stale: older height, unknown parent — caller has seen newer.
		// Skip.
		return nil
	}
	if b.Number == tip.Number && b.Hash == tip.Hash {
		return nil
	}

	// Fallback: unknown parent, new-ish height. Evict anything at or
	// beyond the new height (same-level siblings are invalidated by
	// the incoming head) and push. This is best-effort — deep reorgs
	// beyond the ring window get partial handling here.
	cut := len(c.ring)
	for i := 0; i < len(c.ring); i++ {
		if c.ring[i].Number >= b.Number {
			cut = i
			break
		}
	}
	evicted = append(evicted, c.ring[cut:]...)
	c.ring = c.ring[:cut]
	c.pushLocked(b)
	return evicted
}

// pushLocked appends to the ring, evicting the oldest entry and its
// log index if we'd exceed maxDepth. Must be called with c.mu held.
func (c *canonicalChain) pushLocked(b BlockRef) {
	c.ring = append(c.ring, b)
	for len(c.ring) > c.maxDepth {
		evictBlock := c.ring[0]
		c.ring = c.ring[1:]
		// Drop the log index for the evicted block — it's beyond our
		// re-emission horizon anyway.
		delete(c.logs, evictBlock.Hash)
	}
}

// indexLog records a log against its block hash so it can be re-emitted
// on reorg. No-op if the block hash is empty (ingress couldn't parse
// the log's blockHash, which shouldn't happen for well-formed payloads).
func (c *canonicalChain) indexLog(entry loggedLog) {
	if entry.block.Hash == "" {
		return
	}
	c.mu.Lock()
	c.logs[entry.block.Hash] = append(c.logs[entry.block.Hash], entry)
	c.mu.Unlock()
}

// drainLogsFor returns and removes the recorded logs for a block hash.
// Used during reorg emission to rebuild IndexedEvents with Removed=true.
func (c *canonicalChain) drainLogsFor(blockHash string) []loggedLog {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.logs[blockHash]
	delete(c.logs, blockHash)
	return out
}

// head returns the current tip or the zero BlockRef when empty. Test-only.
func (c *canonicalChain) head() BlockRef {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.ring) == 0 {
		return BlockRef{}
	}
	return c.ring[len(c.ring)-1]
}

// reorgSummaryPayload serialises the evicted segment of a canonical
// chain as the payload for a KindReorg IndexedEvent. Consumers can
// unmarshal into an array of {number, hash, parentHash} objects. Small
// schema, forwards-compatible: the field set is a subset of BlockRef
// (additional fields later remain consumer-safe).
func reorgSummaryPayload(evicted []BlockRef) (json.RawMessage, error) {
	items := make([]map[string]interface{}, 0, len(evicted))
	for _, b := range evicted {
		items = append(items, map[string]interface{}{
			"number":     b.Number,
			"hash":       b.Hash,
			"parentHash": b.ParentHash,
		})
	}
	return common.SonicCfg.Marshal(map[string]interface{}{
		"evicted": items,
	})
}

// parseLogBlockRef extracts the (number, hash) of a log payload's
// containing block. Returns BlockRef{} on parse failure.
func parseLogBlockRef(payload json.RawMessage) BlockRef {
	if len(payload) == 0 {
		return BlockRef{}
	}
	var probe struct {
		BlockNumber string `json:"blockNumber"`
		BlockHash   string `json:"blockHash"`
	}
	if err := common.SonicCfg.Unmarshal(payload, &probe); err != nil {
		return BlockRef{}
	}
	num, _ := common.HexToInt64(probe.BlockNumber)
	return BlockRef{Number: num, Hash: probe.BlockHash}
}
