package thirdparty

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
)

// =============================================================================
// VENDOR REMOTE-DATA CACHE — REQUEST-PATH SAFETY RULE
// =============================================================================
//
// CRITICAL INVARIANT for any vendor that fetches data from a remote API on
// the request path (typically Vendor.SupportsNetwork or Vendor.GenerateConfigs):
//
//   The request hot path MUST NEVER hold a mutex while doing a network call,
//   and SHOULD NOT BLOCK waiting for one. Reads MUST be lock-free, refreshes
//   MUST be asynchronous, and cold-start failures MUST surface as retryable
//   errors so the bootstrap initializer's auto-retry loop picks them up.
//
// WHY THIS MATTERS
//
//   Several vendors ship with built-in dynamic discovery: they periodically
//   fetch the list of supported chains (or endpoints) from a vendor-controlled
//   API and cache the result. The natural-looking implementation is to take a
//   mutex, check whether the cache is stale, and if so do the HTTP fetch while
//   still holding the mutex.
//
//   That pattern is unsafe at scale. Vendor.SupportsNetwork is reachable from
//   the request hot path: UpstreamsRegistry.PrepareUpstreamsForNetwork calls
//   it during routing, and any bootstrap or auto-retry loop will call it
//   repeatedly. If the vendor's discovery endpoint becomes slow or starts
//   hanging, every in-flight request that needs to consult that vendor
//   blocks — and so do all subsequent requests, because they queue up on the
//   same mutex. From the outside this looks like the whole eRPC process has
//   wedged: requests stop completing, CPU saturates servicing the queue, and
//   even pprof can become unresponsive because its handlers run on the same
//   runtime that is now contending on the lock.
//
//   The shape that triggers this is "lock + I/O on the hot path". It is the
//   classic head-of-line-blocking failure mode: one slow remote API turns
//   into a global stall regardless of which network the user actually
//   requested.
//
//   This file is the shared, lock-free, copy-on-write cache that vendors use
//   instead. Any new vendor that needs to refresh data from a remote API and
//   consult it on the request path MUST use RemoteDataCache rather than
//   rolling its own mutex+HTTP pattern. Code review SHOULD reject the
//   mutex-around-HTTP shape on sight.
//
// PATTERN
//
//   1. Cache state lives in atomic.Pointer[remoteCacheSnapshot[T]]. Readers
//      do atomic.Load only — never wait on a mutex, never block on I/O.
//
//   2. A short-held refreshMu (sync.Mutex) guards ONLY the in-flight
//      refresh tracker map. It is NEVER held across the HTTP call.
//
//   3. Refresh runs in a dedicated goroutine. Single-flight per cacheKey:
//      if a refresh is already running, additional callers do not wait —
//      they return immediately with whatever the previous snapshot
//      contains.
//
//   4. Cold start (no snapshot for this key yet): Lookup returns
//      (zero-value, false). Vendors then either fall back to a built-in
//      static map or return ErrRemoteCacheCold so the bootstrap auto-retry
//      loop reschedules.
//
//   5. The fetch goroutine uses a self-contained context.WithTimeout, NOT
//      the caller's context. The original request that triggered the
//      refresh may have already returned by the time the fetch completes;
//      we don't want a slow remote API to be cancelled just because the
//      first user gave up.
// =============================================================================

// ErrRemoteCacheCold is the sentinel returned by vendor request-path
// methods when the cache has not yet been populated for the requested key.
// The bootstrap initializer treats this as retryable; the auto-retry loop
// will call again later, by which point the async refresh kicked off in
// the original call should have populated the snapshot.
var ErrRemoteCacheCold = fmt.Errorf("vendor remote-data cache not yet populated; retry shortly")

