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

	"github.com/blockchain-data-standards/manifesto/evm"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/http2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
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

	cfg := h2cTestConfig()
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

	httpServer, err := NewHttpServer(ctx, &logger, cfg.Server, cfg.HealthCheck, cfg.Admin, erpcInstance)
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

// h2cTestConfig is a non-TLS, gRPC-disabled server with one EVM network whose
// eth_chainId (0x7b) is answered without an upstream call.
func h2cTestConfig() *common.Config {
	localHost := "127.0.0.1"
	httpPort := 4000
	return &common.Config{
		LogLevel: "WARN",
		Server: &common.ServerConfig{
			HttpHostV4: &localHost,
			ListenV4:   util.BoolPtr(true),
			HttpPortV4: &httpPort,
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
}

// A cleartext HTTP/2 connection must drain like any other on SIGTERM: a stream
// in flight when Shutdown starts gets its full response, the connection
// receives GOAWAY so the client stops opening streams on it, and the server
// then closes it. Clients that pool h2c connections behind an L4 load balancer
// rely on that GOAWAY to move to another instance; without it they keep
// sending to the dying process until it exits and every in-flight stream dies.
func TestHttpServer_H2C_ShutdownGoAwaysAndDrainsInFlightStream(t *testing.T) {
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

	cfg := h2cTestConfig()
	cfg.Server.WaitBeforeShutdown = common.Duration(100 * time.Millisecond).Ptr()
	require.NoError(t, cfg.SetDefaults(nil))

	logger := log.Logger
	appCtx, sigterm := context.WithCancel(context.Background())
	defer sigterm()

	erpcInstance, err := NewERPC(appCtx, &logger, nil, nil, nil, cfg)
	require.NoError(t, err)
	erpcInstance.Bootstrap(appCtx)

	httpServer, err := NewHttpServer(appCtx, &logger, cfg.Server, cfg.HealthCheck, cfg.Admin, erpcInstance)
	require.NoError(t, err)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := listener.Addr().String()
	go func() {
		if serveErr := httpServer.serverV4.Serve(listener); serveErr != nil && serveErr != http.ErrServerClosed {
			t.Errorf("server error: %v", serveErr)
		}
	}()
	defer httpServer.serverV4.Close()

	// One prior-knowledge h2c connection carries every stream below, so the
	// assertions are about that connection's lifecycle.
	rawConn, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	cc, err := (&http2.Transport{AllowHTTP: true}).NewClientConn(rawConn)
	require.NoError(t, err)
	defer cc.Close()

	const reqBody = `{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`
	newRequest := func(ctx context.Context, body io.Reader) *http.Request {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+"/main/evm/123", body)
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		return req
	}

	resp, err := cc.RoundTrip(newRequest(context.Background(), strings.NewReader(reqBody)))
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	require.Equal(t, 2, resp.ProtoMajor)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// The slow stream: its body arrives in two halves, so the handler stays
	// blocked on the read until the test releases the second half.
	type result struct {
		resp *http.Response
		body []byte
		err  error
	}
	slow := make(chan result, 1)
	bodyR, bodyW := io.Pipe()
	go func() {
		resp, err := cc.RoundTrip(newRequest(context.Background(), bodyR))
		if err != nil {
			slow <- result{err: err}
			return
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		slow <- result{resp: resp, body: body, err: err}
	}()
	_, err = bodyW.Write([]byte(reqBody[:20]))
	require.NoError(t, err)
	time.Sleep(200 * time.Millisecond)

	sigterm()

	// Shutdown has started once the listener refuses new connections.
	require.Eventually(t, func() bool {
		c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err != nil {
			return true
		}
		c.Close()
		return false
	}, 5*time.Second, 20*time.Millisecond, "Shutdown never closed the listener")

	select {
	case r := <-slow:
		t.Fatalf("in-flight stream ended before its body was complete: err=%v", r.err)
	default:
	}

	require.Eventually(t, func() bool { return cc.State().Closing }, 3*time.Second, 10*time.Millisecond,
		"h2c connection never received GOAWAY after Shutdown started")

	newCtx, cancelNew := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelNew()
	_, err = cc.RoundTrip(newRequest(newCtx, strings.NewReader(reqBody)))
	require.Error(t, err, "a new stream on a GOAWAYed connection must be refused")
	require.NotErrorIs(t, err, context.DeadlineExceeded, "a new stream on a GOAWAYed connection hung instead of being refused")

	_, err = bodyW.Write([]byte(reqBody[20:]))
	require.NoError(t, err)
	require.NoError(t, bodyW.Close())

	select {
	case r := <-slow:
		require.NoError(t, r.err, "the stream in flight when Shutdown started must complete")
		require.Equal(t, 2, r.resp.ProtoMajor)
		require.Equal(t, http.StatusOK, r.resp.StatusCode)
		assert.Contains(t, string(r.body), `"result":"0x7b"`)
	case <-time.After(5 * time.Second):
		t.Fatal("the stream in flight when Shutdown started never completed")
	}

	require.Eventually(t, func() bool { return cc.State().Closed }, 5*time.Second, 20*time.Millisecond,
		"server never closed the drained h2c connection")
}

// Shared-port gRPC rides the HTTP server's cleartext HTTP/2 connections and
// GracefulStop never runs for it, so the HTTP drain is all that keeps its RPCs
// alive on SIGTERM: a unary RPC in flight when Shutdown starts must complete,
// and the client must then be pushed off the connection instead of sending
// more RPCs to the dying process.
func TestHttpServer_SharedGrpc_ShutdownDrainsInFlightRPC(t *testing.T) {
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
	// The upstream answers slowly, so the RPC is still in flight when Shutdown
	// starts (waitBeforeShutdown after SIGTERM).
	gock.New("http://rpc1.localhost").
		Post("").
		Times(1).
		Filter(func(request *http.Request) bool {
			body := util.SafeReadBody(request)
			return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, `"0x1"`)
		}).
		Reply(200).
		Delay(1500 * time.Millisecond).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"number":"0x1","hash":"0x0000000000000000000000000000000000000000000000000000000000000001","parentHash":"0x0000000000000000000000000000000000000000000000000000000000000000","gasLimit":"0x5208","gasUsed":"0x0","timestamp":"0x1","transactions":[]}}`))

	cfg := h2cTestConfig()
	cfg.Server.GrpcEnabled = util.BoolPtr(true)
	cfg.Server.WaitBeforeShutdown = common.Duration(100 * time.Millisecond).Ptr()
	require.NoError(t, cfg.SetDefaults(nil))
	require.True(t, grpcSharesHttpV4(cfg.Server), "precondition: gRPC must share the HTTP port")

	logger := log.Logger
	appCtx, sigterm := context.WithCancel(context.Background())
	defer sigterm()

	erpcInstance, err := NewERPC(appCtx, &logger, nil, nil, nil, cfg)
	require.NoError(t, err)
	erpcInstance.Bootstrap(appCtx)

	httpServer, err := NewHttpServer(appCtx, &logger, cfg.Server, cfg.HealthCheck, cfg.Admin, erpcInstance)
	require.NoError(t, err)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := listener.Addr().String()
	go func() {
		if serveErr := httpServer.serverV4.Serve(listener); serveErr != nil && serveErr != http.ErrServerClosed {
			t.Errorf("server error: %v", serveErr)
		}
	}()
	defer httpServer.serverV4.Close()

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	defer conn.Close()
	grpcCtx := metadata.NewOutgoingContext(context.Background(), metadata.New(map[string]string{
		"x-erpc-project":  "main",
		"x-erpc-chain-id": "123",
	}))
	client := evm.NewRPCQueryServiceClient(conn)

	// Initialize the network first: lazy initialization is bound to appCtx, so a
	// first request racing SIGTERM would fail for that unrelated reason.
	chainId, err := client.ChainId(grpcCtx, &evm.ChainIdRequest{})
	require.NoError(t, err)
	require.Equal(t, uint64(123), chainId.ChainId)

	type result struct {
		resp *evm.GetBlockResponse
		err  error
	}
	inFlight := make(chan result, 1)
	go func() {
		resp, err := client.GetBlockByNumber(grpcCtx, &evm.GetBlockByNumberRequest{BlockNumber: "0x1"})
		inFlight <- result{resp: resp, err: err}
	}()
	time.Sleep(200 * time.Millisecond)

	sigterm()

	// Shutdown has started once the listener refuses new connections.
	require.Eventually(t, func() bool {
		c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err != nil {
			return true
		}
		c.Close()
		return false
	}, 5*time.Second, 20*time.Millisecond, "Shutdown never closed the listener")

	select {
	case r := <-inFlight:
		t.Fatalf("RPC ended before Shutdown could drain it: err=%v", r.err)
	default:
	}

	select {
	case r := <-inFlight:
		require.NoError(t, r.err, "the RPC in flight when Shutdown started must complete")
		require.NotNil(t, r.resp.Block)
		assert.Equal(t, uint64(1), r.resp.Block.Number)
	case <-time.After(5 * time.Second):
		t.Fatal("the RPC in flight when Shutdown started never completed")
	}

	// The drained connection is gone and the listener is closed, so the next
	// RPC fails instead of still being served by the shutting-down server.
	newCtx, cancelNew := context.WithTimeout(grpcCtx, 3*time.Second)
	defer cancelNew()
	_, err = client.ChainId(newCtx, &evm.ChainIdRequest{})
	require.Error(t, err, "a new RPC after the drain must not be served by the shutting-down server")
}
