package common

import (
	"context"
	"io"
	"sync/atomic"
	"testing"

	"github.com/erpc/erpc/util"
)

type testRepeatableWriter struct {
	released atomic.Int64
}

func (w *testRepeatableWriter) ShallowCloneSafe() {}

func (w *testRepeatableWriter) WriteTo(out io.Writer, trimSides bool) (n int64, err error) {
	_ = trimSides
	nn, err := out.Write([]byte(`["ok"]`))
	return int64(nn), err
}

func (w *testRepeatableWriter) IsResultEmptyish() bool { return false }
func (w *testRepeatableWriter) Size(ctx ...context.Context) (int, error) {
	_ = ctx
	return len(`["ok"]`), nil
}
func (w *testRepeatableWriter) Release() { w.released.Add(1) }

// Regression: CloneShallow must not fail for safe repeatable resultWriters used by eth_getLogs merges.
// It must also refcount Release so clones cannot free the underlying writer prematurely.
func TestJsonRpcResponse_CloneShallow_ResultWriter_Shareable(t *testing.T) {
	w := &testRepeatableWriter{}
	jrr := &JsonRpcResponse{}
	jrr.SetResultWriter(w)

	c1, err := jrr.CloneShallow()
	if err != nil {
		t.Fatalf("CloneShallow unexpected error: %v", err)
	}
	c2, err := jrr.CloneShallow()
	if err != nil {
		t.Fatalf("CloneShallow unexpected error: %v", err)
	}

	// Free 2 clones; underlying writer must not be released yet (original still holds a ref).
	c1.Free()
	c2.Free()
	if got := w.released.Load(); got != 0 {
		t.Fatalf("expected underlying writer not released yet; got released=%d", got)
	}

	// Free original; now underlying writer should be released once.
	jrr.Free()
	if got := w.released.Load(); got != 1 {
		t.Fatalf("expected underlying writer released once; got released=%d", got)
	}

	// Sanity: ensure shared wrapper type is used.
	jrr2 := &JsonRpcResponse{}
	jrr2.SetResultWriter(w)
	_, _ = jrr2.CloneShallow()
	jrr2.resultMu.RLock()
	_, ok := jrr2.resultWriter.(*util.SharedReleasableByteWriter)
	jrr2.resultMu.RUnlock()
	if !ok {
		t.Fatalf("expected resultWriter to be wrapped in SharedReleasableByteWriter")
	}
}
