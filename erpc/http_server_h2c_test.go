package erpc

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/http2"
)

// TestHttpServer_H2C_WithoutSharedGrpc pins issue #965: a non-TLS HTTP
// listener must accept cleartext HTTP/2 (h2c) prior-knowledge requests even
// when shared gRPC is disabled. Prior to the fix the h2c wrapper was only
// installed inside the grpcSharesHttpV4 branch, so with gRPC disabled the
// plain http.Server spoke HTTP/1.x only and h2c connections failed.
//
// This exercises a REAL cleartext HTTP/2 connection over a real listener (not
// httptest.NewRecorder with a hand-set ProtoMajor), so it proves protocol
// negotiation actually works end-to-end.
func TestHttpServer_H2C_WithoutSharedGrpc(t *testing.T) {
	mainMutex.Lock()
	defer mainMutex.Unlock()

	defer gock.Off()
	defer gock.DisableNetworking()
	defer gock.Clean()
	defer gock.CleanUnmatchedRequest()

	gock.EnableNetworking()
	gock.NetworkingFilter(func(req *http.Request) bool {
		host := strings.Split(req.URL.Host, ":")[0]
		return host == "localhost" || host == "127.0.0.1"
	})

	util.SetupMocksForEvmStatePoller()

	localHost := "127.0.0.1"
	httpPort := 4000
	cfg := &common.Config{
		LogLevel: "WARN",
		Server: &common.ServerConfig{
			HttpHostV4: &localHost,
			ListenV4:   util.BoolPtr(true),
			HttpPortV4: &httpPort,
			// gRPC disabled (default) and TLS disabled (nil): the exact
			// configuration in which #965 reproduces.
		},
		Projects: []*common.ProjectConfig{
			{
				Id: "main",
				Upstreams: []*common.UpstreamConfig{
					{
						Id:       "good-evm-rpc",
						Endpoint: "http://rpc1.localhost",
						Type:     "evm",
						Evm: &common.EvmUpstreamConfig{
							ChainId: 123,
						},
					},
				},
				Networks: []*common.NetworkConfig{
					{
						Architecture: "evm",
						Evm: &common.EvmNetworkConfig{
							ChainId: 123,
						},
					},
				},
			},
		},
	}
	require.NoError(t, cfg.SetDefaults(nil))
	// Precondition: gRPC does not share the HTTP listener, so the old code
	// path would never install the h2c wrapper.
	require.False(t, grpcSharesHttpV4(cfg.Server))

	logger := log.Logger
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	erpcInstance, err := NewERPC(ctx, &logger, nil, nil, nil, cfg)
	require.NoError(t, err)
	erpcInstance.Bootstrap(ctx)

	httpServer, err := NewHttpServer(ctx, &logger, cfg.Server, cfg.HealthCheck, cfg.Admin, cfg.Indexer, erpcInstance)
	require.NoError(t, err)
	require.Nil(t, httpServer.sharedGrpcServer, "precondition: shared gRPC must be disabled")

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port

	go func() {
		if serveErr := httpServer.serverV4.Serve(listener); serveErr != nil && serveErr != http.ErrServerClosed {
			t.Errorf("server error: %v", serveErr)
		}
	}()
	defer httpServer.serverV4.Shutdown(context.Background())

	time.Sleep(300 * time.Millisecond)

	url := fmt.Sprintf("http://127.0.0.1:%d/main/evm/123", port)
	reqBody := `{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`

	t.Run("h2c_prior_knowledge", func(t *testing.T) {
		// Cleartext HTTP/2 client: AllowHTTP + a plain (non-TLS) dialer for the
		// "TLS" hook is the canonical way to force h2c prior-knowledge.
		transport := &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
		}
		client := &http.Client{Transport: transport}
		defer transport.CloseIdleConnections()

		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(reqBody))
		require.NoError(t, err)
		httpReq.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(httpReq)
		require.NoError(t, err, "cleartext HTTP/2 (h2c) request must connect")
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)

		// Transport-level assertion: the connection actually negotiated HTTP/2.
		require.Equal(t, 2, resp.ProtoMajor, "response must be served over HTTP/2")
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Contains(t, string(body), `"result":"0x7b"`)
	})

	t.Run("http1.1_still_works", func(t *testing.T) {
		// Force HTTP/1.1 explicitly to confirm the same listener still serves it.
		transport := &http.Transport{}
		transport.ForceAttemptHTTP2 = false
		transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
		client := &http.Client{Transport: transport}
		defer transport.CloseIdleConnections()

		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(reqBody))
		require.NoError(t, err)
		httpReq.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(httpReq)
		require.NoError(t, err)
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)

		require.Equal(t, 1, resp.ProtoMajor, "plain client must still be served over HTTP/1.1")
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Contains(t, string(body), `"result":"0x7b"`)
	})
}
