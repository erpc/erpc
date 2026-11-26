package clients

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// capturedHttpRequest is what an httptest upstream saw for one POST.
type capturedHttpRequest struct {
	header http.Header
	body   []byte
	uri    string
}

// recordingUpstream is an httptest JSON-RPC upstream that records every
// request (decompressing gzip bodies) and answers through reply.
type recordingUpstream struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []capturedHttpRequest
}

func newRecordingUpstream(t *testing.T, reply func(w http.ResponseWriter, body []byte)) *recordingUpstream {
	t.Helper()
	u := &recordingUpstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var reader io.Reader = r.Body
		if r.Header.Get("Content-Encoding") == "gzip" {
			gz, err := gzip.NewReader(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			reader = gz
		}
		body, err := io.ReadAll(reader)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		u.mu.Lock()
		u.reqs = append(u.reqs, capturedHttpRequest{header: r.Header.Clone(), body: body, uri: r.RequestURI})
		u.mu.Unlock()
		reply(w, body)
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *recordingUpstream) requests() []capturedHttpRequest {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]capturedHttpRequest(nil), u.reqs...)
}

func newHttpTestClient(t *testing.T, endpoint string, cfg *common.JsonRpcUpstreamConfig, proxy *ProxyPool, extractor common.JsonRpcErrorExtractor) *GenericHttpJsonRpcClient {
	t.Helper()
	parsed, err := url.Parse(endpoint)
	require.NoError(t, err)
	ups := common.NewFakeUpstream("rpc1")
	ups.Config().Type = common.UpstreamTypeEvm
	ups.Config().Endpoint = endpoint
	ups.Config().JsonRpc = cfg
	if extractor == nil {
		extractor = &noopErrorExtractor{}
	}
	logger := zerolog.New(io.Discard)
	appCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c, err := NewGenericHttpJsonRpcClient(appCtx, &logger, "prj1", ups, parsed, cfg, proxy, extractor)
	require.NoError(t, err)
	return c.(*GenericHttpJsonRpcClient)
}

