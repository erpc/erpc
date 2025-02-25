package util

import "io"

type ByteWriter interface {
	WriteTo(w io.Writer, trimSides bool) (n int64, err error)
	IsResultEmptyish() bool
}

func IsBytesEmptyish(b []byte) bool {
	lnr := len(b)
	if lnr > 4 {
		return false
	}
	if lnr == 0 ||
		(lnr == 4 && b[0] == '"' && b[1] == '0' && b[2] == 'x' && b[3] == '"') ||
		(lnr == 4 && b[0] == 'n' && b[1] == 'u' && b[2] == 'l' && b[3] == 'l') ||
		(lnr == 2 && b[0] == '"' && b[1] == '"') ||
		(lnr == 2 && b[0] == '[' && b[1] == ']') ||
		(lnr == 2 && b[0] == '{' && b[1] == '}') {
		return true
	}
	return false
}
