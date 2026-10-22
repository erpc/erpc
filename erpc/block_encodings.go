package erpc

import (
	"encoding/json"
	"sync"

	"github.com/erpc/erpc/indexer"
)

// blockEncodingsKept bounds the encoded blocks kept for reuse.
const blockEncodingsKept = 256

// blockEncodings encodes each block once for every tap of one transport
// (spec §9.1): concurrent taps on one block wait for the one encoding.
type blockEncodings[T any] struct {
	encode func(header json.RawMessage) (T, error)

	mu    sync.Mutex
	byKey map[[32]byte]*blockEncoding[T]
	order [][32]byte
}

type blockEncoding[T any] struct {
	once sync.Once
	v    T
	err  error
}

func newBlockEncodings[T any](encode func(header json.RawMessage) (T, error)) *blockEncodings[T] {
	return &blockEncodings[T]{encode: encode, byKey: make(map[[32]byte]*blockEncoding[T], blockEncodingsKept)}
}

func (p *blockEncodings[T]) get(b indexer.Block) (T, error) {
	p.mu.Lock()
	e := p.byKey[b.Hash]
	if e == nil {
		e = &blockEncoding[T]{}
		p.byKey[b.Hash] = e
		p.order = append(p.order, b.Hash)
		if len(p.order) > blockEncodingsKept {
			delete(p.byKey, p.order[0])
			p.order = p.order[1:]
		}
	}
	p.mu.Unlock()
	e.once.Do(func() { e.v, e.err = p.encode(b.Header) })
	return e.v, e.err
}
