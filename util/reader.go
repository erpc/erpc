package util

import (
	"errors"
	"fmt"
	"io"
)

const maxPreGrowSize = 200 * 1024 * 1024 // 200MB

var ErrReadLimitExceeded = errors.New("read limit exceeded")

type ReadLimitExceededError struct {
	Limit int64
}

func (e *ReadLimitExceededError) Error() string {
	return fmt.Sprintf("%v: limit=%d", ErrReadLimitExceeded, e.Limit)
}

func (e *ReadLimitExceededError) Unwrap() error { return ErrReadLimitExceeded }

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

// ReadAllMax reads all from r up to maxBytes.
// If the stream exceeds maxBytes, it returns ReadLimitExceededError and does not retain
// more than maxBytes+1 bytes in memory.
func ReadAllMax(r io.Reader, expected int, maxBytes int64) (data []byte, returnFunc func(), err error) {
	if maxBytes <= 0 {
		return ReadAll(r, expected)
	}

	buf := BorrowBuf()

	// Pre-grow if we know the size, but never beyond maxBytes.
	if expected > 0 {
		grow := int64(expected)
		if grow > maxPreGrowSize {
			grow = maxPreGrowSize
		}
		if grow > maxBytes {
			grow = maxBytes
		}
		if grow > 0 {
			buf.Grow(int(grow))
		}
	}

	// Read at most maxBytes+1 to detect overflow without unbounded allocation.
	limited := io.LimitReader(r, maxBytes+1)
	_, err = io.Copy(buf, limited)
	if err != nil {
		ReturnBuf(buf)
		return nil, nil, err
	}

	if int64(buf.Len()) > maxBytes {
		ReturnBuf(buf)
		return nil, nil, &ReadLimitExceededError{Limit: maxBytes}
	}

	data = buf.Bytes()
	return data, func() { ReturnBuf(buf) }, nil
}
