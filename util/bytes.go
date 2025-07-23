package util

import (
	"bytes"
	"context"
	"io"
)

type ByteWriter interface {
	WriteTo(w io.Writer, trimSides bool) (n int64, err error)
	IsResultEmptyish() bool
	Size(ctx ...context.Context) (int, error)
}

func IsBytesEmptyish(b []byte) bool {
	lnr := len(b)
	if lnr > 2 {
		if b[0] == '0' && (b[1] == 'x' || b[1] == 'X') {
			// Skip the 0x prefix and trim all leading zeros from the hex value
			remaining := bytes.TrimLeft(b[2:], "0")
			// If nothing remains or only the closing quote remains for quoted hex, it's empty
			if len(remaining) == 0 {
				return true
			}
			// Update b to check remaining content
			b = remaining
		} else if lnr > 4 && b[0] == '"' && b[1] == '0' && (b[2] == 'x' || b[2] == 'X') {
			b = bytes.TrimLeft(b[3:], "0")
			if len(b) == 1 {
				return true
			}
			return false
		}
	}
	lnr = len(b)
	if lnr > 4 {
		return false
	}
	if lnr == 0 ||
		(lnr == 4 && b[0] == '"' && b[1] == '0' && b[2] == 'x' && b[3] == '"') ||
		(lnr == 4 && b[0] == 'n' && b[1] == 'u' && b[2] == 'l' && b[3] == 'l') ||
		(lnr == 2 && b[0] == '"' && b[1] == '"') ||
		(lnr == 2 && b[0] == '0' && (b[1] == 'x' || b[1] == 'X')) ||
		(lnr == 2 && b[0] == '[' && b[1] == ']') ||
		(lnr == 1 && b[0] == '0') ||
		(lnr == 2 && b[0] == '{' && b[1] == '}') {
		return true
	}
	return false
}
