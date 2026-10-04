package blockstore

import "errors"

var (
	ErrNotFound         = errors.New("blockstore: not found")
	ErrStoreUnavailable = errors.New("blockstore: store unavailable")
)

// Scope isolates one network's entries within a shared store.
type Scope struct {
	Namespace string
	ProjectId string
	NetworkId string
}

func (s Scope) Key() string { return s.Namespace + "|" + s.ProjectId + "/" + s.NetworkId }
