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

// ReturnBuf puts buf back in the pool when its capacity is reasonable.
func ReturnBuf(buf *bytes.Buffer) {
	if buf == nil {
		return
	}
	// Critical: don't pool huge buffers. Otherwise a single large response can
	// pin multi-GB allocations in the pool and cause sustained RSS growth.
	if buf.Cap() > maxBufCap {
		return
	}
	buf.Reset()
	byteBufPool.Put(buf)
}
