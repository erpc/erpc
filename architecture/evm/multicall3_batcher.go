package evm

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/erpc/erpc/common"
)

// DirectivesKeyVersion should be bumped when the set of directives
// included in the key changes. This prevents cross-node key mismatches.
const DirectivesKeyVersion = 1

// BatchingKey uniquely identifies a batch for grouping eth_call requests.
type BatchingKey struct {
	ProjectId     string
	NetworkId     string
	BlockRef      string
	DirectivesKey string
	UserId        string // empty if cross-user batching is allowed
}

func (k BatchingKey) String() string {
	return fmt.Sprintf("%s|%s|%s|%s|%s", k.ProjectId, k.NetworkId, k.BlockRef, k.DirectivesKey, k.UserId)
}

// DeriveDirectivesKey creates a stable, versioned key from relevant directives.
// Only includes directives that affect batching behavior.
func DeriveDirectivesKey(dirs *common.RequestDirectives) string {
	if dirs == nil {
		return fmt.Sprintf("v%d:", DirectivesKeyVersion)
	}

	parts := make([]string, 0, 5)
	if dirs.UseUpstream != "" {
		parts = append(parts, fmt.Sprintf("use-upstream=%s", dirs.UseUpstream))
	}
	if dirs.SkipInterpolation {
		parts = append(parts, "skip-interpolation=true")
	}
	if dirs.RetryEmpty {
		parts = append(parts, "retry-empty=true")
	}
	if dirs.RetryPending {
		parts = append(parts, "retry-pending=true")
	}
	if dirs.SkipCacheRead {
		parts = append(parts, "skip-cache-read=true")
	}

	sort.Strings(parts)
	return fmt.Sprintf("v%d:%s", DirectivesKeyVersion, strings.Join(parts, ","))
}

// DeriveCallKey creates a unique key for deduplication within a batch.
// Uses the same derivation as cache keys for consistency.
func DeriveCallKey(req *common.NormalizedRequest) (string, error) {
	if req == nil {
		return "", fmt.Errorf("request is nil")
	}
	jrq, err := req.JsonRpcRequest()
	if err != nil {
		return "", err
	}

	jrq.RLock()
	method := jrq.Method
	params := jrq.Params
	jrq.RUnlock()

	// Use method + params as key (same as cache key derivation)
	paramsJSON, err := common.SonicCfg.Marshal(params)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s:%s", method, string(paramsJSON)), nil
}

// BatchEntry represents a request waiting in a batch.
type BatchEntry struct {
	Ctx       context.Context
	Request   *common.NormalizedRequest
	CallKey   string
	Target    []byte
	CallData  []byte
	ResultCh  chan BatchResult
	CreatedAt time.Time
	Deadline  time.Time
}

// BatchResult is the outcome delivered to a waiting request.
type BatchResult struct {
	Response *common.NormalizedResponse
	Error    error
}

// Batch holds pending requests for a single batching key.
type Batch struct {
	Key       BatchingKey
	Entries   []*BatchEntry
	CallKeys  map[string][]*BatchEntry // for deduplication
	FlushTime time.Time
	Flushing  bool
	mu        sync.Mutex
}

func NewBatch(key BatchingKey, flushTime time.Time) *Batch {
	return &Batch{
		Key:       key,
		Entries:   make([]*BatchEntry, 0, 16),
		CallKeys:  make(map[string][]*BatchEntry),
		FlushTime: flushTime,
	}
}
