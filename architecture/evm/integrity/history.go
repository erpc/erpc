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
