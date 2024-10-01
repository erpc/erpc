package util

import (
	"bytes"
	"io"
)

func ReadAll(reader io.Reader, chunkSize int64, expectedSize int) ([]byte, error) {
	buf := bytes.NewBuffer(make([]byte, 0, 16*1024)) // 16KB default

	if expectedSize > 0 && expectedSize < 50*1024*1024 { // 50MB cap to avoid DDoS by a corrupt/malicious upstream
		n := expectedSize - buf.Cap()
		if n > 0 {
			buf.Grow(n)
		}
	}

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
