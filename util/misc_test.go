package util

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtractUsefulHeaders(t *testing.T) {
	h := http.Header{}
	kept := map[string]string{
		"X-Upstream":           "u1",
		"Traceparent":          "00-abc",
		"X-Debug-Info":         "dbg",
		"Correlation-Key":      "corr",
		"Cf-Ray-Id":            "ray",
		"Request-Id":           "req",
		"Span-Context":         "span",
		"Parent-Ref":           "p",
		"Upstream-Error":       "boom",
		"Rate-Limit-Remaining": "7",
		"Content-Type":         "application/json",
		"Content-Length":       "42",
		"Server":               "nginx",
		"Date":                 "Wed, 21 Oct 2026 07:28:00 GMT",
		"Etag":                 "\"v1\"",
		"Status":               "200",
		"Retry-After":          "3",
		"Cache-Control":        "no-cache",
	}
	for k, v := range kept {
		h.Set(k, v)
	}
	h.Add("X-Multi", "first")
	h.Add("X-Multi", "second")
	for _, dropped := range []string{"Accept", "Vary", "Connection", "Set-Cookie", "Content-Encoding"} {
		h.Set(dropped, "nope")
	}

	got := ExtractUsefulHeaders(&http.Response{Header: h})

	want := map[string]interface{}{"x-multi": "first"}
	for k, v := range kept {
		want[strings.ToLower(k)] = v
	}
	assert.Equal(t, want, got)
	assert.Empty(t, ExtractUsefulHeaders(&http.Response{Header: http.Header{}}))
}

func TestIsNativeProtocol(t *testing.T) {
	for _, ep := range []string{
		"http://h", "https://h", "ws://h", "wss://h", "grpc://h", "grpc+bds://h", "grpcs://h",
	} {
		assert.True(t, IsNativeProtocol(ep), ep)
	}
	for _, ep := range []string{
		"alchemy://key", "envio://", "HTTP://h", "httpx://h", "grpc+other://h", "", "h/http://",
	} {
		assert.False(t, IsNativeProtocol(ep), ep)
	}
}

func TestRedactEndpoint(t *testing.T) {
	cases := []struct {
		name     string
		endpoint string
		want     string
	}{
		{"https keeps host", "https://eth.example.com/v2/SECRETKEY", "https://eth.example.com#redacted="},
		{"wss keeps host", "wss://ws.example.com:8546/key", "wss://ws.example.com:8546#redacted="},
		{"grpc keeps host", "grpc://g.example.com:443", "grpc://g.example.com:443#redacted="},
		{"envio keeps host without hash", "envio://rpc.hypersync.xyz", "envio://rpc.hypersync.xyz"},
		{"evm+envio suffix", "evm+envio://host.io/x", "evm+envio://host.io"},
		{"repository keeps host", "repository://evm-public-endpoints.erpc.cloud", "repository://evm-public-endpoints.erpc.cloud#redacted="},
		{"vendor scheme drops host", "alchemy://SECRETKEY@host", "alchemy#redacted="},
		{"no scheme", "just-a-string", "redacted="},
		{"no host", "mailto:user@example.com", "redacted="},
		{"unparseable", "http://[::1", "redacted="},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactEndpoint(tc.endpoint)
			if strings.HasSuffix(tc.want, "=") {
				require.True(t, strings.HasPrefix(got, tc.want), "got %q", got)
				assert.Len(t, strings.TrimPrefix(got, tc.want), 5, "hash suffix is 5 hex chars")
				assert.NotContains(t, got, "SECRETKEY")
			} else {
				assert.Equal(t, tc.want, got)
			}
		})
	}

	// Same input → same hash; different secret on same host → different hash.
	a := RedactEndpoint("https://h.example/k1")
	assert.Equal(t, a, RedactEndpoint("https://h.example/k1"))
	assert.NotEqual(t, a, RedactEndpoint("https://h.example/k2"))
}

