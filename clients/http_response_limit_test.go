package clients

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/url"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

type responseLimitRoundTripper func(*http.Request) (*http.Response, error)

func (f responseLimitRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type trackedResponseBody struct {
	io.Reader
	closed bool
	read   int
}

type cancelingResponseBody struct {
	ctx    context.Context
	cancel context.CancelFunc
	sent   bool
	closed bool
}

func (b *cancelingResponseBody) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		n := copy(p, `{"jsonrpc":"2.0","id":1,"result":`)
		b.cancel()
		return n, nil
	}
	return 0, b.ctx.Err()
}

func (b *cancelingResponseBody) Close() error {
	b.closed = true
	return nil
}

func (b *trackedResponseBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.read += n
	return n, err
}

func (b *trackedResponseBody) Close() error {
	b.closed = true
	return nil
}

func newResponseLimitClient(t *testing.T, limit int64) *GenericHttpJsonRpcClient {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	endpoint, err := url.Parse("http://response-limit.local")
	require.NoError(t, err)
	upstream := common.NewFakeUpstream("response-limit")
	upstream.Config().Endpoint = endpoint.String()
	var cfg *common.JsonRpcUpstreamConfig
	if limit >= 0 {
		cfg = &common.JsonRpcUpstreamConfig{MaxResponseBytes: &limit}
	}
	client, err := NewGenericHttpJsonRpcClient(ctx, &log.Logger, "test", upstream, endpoint, cfg, nil, &noopErrorExtractor{})
	require.NoError(t, err)
	return client.(*GenericHttpJsonRpcClient)
}

func TestHttpJsonRpcClient_ResponseSizeLimit(t *testing.T) {
	response := []byte(`{"jsonrpc":"2.0","id":1,"result":"` + string(bytes.Repeat([]byte("x"), 4096)) + `"}`)
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	_, err := zw.Write(response)
	require.NoError(t, err)
	require.NoError(t, zw.Close())

	tests := []struct {
		name          string
		limit         int64
		contentLength int64
		gzip          bool
		chunked       bool
		wantTooLarge  bool
	}{
		{"disabled", 0, int64(len(response)), false, false, false},
		{"below limit", int64(len(response) + 1), int64(len(response)), false, false, false},
		{"exact limit", int64(len(response)), int64(len(response)), false, false, false},
		{"above limit", int64(len(response) - 1), int64(len(response)), false, false, true},
		{"missing length", 128, -1, false, false, true},
		{"chunked", 128, -1, false, true, true},
		{"incorrect short length", 128, 1, false, false, true},
		{"incorrect long length", 128, 1 << 30, false, false, true},
		{"overstated length below limit", int64(len(response) + 1), 1 << 30, false, false, false},
		{"decoded gzip above limit", 128, int64(compressed.Len()), true, false, true},
		{"decoded gzip exact limit", int64(len(response)), int64(compressed.Len()), true, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newResponseLimitClient(t, tt.limit)
			payload := response
			if tt.gzip {
				payload = compressed.Bytes()
			}
			body := &trackedResponseBody{Reader: bytes.NewReader(payload)}
			client.httpClient = &http.Client{Transport: responseLimitRoundTripper(func(req *http.Request) (*http.Response, error) {
				header := make(http.Header)
				var transferEncoding []string
				if tt.chunked {
					transferEncoding = []string{"chunked"}
				}
				if tt.gzip {
					header.Set("Content-Encoding", "gzip")
				}
				return &http.Response{StatusCode: 200, Header: header, Body: body, ContentLength: tt.contentLength, TransferEncoding: transferEncoding, Request: req}, nil
			})}
			req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_test","params":[]}`))
			result, err := client.SendRequest(context.Background(), req)
			if tt.wantTooLarge {
				require.Nil(t, result)
				require.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamResponseTooLarge), "%v", err)
			} else {
				require.NoError(t, err)
				require.NotNil(t, result)
				result.Release()
			}
			require.True(t, body.closed)
			if tt.wantTooLarge && !tt.gzip {
				require.Equal(t, int(tt.limit+1), body.read)
			}
		})
	}
}

