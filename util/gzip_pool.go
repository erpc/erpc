package util

import (
	"compress/flate"
	"compress/gzip"
	"io"
	"sync"
)

// eofReader is used to reset gzip readers without allocating bufio.Reader
type eofReader struct{}

func (eofReader) Read([]byte) (int, error) { return 0, io.EOF }
func (eofReader) ReadByte() (byte, error)  { return 0, io.EOF }

// GzipReaderPool wraps a sync.Pool for gzip.Reader with safe Reset/Put helpers
// to minimize allocations and runtime buffer growth.
type GzipReaderPool struct {
	pool sync.Pool
}

func NewGzipReaderPool() *GzipReaderPool {
	return &GzipReaderPool{
		pool: sync.Pool{New: func() interface{} { return nil }},
	}
}

// GetReset returns a gzip.Reader reset to read from r. It may create a new gzip.Reader
// if none are available in the pool or if Reset fails on a pooled instance.
func (p *GzipReaderPool) GetReset(r io.Reader) (*gzip.Reader, error) {
	if pooled := p.pool.Get(); pooled != nil {
		zr := pooled.(*gzip.Reader)
		if err := zr.Reset(r); err == nil {
			return zr, nil
		}
		// Failed to reset; fall through to create a new reader.
	}
	return gzip.NewReader(r)
}

// Put returns a gzip.Reader to the pool after resetting it with a flate.Reader to avoid
// allocating a new bufio.Reader on the next Reset call.
func (p *GzipReaderPool) Put(zr *gzip.Reader) {
	if zr == nil {
		return
	}
	var fr flate.Reader = eofReader{}
	_ = zr.Reset(fr)
	p.pool.Put(zr)
}

// pooledGzipReadCloser wraps a gzip.Reader so that closing it returns it to the pool.
type pooledGzipReadCloser struct {
	zr   *gzip.Reader
	pool *GzipReaderPool
	once sync.Once
}

func (pgrc *pooledGzipReadCloser) Read(b []byte) (int, error) { return pgrc.zr.Read(b) }

func (pgrc *pooledGzipReadCloser) Close() error {
	var err error
	pgrc.once.Do(func() {
		// Close underlying gzip reader first, then return to pool exactly once.
		err = pgrc.zr.Close()
		pgrc.pool.Put(pgrc.zr)
		// Clear references to avoid accidental reuse and help GC
		pgrc.zr = nil
		pgrc.pool = nil
	})
	return err
}

// WrapGzipReader returns an io.ReadCloser wrapper that will return the gzip.Reader to pool on Close.
func (p *GzipReaderPool) WrapGzipReader(zr *gzip.Reader) io.ReadCloser {
	return &pooledGzipReadCloser{zr: zr, pool: p}
}
