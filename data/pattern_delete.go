package data

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/erpc/erpc/common"
)

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

// PurgeConnectors runs pattern deletes across unique writable connectors
// referenced by cache policies. Unsupported connectors yield a per-row
// result with Err set; other connectors still run.
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
		if ro, ok := conn.(ReadOnlyConnector); ok && ro.ReadOnly() {
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

		lockKey := "cache-purge/" + scope.NetworkId
		var held DistributedLock
		if lock, err := conn.Lock(ctx, lockKey, cachePurgeLockTTL); err == nil && lock != nil && !lock.IsNil() {
			held = lock
		}

		deleter, ok := inner.(PatternDeleter)
		if !ok {
			deleter, ok = conn.(PatternDeleter)
		}
		if !ok {
			if held != nil {
				_ = held.Unlock(ctx)
			}
			row.Err = fmt.Errorf("connector %q (%s) does not support pattern delete", id, row.Driver)
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

// matchUnsuffixedMainKey reports whether a physical Redis/Memory main key
// belongs to an unsuffixed partition for networkId. After the networkId
// prefix, an unsuffixed key has exactly two unescaped ':' separators in the
// remainder (blockRef:method:hash). A cacheKeySuffix sibling has three or more.
// Escaped colons inside block refs (\: ) are not counted.
func matchUnsuffixedMainKey(networkId, key string) bool {
	if strings.HasPrefix(key, redisReverseIndexPrefix+"#") {
		return matchUnsuffixedRviKey(networkId, key)
	}
	prefix := networkId + ":"
	if !strings.HasPrefix(key, prefix) {
		return false
	}
	return countUnescapedColons(key[len(prefix):]) == 2
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