func rpcReq(id int, method string) *common.NormalizedRequest {
	return common.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q,"params":[]}`, id, method)))
}

func batchCfg(maxSize int, maxWait time.Duration) *common.JsonRpcUpstreamConfig {
	return &common.JsonRpcUpstreamConfig{
		SupportsBatch: &common.TRUE,
		BatchMaxSize:  maxSize,
		BatchMaxWait:  common.Duration(maxWait),
	}
}

// echoBatch answers a batch with one result per request, "r<id>", in reverse
// order so matching by position would be visibly wrong.
func echoBatch(w http.ResponseWriter, body []byte) {
	var reqs []map[string]interface{}
	if err := sonic.Unmarshal(body, &reqs); err != nil {
		http.Error(w, "not a batch", http.StatusBadRequest)
		return
	}
	out := make([]string, 0, len(reqs))
	for i := len(reqs) - 1; i >= 0; i-- {
		out = append(out, fmt.Sprintf(`{"jsonrpc":"2.0","id":%v,"result":"r%v"}`, reqs[i]["id"], reqs[i]["id"]))
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, "["+strings.Join(out, ",")+"]")
}

type batchOutcome struct {
	result string
	err    error
}

func sendConcurrently(t *testing.T, c *GenericHttpJsonRpcClient, ctx context.Context, ids []int) map[int]batchOutcome {
	t.Helper()
	var mu sync.Mutex
	out := make(map[int]batchOutcome, len(ids))
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			resp, err := c.SendRequest(ctx, rpcReq(id, "eth_blockNumber"))
			o := batchOutcome{err: err}
			if err == nil {
				jrr, jerr := resp.JsonRpcResponse()
				if jerr != nil {
					o.err = jerr
				} else {
					o.result = jrr.GetResultString()
				}
			}
			mu.Lock()
			out[id] = o
			mu.Unlock()
		}(id)
	}
	wg.Wait()
	return out
}

func batchSizes(reqs []capturedHttpRequest) []int {
	sizes := make([]int, 0, len(reqs))
	for _, r := range reqs {
		var arr []interface{}
		if err := sonic.Unmarshal(r.body, &arr); err != nil {
			sizes = append(sizes, -1)
			continue
		}
		sizes = append(sizes, len(arr))
	}
	sort.Ints(sizes)
	return sizes
}

// Each caller gets the result whose id matches its own request, wherever it
// sits in the upstream's array, and batches are cut at BatchMaxSize.
func TestHttpJsonRpcClient_BatchSplitsAtMaxSizeAndMatchesById(t *testing.T) {
	up := newRecordingUpstream(t, echoBatch)
	c := newHttpTestClient(t, up.URL, batchCfg(2, time.Minute), nil, nil)

	out := sendConcurrently(t, c, context.Background(), []int{1, 2, 3, 4})
	for id := 1; id <= 4; id++ {
		require.NoError(t, out[id].err, "id %d", id)
		assert.Equal(t, fmt.Sprintf(`"r%d"`, id), out[id].result)
	}
	assert.Equal(t, []int{2, 2}, batchSizes(up.requests()), "4 requests with max size 2 make two full batches")
}

// A partial batch is flushed when BatchMaxWait elapses, and the batch POST
// carries the configured headers and JSON content type.
func TestHttpJsonRpcClient_BatchFlushesOnMaxWait(t *testing.T) {
	up := newRecordingUpstream(t, echoBatch)
	cfg := batchCfg(100, 20*time.Millisecond)
	cfg.Headers = map[string]string{"X-Api-Key": "secret"}
	c := newHttpTestClient(t, up.URL, cfg, nil, nil)

	out := sendConcurrently(t, c, context.Background(), []int{7})
	require.NoError(t, out[7].err)
	assert.Equal(t, `"r7"`, out[7].result)

	reqs := up.requests()
	require.Len(t, reqs, 1)
	assert.Equal(t, []int{1}, batchSizes(reqs))
	assert.Equal(t, "secret", reqs[0].header.Get("X-Api-Key"))
	assert.Equal(t, "application/json", reqs[0].header.Get("Content-Type"))
	assert.Contains(t, reqs[0].header.Get("User-Agent"), "Project/prj1")
}

// Two callers using the same JSON-RPC id cannot share a batch (the response
// could not be routed); the second one goes out in a batch of its own and
// both still get an answer.
func TestHttpJsonRpcClient_BatchDuplicateIdStartsNewBatch(t *testing.T) {
	up := newRecordingUpstream(t, echoBatch)
	c := newHttpTestClient(t, up.URL, batchCfg(10, 50*time.Millisecond), nil, nil)

	errs := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := c.SendRequest(context.Background(), rpcReq(5, "eth_chainId"))
			errs <- err
		}()
	}
	for range 2 {
		require.NoError(t, <-errs)
	}
	assert.Equal(t, []int{1, 1}, batchSizes(up.requests()))
}

// Batch responses the client cannot route or understand must fail every
// affected caller with the right error instead of hanging or mixing answers.
func TestHttpJsonRpcClient_BatchResponseShapes(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		gzip   bool
		check  func(t *testing.T, out map[int]batchOutcome)
	}{
		{
			name:   "unknown and missing ids",
			status: 200,
			body:   `[{"jsonrpc":"2.0","id":1,"result":"0x1"},{"jsonrpc":"2.0","id":99,"result":"0x63"},{"jsonrpc":"2.0","result":"0x0"}]`,
			check: func(t *testing.T, out map[int]batchOutcome) {
				require.NoError(t, out[1].err)
				assert.Equal(t, `"0x1"`, out[1].result)
				require.Error(t, out[2].err)
				assert.Contains(t, out[2].err.Error(), "no response received for request ID: 2")
			},
		},
		{
			name:   "per-item error goes only to its caller",
			status: 200,
			body:   `[{"jsonrpc":"2.0","id":1,"result":"0x1"},{"jsonrpc":"2.0","id":2,"error":{"code":-32000,"message":"header not found"}}]`,
			check: func(t *testing.T, out map[int]batchOutcome) {
				require.NoError(t, out[1].err)
				require.Error(t, out[2].err)
				assert.True(t, common.HasErrorCode(out[2].err, common.ErrCodeJsonRpcExceptionInternal), "got %v", out[2].err)
				assert.Contains(t, out[2].err.Error(), "header not found")
			},
		},
		{
			name:   "item without result or error is a parse error",
			status: 200,
			body:   `[{"jsonrpc":"2.0","id":1,"result":"0x1"},{"jsonrpc":"2.0","id":2}]`,
			check: func(t *testing.T, out map[int]batchOutcome) {
				require.NoError(t, out[1].err)
				require.Error(t, out[2].err)
				assert.Contains(t, out[2].err.Error(), "cannot parse json rpc response from upstream")
			},
		},
		{
			name:   "single object answers every caller",
			status: 200,
			body:   `{"jsonrpc":"2.0","id":1,"result":"0xshared"}`,
			check: func(t *testing.T, out map[int]batchOutcome) {
				for _, id := range []int{1, 2} {
					require.NoError(t, out[id].err)
					assert.Equal(t, `"0xshared"`, out[id].result)
				}
			},
		},
		{
			name:   "single object error fails every caller",
			status: 429,
			body:   `{"jsonrpc":"2.0","id":null,"error":{"code":-32005,"message":"too many requests"}}`,
			check: func(t *testing.T, out map[int]batchOutcome) {
				for _, id := range []int{1, 2} {
					require.Error(t, out[id].err)
					assert.Contains(t, out[id].err.Error(), "too many requests")
				}
			},
		},
		{
			name:   "scalar body is malformed",
			status: 200,
			body:   `"just a string"`,
			check: func(t *testing.T, out map[int]batchOutcome) {
				for _, id := range []int{1, 2} {
					require.Error(t, out[id].err)
					assert.True(t, common.HasErrorCode(out[id].err, "ErrUpstreamMalformedResponse"), "got %v", out[id].err)
				}
			},
		},
		{
			name:   "empty body fails every caller",
			status: 502,
			body:   ``,
			check: func(t *testing.T, out map[int]batchOutcome) {
				for _, id := range []int{1, 2} {
					require.Error(t, out[id].err)
					assert.Contains(t, out[id].err.Error(), "empty response")
				}
			},
		},
		{
			name:   "gzip encoded batch is decoded",
			status: 200,
			gzip:   true,
			body:   `[{"jsonrpc":"2.0","id":2,"result":"0x2"},{"jsonrpc":"2.0","id":1,"result":"0x1"}]`,
			check: func(t *testing.T, out map[int]batchOutcome) {
				require.NoError(t, out[1].err)
				require.NoError(t, out[2].err)
				assert.Equal(t, `"0x1"`, out[1].result)
				assert.Equal(t, `"0x2"`, out[2].result)
			},
		},
		{
			name:   "corrupt gzip body fails every caller",
			status: 200,
			body:   "not gzip at all",
			check: func(t *testing.T, out map[int]batchOutcome) {
				for _, id := range []int{1, 2} {
					require.Error(t, out[id].err)
					assert.Contains(t, out[id].err.Error(), "gzip")
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := newRecordingUpstream(t, func(w http.ResponseWriter, _ []byte) {
				body := []byte(tc.body)
				if tc.gzip || strings.HasPrefix(tc.name, "corrupt gzip") {
					w.Header().Set("Content-Encoding", "gzip")
				}
				if tc.gzip {
					var buf bytes.Buffer
					gz := gzip.NewWriter(&buf)
					_, _ = gz.Write(body)
					_ = gz.Close()
					body = buf.Bytes()
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write(body)
			})
			c := newHttpTestClient(t, up.URL, batchCfg(2, time.Minute), nil, nil)
			tc.check(t, sendConcurrently(t, c, context.Background(), []int{1, 2}))
		})
	}
}

// A caller whose context is already done is answered from its own context
// and never sent upstream.
func TestHttpJsonRpcClient_BatchSkipsDoneCallers(t *testing.T) {
	up := newRecordingUpstream(t, echoBatch)
	c := newHttpTestClient(t, up.URL, batchCfg(10, 10*time.Millisecond), nil, nil)

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.SendRequest(canceled, rpcReq(1, "eth_blockNumber"))
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointRequestCanceled), "got %v", err)

	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()
	_, err = c.SendRequest(expired, rpcReq(2, "eth_blockNumber"))
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointRequestTimeout), "got %v", err)

	assert.Empty(t, up.requests())
}

// A queued caller that cancels while the batch is still waiting returns at
// once with a cancellation and is dropped from the batch that goes out.
func TestHttpJsonRpcClient_BatchCallerCancelsWhileQueued(t *testing.T) {
	up := newRecordingUpstream(t, echoBatch)
	c := newHttpTestClient(t, up.URL, batchCfg(2, time.Minute), nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.SendRequest(ctx, rpcReq(1, "eth_blockNumber"))
		done <- err
	}()
	require.Eventually(t, func() bool {
		c.batchMu.Lock()
		defer c.batchMu.Unlock()
		return len(c.batchRequests) == 1
	}, 5*time.Second, time.Millisecond)
	cancel()
	err := <-done
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointRequestCanceled), "got %v", err)

	// The next request fills the batch; the canceled one must not be sent.
	resp, err := c.SendRequest(context.Background(), rpcReq(2, "eth_blockNumber"))
	require.NoError(t, err)
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	assert.Equal(t, `"r2"`, jrr.GetResultString())
	assert.Equal(t, []int{1}, batchSizes(up.requests()))
}

// A batch whose upstream never answers in time fails each caller with a
// request timeout bounded by the earliest caller deadline.
func TestHttpJsonRpcClient_BatchUpstreamTimeout(t *testing.T) {
	release := make(chan struct{})
	up := newRecordingUpstream(t, func(w http.ResponseWriter, _ []byte) { <-release })
	t.Cleanup(func() { close(release) }) // runs before up.Close (LIFO)
	c := newHttpTestClient(t, up.URL, batchCfg(2, time.Minute), nil, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	out := sendConcurrently(t, c, ctx, []int{1, 2})
	for _, id := range []int{1, 2} {
		require.Error(t, out[id].err)
		assert.True(t, common.HasErrorCode(out[id].err, common.ErrCodeEndpointRequestTimeout), "got %v", out[id].err)
	}
}

// An unreachable endpoint is a transport failure for every batched caller.
func TestHttpJsonRpcClient_BatchTransportFailure(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	addr := dead.URL
	dead.Close()
	c := newHttpTestClient(t, addr, batchCfg(2, time.Minute), nil, nil)

	out := sendConcurrently(t, c, context.Background(), []int{1, 2})
	for _, id := range []int{1, 2} {
		require.Error(t, out[id].err)
		assert.True(t, common.HasErrorCode(out[id].err, common.ErrCodeEndpointTransportFailure), "got %v", out[id].err)
	}
}

// Shutting the client down (app context canceled) releases queued callers
// with a cancellation instead of leaving them waiting for the batch timer.
func TestHttpJsonRpcClient_BatchAppShutdownReleasesCallers(t *testing.T) {
	up := newRecordingUpstream(t, echoBatch)
	parsed, err := url.Parse(up.URL)
	require.NoError(t, err)
	ups := common.NewFakeUpstream("rpc1")
	logger := zerolog.New(io.Discard)
	appCtx, appCancel := context.WithCancel(context.Background())
	hc, err := NewGenericHttpJsonRpcClient(appCtx, &logger, "prj1", ups, parsed, batchCfg(10, time.Minute), nil, &noopErrorExtractor{})
	require.NoError(t, err)
	c := hc.(*GenericHttpJsonRpcClient)

	done := make(chan error, 1)
	go func() {
		_, err := c.SendRequest(context.Background(), rpcReq(1, "eth_blockNumber"))
		done <- err
	}()
	require.Eventually(t, func() bool {
		c.batchMu.Lock()
		defer c.batchMu.Unlock()
		return len(c.batchRequests) == 1
	}, 5*time.Second, time.Millisecond)
	appCancel()
	select {
	case err := <-done:
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointRequestCanceled), "got %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("queued caller not released on shutdown")
	}
	assert.Empty(t, up.requests())
}

// A body that is not a JSON-RPC request fails before anything is sent.
// (The batch path is not covered: SendRequest dereferences the nil parsed
// request on that error path - see http_json_rpc_client.go SendRequest.)
func TestHttpJsonRpcClient_UnparsableRequest(t *testing.T) {
	up := newRecordingUpstream(t, echoBatch)
	c := newHttpTestClient(t, up.URL, nil, nil, nil)
	_, err := c.SendRequest(context.Background(), common.NewNormalizedRequest([]byte(`{oops`)))
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamRequest), "got %v", err)
	assert.Empty(t, up.requests())
}

// What the single-request path returns for each kind of HTTP reply.
func TestHttpJsonRpcClient_SingleResponseClassification(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		ctype    string
		body     string
		gzip     bool
		wantErr  common.ErrorCode
		contains string
		result   string
	}{
		{name: "200 result", status: 200, body: `{"jsonrpc":"2.0","id":1,"result":"0x10"}`, result: `"0x10"`},
		{name: "gzip result", status: 200, gzip: true, body: `{"jsonrpc":"2.0","id":1,"result":"0x20"}`, result: `"0x20"`},
		{name: "429 json-rpc error", status: 429, body: `{"jsonrpc":"2.0","id":1,"error":{"code":-32005,"message":"rate limited"}}`, wantErr: common.ErrCodeJsonRpcExceptionInternal, contains: "rate limited"},
		{name: "401 json-rpc error", status: 401, body: `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"unauthorized"}}`, wantErr: common.ErrCodeJsonRpcExceptionInternal, contains: "unauthorized"},
		{name: "403 plain text", status: 403, ctype: "text/plain", body: `forbidden`, wantErr: common.ErrCodeJsonRpcExceptionInternal, contains: "forbidden"},
		{name: "503 html", status: 503, ctype: "text/html", body: `<html>bad gateway</html>`, wantErr: common.ErrCodeJsonRpcExceptionInternal, contains: "bad gateway"},
		{name: "413 plain text", status: 413, ctype: "text/plain", body: `request entity too large`, wantErr: common.ErrCodeJsonRpcExceptionInternal, contains: "request entity too large"},
		{name: "context canceled message", status: 200, body: `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"upstream: context canceled"}}`, wantErr: common.ErrCodeEndpointRequestCanceled},
		{name: "deadline exceeded message", status: 200, body: `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"query: context deadline exceeded"}}`, wantErr: common.ErrCodeEndpointRequestTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := newRecordingUpstream(t, func(w http.ResponseWriter, _ []byte) {
				body := []byte(tc.body)
				if tc.ctype != "" {
					w.Header().Set("Content-Type", tc.ctype)
				}
				if tc.gzip {
					var buf bytes.Buffer
					gz := gzip.NewWriter(&buf)
					_, _ = gz.Write(body)
					_ = gz.Close()
					body = buf.Bytes()
					w.Header().Set("Content-Encoding", "gzip")
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write(body)
			})
			c := newHttpTestClient(t, up.URL, nil, nil, nil)
			resp, err := c.SendRequest(context.Background(), rpcReq(1, "eth_blockNumber"))
			if tc.wantErr == "" {
				require.NoError(t, err)
				jrr, err := resp.JsonRpcResponse()
				require.NoError(t, err)
				assert.Equal(t, tc.result, jrr.GetResultString())
				return
			}
			require.Error(t, err)
			assert.True(t, common.HasErrorCode(err, tc.wantErr), "want %s, got %v", tc.wantErr, err)
			assert.Contains(t, err.Error(), tc.contains)
			if tc.wantErr == common.ErrCodeJsonRpcExceptionInternal {
				assert.Contains(t, err.Error(), fmt.Sprintf(`"statusCode":%d`, tc.status), "status code must be kept for policies")
			}
		})
	}
}

// statusExtractor maps HTTP status to an endpoint error the way the EVM
// extractor does, to show the client hands the extractor the real response.
type statusExtractor struct{}

func (statusExtractor) Extract(resp *http.Response, _ *common.NormalizedResponse, _ *common.JsonRpcResponse, _ common.Upstream) error {
	if resp.StatusCode == http.StatusTooManyRequests {
		return common.NewErrEndpointCapacityExceeded(fmt.Errorf("status %d", resp.StatusCode))
	}
	return nil
}

func TestHttpJsonRpcClient_ExtractorDecidesBeforeFallback(t *testing.T) {
	up := newRecordingUpstream(t, func(w http.ResponseWriter, _ []byte) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"error":{"code":-32005,"message":"slow down"}}`)
	})
	c := newHttpTestClient(t, up.URL, nil, nil, statusExtractor{})
	_, err := c.SendRequest(context.Background(), rpcReq(1, "eth_call"))
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointCapacityExceeded), "got %v", err)
}

