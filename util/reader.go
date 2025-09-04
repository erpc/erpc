package util

import (
	"bytes"
	"io"
)

func ReadAll(r io.Reader, expected int) ([]byte, error) {
	// Use expected purely as allocation hint, nothing else
	if expected > 0 {
		buf := bytes.NewBuffer(make([]byte, 0, expected))
		_, err := io.Copy(buf, r)
		return buf.Bytes(), err
	}
	return io.ReadAll(r)
}
