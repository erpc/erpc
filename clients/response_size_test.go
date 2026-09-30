package clients

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bigResult is a JSON-RPC success response whose body is about size bytes.
func bigResult(id int, size int) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":"0x%s"}`, id, strings.Repeat("a", size))
}

func newCappedTestClient(t *testing.T, srv *httptest.Server, cfg *common.JsonRpcUpstreamConfig) *GenericHttpJsonRpcClient {
	t.Helper()
	logger := log.Logger
	u, err := url.Parse(srv.URL)
	require.NoError(t, err)

	ups := common.NewFakeUpstream("rpc1")
	ups.Config().Type = common.UpstreamTypeEvm
	ups.Config().Endpoint = srv.URL
	ups.Config().JsonRpc = cfg

	c, err := NewGenericHttpJsonRpcClient(context.Background(), &logger, "prj1", ups, u, cfg, nil, &noopErrorExtractor{})
	require.NoError(t, err)

	client := c.(*GenericHttpJsonRpcClient)
	client.httpClient = srv.Client()
	return client
}

func assertResponseTooBig(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)

	var rpcErr *common.ErrJsonRpcExceptionExternal
	require.True(t, errors.As(err, &rpcErr), "expected the upstream-style JSON-RPC error in the chain, got: %v", err)
	assert.Equal(t, -32008, rpcErr.Code)
	assert.Equal(t, "Response is too big", rpcErr.Message)
}

func TestCappedBody(t *testing.T) {
	t.Run("StopsOneBytePastTheCap", func(t *testing.T) {
		src := &countingReader{r: bytes.NewReader(make([]byte, 10<<20))}
		body := newCappedBody(io.NopCloser(src), 1024)

		n, err := io.Copy(io.Discard, body)

		assert.ErrorIs(t, err, errResponseTooBig)
		assert.True(t, body.exceeded)
		assert.Equal(t, int64(1025), n)
		assert.Equal(t, int64(1025), src.n)
	})

	t.Run("ExactlyAtTheCapPasses", func(t *testing.T) {
		body := newCappedBody(io.NopCloser(bytes.NewReader(make([]byte, 1024))), 1024)

		n, err := io.Copy(io.Discard, body)

		assert.NoError(t, err)
		assert.False(t, body.exceeded)
		assert.Equal(t, int64(1024), n)
	})
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func TestHttpJsonRpcClient_MaxResponseBytes(t *testing.T) {
	const limit = 64 << 10

	t.Run("UnderTheCapPassesThrough", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, bigResult(1, 1024))
		}))
		defer srv.Close()
		client := newCappedTestClient(t, srv, &common.JsonRpcUpstreamConfig{MaxResponseBytes: limit})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"debug_traceBlockByHash","params":[]}`))
		resp, err := client.SendRequest(context.Background(), req)

		require.NoError(t, err)
		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Nil(t, jrr.Error)
		assert.Len(t, jrr.GetResultBytes(), 1024+4)
	})

	t.Run("DeclaredLengthOverTheCapIsNotRead", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body := bigResult(1, 4*limit)
			w.Header().Set("Content-Length", fmt.Sprint(len(body)))
			_, _ = io.WriteString(w, body)
		}))
		defer srv.Close()
		client := newCappedTestClient(t, srv, &common.JsonRpcUpstreamConfig{MaxResponseBytes: limit})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"debug_traceBlockByHash","params":[]}`))
		_, err := client.SendRequest(context.Background(), req)

		assertResponseTooBig(t, err)
	})

	t.Run("StreamedBodyOverTheCapIsCutOff", func(t *testing.T) {
		written := make(chan int64, 1)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// No Content-Length: the body streams chunked until the client hangs up.
			var total int64
			chunk := []byte(strings.Repeat("a", 32<<10))
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":"0x`)
			for i := 0; i < 2048; i++ {
				n, err := w.Write(chunk)
				total += int64(n)
				if err != nil {
					break
				}
				w.(http.Flusher).Flush()
			}
			written <- total
		}))
		defer srv.Close()
		client := newCappedTestClient(t, srv, &common.JsonRpcUpstreamConfig{MaxResponseBytes: limit})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"debug_traceBlockByHash","params":[]}`))
		_, err := client.SendRequest(context.Background(), req)

		assertResponseTooBig(t, err)
		select {
		case total := <-written:
			assert.Less(t, total, int64(64<<20), "the client should hang up long before the 64 MiB body is sent")
		case <-time.After(10 * time.Second):
			t.Fatal("server kept writing after the client dropped the body")
		}
	})

	t.Run("GzipBodyIsCappedOnDecodedSize", func(t *testing.T) {
		var compressed bytes.Buffer
		gz := gzip.NewWriter(&compressed)
		_, _ = io.WriteString(gz, bigResult(1, 16*limit))
		require.NoError(t, gz.Close())
		require.Less(t, compressed.Len(), limit, "the compressed body must fit under the cap for this test to mean anything")

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Set("Content-Length", fmt.Sprint(compressed.Len()))
			_, _ = w.Write(compressed.Bytes())
		}))
		defer srv.Close()
		client := newCappedTestClient(t, srv, &common.JsonRpcUpstreamConfig{MaxResponseBytes: limit})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"debug_traceBlockByHash","params":[]}`))
		_, err := client.SendRequest(context.Background(), req)

		assertResponseTooBig(t, err)
	})

	t.Run("ZeroLeavesResponsesUnbounded", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, bigResult(1, 4*limit))
		}))
		defer srv.Close()
		client := newCappedTestClient(t, srv, &common.JsonRpcUpstreamConfig{})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"debug_traceBlockByHash","params":[]}`))
		resp, err := client.SendRequest(context.Background(), req)

		require.NoError(t, err)
		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Len(t, jrr.GetResultBytes(), 4*limit+4)
	})

	t.Run("BatchOverTheCapFailsEveryRequest", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, "["+bigResult(1, limit)+","+bigResult(2, limit)+"]")
		}))
		defer srv.Close()
		client := newCappedTestClient(t, srv, &common.JsonRpcUpstreamConfig{
			SupportsBatch:    &common.TRUE,
			BatchMaxSize:     2,
			BatchMaxWait:     common.Duration(50 * time.Millisecond),
			MaxResponseBytes: limit,
		})

		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				req := common.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"debug_traceBlockByHash","params":[]}`, i+1)))
				_, errs[i] = client.SendRequest(context.Background(), req)
			}(i)
		}
		wg.Wait()

		for _, err := range errs {
			assertResponseTooBig(t, err)
		}
	})
}
