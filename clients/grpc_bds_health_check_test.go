package clients

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"testing"
	"time"

	"github.com/blockchain-data-standards/manifesto/evm"
	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

const bdsHealthService = "bds.evm.RPCQueryService"

// startHealthReportingServer serves the BDS happy server plus grpc.health,
// with bdsHealthService registered at `initial`.
func startHealthReportingServer(t *testing.T, initial healthpb.HealthCheckResponse_ServingStatus) (string, *health.Server) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := grpc.NewServer()
	evm.RegisterRPCQueryServiceServer(srv, &happyRPCServer{chainID: 1})
	hs := health.NewServer()
	hs.SetServingStatus(bdsHealthService, initial)
	healthpb.RegisterHealthServer(srv, hs)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String(), hs
}

func newHealthCheckedClient(t *testing.T, addr, healthCheckService string) GrpcBdsClient {
	t.Helper()
	parsedURL, err := url.Parse(fmt.Sprintf("grpc://%s", addr))
	require.NoError(t, err)
	logger := zerolog.New(io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	client, err := NewGrpcBdsClient(ctx, &logger, "test-project", nil, parsedURL, 0, healthCheckService)
	require.NoError(t, err)
	return client
}

func chainIdWithin(client GrpcBdsClient, d time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`))
	_, err := client.SendRequest(ctx, req)
	return err
}

// A reader that is up but has not finished init reports NOT_SERVING; with a
// health-check service configured, the pool must not send it requests until it
// flips to SERVING.
func TestGrpcBdsClient_HealthCheckService_GatesOnServing(t *testing.T) {
	addr, hs := startHealthReportingServer(t, healthpb.HealthCheckResponse_NOT_SERVING)
	client := newHealthCheckedClient(t, addr, bdsHealthService)

	require.Error(t, chainIdWithin(client, 500*time.Millisecond), "a NOT_SERVING reader received the request")

	hs.SetServingStatus(bdsHealthService, healthpb.HealthCheckResponse_SERVING)
	require.Eventually(t, func() bool { return chainIdWithin(client, 500*time.Millisecond) == nil },
		5*time.Second, 50*time.Millisecond, "the pool never used the reader after SERVING")
}

// Unset keeps today's behaviour: health status is not consulted.
func TestGrpcBdsClient_HealthCheckService_UnsetIgnoresHealth(t *testing.T) {
	addr, _ := startHealthReportingServer(t, healthpb.HealthCheckResponse_NOT_SERVING)
	client := newHealthCheckedClient(t, addr, "")
	require.NoError(t, chainIdWithin(client, 2*time.Second))
}

// A server without grpc.health (e.g. a gateway) answers UNIMPLEMENTED, which
// grpc-go treats as healthy: configuring the service must not black-hole it.
func TestGrpcBdsClient_HealthCheckService_ServerWithoutHealthIsUsed(t *testing.T) {
	addr, _, stop := startHappyServer(t, 1, 0)
	defer stop()
	client := newHealthCheckedClient(t, addr, bdsHealthService)
	require.NoError(t, chainIdWithin(client, 2*time.Second))
}
