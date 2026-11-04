package data

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/erpc/erpc/common"
)

// ErrPatternDeleteUnsupported is returned when a connector cannot purge by
// pattern. Admin metrics/labels must use errors.Is against this sentinel —
// never substring-match error text.
var ErrPatternDeleteUnsupported = errors.New("pattern delete unsupported")

// PatternDeleter is an optional connector capability used by admin cache purge.
// Connectors that do not implement it are reported as unsupported per-row.
type PatternDeleter interface {
	DeleteByPattern(ctx context.Context, opts PatternDeleteOpts) (matched, deleted int64, err error)
}

// PatternDeleteOpts controls a pattern-based delete (or dry-run count).
type PatternDeleteOpts struct {
	// MainPatterns are glob patterns matching physical main keys
	// (Redis: partitionKey:rangeKey) or partition-key prefixes (SQL/Dynamo).
	MainPatterns []string
	// RviPatterns match reverse-index companion keys (Redis/Memory rvi#…).
	// Empty for drivers that use a GSI/index instead of companion keys.
	RviPatterns []string
	DryRun      bool
	// UnsuffixedOnly drops SCAN over-matches when scope.Suffix == "" so
	// evm:1:* does not delete evm:1:systx:… siblings on a shared store.
	UnsuffixedOnly bool
	NetworkId      string
	Method         string // optional range-key method prefix filter for SQL/Dynamo
	BlockRef       string
	Suffix         string
}

// BuildPurgePatterns returns Redis/Memory-style globs for main keys and rvi#
// companions from a CachePurgeScope. Always uses CachePartitionKey — never
// invents key parsing that splits on ':' blindly.
func BuildPurgePatterns(scope common.CachePurgeScope) (main []string, rvi []string) {
	switch {
	case scope.BlockRef != "" && scope.Method != "":
		part := common.CachePartitionKey(scope.NetworkId, scope.Suffix, scope.BlockRef)
		main = []string{part + ":" + scope.Method + ":*"}
	case scope.BlockRef != "":
		part := common.CachePartitionKey(scope.NetworkId, scope.Suffix, scope.BlockRef)
		main = []string{part + ":*"}
	case scope.Method != "":
		part := common.CachePartitionKey(scope.NetworkId, scope.Suffix, "*")
		main = []string{part + ":" + scope.Method + ":*"}
	default:
		// CachePartitionKey(..., "*") is already the network-wide glob (evm:1:*).
		main = []string{common.CachePartitionKey(scope.NetworkId, scope.Suffix, "*")}
	}

	// blockRef-scoped main keys are exact partitions; the rvi# wildcard is
	// network-wide (rvi#evm:1:*#*), so deleting companions by that pattern
	// would wipe reverse-index entries for every block. Skip rvi# for
	// blockRef scopes — dangling companions resolve to a cache miss.
	if scope.BlockRef != "" {
		return main, nil
	}

	wildcard := common.CachePartitionKey(scope.NetworkId, scope.Suffix, "*")
	if scope.Method != "" {
		rvi = []string{fmt.Sprintf("%s#%s#%s:*", redisReverseIndexPrefix, wildcard, scope.Method)}
	} else {
		rvi = []string{fmt.Sprintf("%s#%s#*", redisReverseIndexPrefix, wildcard)}
	}
	return main, rvi
}

// connectorDriverName returns a stable driver label for purge results.
func connectorDriverName(c Connector) string {
	switch v := c.(type) {
	case *RedisConnector:
		return RedisDriverName
	case *MemoryConnector:
		return MemoryDriverName
	case *DynamoDBConnector:
		return DynamoDBDriverName
	case *PostgreSQLConnector:
		return PostgreSQLDriverName
	case *GrpcConnector:
		return GrpcDriverName
	case *FailsafeConnector:
		return connectorDriverName(v.wrapped)
	case *MockConnector, *MockMemoryConnector:
		return "mock"
	default:
		return "unknown"
	}
}

// unwrapConnector returns the innermost connector behind FailsafeConnector.
func unwrapConnector(c Connector) Connector {
	for {
		if f, ok := c.(*FailsafeConnector); ok {
			c = f.wrapped
			continue
		}
		return c
	}
}

const cachePurgeLockTTL = 30 * time.Second

