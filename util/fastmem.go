package util

import (
	"bytes"
	"io"
	"unsafe"
)

type GoSlice struct {
	Ptr unsafe.Pointer
	Len int
	Cap int
}

type GoString struct {
	Ptr unsafe.Pointer
	Len int
}

//go:nosplit
func Mem2Str(v []byte) (s string) {
	(*GoString)(unsafe.Pointer(&s)).Len = (*GoSlice)(unsafe.Pointer(&v)).Len // #nosec G103
	(*GoString)(unsafe.Pointer(&s)).Ptr = (*GoSlice)(unsafe.Pointer(&v)).Ptr // #nosec G103
	return
}

//go:nosplit
func Str2Mem(s string) (v []byte) {
	(*GoSlice)(unsafe.Pointer(&v)).Cap = (*GoString)(unsafe.Pointer(&s)).Len // #nosec G103
	(*GoSlice)(unsafe.Pointer(&v)).Len = (*GoString)(unsafe.Pointer(&s)).Len // #nosec G103
	(*GoSlice)(unsafe.Pointer(&v)).Ptr = (*GoString)(unsafe.Pointer(&s)).Ptr // #nosec G103
	return
}

func StringToReaderCloser(b string) io.ReadCloser {
	return io.NopCloser(bytes.NewBuffer(Str2Mem(b)))
}
