package evm

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/erpc/erpc/util"
	"github.com/klauspost/compress/zstd"
)

const (
	// Cache envelope format:
	// [0..3]  magic "ERPC"
	// [4]     version (1)
	// [5..12] big-endian unix seconds (cached-at)
	cacheEnvelopeMagic   = "ERPC"
	cacheEnvelopeVersion = byte(1)
	cacheEnvelopeHeader  = 4 + 1 + 8
)

func safeUint64ToInt64(v uint64) (int64, bool) {
	const maxInt64 = uint64(^uint64(0) >> 1)
	if v > maxInt64 {
		return 0, false
	}
	return int64(v), true
}

func wrapCacheEnvelope(payload []byte) ([]byte, bool) {
	if len(payload) == 0 {
		return payload, false
	}
	out := make([]byte, cacheEnvelopeHeader+len(payload))
	copy(out[:4], []byte(cacheEnvelopeMagic))
	out[4] = cacheEnvelopeVersion
	binary.BigEndian.PutUint64(out[5:13], uint64(time.Now().Unix()))
	copy(out[cacheEnvelopeHeader:], payload)
	return out, true
}

func unwrapCacheEnvelope(b []byte) ([]byte, int64, bool) {
	if len(b) < cacheEnvelopeHeader {
		return b, 0, false
	}
	if string(b[:4]) != cacheEnvelopeMagic || b[4] != cacheEnvelopeVersion {
		return b, 0, false
	}
	cachedAt, ok := safeUint64ToInt64(binary.BigEndian.Uint64(b[5:13]))
	if !ok {
		return b, 0, false
	}
	return b[cacheEnvelopeHeader:], cachedAt, true
}

// cacheResultStreamWriter streams a cached JSON-RPC "result" value (not full response)
// without materializing the decompressed bytes in memory.
//
// It is intentionally one-shot: callers should not expect to read twice.
// NormalizedResponse.Release -> JsonRpcResponse.Free will call Release().
type cacheResultStreamWriter struct {
	prefix   []byte
	r        io.Reader
	emptyish bool
	size     int // -1 => unknown

	releaseOnce sync.Once
	releaseFn   func()
}

func (w *cacheResultStreamWriter) IsResultEmptyish() bool {
	if w == nil {
		return true
	}
	return w.emptyish
}

func (w *cacheResultStreamWriter) Size(ctx ...context.Context) (int, error) {
	_ = ctx
	if w == nil {
		return 0, nil
	}
	if w.size >= 0 {
		return w.size, nil
	}
	return 0, fmt.Errorf("cache result size unknown")
}

func (w *cacheResultStreamWriter) Release() {
	if w == nil {
		return
	}
	w.releaseOnce.Do(func() {
		if w.releaseFn != nil {
			w.releaseFn()
		}
		w.prefix = nil
		w.r = nil
	})
}

func (w *cacheResultStreamWriter) reader() io.Reader {
	if w == nil {
		return bytes.NewReader(nil)
	}
	if w.r == nil {
		return bytes.NewReader(w.prefix)
	}
	if len(w.prefix) == 0 {
		return w.r
	}
	return io.MultiReader(bytes.NewReader(w.prefix), w.r)
}

func (w *cacheResultStreamWriter) WriteTo(out io.Writer, trimSides bool) (n int64, err error) {
	if w == nil {
		return 0, nil
	}
	r := w.reader()
	if !trimSides {
		return io.CopyBuffer(out, r, make([]byte, 32*1024))
	}
	return copyTrimSides(out, r)
}

