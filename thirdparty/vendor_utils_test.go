package thirdparty

import (
	"errors"
	"testing"
)

type closeFunc func() error

func (fn closeFunc) Close() error {
	return fn()
}

func TestCloseResponseBody(t *testing.T) {
	precedingErr := errors.New("request failed")
	closeErr := errors.New("close failed")

	err := closeResponseBody(closeFunc(func() error { return closeErr }), precedingErr)
	if !errors.Is(err, precedingErr) {
		t.Fatalf("expected preceding error, got %v", err)
	}
	if !errors.Is(err, closeErr) {
		t.Fatalf("expected close error, got %v", err)
	}
}
