package util

import (
	"io"
)

func ReadAll(r io.Reader, chunkSize int64, expected int) ([]byte, error) {
	buf := BorrowBuf()
	defer ReturnBuf(buf)

	// grow up to expected when it is smaller than pool cap and reasonable
	if expected > 0 && expected < maxBufCap {
		if need := expected - buf.Cap(); need > 0 {
			buf.Grow(need)
		}
	}

	for {
		n, err := io.CopyN(buf, r, chunkSize)
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

	// copy out to a minimal slice so caller cannot retain pooled buffer
	out := make([]byte, buf.Len())
	copy(out, buf.Bytes())
	return out, nil
}
