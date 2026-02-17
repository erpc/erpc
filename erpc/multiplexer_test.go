package erpc

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
)

func TestMultiplexerClose_DoesNotCloneLargeResult(t *testing.T) {
	ctx := context.Background()

	orig := &common.JsonRpcResponse{}
	// Large-ish result slice (10MiB) to catch accidental deep clones.
	origResult := make([]byte, 10*1024*1024)
	orig.SetResult(origResult)
	_ = orig.SetID(1)

	resp := common.NewNormalizedResponse().WithJsonRpcResponse(orig)

	m := NewMultiplexer("hash")
	m.Close(ctx, resp, nil)

	<-m.done

	m.mu.RLock()
	stored := m.resp
	m.mu.RUnlock()
	if stored == nil {
		t.Fatalf("expected stored resp")
	}

	jrr, err := stored.JsonRpcResponse(ctx)
	if err != nil {
		t.Fatalf("unexpected parse err: %v", err)
	}
	if jrr == nil {
		t.Fatalf("expected json rpc response")
	}

	// Verify shared backing array (no deep copy).
	origBytes := orig.GetResultBytes()
	storedBytes := jrr.GetResultBytes()
	if len(origBytes) == 0 || len(storedBytes) == 0 {
		t.Fatalf("unexpected empty result bytes")
	}
	if len(origBytes) != len(storedBytes) {
		t.Fatalf("unexpected len mismatch: %d != %d", len(origBytes), len(storedBytes))
	}
	if &origBytes[0] != &storedBytes[0] {
		t.Fatalf("expected multiplexer to store response without deep clone")
	}
}
