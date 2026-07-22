package erpc

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gzipTestClient performs a request against a gzipHandler-wrapped handler and
// returns the raw (non-auto-decompressed) response.
func gzipTestRequest(t *testing.T, handler http.HandlerFunc, acceptGzip bool) (*http.Response, []byte) {
	t.Helper()

	srv := httptest.NewServer(gzipHandler(handler))
	t.Cleanup(srv.Close)

	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	if acceptGzip {
		// Setting the header explicitly disables the transport's transparent
		// decompression, so we can observe the wire representation.
		req.Header.Set("Accept-Encoding", "gzip")
	}

	client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	resp, err := client.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, body
}

func gunzip(t *testing.T, b []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	require.NoError(t, err)
	out, err := io.ReadAll(zr)
	require.NoError(t, err)
	require.NoError(t, zr.Close())
	return out
}

// Regression test for https://github.com/erpc/erpc/issues/990: JSON-RPC
// responses are streamed via JsonRpcResponse.WriteTo as an explicit
// WriteHeader followed by many small writes (envelope prefix first, large
// result later). The compression decision must be based on cumulative size,
// not the size of the first write.
func TestGzipHandler_StreamedJsonRpcStyleResponseIsCompressed(t *testing.T) {
	largeResult := strings.Repeat(`{"logIndex":"0x1","data":"0xdeadbeef"},`, 500) // ~19KB

	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK) // JSON-RPC path calls WriteHeader before the body
		// Mimic JsonRpcResponse.WriteTo: tiny envelope writes, then the payload.
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":`))
		_, _ = w.Write([]byte(`1`))
		_, _ = w.Write([]byte(`,"result":[`))
		_, _ = w.Write([]byte(largeResult))
		_, _ = w.Write([]byte(`]}`))
	}

	resp, body := gzipTestRequest(t, handler, true)

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "gzip", resp.Header.Get("Content-Encoding"))
	assert.Equal(t, "Accept-Encoding", resp.Header.Get("Vary"))

	expected := `{"jsonrpc":"2.0","id":1,"result":[` + largeResult + `]}`
	assert.Equal(t, expected, string(gunzip(t, body)))
	assert.Less(t, len(body), len(expected), "wire body should be smaller than the plain payload")
}

func TestGzipHandler_SmallResponseStaysUncompressed(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x1"}`))
	}

	resp, body := gzipTestRequest(t, handler, true)

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Empty(t, resp.Header.Get("Content-Encoding"))
	assert.Equal(t, "Accept-Encoding", resp.Header.Get("Vary"))
	assert.Equal(t, `{"jsonrpc":"2.0","id":1,"result":"0x1"}`, string(body))
}

// Single large write (e.g. verbose /healthcheck) — the pre-existing behavior
// that already worked must keep working.
func TestGzipHandler_SingleLargeWriteIsCompressed(t *testing.T) {
	payload := strings.Repeat("a", 4096)
	handler := func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(payload))
	}

	resp, body := gzipTestRequest(t, handler, true)

	assert.Equal(t, "gzip", resp.Header.Get("Content-Encoding"))
	assert.Equal(t, payload, string(gunzip(t, body)))
}

func TestGzipHandler_ExplicitStatusCodePreservedWhenCompressing(t *testing.T) {
	payload := strings.Repeat("b", 8192)
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"`))
		_, _ = w.Write([]byte(payload))
		_, _ = w.Write([]byte(`"}`))
	}

	resp, body := gzipTestRequest(t, handler, true)

	assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	assert.Equal(t, "gzip", resp.Header.Get("Content-Encoding"))
	assert.Equal(t, `{"error":"`+payload+`"}`, string(gunzip(t, body)))
}

func TestGzipHandler_ClientWithoutGzipGetsPlainBody(t *testing.T) {
	payload := strings.Repeat("c", 4096)
	handler := func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(payload))
	}

	resp, body := gzipTestRequest(t, handler, false)

	assert.Empty(t, resp.Header.Get("Content-Encoding"))
	assert.Equal(t, "Accept-Encoding", resp.Header.Get("Vary"))
	assert.Equal(t, payload, string(body))
}

// A Flush before the threshold is reached commits the response to passthrough:
// buffered bytes must reach the wire and later writes stay uncompressed.
func TestGzipHandler_FlushBeforeThresholdCommitsPassthrough(t *testing.T) {
	late := strings.Repeat("d", 4096)
	handler := func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("early"))
		w.(http.Flusher).Flush()
		_, _ = w.Write([]byte(late))
	}

	resp, body := gzipTestRequest(t, handler, true)

	assert.Empty(t, resp.Header.Get("Content-Encoding"))
	assert.Equal(t, "early"+late, string(body))
}

func TestGzipHandler_HeaderOnlyResponseDeliversStatus(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}

	resp, body := gzipTestRequest(t, handler, true)

	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	assert.Empty(t, resp.Header.Get("Content-Encoding"))
	assert.Empty(t, body)
}

// Cumulative accounting: many writes individually below the threshold must
// still trigger compression once their total crosses it.
func TestGzipHandler_ManySmallWritesCrossingThresholdCompress(t *testing.T) {
	chunk := strings.Repeat("e", 100)
	handler := func(w http.ResponseWriter, r *http.Request) {
		for range 50 { // 5000 bytes total, 100 at a time
			_, _ = w.Write([]byte(chunk))
		}
	}

	resp, body := gzipTestRequest(t, handler, true)

	assert.Equal(t, "gzip", resp.Header.Get("Content-Encoding"))
	assert.Equal(t, strings.Repeat(chunk, 50), string(gunzip(t, body)))
}
