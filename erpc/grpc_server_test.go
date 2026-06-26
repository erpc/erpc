package erpc

import (
	"context"
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
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
)

func TestGrpcClientIP_IgnoresForwardedHeaderFromUntrustedPeer(t *testing.T) {
	gs := &GrpcServer{
		trustedForwarderIPs: map[string]struct{}{
			"127.0.0.1": {},
		},
		trustedIPHeaders: []string{"x-forwarded-for"},
	}
	md := metadata.New(map[string]string{
		"x-forwarded-for": "127.0.0.1",
	})
	ctx := peer.NewContext(context.Background(), &peer.Peer{
		Addr: &net.TCPAddr{IP: net.ParseIP("198.51.100.25"), Port: 9000},
	})

	assert.Equal(t, "198.51.100.25", gs.grpcClientIP(ctx, md))
}

func TestGrpcClientIP_UsesConfiguredForwardedHeaderFromTrustedPeer(t *testing.T) {
	gs := &GrpcServer{
		trustedForwarderIPs: map[string]struct{}{
			"127.0.0.1": {},
		},
		trustedIPHeaders: []string{"x-forwarded-for"},
	}
	md := metadata.New(map[string]string{
		"x-forwarded-for": "203.0.113.9",
	})
	ctx := peer.NewContext(context.Background(), &peer.Peer{
		Addr: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 9000},
	})

	assert.Equal(t, "203.0.113.9", gs.grpcClientIP(ctx, md))
}

func TestGrpcClientIP_IgnoresForwardedHeaderWhenHeaderNotTrusted(t *testing.T) {
	gs := &GrpcServer{
		trustedForwarderIPs: map[string]struct{}{
			"127.0.0.1": {},
		},
	}
	md := metadata.New(map[string]string{
		"x-forwarded-for": "203.0.113.9",
	})
	ctx := peer.NewContext(context.Background(), &peer.Peer{
		Addr: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 9000},
	})

	assert.Equal(t, "127.0.0.1", gs.grpcClientIP(ctx, md))
}

func TestHttpServer_CanSharePortWithGrpc(t *testing.T) {
	mainMutex.Lock()
	defer mainMutex.Unlock()

	defer gock.Off()
	defer gock.DisableNetworking()
	defer gock.Clean()
	defer gock.CleanUnmatchedRequest()

	gock.EnableNetworking()
	gock.NetworkingFilter(func(req *http.Request) bool {
		return strings.Split(req.URL.Host, ":")[0] == "localhost" || strings.Split(req.URL.Host, ":")[0] == "127.0.0.1"
	})

	util.SetupMocksForEvmStatePoller()
	gock.New("http://rpc1.localhost").
		Post("").
		Times(1).
		Filter(func(request *http.Request) bool {
			body := util.SafeReadBody(request)
			return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, "\"0x1\"")
		}).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"number":"0x1","hash":"0x0000000000000000000000000000000000000000000000000000000000000001","parentHash":"0x0000000000000000000000000000000000000000000000000000000000000000","sha3Uncles":"0x1dcc4de8dec75d7aab85b567b6ccd41ad312451b948a7413f0a142fd40d49347","miner":"0x0000000000000000000000000000000000000000","stateRoot":"0x0000000000000000000000000000000000000000000000000000000000000000","transactionsRoot":"0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421","receiptsRoot":"0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421","logsBloom":"0x00000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000","difficulty":"0x0","gasLimit":"0x5208","gasUsed":"0x0","timestamp":"0x1","extraData":"0x","mixHash":"0x0000000000000000000000000000000000000000000000000000000000000000","nonce":"0x0000000000000000","baseFeePerGas":"0x7","size":"0x1","transactions":[]}}`))

	localHost := "127.0.0.1"
	httpPort := 4000
	cfg := &common.Config{
		LogLevel: "DEBUG",
		Server: &common.ServerConfig{
			HttpHostV4:  &localHost,
			ListenV4:    util.BoolPtr(true),
			HttpPortV4:  &httpPort,
			GrpcEnabled: util.BoolPtr(true),
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
	require.Equal(t, *cfg.Server.HttpHostV4, *cfg.Server.GrpcHostV4)
	require.Equal(t, *cfg.Server.HttpPortV4, *cfg.Server.GrpcPortV4)

	logger := log.Logger
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	erpcInstance, err := NewERPC(ctx, &logger, nil, nil, nil, cfg)
	require.NoError(t, err)
	erpcInstance.Bootstrap(ctx)

	httpServer, err := NewHttpServer(ctx, &logger, cfg.Server, cfg.HealthCheck, cfg.Admin, erpcInstance)
	require.NoError(t, err)
	require.NotNil(t, httpServer.sharedGrpcServer)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port

	go func() {
		err := httpServer.serverV4.Serve(listener)
		if err != nil && err != http.ErrServerClosed {
			t.Errorf("server error: %v", err)
		}
	}()
	defer httpServer.serverV4.Shutdown(context.Background())

	time.Sleep(300 * time.Millisecond)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/main/evm/123", port), strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`))
	require.NoError(t, err)
	httpReq.Header.Set("Content-Type", "application/json")
	httpResp, err := http.DefaultClient.Do(httpReq)
	require.NoError(t, err)
	defer httpResp.Body.Close()
	httpBody, err := io.ReadAll(httpResp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, httpResp.StatusCode)
	assert.Contains(t, string(httpBody), `"result":"0x7b"`)

	conn, err := grpc.NewClient(
		fmt.Sprintf("127.0.0.1:%d", port),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	defer conn.Close()

	grpcCtx := metadata.NewOutgoingContext(ctx, metadata.New(map[string]string{
		"x-erpc-project":  "main",
		"x-erpc-chain-id": "123",
	}))
	grpcClient := evm.NewRPCQueryServiceClient(conn)
	grpcResp, err := grpcClient.GetBlockByNumber(grpcCtx, &evm.GetBlockByNumberRequest{
		BlockNumber:         "0x1",
		IncludeTransactions: false,
	})
	require.NoError(t, err)
	require.NotNil(t, grpcResp.Block)
	assert.Equal(t, uint64(1), grpcResp.Block.Number)
	assert.Empty(t, grpcResp.FullTransactions)
}
