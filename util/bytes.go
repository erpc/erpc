package util

import (
	"bytes"
	"context"
	"io"
	"sync"
	"sync/atomic"
)

type ByteWriter interface {
	WriteTo(w io.Writer, trimSides bool) (n int64, err error)
	IsResultEmptyish() bool
	Size(ctx ...context.Context) (int, error)
}

// ReleasableByteWriter extends ByteWriter with a Release method for cleanup.
// Implementations should use Release to free resources safely without data races.
type ReleasableByteWriter interface {
	ByteWriter
	Release()
}

// ShallowCloneSafeByteWriter marks writers that are safe to share between shallow-cloned
// JsonRpcResponses (multiplexing/shadow fan-out) as long as their lifetime is reference-counted.
//
// Requirements:
// - WriteTo/IsResultEmptyish/Size must be safe to call concurrently.
// - Writer must be repeatable (not one-shot stream consumption), OR callers must ensure single-use.
//   (eRPC only marks repeatable writers as ShallowCloneSafeByteWriter.)
type ShallowCloneSafeByteWriter interface {
	ReleasableByteWriter
	ShallowCloneSafe()
}

type sharedWriterState struct {
	mu   sync.Mutex
	w    ShallowCloneSafeByteWriter
	refs atomic.Int64
}

// SharedReleasableByteWriter wraps a ShallowCloneSafeByteWriter with reference-counted Release(),
// preventing premature cleanup when the same writer is shared across shallow clones.
type SharedReleasableByteWriter struct {
	st *sharedWriterState
}

func NewSharedReleasableByteWriter(w ShallowCloneSafeByteWriter) *SharedReleasableByteWriter {
	if w == nil {
		return &SharedReleasableByteWriter{st: nil}
	}
	st := &sharedWriterState{w: w}
	st.refs.Store(1)
	return &SharedReleasableByteWriter{st: st}
}

func (w *SharedReleasableByteWriter) Clone() *SharedReleasableByteWriter {
	if w == nil || w.st == nil {
		return &SharedReleasableByteWriter{st: nil}
	}
	w.st.refs.Add(1)
	return &SharedReleasableByteWriter{st: w.st}
}

func (w *SharedReleasableByteWriter) ShallowCloneSafe() {}

func (w *SharedReleasableByteWriter) WriteTo(out io.Writer, trimSides bool) (n int64, err error) {
	if w == nil || w.st == nil {
		return 0, nil
	}
	w.st.mu.Lock()
	inner := w.st.w
	w.st.mu.Unlock()
	if inner == nil {
		return 0, nil
	}
	return inner.WriteTo(out, trimSides)
}

func (w *SharedReleasableByteWriter) IsResultEmptyish() bool {
	if w == nil || w.st == nil {
		return true
	}
	w.st.mu.Lock()
	inner := w.st.w
	w.st.mu.Unlock()
	if inner == nil {
		return true
	}
	return inner.IsResultEmptyish()
}

func (w *SharedReleasableByteWriter) Size(ctx ...context.Context) (int, error) {
	if w == nil || w.st == nil {
		return 0, nil
	}
	w.st.mu.Lock()
	inner := w.st.w
	w.st.mu.Unlock()
	if inner == nil {
		return 0, nil
	}
	return inner.Size(ctx...)
}

func (w *SharedReleasableByteWriter) Release() {
	if w == nil || w.st == nil {
		return
	}
	if w.st.refs.Add(-1) != 0 {
		return
	}
	w.st.mu.Lock()
	inner := w.st.w
	w.st.w = nil
	w.st.mu.Unlock()
	if inner != nil {
		inner.Release()
	}
}

// IsBytesEmptyish reports whether b represents a semantically empty JSON-RPC result value.
// It returns true for: empty bytes, "0", "0x"/"0X", "0x0000..." (any number of zeros),
// quoted variants ("0x", "0x000..."), null, empty string (""), empty array ([]), and empty object ({}).
func IsBytesEmptyish(b []byte) bool {
	lnr := len(b)
	if lnr > 2 {
		if b[0] == '0' && (b[1] == 'x' || b[1] == 'X') {
			// Skip the 0x prefix and trim all leading zeros from the hex value
			remaining := bytes.TrimLeft(b[2:], "0")
			// If nothing remains after stripping leading zeros, the hex value is zero-valued
			if len(remaining) == 0 {
				return true
			}
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
