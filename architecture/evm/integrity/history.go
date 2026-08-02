package integrity

import "context"

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

// ChainSegment optionally extends History with the CONTIGUOUS, parent-linked
// chain segment a follower has verified block by block.
//
// It exists to keep a hard line between two very different kinds of knowledge.
// HashAt answers from whatever the view happens to hold, including sparse pins
// learned from passing traffic — fine for "have I seen this height", useless as
// a basis for reasoning about adjacency. A check that compares a block to its
// PARENT (base-fee derivation, timestamp ordering, gas-limit drift) is only
// sound when the parent is genuinely the block before it on one verified
// chain — otherwise it would compare across a fork boundary and reject honest
// data. Checks therefore ask for the segment first and skip when the height
// they need is outside it.
type ChainSegment interface {
	// FollowedRange is the inclusive verified segment; ok=false when nothing
	// has been followed (the follower is off or still bootstrapping).
	FollowedRange() (from, to int64, ok bool)
	// HeaderAt returns the header committed at a height within that segment.
	HeaderAt(number int64) (*Header, bool)
}

// segmentFrom returns the verified chain segment, if the History implementation
// provides one.
func segmentFrom(ctx context.Context) ChainSegment {
	seg, _ := historyFrom(ctx).(ChainSegment)
	return seg
}

// parentInSegment returns the header for number-1 when BOTH it and number sit
// inside the verified segment, so the caller knows the two are genuinely
// adjacent on one chain. Any other situation returns false and the caller must
// skip rather than guess.
func parentInSegment(ctx context.Context, number int64) (*Header, bool) {
	seg := segmentFrom(ctx)
	if seg == nil || number <= 0 {
		return nil, false
	}
	from, to, ok := seg.FollowedRange()
	if !ok || number < from+1 || number > to {
		return nil, false
	}
	return seg.HeaderAt(number - 1)
}