// Single-request transport outcomes: unreachable host, caller deadline,
// caller cancel, and a corrupt gzip body.
func TestHttpJsonRpcClient_SingleTransportOutcomes(t *testing.T) {
	t.Run("unreachable", func(t *testing.T) {
		dead := httptest.NewServer(http.NotFoundHandler())
		addr := dead.URL
		dead.Close()
		c := newHttpTestClient(t, addr, nil, nil, nil)
		_, err := c.SendRequest(context.Background(), rpcReq(1, "eth_blockNumber"))
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointTransportFailure), "got %v", err)
	})

	release := make(chan struct{})
	slow := newRecordingUpstream(t, func(w http.ResponseWriter, _ []byte) { <-release })
	t.Cleanup(func() { close(release) }) // runs before slow.Close (LIFO)

	t.Run("deadline", func(t *testing.T) {
		c := newHttpTestClient(t, slow.URL, nil, nil, nil)
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		_, err := c.SendRequest(ctx, rpcReq(1, "eth_blockNumber"))
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointRequestTimeout), "got %v", err)
	})

	t.Run("cancel", func(t *testing.T) {
		c := newHttpTestClient(t, slow.URL, nil, nil, nil)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, err := c.SendRequest(ctx, rpcReq(1, "eth_blockNumber"))
			done <- err
		}()
		require.Eventually(t, func() bool { return len(slow.requests()) >= 2 }, 5*time.Second, time.Millisecond)
		cancel()
		err := <-done
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointRequestCanceled), "got %v", err)
	})

	t.Run("corrupt gzip", func(t *testing.T) {
		up := newRecordingUpstream(t, func(w http.ResponseWriter, _ []byte) {
			w.Header().Set("Content-Encoding", "gzip")
			_, _ = io.WriteString(w, "plain bytes")
		})
		c := newHttpTestClient(t, up.URL, nil, nil, nil)
		_, err := c.SendRequest(context.Background(), rpcReq(1, "eth_blockNumber"))
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointTransportFailure), "got %v", err)
		assert.Contains(t, err.Error(), "gzip")
	})
}

