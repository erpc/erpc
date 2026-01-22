package util

import (
	"io"
	"sync"
	"testing"
)

// Test that reading after close returns EOF and does not panic.
func TestPooledBufferReadCloserReadAfterClose(t *testing.T) {
	t.Parallel()
	prc := NewPooledBufferReadCloser(nil)
	buf := make([]byte, 10)
	if n, err := prc.Read(buf); err != io.EOF || n != 0 {
		t.Fatalf("expected immediate EOF on nil buffer reader, got n=%d err=%v", n, err)
	}
	// Close should be idempotent
	if err := prc.Close(); err != nil {
		t.Fatalf("unexpected error on close: %v", err)
	}
	if n, err := prc.Read(buf); err != io.EOF || n != 0 {
		t.Fatalf("expected EOF after close, got n=%d err=%v", n, err)
	}
}

// Test concurrent Close and Read do not panic and eventually yield EOF.
func TestPooledBufferReadCloserConcurrent(t *testing.T) {
	t.Parallel()
	b := BorrowBuf()
	b.WriteString("hello world")
	prc := NewPooledBufferReadCloser(b)

	var wg sync.WaitGroup
	readIterations := 100
	wg.Add(2)

	go func() {
		defer wg.Done()
		buf := make([]byte, 4)
		for i := 0; i < readIterations; i++ {
			_, _ = prc.Read(buf) // ignore data/EOF
		}
	}()

	go func() {
		defer wg.Done()
		// Issue multiple closes; should be safe/idempotent
		for i := 0; i < readIterations; i++ {
			_ = prc.Close()
		}
	}()

	wg.Wait()
}