// RemoteDataCache is a generic lock-free, copy-on-write cache keyed by
// arbitrary string (typically apiKey, apiUrl, or apiKey+filterHash) and
// backed by a periodic-refresh fetcher. Hot-path Lookup is a single
// atomic.Load; refreshes are async, single-flight, and never hold a mutex
// while doing I/O.
//
// Type parameter T is the cached value type per cacheKey, e.g.:
//
//	[]*QuicknodeEndpoint
//	[]*ChainstackNode
//	map[int64]string  (alchemy network subdomains)
//	map[int64]*ConduitNetwork
type RemoteDataCache[T any] struct {
	// snapshot holds the immutable view read by every hot-path call.
	// Refreshes build a new snapshot off-thread and CAS it in place.
	snapshot atomic.Pointer[remoteCacheSnapshot[T]]

	// refreshMu serializes the inflight tracker only — NEVER held during
	// the HTTP fetch. Holding it for any reason longer than a few
	// nanoseconds violates the request-path safety rule.
	refreshMu sync.Mutex
	inflight  map[string]struct{} // key: cacheKey; presence = refresh running

	// loggerName is included in async-refresh log lines so messages
	// identify which vendor is refreshing.
	loggerName string

	// refreshTimeout is the deadline applied to each background refresh
	// goroutine. Defaults to 90s; vendors with longer sweeps (e.g.
	// QuickNode Chain Prism probing ~137 URLs) should raise it via
	// WithRefreshTimeout to avoid hitting the cap mid-sweep.
	refreshTimeout time.Duration
}

type remoteCacheSnapshot[T any] struct {
	values    map[string]T
	fetchedAt map[string]time.Time
}

// NewRemoteDataCache builds an empty cache. loggerName appears in async
// refresh log lines (e.g. "alchemy", "quicknode") so logs are diagnosable
// without grepping the file path.
func NewRemoteDataCache[T any](loggerName string) *RemoteDataCache[T] {
	return &RemoteDataCache[T]{
		inflight:       make(map[string]struct{}),
		loggerName:     loggerName,
		refreshTimeout: 90 * time.Second,
	}
}

// WithRefreshTimeout overrides the per-refresh goroutine deadline.
// Returns the receiver for chaining: NewRemoteDataCache[T]("x").WithRefreshTimeout(3*time.Minute).
func (c *RemoteDataCache[T]) WithRefreshTimeout(d time.Duration) *RemoteDataCache[T] {
	c.refreshTimeout = d
	return c
}

// Lookup returns (value-for-key, fresh) for the current snapshot.
// Reading the snapshot is lock-free; this is the hot path called from
// vendor SupportsNetwork on every routing decision.
//
//   - The second return is `true` iff the cached fetchedAt is within
//     recheckInterval. `false` does NOT mean the value is missing — only
//     that callers SHOULD trigger an async refresh.
//   - When no snapshot has been published yet for the key, Lookup returns
//     (zero, false) AND the caller should branch (fall back to a built-in
//     default, or return ErrRemoteCacheCold).
func (c *RemoteDataCache[T]) Lookup(cacheKey string, recheckInterval time.Duration) (T, bool) {
	var zero T
	snap := c.snapshot.Load()
	if snap == nil {
		return zero, false
	}
	val, ok := snap.values[cacheKey]
	if !ok {
		return zero, false
	}
	fetchedAt := snap.fetchedAt[cacheKey]
	return val, time.Since(fetchedAt) < recheckInterval
}

// Has reports whether a snapshot value exists for the given key. Used by
// vendors to decide between "trigger async refresh and return cold-start
// error" vs "use stale data and trigger async refresh in the background".
func (c *RemoteDataCache[T]) Has(cacheKey string) bool {
	snap := c.snapshot.Load()
	if snap == nil {
		return false
	}
	_, ok := snap.values[cacheKey]
	return ok
}