func TestHttpJsonRpcClient_BatchResponseSizeLimit(t *testing.T) {
	batch := []byte(`[{"jsonrpc":"2.0","id":1,"result":"` + string(bytes.Repeat([]byte("x"), 128)) + `"},{"jsonrpc":"2.0","id":2,"result":"ok"}]`)
	for _, compressed := range []bool{false, true} {
		name := "plain"
		if compressed {
			name = "gzip"
		}
		t.Run(name, func(t *testing.T) {
			client := newResponseLimitClient(t, 64)
			payload := batch
			header := make(http.Header)
			if compressed {
				var buffer bytes.Buffer
				zw := gzip.NewWriter(&buffer)
				_, err := zw.Write(batch)
				require.NoError(t, err)
				require.NoError(t, zw.Close())
				payload = buffer.Bytes()
				header.Set("Content-Encoding", "gzip")
			}
			body := &trackedResponseBody{Reader: bytes.NewReader(payload)}
			requests := map[interface{}]*batchRequest{}
			for _, id := range []int{1, 2} {
				requests[id] = &batchRequest{
					request:  common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_test","params":[]}`)),
					response: make(chan *common.NormalizedResponse, 1),
					err:      make(chan error, 1),
				}
			}
			client.processBatchResponse(requests, &http.Response{StatusCode: 200, Header: header, Body: body, ContentLength: -1})
			for _, req := range requests {
				select {
				case err := <-req.err:
					require.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamResponseTooLarge), "%v", err)
				default:
					t.Fatal("batch request received no limit error")
				}
				select {
				case resp := <-req.response:
					resp.Release()
					t.Fatal("oversized batch produced a response")
				default:
				}
			}
			require.True(t, body.closed)
			if !compressed {
				require.Equal(t, 65, body.read)
			}
		})
	}
}

func TestHttpJsonRpcClient_BatchResponseAtLimit(t *testing.T) {
	batch := []byte(`[{"jsonrpc":"2.0","id":1,"result":"0x1"}]`)
	client := newResponseLimitClient(t, int64(len(batch)))
	body := &trackedResponseBody{Reader: bytes.NewReader(batch)}
	req := &batchRequest{
		ctx:      context.Background(),
		request:  common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_test","params":[]}`)),
		response: make(chan *common.NormalizedResponse, 1),
		err:      make(chan error, 1),
	}
	client.processBatchResponse(map[interface{}]*batchRequest{int64(1): req}, &http.Response{
		StatusCode: 200, Header: make(http.Header), Body: body, ContentLength: int64(len(batch)),
	})
	select {
	case err := <-req.err:
		t.Fatalf("response at the limit was rejected: %v", err)
	case resp := <-req.response:
		defer resp.Release()
		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		require.Equal(t, `"0x1"`, jrr.GetResultString())
	default:
		t.Fatal("batch request received no response")
	}
	require.True(t, body.closed)
}

func TestHttpJsonRpcClient_PartialResponseUnderLimit(t *testing.T) {
	client := newResponseLimitClient(t, 128)
	body := &trackedResponseBody{Reader: bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":1,"result":`))}
	client.httpClient = &http.Client{Transport: responseLimitRoundTripper(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: body, ContentLength: 512, Request: req}, nil
	})}
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_test","params":[]}`))
	resp, err := client.SendRequest(context.Background(), req)
	if resp != nil {
		resp.Release()
	}
	require.Error(t, err)
	require.False(t, common.HasErrorCode(err, common.ErrCodeUpstreamResponseTooLarge))
	require.True(t, body.closed)
}

func TestHttpJsonRpcClient_CanceledResponseClosesBody(t *testing.T) {
	client := newResponseLimitClient(t, 128)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body := &cancelingResponseBody{ctx: ctx, cancel: cancel}
	client.httpClient = &http.Client{Transport: responseLimitRoundTripper(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: body, ContentLength: -1, Request: req}, nil
	})}
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_test","params":[]}`))
	resp, err := client.SendRequest(ctx, req)
	if resp != nil {
		resp.Release()
	}
	require.Error(t, err)
	require.False(t, common.HasErrorCode(err, common.ErrCodeUpstreamResponseTooLarge))
	require.True(t, body.closed)
}