func TestParseByteSize(t *testing.T) {
	cases := []struct {
		in      string
		want    int
		wantErr string
	}{
		{"0", 0, ""},
		{"512", 512, ""},
		{"512B", 512, ""},
		{"1kb", 1024, ""},
		{" 2 KB ", 2048, ""},
		{"3MB", 3 * 1024 * 1024, ""},
		{"10 mb", 10 * 1024 * 1024, ""},
		{"", 0, "empty size string"},
		{"   ", 0, "empty size string"},
		{"-1KB", 0, "size cannot be negative"},
		{"1.5MB", 0, "invalid size format"},
		{"1GB", 0, "invalid size format"},
		{"abc", 0, "invalid size format"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseByteSize(tc.in)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestPtrHelpers(t *testing.T) {
	s := StringPtr("x")
	assert.Equal(t, "x", *s)
	assert.NotSame(t, s, StringPtr("x"), "each call returns a fresh pointer")
	assert.Equal(t, 7, *IntPtr(7))
	assert.Equal(t, true, *BoolPtr(true))
	assert.Equal(t, 1.5, *Float64Ptr(1.5))
}

func TestAtomicValue(t *testing.T) {
	av := AtomicValue("initial")
	assert.Equal(t, "initial", av.Load())
	av.Store("next")
	assert.Equal(t, "next", av.Load())
}

func TestFastMemConversions(t *testing.T) {
	assert.Equal(t, "", B2Str(nil))
	assert.Equal(t, "", B2Str([]byte{}))
	assert.Equal(t, "hello", B2Str([]byte("hello")))

	assert.Nil(t, S2Bytes(""))
	assert.Equal(t, []byte("world"), S2Bytes("world"))

	rc := StringToReaderCloser("payload")
	b, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, "payload", string(b))
	assert.NoError(t, rc.Close())
}

func TestDefaultOutboundDialer(t *testing.T) {
	d := DefaultOutboundDialer()
	assert.Equal(t, 10*time.Second, d.Timeout)
	assert.Equal(t, 15*time.Second, d.KeepAlive)
	assert.NotSame(t, d, DefaultOutboundDialer(), "callers must get independent dialers")
}

type errAfterReader struct {
	data []byte
	err  error
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func TestReadAll(t *testing.T) {
	payload := bytes.Repeat([]byte("abc"), 1000)
	// maxPreGrowSize+1 would allocate 200MB; the cap branch is not worth that in CI.
	for _, expected := range []int{0, -1, 10, len(payload)} {
		data, release, err := ReadAll(bytes.NewReader(payload), expected)
		require.NoError(t, err)
		require.NotNil(t, release)
		assert.Equal(t, payload, data, "expected=%d", expected)
		release()
	}

	data, release, err := ReadAll(&errAfterReader{data: []byte("partial"), err: errors.New("read failed")}, 0)
	require.EqualError(t, err, "read failed")
	assert.Nil(t, data)
	assert.Nil(t, release)
}

func TestReturnBuf_DropsNilAndOversized(t *testing.T) {
	ReturnBuf(nil) // must not reach the pool

	big := bytes.NewBuffer(make([]byte, 0, 4*maxBufCap+1))
	big.WriteString("SENTINEL")
	ReturnBuf(big)
	for range 16 {
		b := BorrowBuf()
		assert.Equal(t, 0, b.Len(), "borrowed buffers are reset")
		assert.LessOrEqual(t, b.Cap(), 4*maxBufCap, "oversized buffers must not be pooled")
	}
}

func TestBoundedCall(t *testing.T) {
	t.Run("returns fn result", func(t *testing.T) {
		v, err := BoundedCallT(context.Background(), func(ctx context.Context) (int, error) { return 42, nil })
		require.NoError(t, err)
		assert.Equal(t, 42, v)
	})

	t.Run("returns fn error", func(t *testing.T) {
		boom := errors.New("boom")
		v, err := BoundedCallT(context.Background(), func(ctx context.Context) (string, error) { return "partial", boom })
		assert.ErrorIs(t, err, boom)
		assert.Equal(t, "partial", v)
	})

	t.Run("already-done ctx skips fn and returns cause", func(t *testing.T) {
		cause := errors.New("budget exhausted")
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(cause)
		called := false
		v, err := BoundedCallT(ctx, func(ctx context.Context) (int, error) { called = true; return 1, nil })
		assert.ErrorIs(t, err, cause)
		assert.Zero(t, v)
		assert.False(t, called)
	})

	t.Run("ctx fires while fn is wedged", func(t *testing.T) {
		cause := errors.New("dynamic timeout")
		ctx, cancel := context.WithCancelCause(context.Background())
		release := make(chan struct{})
		defer close(release)
		started := make(chan struct{})
		go func() {
			<-started
			cancel(cause)
		}()
		v, err := BoundedCallT(ctx, func(ctx context.Context) (int, error) {
			close(started)
			<-release // ignores ctx, like a wedged transport
			return 7, nil
		})
		assert.ErrorIs(t, err, cause)
		assert.Zero(t, v)
	})

	t.Run("ctx done by the time fn returns wins over result", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		v, err := BoundedCallT(ctx, func(ctx context.Context) (int, error) {
			cancel()
			return 9, nil
		})
		assert.ErrorIs(t, err, context.Canceled)
		assert.Zero(t, v)
	})

	t.Run("panic surfaces as error", func(t *testing.T) {
		v, err := BoundedCallT(context.Background(), func(ctx context.Context) (int, error) { panic("kaboom") })
		require.Error(t, err)
		assert.Contains(t, err.Error(), "bounded-call goroutine panic: kaboom")
		assert.Zero(t, v)
	})

	t.Run("untyped variant", func(t *testing.T) {
		boom := errors.New("drain failed")
		assert.ErrorIs(t, BoundedCall(context.Background(), func(ctx context.Context) error { return boom }), boom)
		assert.NoError(t, BoundedCall(context.Background(), func(ctx context.Context) error { return nil }))
	})
}

func TestEvmNetworkIdAndIdentifiers(t *testing.T) {
	assert.Equal(t, "evm:1", EvmNetworkId(1))
	assert.Equal(t, "evm:42161", EvmNetworkId(int64(42161)))

	for _, s := range []string{"abc", "A_b-9", "x"} {
		assert.True(t, IsValidIdentifier(s), s)
	}
	for _, s := range []string{"", "a b", "a.b", "a:b", "é"} {
		assert.False(t, IsValidIdentifier(s), s)
	}

	assert.True(t, IsValidNetworkId("evm:1"))
	assert.False(t, IsValidNetworkId("evm:abc"))
	assert.False(t, IsValidNetworkId("evm:"))
	assert.False(t, IsValidNetworkId("cosmos:hub"))
	assert.False(t, IsValidNetworkId(""))
}

func TestIncrementAndGetIndex(t *testing.T) {
	// Counters are process-global; a per-run key keeps -count=N runs isolated.
	key := t.Name() + time.Now().Format(time.RFC3339Nano)
	assert.Equal(t, "1", IncrementAndGetIndex(key, "a"))
	assert.Equal(t, "2", IncrementAndGetIndex(key, "a"))
	assert.Equal(t, "1", IncrementAndGetIndex(key, "b"), "counters are per key")
	// Parts are joined with a separator, so ("ab") and ("a","b") differ.
	assert.Equal(t, "1", IncrementAndGetIndex(key+"a", "b"))

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			IncrementAndGetIndex(key, "concurrent")
		}()
	}
	wg.Wait()
	assert.Equal(t, "51", IncrementAndGetIndex(key, "concurrent"))
}

