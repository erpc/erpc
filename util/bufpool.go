package util

import (
	"bytes"
	"sync"
)

const maxBufCap = 16 << 10 // 16 KiB

var byteBufPool = sync.Pool{
	New: func() any { return bytes.NewBuffer(make([]byte, 0, maxBufCap)) },
}

// borrowBuf returns a cleared *bytes.Buffer from the pool.
func borrowBuf() *bytes.Buffer {
	buf := byteBufPool.Get().(*bytes.Buffer)
	buf.Reset()
	return buf
}

// returnBuf puts buf back in the pool when its capacity is reasonable.
func returnBuf(buf *bytes.Buffer) {
	if buf.Cap() <= maxBufCap {
		byteBufPool.Put(buf)
	}
}