// The outgoing single request carries configured headers, forwarded client
// headers, gzip encoding when enabled, and the exact JSON-RPC payload.
func TestHttpJsonRpcClient_SingleRequestWireFormat(t *testing.T) {
	for _, gz := range []bool{false, true} {
		t.Run(fmt.Sprintf("gzip=%v", gz), func(t *testing.T) {
			up := newRecordingUpstream(t, func(w http.ResponseWriter, _ []byte) {
				_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":3,"result":"0x1"}`)
			})
			enable := gz
			cfg := &common.JsonRpcUpstreamConfig{
				EnableGzip: &enable,
				Headers:    map[string]string{"Authorization": "Bearer abc"},
			}
			c := newHttpTestClient(t, up.URL, cfg, nil, nil)
			req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":3,"method":"eth_getBalance","params":["0xabc","latest"]}`))
			req.ForwardHeaders = http.Header{"X-Forwarded-For": []string{"1.2.3.4"}}
			_, err := c.SendRequest(context.Background(), req)
			require.NoError(t, err)

			reqs := up.requests()
			require.Len(t, reqs, 1)
			h := reqs[0].header
			assert.Equal(t, "Bearer abc", h.Get("Authorization"))
			assert.Equal(t, "1.2.3.4", h.Get("X-Forwarded-For"))
			assert.Equal(t, "application/json", h.Get("Content-Type"))
			if gz {
				assert.Equal(t, "gzip", h.Get("Content-Encoding"))
			} else {
				assert.Empty(t, h.Get("Content-Encoding"))
			}
			var sent map[string]interface{}
			require.NoError(t, sonic.Unmarshal(reqs[0].body, &sent))
			assert.Equal(t, "eth_getBalance", sent["method"])
			assert.Equal(t, []interface{}{"0xabc", "latest"}, sent["params"])
			assert.EqualValues(t, 3, sent["id"])
		})
	}
}

