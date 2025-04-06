package common

import (
	"reflect"
	"runtime"
	"testing"
	"time"
)

func TestEnsureCachedNode_HeapAllocation(t *testing.T) {
	// Create a JsonRpcResponse
	r := &JsonRpcResponse{
		Result: []byte(`{"id":1,"error":null,"result":"value"}`),
	}

	// First call to ensure we have a cached node
	if err := r.ensureCachedNode(); err != nil {
		t.Fatalf("First ensureCachedNode failed: %v", err)
	}

	// Save the address of the cached node
	originalCachedNode := r.cachedNode

	// Verify it's not nil
	if originalCachedNode == nil {
		t.Fatal("cachedNode is nil after initial call")
	}

	// Make a copy of the node data to compare later
	originalNodeValue := *originalCachedNode

	// Run GC to ensure our node stays alive
	for i := 0; i < 10; i++ {
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
	}

	// Second call should use the existing cached node
	if err := r.ensureCachedNode(); err != nil {
		t.Fatalf("Second ensureCachedNode failed: %v", err)
	}

	// Verify the node address stayed the same (proper caching)
	if r.cachedNode != originalCachedNode {
		t.Fatal("cached node address changed on second call")
	}

	// Verify the node content is still intact
	if !reflect.DeepEqual(*r.cachedNode, originalNodeValue) {
		t.Fatal("cached node content changed unexpectedly")
	}
}
