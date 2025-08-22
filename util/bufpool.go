package util

import (
	"bytes"
	"sync"
)

const maxBufCap = 16 << 10 // 16 KiB

var byteBufPool = sync.Pool{
	New: func() any { return bytes.NewBuffer(make([]byte, 0, maxBufCap)) },
}

// BorrowBuf returns a cleared *bytes.Buffer from the pool.
func BorrowBuf() *bytes.Buffer {
	buf := byteBufPool.Get().(*bytes.Buffer)
	buf.Reset()
	return buf
}

// ReturnBuf puts buf back in the pool when its capacity is reasonable.
func ReturnBuf(buf *bytes.Buffer) {
	if buf == nil {
		return
	}
	// Only return buffers with reasonable capacity to prevent memory bloat.
	// Without this check, large buffers would accumulate in the pool, consuming
	// excessive memory. The pool would grow unbounded as each large buffer
	// stays allocated even when not in use.
	if buf.Cap() <= maxBufCap {
		byteBufPool.Put(buf)
	}
}