// copyTrimSides streams r to out, dropping the first and last bytes of the stream.
// Used to write the inner JSON of array/object results (e.g. "[...]" -> "...").
func copyTrimSides(out io.Writer, r io.Reader) (n int64, err error) {
	// Discard first byte.
	var b1 [1]byte
	for {
		nn, rerr := r.Read(b1[:])
		if nn > 0 {
			break
		}
		if rerr != nil {
			if rerr == io.EOF {
				return 0, nil
			}
			return 0, rerr
		}
	}

	var buf [32 * 1024]byte
	var last [1]byte
	haveLast := false

	for {
		nn, rerr := r.Read(buf[:])
		if nn > 0 {
			chunk := buf[:nn]
			if !haveLast {
				if len(chunk) == 1 {
					last[0] = chunk[0]
					haveLast = true
				} else {
					wn, werr := out.Write(chunk[:len(chunk)-1])
					n += int64(wn)
					if werr != nil {
						return n, werr
					}
					last[0] = chunk[len(chunk)-1]
					haveLast = true
				}
			} else {
				wn, werr := out.Write(last[:])
				n += int64(wn)
				if werr != nil {
					return n, werr
				}
				if len(chunk) > 1 {
					wn, werr = out.Write(chunk[:len(chunk)-1])
					n += int64(wn)
					if werr != nil {
						return n, werr
					}
				}
				last[0] = chunk[len(chunk)-1]
			}
		}

		if rerr != nil {
			if rerr == io.EOF {
				// Drop last byte by not writing it.
				return n, nil
			}
			return n, rerr
		}
	}
}

// newCacheResultWriterFromCompressed builds a streaming writer over a compressed cache value.
// It strips the cache envelope if present and returns (writer, cachedAtUnix, envelopeOK, error).
func (c *EvmJsonRpcCache) newCacheResultWriterFromCompressed(compressedData []byte) (util.ReleasableByteWriter, int64, bool, error) {
	if c == nil || c.decoderPool == nil {
		return nil, 0, false, fmt.Errorf("cache decoder pool not configured")
	}

	decoderInterface := c.decoderPool.Get()
	if decoderInterface == nil {
		return nil, 0, false, fmt.Errorf("failed to get decoder from pool")
	}
	decoder, ok := decoderInterface.(*zstd.Decoder)
	if !ok {
		return nil, 0, false, fmt.Errorf("decoder pool returned wrong type")
	}

	release := func() {
		c.decoderPool.Put(decoder)
	}

	if err := decoder.Reset(bytes.NewReader(compressedData)); err != nil {
		release()
		return nil, 0, false, fmt.Errorf("failed to reset zstd decoder: %w", err)
	}

	// Peek envelope header from decompressed stream.
	hdr := make([]byte, cacheEnvelopeHeader)
	n, err := io.ReadFull(decoder, hdr)
	if err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			// Too small to contain an envelope; treat as legacy payload (replay bytes we read).
			prefix := hdr[:n]
			emptyish := util.IsBytesEmptyish(bytes.TrimSpace(prefix))
			return &cacheResultStreamWriter{
				prefix:    prefix,
				r:         decoder,
				emptyish:  emptyish,
				size:      -1,
				releaseFn: release,
			}, 0, false, nil
		}
		release()
		return nil, 0, false, fmt.Errorf("failed to read cache envelope header: %w", err)
	}

	if string(hdr[:4]) != cacheEnvelopeMagic || hdr[4] != cacheEnvelopeVersion {
		// Not an envelope; replay header bytes as payload prefix.
		return &cacheResultStreamWriter{
			prefix:    hdr,
			r:         decoder,
			emptyish:  false,
			size:      -1,
			releaseFn: release,
		}, 0, false, nil
	}

	cachedAt, ok := safeUint64ToInt64(binary.BigEndian.Uint64(hdr[5:13]))
	if !ok {
		// Can't safely parse; replay header bytes (legacy).
		return &cacheResultStreamWriter{
			prefix:    hdr,
			r:         decoder,
			emptyish:  false,
			size:      -1,
			releaseFn: release,
		}, 0, false, nil
	}

	// Read a tiny payload prefix for emptyish detection and to avoid a "first Read" syscall later.
	var peek [8]byte
	pn, perr := decoder.Read(peek[:])
	if perr != nil && perr != io.EOF {
		release()
		return nil, 0, false, fmt.Errorf("failed to read cache payload prefix: %w", perr)
	}
	prefix := peek[:pn]
	emptyish := util.IsBytesEmptyish(bytes.TrimSpace(prefix))

	return &cacheResultStreamWriter{
		prefix:    prefix,
		r:         decoder,
		emptyish:  emptyish,
		size:      -1,
		releaseFn: release,
	}, cachedAt, true, nil
}
