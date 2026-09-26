package clients

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog/log"
)

func init() {
	util.ConfigureTestLogger()
}

func BenchmarkHttpJsonRpcClient_ResponseSize(b *testing.B) {
	for _, size := range []int{1 << 10, 64 << 10, 1 << 20, 8 << 20, 32 << 20, 64 << 20} {
		b.Run(fmt.Sprintf("%dKiB", size>>10), func(b *testing.B) {
			benchmarkHttpJsonRpcAcceptedResponse(b, size, false)
		})
	}
}

func BenchmarkHttpJsonRpcClient_AcceptedResponseWithLimit(b *testing.B) {
	for _, size := range []int{1 << 10, 1 << 20, 8 << 20} {
		b.Run(fmt.Sprintf("%dKiB", size>>10), func(b *testing.B) {
			benchmarkHttpJsonRpcAcceptedResponse(b, size, true)
		})
	}
}

func benchmarkHttpJsonRpcAcceptedResponse(b *testing.B, size int, withLimit bool) {
	body := []byte(`{"jsonrpc":"2.0","id":1,"result":"` + strings.Repeat("x", size) + `"}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer server.Close()

	endpoint, err := url.Parse(server.URL)
	if err != nil {
		b.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	upstream := common.NewFakeUpstream("benchmark")
	upstream.Config().Endpoint = server.URL
	var cfg *common.JsonRpcUpstreamConfig
	if withLimit {
		limit := int64(len(body) + 1)
		cfg = &common.JsonRpcUpstreamConfig{MaxResponseBytes: &limit}
	}
	client, err := NewGenericHttpJsonRpcClient(ctx, &log.Logger, "benchmark", upstream, endpoint, cfg, nil, &noopErrorExtractor{})
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_test","params":[]}`))
		resp, err := client.SendRequest(ctx, req)
		if err != nil {
			b.Fatal(err)
		}
		resp.Release()
	}
}

func BenchmarkHttpJsonRpcClient_RejectedResponse(b *testing.B) {
	for _, size := range []int{8 << 20, 32 << 20, 64 << 20} {
		b.Run(fmt.Sprintf("%dMiB", size>>20), func(b *testing.B) {
			body := []byte(`{"jsonrpc":"2.0","id":1,"result":"` + strings.Repeat("x", size) + `"}`)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(body)
			}))
			defer server.Close()
			endpoint, err := url.Parse(server.URL)
			if err != nil {
				b.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			upstream := common.NewFakeUpstream("benchmark")
			upstream.Config().Endpoint = server.URL
			limit := int64(1 << 20)
			cfg := &common.JsonRpcUpstreamConfig{MaxResponseBytes: &limit}
			client, err := NewGenericHttpJsonRpcClient(ctx, &log.Logger, "benchmark", upstream, endpoint, cfg, nil, &noopErrorExtractor{})
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_test","params":[]}`))
				resp, err := client.SendRequest(ctx, req)
				if resp != nil {
					resp.Release()
					b.Fatal("oversized response was accepted")
				}
				if !common.HasErrorCode(err, common.ErrCodeUpstreamResponseTooLarge) {
					b.Fatalf("expected response limit error, got %v", err)
				}
			}
		})
	}
}