// Batches are gzip-compressed too when enabled.
func TestHttpJsonRpcClient_BatchGzipRequest(t *testing.T) {
	up := newRecordingUpstream(t, echoBatch)
	cfg := batchCfg(2, time.Minute)
	cfg.EnableGzip = &common.TRUE
	c := newHttpTestClient(t, up.URL, cfg, nil, nil)
	out := sendConcurrently(t, c, context.Background(), []int{1, 2})
	require.NoError(t, out[1].err)
	require.NoError(t, out[2].err)
	reqs := up.requests()
	require.Len(t, reqs, 1)
	assert.Equal(t, "gzip", reqs[0].header.Get("Content-Encoding"))
	assert.Equal(t, []int{2}, batchSizes(reqs))
}

// With a proxy pool configured, requests go through the proxy (which sees
// the absolute upstream URI) rather than straight to the upstream.
func TestHttpJsonRpcClient_RoutesThroughProxyPool(t *testing.T) {
	proxy := newRecordingUpstream(t, func(w http.ResponseWriter, _ []byte) {
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":"0xproxied"}`)
	})
	logger := zerolog.New(io.Discard)
	reg, err := NewProxyPoolRegistry([]*common.ProxyPoolConfig{{ID: "p1", Urls: []string{proxy.URL}}}, &logger)
	require.NoError(t, err)
	pool, err := reg.GetPool("p1")
	require.NoError(t, err)

	const upstreamURL = "http://upstream.invalid:8545/rpc"
	c := newHttpTestClient(t, upstreamURL, &common.JsonRpcUpstreamConfig{ProxyPool: "p1"}, pool, nil)
	resp, err := c.SendRequest(context.Background(), rpcReq(1, "eth_blockNumber"))
	require.NoError(t, err)
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	assert.Equal(t, `"0xproxied"`, jrr.GetResultString())

	reqs := proxy.requests()
	require.Len(t, reqs, 1)
	assert.Equal(t, upstreamURL, reqs[0].uri)
}

