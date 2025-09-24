package util

import (
	"io"
)

const maxPreGrowSize = 200 * 1024 * 1024 // 200MB

// ReadAll reads all from r and returns the data plus a function to return the buffer.
// Callers SHOULD call the return function when done with the data to enable buffer reuse.
// If returnFunc is nil, no cleanup is needed.
func ReadAll(r io.Reader, expected int) (data []byte, returnFunc func(), err error) {
	buf := BorrowBuf()

	// Pre-grow if we know the size
	if expected > 0 {
		if expected <= maxPreGrowSize {
			buf.Grow(expected)
		} else {
			buf.Grow(maxPreGrowSize)
		}
	}

	_, err = io.Copy(buf, r)
	if err != nil {
		ReturnBuf(buf)
		return nil, nil, err
	}

	data = buf.Bytes()
	return data, func() { ReturnBuf(buf) }, nil
}
