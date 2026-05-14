package util

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
)

// countingCloser tracks Close call count + records prior Close error if any.
type countingCloser struct {
	io.Reader
	closes  atomic.Int32
	closeErr error
}

func (c *countingCloser) Close() error {
	c.closes.Add(1)
	return c.closeErr
}

func gzipped(t *testing.T, payload string) io.Reader {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

// pooledGzipReadCloser.Close MUST close the underlying source.
// Regression test for the leak in clients/http_json_rpc_client.go single-
// request path: gzip wrapper close used to leave resp.Body open, pinning the
// HTTP/2 stream and accumulating http2ClientConn.readLoop goroutines.
func TestPooledGzipReadCloser_ClosesSource(t *testing.T) {
	pool := NewGzipReaderPool()
	src := &countingCloser{Reader: gzipped(t, "hello")}

	zr, err := pool.GetReset(src)
	if err != nil {
		t.Fatalf("GetReset: %v", err)
	}
	rc := pool.WrapGzipReader(zr, src)

	// Drain
	if _, err := io.ReadAll(rc); err != nil {
		t.Fatalf("read: %v", err)
	}

	if err := rc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if got := src.closes.Load(); got != 1 {
		t.Fatalf("expected source.Close called exactly once, got %d", got)
	}
}

// Close must be idempotent across many goroutines and only Close the source
// once even under contention.
func TestPooledGzipReadCloser_CloseIdempotent(t *testing.T) {
	pool := NewGzipReaderPool()
	src := &countingCloser{Reader: gzipped(t, "hello")}

	zr, err := pool.GetReset(src)
	if err != nil {
		t.Fatalf("GetReset: %v", err)
	}
	rc := pool.WrapGzipReader(zr, src)

	if _, err := io.ReadAll(rc); err != nil {
		t.Fatalf("read: %v", err)
	}

	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() { defer wg.Done(); _ = rc.Close() }()
	}
	wg.Wait()

	if got := src.closes.Load(); got != 1 {
		t.Fatalf("expected source.Close called exactly once even under contention, got %d", got)
	}
}

// Source is optional — passing nil is supported (some callers manage the
// source lifetime themselves) and Close should not panic.
func TestPooledGzipReadCloser_NilSourceOk(t *testing.T) {
	pool := NewGzipReaderPool()
	src := gzipped(t, "hello")

	zr, err := pool.GetReset(src)
	if err != nil {
		t.Fatalf("GetReset: %v", err)
	}
	rc := pool.WrapGzipReader(zr, nil)

	if _, err := io.ReadAll(rc); err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("close with nil source: %v", err)
	}
}

// If the source's Close errors, the wrapper surfaces that error (preferring
// the gzip-reader error if both fail, since gzip is closed first).
func TestPooledGzipReadCloser_PropagatesSourceError(t *testing.T) {
	pool := NewGzipReaderPool()
	wantErr := errors.New("source close failed")
	src := &countingCloser{Reader: gzipped(t, "hello"), closeErr: wantErr}

	zr, err := pool.GetReset(src)
	if err != nil {
		t.Fatalf("GetReset: %v", err)
	}
	rc := pool.WrapGzipReader(zr, src)
	if _, err := io.ReadAll(rc); err != nil {
		t.Fatalf("read: %v", err)
	}

	if err := rc.Close(); !errors.Is(err, wantErr) {
		t.Fatalf("expected source close error to propagate, got %v", err)
	}
}
