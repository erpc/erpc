package util

import "io"

type ByteWriter interface {
	WriteTo(w io.Writer) (n int64, err error)
}
