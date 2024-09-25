package util

import (
	"bytes"
	"io"
)

func ReadAll(reader io.Reader, chunkSize int64) ([]byte, error) {
	buf := bytes.NewBuffer(make([]byte, 0, chunkSize))
	for {
		n, err := io.CopyN(buf, reader, chunkSize)
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		if n == 0 {
			break
		}
	}

	return buf.Bytes(), nil
}