// PurgeConnectors runs pattern deletes across unique connectors referenced by
// cache policies. Read-only connectors (e.g. gRPC) are still walked so they
// surface ErrPatternDeleteUnsupported instead of vanishing from the response.
// Unsupported connectors yield a per-row result with Err set; others still run.
func PurgeConnectors(ctx context.Context, policies []*CachePolicy, scope common.CachePurgeScope) []common.CachePurgeResult {
	seen := make(map[string]struct{})
	var results []common.CachePurgeResult

	mainPatterns, rviPatterns := BuildPurgePatterns(scope)
	opts := PatternDeleteOpts{
		MainPatterns:   mainPatterns,
		RviPatterns:    rviPatterns,
		DryRun:         scope.DryRun,
		UnsuffixedOnly: scope.Suffix == "",
		NetworkId:      scope.NetworkId,
		Method:         scope.Method,
		BlockRef:       scope.BlockRef,
		Suffix:         scope.Suffix,
	}

	for _, policy := range policies {
		if policy == nil {
			continue
		}
		conn := policy.GetConnector()
		if conn == nil {
			continue
		}
		id := conn.Id()
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}

		start := time.Now()
		inner := unwrapConnector(conn)
		row := common.CachePurgeResult{
			ConnectorId: id,
			Driver:      connectorDriverName(inner),
		}

		// Best-effort lock. PostgreSQL advisory locks hold a pooled connection
		// until Unlock; with minConns=maxConns=1 that deadlocks DeleteByPattern
		// waiting for another connection — skip the lock on that driver.
		lockKey := "cache-purge/" + scope.NetworkId
		var held DistributedLock
		if holdPurgeDistributedLock(inner) {
			if lock, err := conn.Lock(ctx, lockKey, cachePurgeLockTTL); err == nil && lock != nil && !lock.IsNil() {
				held = lock
			}
		}

		deleter, ok := inner.(PatternDeleter)
		if !ok {
			deleter, ok = conn.(PatternDeleter)
		}
		if !ok {
			if held != nil {
				_ = held.Unlock(ctx)
			}
			row.Err = fmt.Errorf("connector %q (%s): %w", id, row.Driver, ErrPatternDeleteUnsupported)
			row.DurationMs = time.Since(start).Milliseconds()
			results = append(results, row)
			continue
		}

		matched, deleted, err := deleter.DeleteByPattern(ctx, opts)
		if held != nil {
			_ = held.Unlock(ctx)
		}
		row.Matched = matched
		row.Deleted = deleted
		row.Err = err
		row.DurationMs = time.Since(start).Milliseconds()
		results = append(results, row)
	}

	return results
}

// holdPurgeDistributedLock is false for PostgreSQL: its Lock keeps a pool
// connection checked out for the lock TTL, which starves DeleteByPattern when
// the pool has only one connection.
func holdPurgeDistributedLock(inner Connector) bool {
	_, isPG := inner.(*PostgreSQLConnector)
	return !isPG
}

// matchUnsuffixedMainKey reports whether a physical Redis/Memory main key
// belongs to an unsuffixed partition for networkId.
//
// When method is non-empty, the method marker splits partition from range so
// colon-containing method names (SVM) are not mistaken for a cacheKeySuffix
// segment. When method is empty, falls back to "exactly two unescaped ':' in
// the remainder" (blockRef:method:hash) — safe for EVM, may miss rare
// colon-in-method keys under a network-wide unsuffixed purge.
func matchUnsuffixedMainKey(networkId, key, method string) bool {
	if strings.HasPrefix(key, redisReverseIndexPrefix+"#") {
		return matchUnsuffixedRviKey(networkId, key)
	}
	prefix := networkId + ":"
	if !strings.HasPrefix(key, prefix) {
		return false
	}
	rest := key[len(prefix):]
	if method != "" {
		return unsuffixedRestBeforeMethod(rest, method)
	}
	return countUnescapedColons(rest) == 2
}

// unsuffixedRestBeforeMethod is true when rest is `{escapedBlockRef}:{method}:…`
// with no extra unescaped partition segment (which would be a suffix sibling).
func unsuffixedRestBeforeMethod(rest, method string) bool {
	marker := method + ":"
	// Prefer method:hash form; also accept a bare trailing method.
	idx := strings.Index(rest, ":"+marker)
	if idx >= 0 {
		return countUnescapedColons(rest[:idx]) == 0
	}
	if strings.HasSuffix(rest, ":"+method) {
		return countUnescapedColons(rest[:len(rest)-len(method)-1]) == 0
	}
	if rest == method {
		return true
	}
	return false
}

// matchUnsuffixedRviKey keeps only reverse-index companions whose wildcard
// partition is exactly CachePartitionKey(networkId, "", "*").
func matchUnsuffixedRviKey(networkId, key string) bool {
	expected := fmt.Sprintf("%s#%s#", redisReverseIndexPrefix, common.CachePartitionKey(networkId, "", "*"))
	return strings.HasPrefix(key, expected)
}

func countUnescapedColons(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			continue
		}
		if s[i] == ':' {
			n++
		}
	}
	return n
}

// partitionMatchesScope reports whether a stored partition_key matches the
// purge scope for SQL/Dynamo drivers (separate partition/range columns).
func partitionMatchesScope(partitionKey string, opts PatternDeleteOpts) bool {
	if opts.BlockRef != "" {
		return partitionKey == common.CachePartitionKey(opts.NetworkId, opts.Suffix, opts.BlockRef)
	}
	if opts.Suffix != "" {
		prefix := common.CachePartitionKey(opts.NetworkId, opts.Suffix, "")
		return strings.HasPrefix(partitionKey, prefix)
	}
	// Unsuffixed network-wide: partition is networkId:blockRef with no extra
	// unescaped suffix segment after networkId.
	prefix := opts.NetworkId + ":"
	if !strings.HasPrefix(partitionKey, prefix) {
		return false
	}
	if opts.UnsuffixedOnly {
		return countUnescapedColons(partitionKey[len(prefix):]) == 0
	}
	return true
}

func rangeMatchesMethod(rangeKey, method string) bool {
	if method == "" {
		return true
	}
	return strings.HasPrefix(rangeKey, method+":") || rangeKey == method
}
