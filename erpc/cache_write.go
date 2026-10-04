package erpc

import "context"

type cacheWriteBypassKey struct{}

// withCacheWriteBypass marks a context whose Forward must not write the
// response into the ordinary JSON-RPC cache. Head cache hydration uses it:
// hydration reads unfinalized head data that may be orphaned by a reorg the
// head cache detects and discards, and must not leave copies behind in the
// general cache that only expire by TTL.
func withCacheWriteBypass(ctx context.Context) context.Context {
	return context.WithValue(ctx, cacheWriteBypassKey{}, true)
}

func cacheWriteBypassed(ctx context.Context) bool {
	v, _ := ctx.Value(cacheWriteBypassKey{}).(bool)
	return v
}