// TriggerAsyncRefresh starts a single-flight background refresh for
// cacheKey. NEVER holds a mutex while calling fetcher. If a refresh is
// already running for the same key, this call is a no-op (the in-flight
// fetch will publish for everyone).
//
// fetcher is called with a self-contained 90s timeout context; the
// caller's context is intentionally NOT used because it may belong to a
// transient request that will return long before the fetch completes.
//
// Failures are logged and silently dropped — readers continue to see the
// previous snapshot until a future refresh succeeds. There is no return
// path that can block a request goroutine.
func (c *RemoteDataCache[T]) TriggerAsyncRefresh(
	logger *zerolog.Logger,
	cacheKey string,
	fetcher func(ctx context.Context) (T, error),
) {
	c.refreshMu.Lock()
	if _, busy := c.inflight[cacheKey]; busy {
		c.refreshMu.Unlock()
		return
	}
	c.inflight[cacheKey] = struct{}{}
	c.refreshMu.Unlock()

	go func() {
		defer func() {
			c.refreshMu.Lock()
			delete(c.inflight, cacheKey)
			c.refreshMu.Unlock()
			if rec := recover(); rec != nil {
				logger.Error().
					Interface("panic", rec).
					Str("vendor", c.loggerName).
					Str("cacheKey", cacheKey).
					Msg("panic recovered during vendor remote-data async refresh")
			}
		}()

		ctx, cancel := context.WithTimeout(context.Background(), c.refreshTimeout)
		defer cancel()

		val, err := fetcher(ctx)
		if err != nil {
			// Keep previous snapshot on failure. This matches the old
			// "use stale data" behaviour but without ever blocking a
			// request goroutine.
			logger.Warn().
				Err(err).
				Str("vendor", c.loggerName).
				Str("cacheKey", cacheKey).
				Msg("vendor remote-data refresh failed; keeping previous snapshot")
			return
		}

		// Copy-on-write snapshot publish.
		old := c.snapshot.Load()
		newSnap := &remoteCacheSnapshot[T]{
			values:    make(map[string]T),
			fetchedAt: make(map[string]time.Time),
		}
		if old != nil {
			for k, v := range old.values {
				newSnap.values[k] = v
			}
			for k, v := range old.fetchedAt {
				newSnap.fetchedAt[k] = v
			}
		}
		newSnap.values[cacheKey] = val
		newSnap.fetchedAt[cacheKey] = time.Now()
		c.snapshot.Store(newSnap)
	}()
}

// WaitForKey blocks until the cache has a populated entry for cacheKey, or
// timeout elapses. Returns true when the value is available. Intended for
// test synchronisation: call the vendor method that triggers an async
// refresh, then call WaitForKey before asserting results.
func (c *RemoteDataCache[T]) WaitForKey(cacheKey string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c.refreshMu.Lock()
		_, busy := c.inflight[cacheKey]
		c.refreshMu.Unlock()
		// Both conditions are required: snapshot.Store happens before the
		// defer removes the key from inflight, so Has can be true while busy
		// is still true. Waiting for !busy ensures the goroutine has fully
		// committed before we declare the key ready.
		if !busy && c.Has(cacheKey) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return c.Has(cacheKey)
}

// EnsureFresh is the canonical hot-path call. It returns the cached value
// for cacheKey, plus whether the value should be considered usable. If
// the value is missing or stale, an async refresh is kicked off.
//
//   - If the snapshot has a value and it is fresh, returns (value, true).
//   - If the snapshot has a value but it is stale, returns (value, true)
//     AND triggers an async refresh — callers may use the stale value
//     while the refresh happens in the background.
//   - If no snapshot exists for this key, returns (zero, false) AND
//     triggers an async refresh — caller should fall back to a built-in
//     default or return ErrRemoteCacheCold.
func (c *RemoteDataCache[T]) EnsureFresh(
	logger *zerolog.Logger,
	cacheKey string,
	recheckInterval time.Duration,
	fetcher func(ctx context.Context) (T, error),
) (T, bool) {
	val, fresh := c.Lookup(cacheKey, recheckInterval)
	if !fresh {
		c.TriggerAsyncRefresh(logger, cacheKey, fetcher)
	}
	if !c.Has(cacheKey) {
		var zero T
		return zero, false
	}
	// Re-lookup after Has confirms the key is present: the async refresh may
	// have completed between the original Lookup and the Has check, storing a
	// populated snapshot. Without this second Lookup we would return the
	// zero/nil val from the original cold-start Lookup and callers would see
	// ok=true with no data — a false-negative cold start.
	val, _ = c.Lookup(cacheKey, recheckInterval)
	return val, true
}
