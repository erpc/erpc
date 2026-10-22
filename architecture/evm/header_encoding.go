package evm

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/indexer"
	"github.com/erpc/erpc/telemetry"
)

// HeaderEncoding is one transport's encoding of pinned headers (spec §9.1):
// every tap of the transport on a network shares one encoding per block.
// It lives on the held header, so it lasts as long as the tracker holds the
// block and goes when the window or the orphan cap drops it.
type HeaderEncoding[T any] struct {
	transport string
	encode    func(header json.RawMessage) (T, error)
}

type headerEncoded[T any] struct {
	once sync.Once
	v    T
	err  error
}

// NewHeaderEncoding is transport's encoding; transport labels panics.
func NewHeaderEncoding[T any](transport string, encode func(header json.RawMessage) (T, error)) *HeaderEncoding[T] {
	return &HeaderEncoding[T]{transport: transport, encode: encode}
}

// Of is b's encoding while b is pinned in t; pinned is false, and nothing
// is encoded, once b left the pins. One load of the published state decides
// both. A panic in the encoding is its error, for every later tap too.
func (e *HeaderEncoding[T]) Of(t *ChainTracker, b indexer.Block) (v T, pinned bool, err error) {
	h := t.state.Load().pinned(indexer.Cursor{Number: b.Number, Hash: b.Hash})
	if h == nil {
		return v, false, nil
	}
	x, ok := h.encodings.Load(e)
	if !ok {
		x, _ = h.encodings.LoadOrStore(e, &headerEncoded[T]{})
	}
	enc := x.(*headerEncoded[T])
	enc.once.Do(func() {
		defer func() {
			if rec := recover(); rec != nil {
				telemetry.MetricUnexpectedPanicTotal.WithLabelValues(
					"header-encoding",
					fmt.Sprintf("network:%s transport:%s", t.networkLabel(), e.transport),
					common.ErrorFingerprint(rec),
				).Inc()
				enc.err = fmt.Errorf("encoding panicked: %v", rec)
			}
		}()
		enc.v, enc.err = e.encode(h.raw)
	})
	return enc.v, true, enc.err
}
