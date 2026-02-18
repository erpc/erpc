package common

import "testing"

func TestJsonRpcResponse_CloneShallow_SharesResultBytes(t *testing.T) {
	orig := &JsonRpcResponse{}
	b := make([]byte, 4*1024*1024)
	orig.SetResult(b)
	_ = orig.SetID(1)

	clone, err := orig.CloneShallow()
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if clone == nil {
		t.Fatalf("expected clone")
	}

	ob := orig.GetResultBytes()
	cb := clone.GetResultBytes()
	if len(ob) != len(cb) {
		t.Fatalf("len mismatch: %d != %d", len(ob), len(cb))
	}
	if len(ob) == 0 {
		t.Fatalf("unexpected empty result")
	}
	if &ob[0] != &cb[0] {
		t.Fatalf("expected shallow clone to share result backing array")
	}
}