func TestRandomID_Range(t *testing.T) {
	for range 1000 {
		id := RandomID()
		require.GreaterOrEqual(t, id, int64(0))
		require.Less(t, id, int64(1<<31-1))
	}
}

func TestNormalizeBlockHashHexString_Errors(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr string
	}{
		{"", "", "empty block hash"},
		{"0xzz", "", "invalid hex character"},
		{"  0XAB  ", "0x" + strings.Repeat("0", 62) + "ab", ""},
		{"0xabc", "0x" + strings.Repeat("0", 61) + "abc", ""},
		{"0x" + strings.Repeat("0", 3) + strings.Repeat("f", 64), "0x" + strings.Repeat("f", 64), ""},
		{"0x" + strings.Repeat("0", 4) + "1" + strings.Repeat("f", 63), "0x1" + strings.Repeat("f", 63), ""},
		{"0x1" + strings.Repeat("f", 64), "", "block hash too long"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := NormalizeBlockHashHexString(tc.in)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}

	_, err := ParseBlockHashHexToBytes("0xnothex")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid hex character")
}

func TestParseBlockParameter_Errors(t *testing.T) {
	_, _, err := ParseBlockParameter("0x" + strings.Repeat("g", 64))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to parse block hash")

	_, _, err = ParseBlockParameter(map[string]interface{}{"blockHash": "0xnothex"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to parse blockHash from object")

	// Non-string fields are ignored; with nothing usable left it is an error.
	_, _, err = ParseBlockParameter(map[string]interface{}{"blockHash": 1, "blockNumber": 2, "blockTag": true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must contain blockHash, blockNumber, or blockTag")

	// blockTag takes precedence over blockNumber when both are present.
	bn, bh, err := ParseBlockParameter(map[string]interface{}{"blockNumber": "0x10", "blockTag": "safe"})
	require.NoError(t, err)
	assert.Equal(t, "safe", bn)
	assert.Nil(t, bh)
}
