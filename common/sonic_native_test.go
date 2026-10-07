//go:build amd64 || arm64

package common

import (
	"reflect"
	"testing"

	"github.com/bytedance/sonic"
)

// TestSonicNativePathBuiltIn guards against sonic silently falling back to
// encoding/json (it does so on toolchains newer than it supports, printing
// only a stderr warning). The native frozen config carries encoder/decoder
// options; the encoding/json fallback's does not.
func TestSonicNativePathBuiltIn(t *testing.T) {
	typ := reflect.TypeOf(sonic.ConfigDefault)
	if typ.Kind() == reflect.Ptr {
		typ = typ.Elem()
	}
	if _, ok := typ.FieldByName("decoderOpts"); !ok {
		t.Fatalf("sonic is built with its encoding/json fallback (%s): bump github.com/bytedance/sonic for this Go toolchain", typ)
	}
}
