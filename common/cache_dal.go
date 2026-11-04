package common

import (
	"context"
)

type CacheDAL interface {
	Set(ctx context.Context, req *NormalizedRequest, res *NormalizedResponse) error
	Get(ctx context.Context, req *NormalizedRequest) (*NormalizedResponse, error)
	IsObjectNull() bool
}

// CachePurgeScope describes which cache keys an admin purge should match.
// Suffix must come from network config (never from API params).
type CachePurgeScope struct {
	NetworkId string
	Suffix    string // network cacheKeySuffix
	Method    string // "" = all methods
	BlockRef  string // "" = all refs
	DryRun    bool
}

// CachePurgeResult is one connector's outcome from a purge (or dry-run).
type CachePurgeResult struct {
	ConnectorId string
	Driver      string
	Matched     int64
	Deleted     int64
	DurationMs  int64
	Err         error
}

// CachePurger is an optional capability on a CacheDAL. Unsupported
// implementations must surface an error — never silently no-op.
type CachePurger interface {
	Purge(ctx context.Context, scope CachePurgeScope) ([]CachePurgeResult, error)
}