// Proxy pool registry lookups and invalid configurations.
func TestProxyPoolRegistry_Configs(t *testing.T) {
	logger := zerolog.New(io.Discard)

	reg, err := NewProxyPoolRegistry(nil, &logger)
	require.NoError(t, err)
	p, err := reg.GetPool("")
	require.NoError(t, err)
	assert.Nil(t, p, "empty pool id means direct connection")
	_, err = reg.GetPool("missing")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing")

	_, err = NewProxyPoolRegistry([]*common.ProxyPoolConfig{{ID: "empty"}}, &logger)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no proxy URLs defined for pool 'empty'")

	_, err = NewProxyPoolRegistry([]*common.ProxyPoolConfig{{ID: "bad", Urls: []string{"http://[::1"}}}, &logger)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid proxy URL")

	reg, err = NewProxyPoolRegistry([]*common.ProxyPoolConfig{{ID: "rr", Urls: []string{"http://a:1", "http://b:2"}}}, &logger)
	require.NoError(t, err)
	pool, err := reg.GetPool("rr")
	require.NoError(t, err)
	seen := map[string]bool{}
	for range 4 {
		hc, err := pool.GetClient()
		require.NoError(t, err)
		u, err := hc.Transport.(*http.Transport).Proxy(&http.Request{URL: &url.URL{Scheme: "http", Host: "x"}})
		require.NoError(t, err)
		seen[u.Host] = true
	}
	assert.Equal(t, map[string]bool{"a:1": true, "b:2": true}, seen, "clients are handed out round-robin")

	_, err = (&ProxyPool{ID: "none"}).GetClient()
	require.Error(t, err)
}
