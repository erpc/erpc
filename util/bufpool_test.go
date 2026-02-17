package util

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBufPool_DoesNotRetainHugeBuffers(t *testing.T) {
	// Grow beyond maxBufCap (64KiB). Without the cap guard in ReturnBuf,
	// this buffer would be put back in the pool and returned by BorrowBuf,
	// pinning large allocations across requests.
	b := BorrowBuf()
	_, _ = b.Write(bytes.Repeat([]byte{'x'}, (maxBufCap*8)+1))
	require.Greater(t, b.Cap(), maxBufCap)

	ReturnBuf(b)

	b2 := BorrowBuf()
	defer ReturnBuf(b2)
	require.LessOrEqual(t, b2.Cap(), maxBufCap)
}

