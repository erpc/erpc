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
	ReconfirmPin(ctx context.Context, number int64) (hash string, ok bool)
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
