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

// Snapshot is a process-local, header-verified canonical window.
type Snapshot struct {
	Head   int64     `json:"head"`
	Hashes []string  `json:"hashes"`
	At     time.Time `json:"at"`
}

func (s *Snapshot) Base() int64 { return s.Head - int64(len(s.Hashes)) + 1 }

func (s *Snapshot) HashAt(n int64) string {
	if s == nil || n < s.Base() || n > s.Head {
		return ""
	}
	return s.Hashes[n-s.Base()]
}

// Store shares only immutable block payloads. Canonicality is verified locally
// by each Cache and is never inferred from Redis contents.
type Store interface {
	PutBlock(ctx context.Context, scope Scope, rec *BlockRecord, ttl time.Duration) error
	GetBlock(ctx context.Context, scope Scope, hash string) (*BlockRecord, error)
}
