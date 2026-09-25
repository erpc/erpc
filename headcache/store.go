// Package headcache implements a head-driven, parent-hash-verified window of
// recent canonical EVM blocks (full block with transactions plus all logs of
// the block). A single follower hydrates records and publishes canonical
// snapshots through a Store; every instance serves reads from the latest
// committed snapshot it holds.
//
// The Store abstraction splits data into two kinds:
//
//   - Immutable, hash-addressed BlockRecords. A block hash fully determines its
//     contents, so records are write-once and safe to share without
//     coordination. Records are written BEFORE any snapshot references them
//     (publish-after-hydrate).
//   - A mutable canonical Snapshot per scope (project+network), replaced only
//     through an epoch-checked compare-and-set under a Lease. A holder whose
//     lease was superseded (higher epoch granted to someone else) has its
//     publications rejected with ErrLeaseLost and must stop following.
package headcache

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

var (
	// ErrNotFound means the requested record or snapshot does not exist.
	ErrNotFound = errors.New("headcache: not found")
	// ErrLeaseHeld means another holder owns a live lease for the scope.
	ErrLeaseHeld = errors.New("headcache: lease held by another instance")
	// ErrLeaseLost means the caller's lease epoch is no longer current. The
	// caller must stop publishing immediately.
	ErrLeaseLost = errors.New("headcache: lease lost")
	// ErrStoreUnavailable means the backend is unreachable. Callers must fail
	// safe: bypass the cache and use the normal upstream path.
	ErrStoreUnavailable = errors.New("headcache: store unavailable")
	// ErrInvalidSnapshot means a publication was structurally invalid (empty
	// window, blank hash) or did not advance Seq within the same epoch.
	ErrInvalidSnapshot = errors.New("headcache: invalid snapshot")
)

// Scope isolates all state. Namespace identifies the deployment and the trust
// configuration that produced the data (cluster key plus a fingerprint of the
// network's upstream/trust config), so deployments with different configs never
// share records even for the same project/network. Every backend key MUST be
// derived from Key().
type Scope struct {
	Namespace string
	ProjectId string
	NetworkId string
}

func (s Scope) Key() string { return s.Namespace + "|" + s.ProjectId + "/" + s.NetworkId }

// BlockRecord is one fully hydrated block. It is only ever constructed from a
// complete, self-consistent fetch: the full block and every log of that block,
// with every log's blockHash/blockNumber matching the block.
type BlockRecord struct {
	Number     int64           `json:"n"`
	Hash       string          `json:"h"`
	ParentHash string          `json:"p"`
	Block      json.RawMessage `json:"b"` // eth_getBlockByHash(hash, true) result
	Logs       json.RawMessage `json:"l"` // eth_getLogs({blockHash}) result (array)
}

// Size approximates the retained bytes of the record.
func (r *BlockRecord) Size() int64 {
	return int64(len(r.Block) + len(r.Logs) + len(r.Hash) + len(r.ParentHash) + 16)
}

// Snapshot is the canonical chain segment [Head-len(Hashes)+1 .. Head].
// Hashes[i] is the canonical hash at height Base()+i and every consecutive
// pair is parent-linked (verified before publication).
type Snapshot struct {
	Epoch  uint64    `json:"e"`
	Seq    uint64    `json:"s"` // strictly increasing within an epoch
	Head   int64     `json:"head"`
	Hashes []string  `json:"hashes"`
	At     time.Time `json:"at"`
}

func (s *Snapshot) Base() int64 { return s.Head - int64(len(s.Hashes)) + 1 }

// Valid reports structural validity (non-empty window, no blank hashes).
func (s *Snapshot) Valid() bool {
	if s == nil || len(s.Hashes) == 0 || s.Base() < 0 {
		return false
	}
	for _, h := range s.Hashes {
		if h == "" {
			return false
		}
	}
	return true
}

// HashAt returns the canonical hash at height n, or "" when outside the window.
func (s *Snapshot) HashAt(n int64) string {
	if s == nil || n < s.Base() || n > s.Head {
		return ""
	}
	return s.Hashes[n-s.Base()]
}

// Lease is a fencing token. Epoch strictly increases each time a lease is
// granted for a scope (to any holder), so a stale holder is detectable.
type Lease struct {
	Scope     Scope
	Holder    string
	Epoch     uint64
	ExpiresAt time.Time
}

// Store is the shared (or local) persistence and coordination backend.
// Implementations must be safe for concurrent use.
type Store interface {
	// PutBlock stores an immutable record. Idempotent by hash.
	PutBlock(ctx context.Context, scope Scope, rec *BlockRecord, ttl time.Duration) error
	// GetBlock loads a record by hash or returns ErrNotFound.
	GetBlock(ctx context.Context, scope Scope, hash string) (*BlockRecord, error)

	// AcquireLease grants a new lease (with a fresh, higher epoch) when none
	// is live, or returns ErrLeaseHeld.
	AcquireLease(ctx context.Context, scope Scope, holder string, ttl time.Duration) (*Lease, error)
	// RenewLease extends a lease only if its epoch and holder are still
	// current, else ErrLeaseLost.
	RenewLease(ctx context.Context, lease *Lease, ttl time.Duration) (*Lease, error)
	// ReleaseLease drops the lease if still current. Best-effort.
	ReleaseLease(ctx context.Context, lease *Lease) error

	// PublishSnapshot atomically replaces the canonical snapshot iff the lease
	// epoch is the current epoch for the scope (and the lease is unexpired),
	// else ErrLeaseLost. snap.Epoch is set by the caller to lease.Epoch.
	// Structurally invalid snapshots (!Valid) and a Seq that does not exceed
	// the stored Seq of the same epoch are rejected with ErrInvalidSnapshot.
	// Epoch counters never reset.
	PublishSnapshot(ctx context.Context, lease *Lease, snap *Snapshot) error
	// LoadSnapshot returns the latest committed snapshot or ErrNotFound.
	LoadSnapshot(ctx context.Context, scope Scope) (*Snapshot, error)
	// WatchSnapshots delivers a signal whenever a new snapshot is published.
	// Consumers must call LoadSnapshot afterwards (signals may coalesce).
	// The returned stop func releases resources.
	WatchSnapshots(ctx context.Context, scope Scope) (<-chan struct{}, func(), error)
}
