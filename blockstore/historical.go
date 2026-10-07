package blockstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// HistoricalStore stores independently addressable finalized full blocks and
// the finalized height-to-hash index. GetFinalizedHash returns an error
// wrapping ErrNotFound when nothing is indexed; any other error means the
// index could not be read. PutFinalizedHash only writes an absent height: it
// is a no-op when the same hash is indexed and returns ErrIndexConflict when
// a different one is.
type HistoricalStore interface {
	GetFinalizedHash(context.Context, Scope, int64) (string, error)
	PutFinalizedHash(context.Context, Scope, int64, string, time.Duration) error
	DeleteFinalizedHash(context.Context, Scope, int64) error
	GetHistoricalBlock(context.Context, Scope, string) (json.RawMessage, error)
	PutHistoricalBlock(context.Context, Scope, string, json.RawMessage, time.Duration) error
}

type HistoricalOptions struct {
	Scope        Scope
	TTL          time.Duration
	MaxBlockSize int64
	LiveHash     func(int64) string
}

// Historical serves finalized full blocks outside the moving live block
// window. It never calls upstream: it only adopts block responses already
// served to clients (see Adopt). Historical logs are the logs fill's
// finalized per-height entries (one unfiltered range call), not a separate
// store.
type Historical struct {
	scope           Scope
	store           HistoricalStore
	finalizedHeight func(context.Context) int64
	maxBlockSize    int64
	ttl             time.Duration
	liveHash        func(int64) string
}

// NewHistorical constructs a history reader. TTL defaults to one hour.
func NewHistorical(opt HistoricalOptions, store HistoricalStore, finalized func(context.Context) int64) *Historical {
	if opt.TTL <= 0 {
		opt.TTL = time.Hour
	}
	opt.Scope.Namespace += ":historical"
	return &Historical{scope: opt.Scope, store: store, finalizedHeight: finalized,
		maxBlockSize: opt.MaxBlockSize, ttl: opt.TTL, liveHash: opt.LiveHash}
}

func (h *Historical) finalHeight(ctx context.Context) (int64, bool) {
	if h == nil || h.store == nil || h.finalizedHeight == nil {
		return 0, false
	}
	n := h.finalizedHeight(ctx)
	return n, n > 0
}

// ReadBlockByNumber returns a finalized full block.
func (h *Historical) ReadBlockByNumber(ctx context.Context, n int64) (*BlockRecord, bool) {
	if n < 0 {
		return nil, false
	}
	finalized, ok := h.finalHeight(ctx)
	if !ok || n > finalized {
		return nil, false
	}
	hash, ok := h.indexedHash(ctx, n)
	if !ok {
		return nil, false
	}
	return h.readBlock(ctx, hash, n)
}

// ReadBlockByHash returns a finalized full block only when its height index
// resolves to the requested hash.
func (h *Historical) ReadBlockByHash(ctx context.Context, hash string) (*BlockRecord, bool) {
	if hash == "" {
		return nil, false
	}
	finalized, ok := h.finalHeight(ctx)
	if !ok {
		return nil, false
	}
	rec, ok := h.readBlock(ctx, normHash(hash), -1)
	if !ok || rec.Number > finalized {
		return nil, false
	}
	indexed, ok := h.indexedHash(ctx, rec.Number)
	if !ok || normHash(indexed) != normHash(hash) {
		return nil, false
	}
	return rec, true
}

// readBlock loads and validates the full block stored under hash; want < 0
// accepts any height.
func (h *Historical) readBlock(ctx context.Context, hash string, want int64) (*BlockRecord, bool) {
	block, err := h.store.GetHistoricalBlock(ctx, h.scope, hash)
	if err != nil || len(block) == 0 {
		return nil, false
	}
	b, n, err := parseBlockHeader(block)
	if err != nil || (want >= 0 && n != want) || normHash(b.Hash) != normHash(hash) {
		return nil, false
	}
	if _, full, err := txHashesOf(b); b.Transactions == nil || err != nil || !full {
		return nil, false
	}
	if h.maxBlockSize > 0 && int64(len(block)) > h.maxBlockSize {
		return nil, false
	}
	return &BlockRecord{Number: n, Hash: normHash(b.Hash), ParentHash: normHash(b.ParentHash), Block: append(json.RawMessage(nil), block...)}, true
}

func (h *Historical) indexedHash(ctx context.Context, n int64) (string, bool) {
	hash, err := h.store.GetFinalizedHash(ctx, h.scope, n)
	if err != nil || hash == "" {
		return "", false
	}
	if h.liveHash != nil {
		if live := h.liveHash(n); live != "" && normHash(live) != normHash(hash) {
			_ = h.store.DeleteFinalizedHash(ctx, h.scope, n)
			return "", false
		}
	}
	return hash, true
}

// Adopt stores a block response already served to a client: no upstream
// call. byNumber marks a response to eth_getBlockByNumber(n), which is the
// upstream's observation of the canonical hash at n. Only heights at or
// below the finalized height are kept. A by-number result indexes its hash
// at n when no different hash is indexed there and the live window does not
// disagree; a different indexed hash is dropped (fail closed) and nothing is
// stored, rather than resolved by a fetch. A
// full body (by number or by hash) is stored under its hash; it is served
// only while the height index names that hash. Hash-only responses index the
// height without a body.
func (h *Historical) Adopt(ctx context.Context, raw json.RawMessage, byNumber bool) error {
	finalized, ok := h.finalHeight(ctx)
	if !ok {
		return nil
	}
	b, n, err := parseBlockHeader(raw)
	if err != nil {
		return fmt.Errorf("invalid historical block: %w", err)
	}
	if n > finalized {
		return nil
	}
	_, full, err := txHashesOf(b)
	if err != nil {
		return fmt.Errorf("invalid historical block %d: %w", n, err)
	}
	hash := normHash(b.Hash)
	if h.liveHash != nil {
		if live := h.liveHash(n); live != "" && normHash(live) != hash {
			return nil
		}
	}
	indexed, err := h.store.GetFinalizedHash(ctx, h.scope, n)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			// The index could not be read: whether a conflicting hash is
			// indexed is unknown, so adopt nothing (fail closed).
			return fmt.Errorf("read historical block index %d: %w", n, err)
		}
		indexed = ""
	}
	if indexed != "" && normHash(indexed) != hash {
		// Conflicting observations of a finalized height: fail closed (the
		// height misses until a later observation indexes it again) rather
		// than resolve it with an upstream recheck.
		if byNumber {
			_ = h.store.DeleteFinalizedHash(ctx, h.scope, n)
		}
		return nil
	}
	if !byNumber && indexed == "" && !full {
		return nil
	}
	if full && (h.maxBlockSize <= 0 || int64(len(raw)) <= h.maxBlockSize) {
		if _, hit := h.readBlock(ctx, hash, n); !hit {
			if err := h.store.PutHistoricalBlock(ctx, h.scope, hash, append(json.RawMessage(nil), raw...), h.ttl); err != nil {
				return fmt.Errorf("store historical block %d: %w", n, err)
			}
		}
	}
	if byNumber && indexed == "" {
		if err := h.store.PutFinalizedHash(ctx, h.scope, n, hash, h.ttl); err != nil {
			if errors.Is(err, ErrIndexConflict) {
				// A concurrent adopt indexed a different hash at n after our
				// read: conflicting observations, fail closed as above.
				_ = h.store.DeleteFinalizedHash(ctx, h.scope, n)
				return nil
			}
			return fmt.Errorf("store historical block index %d: %w", n, err)
		}
	}
	return nil
}
