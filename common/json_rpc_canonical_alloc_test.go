package common

import (
	"testing"
)

// Test that CanonicalHash caches the result and repeated calls allocate ~0.
func TestJsonRpcResponse_CanonicalHash_AllocsCached(t *testing.T) {
	// Cannot use t.Parallel() with testing.AllocsPerRun
	r := &JsonRpcResponse{result: []byte(ethGetBlockByNumberResponse)}

	// First compute warms the cache
	if _, err := r.CanonicalHash(); err != nil {
		t.Fatalf("first canonical hash failed: %v", err)
	}

	allocs := testing.AllocsPerRun(500, func() {
		_, _ = r.CanonicalHash()
	})
	// Allow a tiny threshold in case of runtime variations
	if allocs > 0.5 {
		t.Fatalf("expected ~0 allocs on cached CanonicalHash, got %.2f", allocs)
	}
}

// Test that CanonicalHashWithIgnoredFields caches by the slice pointer and
// repeated calls with the same backing slice allocate ~0.
func TestJsonRpcResponse_CanonicalHashWithIgnoredFields_AllocsCached(t *testing.T) {
	// Cannot use t.Parallel() with testing.AllocsPerRun
	r := &JsonRpcResponse{result: []byte(ethGetLogsResponse)}
	ignore := []string{"logs.*.blockNumber", "logs.*.transactionIndex"}

	// Warm cache
	if _, err := r.CanonicalHashWithIgnoredFields(ignore); err != nil {
		t.Fatalf("first canonical hash (ignored) failed: %v", err)
	}

	allocs := testing.AllocsPerRun(500, func() {
		_, _ = r.CanonicalHashWithIgnoredFields(ignore)
	})
	if allocs > 0.5 {
		t.Fatalf("expected ~0 allocs on cached CanonicalHashWithIgnoredFields, got %.2f", allocs)
	}
}
