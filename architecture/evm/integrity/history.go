package integrity

import (
	"context"

	"github.com/erpc/erpc/common"
)

// History remembers number→hash for recently-observed blocks so continuity
// checks can link a new block against earlier observations. It is implemented
// per-network outside this package (a bounded store fed from observed traffic)
// and injected via Input; nil disables the continuity checks (they no-op).
type History interface {
	// HashAt returns the hash previously observed for a block number.
	HashAt(number int64) (hash string, known bool)
}

// PinReconfirmer optionally extends History: re-resolve a block number's
// canonical hash via a fresh trusted fetch (bypassing the cached pin), adopting
// whatever the network now returns. The engine uses it to corroborate a
// reorg-sensitive violation before applying the verdict: after a routine reorg
// the cached pin is stale, and without re-confirmation every honest new-fork
// response mismatches it — rejecting them all blocks the pin from ever adopting
// the new fork (a self-inflicted outage). Only a mismatch that survives the
// fresh pin is treated as genuine.
type PinReconfirmer interface {
	ReconfirmPin(ctx context.Context, number int64) (hash string, status PinConfirmation)
}

// PinConfirmation is what a PinReconfirmer was able to establish about a cached
// pin. The three states are deliberately distinct because they license
// different verdicts — collapsing "I re-checked it" with "I was not allowed to
// re-check it" is what turned one stale pin into 24 hard rejections of honest
// responses from three independent upstreams inside ~700ms (mainnet 25589196,
// 2026-07-22: 8 client errors, 0 saves, the pin was the non-canonical one).
type PinConfirmation int

const (
	// PinUnverifiable — no fresh evidence could be obtained at all (the
	// canonical fetch failed, or the number is invalid). The engine keeps the
	// strict class verdict: with no corroboration facility answering, a
	// reorg-sensitive mismatch is treated as it would be without a reconfirmer.
	PinUnverifiable PinConfirmation = iota
	// PinRateLimited — re-confirmation for this number was suppressed to bound
	// fetch volume, so the pin comes back UNVERIFIED. It says nothing about
	// whether the pin is genuine; the engine must not hard-reject on it.
	PinRateLimited
	// PinFresh — the pin was re-resolved against a fresh canonical fetch and
	// adopted. Only this state licenses acting on the outcome: a mismatch that
	// survives it is genuine, and one that clears is a reorg.
	PinFresh
)

// ReceiptCache optionally extends History: a CACHE-ONLY read of a block's
// canonical receipts by hash (immutable content) — never fetches. Checks use it
// for opportunistic cross-referencing (e.g. eth_getLogs completeness): validate
// when the data is already warm, skip when it isn't, add zero upstream cost.
type ReceiptCache interface {
	CachedReceipts(blockHash string) ([]Receipt, bool)
}

type historyKey struct{}

func withHistory(ctx context.Context, h History) context.Context {
	if h == nil {
		return ctx
	}
	return context.WithValue(ctx, historyKey{}, h)
}

func historyFrom(ctx context.Context) History {
	h, _ := ctx.Value(historyKey{}).(History)
	return h
}

// ChainSegment optionally extends History with the headers the chain tracker
// holds: its pins, one parentHash-linked chain, and every held header by
// hash.
//
// It exists to keep a hard line between two very different kinds of knowledge.
// HashAt answers from whatever the view happens to hold, including finalized
// anchors learned from passing traffic — fine for "have I seen this height",
// useless as a basis for reasoning about adjacency. A check that compares a
// block to its PARENT (base-fee derivation, timestamp ordering) is only sound
// when the parent is the block its parentHash names — a parent found by
// number may sit on another fork, and comparing across that boundary would
// reject honest data. Checks therefore find the parent by hash and skip when
// it is not held.
type ChainSegment interface {
	// FollowedRange is the inclusive range of the pins; ok=false while
	// nothing is pinned.
	FollowedRange() (from, to int64, ok bool)
	// HeaderAt returns the pinned header at a height within that range.
	HeaderAt(number int64) (*Header, bool)
	// HeaderByHash returns a held header, pinned or not. Held data only:
	// it never fetches.
	HeaderByHash(hash string) (*Header, bool)
}

// segmentFrom returns the verified chain segment, if the History implementation
// provides one.
func segmentFrom(ctx context.Context) ChainSegment {
	seg, _ := historyFrom(ctx).(ChainSegment)
	return seg
}

// parentInSegment returns the parent of h, block number, among the held
// headers: the one its parentHash names, never the one at number-1. A
// parent that is not held returns false and the caller skips.
func parentInSegment(ctx context.Context, h *Header, number int64) (*Header, bool) {
	seg := segmentFrom(ctx)
	if seg == nil || number <= 0 || h.ParentHash == "" {
		return nil, false
	}
	p, ok := seg.HeaderByHash(h.ParentHash)
	if !ok || p == nil {
		return nil, false
	}
	if pn, err := common.HexToInt64(p.Number); err != nil || pn != number-1 {
		return nil, false
	}
	return p, true
}
