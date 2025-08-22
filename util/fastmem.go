package util

import (
	"io"
	"strings"
	"unsafe"
)

// B2Str converts a byte slice to a read‑only string without allocation.
//
// WARNING: The caller must **guarantee** that `bs` will not be mutated for as
// long as the returned string is alive, or a data race / memory corruption will
// occur.
//
//go:nosplit
//go:nocheckptr
func B2Str(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return unsafe.String(&b[0], len(b)) // #nosec G103
}

// S2Bytes converts a string to a mutable byte slice **without** copying.
//
// WARNING: Mutating the returned slice is *undefined behaviour* (strings are
// immutable by spec).  Use this only for read‑only access or when you can
// prove the string will never be reused elsewhere.
//
//go:nosplit
//go:nocheckptr
func S2Bytes(s string) (v []byte) {
	if len(s) == 0 {
		return nil
	}
	return unsafe.Slice(unsafe.StringData(s), len(s)) // #nosec G103
}

func StringToReaderCloser(s string) io.ReadCloser {
	return io.NopCloser(strings.NewReader(s))
}
