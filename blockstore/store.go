package blockstore

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

var (
	ErrNotFound         = errors.New("blockstore: not found")
	ErrStoreUnavailable = errors.New("blockstore: store unavailable")
)

type Scope struct {
	Namespace string
	ProjectId string
	NetworkId string
}

func (s Scope) Key() string { return s.Namespace + "|" + s.ProjectId + "/" + s.NetworkId }

// BlockRecord is one block as delivered to subscribers and the historical
// cache: Block is the block JSON (a hash-only header for live-window events,
// a full block for historical reads) and Logs its complete log list, or nil
// when the logs were never fetched.
type BlockRecord struct {
	Number     int64           `json:"n"`
	Hash       string          `json:"h"`
	ParentHash string          `json:"p"`
	Block      json.RawMessage `json:"b"`
	Logs       json.RawMessage `json:"l"`
}

func (r *BlockRecord) Size() int64 {
	return int64(len(r.Block) + len(r.Logs) + len(r.Hash) + len(r.ParentHash) + 16)
}

// Snapshot is a header-verified canonical window shared with fleet followers.
// Incomplete marks a window that has not yet been backfilled to full depth.
type Snapshot struct {
	Head       int64     `json:"head"`
	Hashes     []string  `json:"hashes"`
	At         time.Time `json:"at"`
	Incomplete bool      `json:"incomplete,omitempty"`
}

func (s *Snapshot) Base() int64 { return s.Head - int64(len(s.Hashes)) + 1 }

func (s *Snapshot) HashAt(n int64) string {
	if s == nil || n < s.Base() || n > s.Head {
		return ""
	}
	return s.Hashes[n-s.Base()]
}

// PayloadKind names one hash-addressed, immutable payload of a block.
type PayloadKind string

const (
	// PayloadHeader is eth_getBlockByNumber(n, false): the window header.
	PayloadHeader PayloadKind = "header"
	// PayloadBlock is eth_getBlockByNumber(n, true): the full block body.
	PayloadBlock PayloadKind = "block"
	// PayloadLogs is the complete unfiltered log list of the block.
	PayloadLogs PayloadKind = "logs"
)

// Store shares immutable payloads keyed by block hash. A payload never
// establishes canonicality on its own: it is served only for a hash in a
// verified window, and is validated against that window's header on read.
type Store interface {
	PutPayload(ctx context.Context, scope Scope, kind PayloadKind, hash string, raw json.RawMessage, ttl time.Duration) error
	GetPayload(ctx context.Context, scope Scope, kind PayloadKind, hash string) (json.RawMessage, error)
}

// FleetStore optionally coordinates one refresher across cache replicas.
// Stores that do not implement it retain process-local refresh behavior.
type FleetStore interface {
	Store
	Acquire(ctx context.Context, scope Scope, ttl time.Duration) (Lease, error)
	ReadSnapshot(ctx context.Context, scope Scope) (*Snapshot, error)
}

// Lease is the refresh right held by one cache replica.
type Lease interface {
	Renew(ctx context.Context, ttl time.Duration) (bool, error)
	Publish(ctx context.Context, snap *Snapshot, ttl time.Duration) (bool, error)
	Release(ctx context.Context) error
}

// FillLocker is optionally implemented by a shared store to let replicas
// coalesce one upstream fill per key. The holder stores its result before
// releasing, so "lock no longer held" doubles as the completion signal.
type FillLocker interface {
	// TryLock acquires key for ttl. ok=false means another holder has it.
	// release is non-nil only when ok.
	TryLock(ctx context.Context, scope Scope, key string, ttl time.Duration) (release func(context.Context), ok bool, err error)
	// Locked reports whether some holder currently has key.
	Locked(ctx context.Context, scope Scope, key string) (bool, error)
}
