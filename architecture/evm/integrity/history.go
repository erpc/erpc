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
