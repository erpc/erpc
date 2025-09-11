package util

import (
	"bytes"
)

// pooledBufferReadCloser wraps a pooled *bytes.Buffer and exposes it as an io.ReadCloser.
// On Close, it returns the buffer to the pool via ReturnBuf.
type pooledBufferReadCloser struct {
	reader *bytes.Reader
	buf    *bytes.Buffer
}

// NewPooledBufferReadCloser creates a ReadCloser over the current contents of buf.
// The underlying buffer will be returned to the pool when Close is called.
func NewPooledBufferReadCloser(buf *bytes.Buffer) *pooledBufferReadCloser {
	if buf == nil {
		return &pooledBufferReadCloser{reader: bytes.NewReader(nil)}
	}
	return &pooledBufferReadCloser{reader: bytes.NewReader(buf.Bytes()), buf: buf}
}

func (p *pooledBufferReadCloser) Read(b []byte) (int, error) {
	return p.reader.Read(b)
}

func (p *pooledBufferReadCloser) Close() error {
	if p.buf != nil {
		ReturnBuf(p.buf)
		p.buf = nil
	}
	p.reader = nil
	return nil
}
