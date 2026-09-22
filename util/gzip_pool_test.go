package util

import (
	"bytes"
	stdgzip "compress/gzip"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// syntheticJsonRpcPayload builds a payload shaped like a large eth_getLogs /
// block-receipts JSON-RPC result, which is the dominant class of responses
// the HTTP gzip path compresses in production.
func syntheticJsonRpcPayload(entries int) []byte {
	var sb strings.Builder
	sb.WriteString(`{"jsonrpc":"2.0","id":1,"result":[`)
	for i := range entries {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb,
			`{"address":"0x%040x","topics":["0x%064x","0x%064x"],"data":"0x%0128x","blockNumber":"0x%x","transactionHash":"0x%064x","logIndex":"0x%x","removed":false}`,
			i, i*7, i*13, i*104729, 21000000+i, i*31, i%300)
	}
	sb.WriteString(`]}`)
	return []byte(sb.String())
}

// The pool writer must produce standard RFC 1952 gzip streams that stdlib
// readers (i.e. any HTTP client) can decode, and the pool reader must decode
// streams produced by stdlib writers (i.e. any upstream server).
func TestGzipPools_InteropWithStdlib(t *testing.T) {
	payload := syntheticJsonRpcPayload(50)

	t.Run("pool writer -> stdlib reader", func(t *testing.T) {
		wp := NewGzipWriterPool()
		var buf bytes.Buffer
		zw := wp.Get(&buf)
		_, err := zw.Write(payload)
		require.NoError(t, err)
		require.NoError(t, zw.Close())
		wp.Put(zw)

		zr, err := stdgzip.NewReader(&buf)
		require.NoError(t, err)
		out, err := io.ReadAll(zr)
		require.NoError(t, err)
		assert.Equal(t, payload, out)
	})

	t.Run("stdlib writer -> pool reader", func(t *testing.T) {
		var buf bytes.Buffer
		zw := stdgzip.NewWriter(&buf)
		_, err := zw.Write(payload)
		require.NoError(t, err)
		require.NoError(t, zw.Close())

		rp := NewGzipReaderPool()
		zr, err := rp.GetReset(&buf)
		require.NoError(t, err)
		out, err := io.ReadAll(zr)
		require.NoError(t, err)
		rp.Put(zr)
		assert.Equal(t, payload, out)
	})
}

// Reused pooled writers/readers must keep producing valid streams after Reset.
func TestGzipPools_ReuseAfterPut(t *testing.T) {
	wp := NewGzipWriterPool()
	rp := NewGzipReaderPool()

	for i := range 3 {
		payload := syntheticJsonRpcPayload(10 + i)

		var buf bytes.Buffer
		zw := wp.Get(&buf)
		_, err := zw.Write(payload)
		require.NoError(t, err)
		require.NoError(t, zw.Close())
		wp.Put(zw)

		zr, err := rp.GetReset(&buf)
		require.NoError(t, err)
		out, err := io.ReadAll(zr)
		require.NoError(t, err)
		rp.Put(zr)
		assert.Equal(t, payload, out, "roundtrip %d", i)
	}
}

// BenchmarkGzipWriterPool_CompressJsonRpc measures the in-process CPU cost of
// compressing a large JSON-RPC response body — the per-request tax paid on the
// HTTP serving hop when enableGzip is on.
func BenchmarkGzipWriterPool_CompressJsonRpc(b *testing.B) {
	payload := syntheticJsonRpcPayload(2500) // ~1MB
	pool := NewGzipWriterPool()

	var sized bytes.Buffer
	zw := pool.Get(&sized)
	_, _ = zw.Write(payload)
	_ = zw.Close()
	pool.Put(zw)

	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		zw := pool.Get(io.Discard)
		_, _ = zw.Write(payload)
		_ = zw.Close()
		pool.Put(zw)
	}
	// Report the compression ratio so speed can be weighed against size.
	// (Must run after the loop: ResetTimer deletes user-reported metrics.)
	b.ReportMetric(float64(sized.Len())/float64(len(payload)), "ratio")
}

// BenchmarkGzipReaderPool_DecompressJsonRpc measures the cost of decompressing
// a gzipped upstream response body, which also runs in-process per request.
func BenchmarkGzipReaderPool_DecompressJsonRpc(b *testing.B) {
	payload := syntheticJsonRpcPayload(2500) // ~1MB
	var compressed bytes.Buffer
	zw := stdgzip.NewWriter(&compressed)
	_, _ = zw.Write(payload)
	_ = zw.Close()
	data := compressed.Bytes()

	pool := NewGzipReaderPool()
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		zr, err := pool.GetReset(bytes.NewReader(data))
		if err != nil {
			b.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, zr); err != nil {
			b.Fatal(err)
		}
		pool.Put(zr)
	}
}
