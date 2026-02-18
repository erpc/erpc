package util

import (
	"bytes"
	"sync"
)

const maxBufCap = 64 << 10 // 64 KiB

var byteBufPool = sync.Pool{
	New: func() any { return bytes.NewBuffer(make([]byte, 0, maxBufCap)) },
}

// BorrowBuf returns a cleared *bytes.Buffer from the pool.
func BorrowBuf() *bytes.Buffer {
	buf := byteBufPool.Get().(*bytes.Buffer)
	buf.Reset()
	return buf
}

// ReturnBuf puts buf back in the pool only if its capacity is reasonable.
// Oversized buffers (e.g., from large responses) are discarded to avoid
// bloating the pool with memory that won't be reused efficiently.
func ReturnBuf(buf *bytes.Buffer) {
	if buf == nil {
		return
	}
	if buf.Cap() > 4*maxBufCap {
		return // let GC reclaim oversized buffers
	}
	byteBufPool.Put(buf)
}
