package util

import (
	"io"
)

func ReadAll(r io.Reader, chunkSize int64, expected int) ([]byte, error) {
	return io.ReadAll(r)
}
