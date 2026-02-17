package evm

import (
	"bytes"
	"sync"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
)

func TestCacheResultWriterFromCompressed_Envelope_StripsAndStreams(t *testing.T) {
	payload := []byte(`[{"a":1},{"b":2}]`)
	env, wrapped := wrapCacheEnvelope(payload)
	require.True(t, wrapped)

	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest))
	require.NoError(t, err)
	compressed := enc.EncodeAll(env, nil)

	cache := &EvmJsonRpcCache{
		compressionEnabled: true,
		decoderPool: &sync.Pool{New: func() interface{} {
			d, _ := zstd.NewReader(nil)
			return d
		}},
	}

	w, cachedAt, ok, err := cache.newCacheResultWriterFromCompressed(compressed)
	require.NoError(t, err)
	require.True(t, ok)
	require.NotZero(t, cachedAt)
	defer w.Release()

	var buf bytes.Buffer
	_, err = w.WriteTo(&buf, false)
	require.NoError(t, err)
	require.Equal(t, payload, buf.Bytes())
}

func TestCacheResultWriterFromCompressed_TrimSides(t *testing.T) {
	payload := []byte(`[1,2,3]`)
	env, wrapped := wrapCacheEnvelope(payload)
	require.True(t, wrapped)

	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest))
	require.NoError(t, err)
	compressed := enc.EncodeAll(env, nil)

	cache := &EvmJsonRpcCache{
		compressionEnabled: true,
		decoderPool: &sync.Pool{New: func() interface{} {
			d, _ := zstd.NewReader(nil)
			return d
		}},
	}

	w, _, ok, err := cache.newCacheResultWriterFromCompressed(compressed)
	require.NoError(t, err)
	require.True(t, ok)
	defer w.Release()

	var buf bytes.Buffer
	_, err = w.WriteTo(&buf, true)
	require.NoError(t, err)
	require.Equal(t, []byte(`1,2,3`), buf.Bytes())
}

func TestCacheResultWriterFromCompressed_Emptyish(t *testing.T) {
	payload := []byte(`[]`)
	env, wrapped := wrapCacheEnvelope(payload)
	require.True(t, wrapped)

	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest))
	require.NoError(t, err)
	compressed := enc.EncodeAll(env, nil)

	cache := &EvmJsonRpcCache{
		compressionEnabled: true,
		decoderPool: &sync.Pool{New: func() interface{} {
			d, _ := zstd.NewReader(nil)
			return d
		}},
	}

	w, _, ok, err := cache.newCacheResultWriterFromCompressed(compressed)
	require.NoError(t, err)
	require.True(t, ok)
	defer w.Release()

	require.True(t, w.IsResultEmptyish())
}

func TestCacheResultWriterFromCompressed_LegacyNoEnvelope(t *testing.T) {
	payload := []byte(`[{"x":1}]`)

	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest))
	require.NoError(t, err)
	compressed := enc.EncodeAll(payload, nil)

	cache := &EvmJsonRpcCache{
		compressionEnabled: true,
		decoderPool: &sync.Pool{New: func() interface{} {
			d, _ := zstd.NewReader(nil)
			return d
		}},
	}

	w, cachedAt, ok, err := cache.newCacheResultWriterFromCompressed(compressed)
	require.NoError(t, err)
	require.False(t, ok)
	require.Zero(t, cachedAt)
	defer w.Release()

	var buf bytes.Buffer
	_, err = w.WriteTo(&buf, false)
	require.NoError(t, err)
	require.Equal(t, payload, buf.Bytes())
}
