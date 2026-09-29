package headcache

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

var (
	ErrNotFound         = errors.New("headcache: not found")
	ErrStoreUnavailable = errors.New("headcache: store unavailable")
)

type Scope struct {
	Namespace string
	ProjectId string
	NetworkId string
}

func (s Scope) Key() string { return s.Namespace + "|" + s.ProjectId + "/" + s.NetworkId }

// BlockRecord contains one validated full block and its complete log list.
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

// Store shares immutable block payloads, which do not establish canonicality
// without upstream verification or a verified FleetStore snapshot.
type Store interface {
	PutBlock(ctx context.Context, scope Scope, rec *BlockRecord, ttl time.Duration) error
	GetBlock(ctx context.Context, scope Scope, hash string) (*BlockRecord, error)
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
