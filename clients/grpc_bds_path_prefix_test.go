package clients

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/blockchain-data-standards/manifesto/evm"
	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// startMountedServer serves ChainId ONLY at mount + the canonical method, the
// way edge-api serves Boost under `/boost/bds.evm.RPCQueryService/...`. Any
// other method name, the bare canonical one included, is Unimplemented.
func startMountedServer(t *testing.T, mount string, chainID uint64) (string, func() []string) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	var mu sync.Mutex
	var seen []string
	want := mount + evm.RPCQueryService_ChainId_FullMethodName
	srv := grpc.NewServer(grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
		method, _ := grpc.MethodFromServerStream(stream)
		mu.Lock()
		seen = append(seen, method)
		mu.Unlock()
		if method != want {
			return status.Errorf(codes.Unimplemented, "unknown method %s", method)
		}
		if err := stream.RecvMsg(&evm.ChainIdRequest{}); err != nil && err != io.EOF {
			return err
		}
		return stream.SendMsg(&evm.ChainIdResponse{ChainId: chainID})
	}))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String(), func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

func newPrefixedClient(t *testing.T, rawURL string) GrpcBdsClient {
	t.Helper()
	parsedURL, err := url.Parse(rawURL)
	require.NoError(t, err)
	logger := zerolog.New(io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	client, err := NewGrpcBdsClient(ctx, &logger, "test-project", nil, parsedURL, 1, "")
	require.NoError(t, err)
	return client
}

// A URL path is the server's mount point: every call goes to path + method.
// Before, the path was dropped and the call hit the bare method, which a
// mounted server does not serve.
func TestGrpcBdsClient_URLPathMountsEveryMethod(t *testing.T) {
	addr, seen := startMountedServer(t, "/boost", 1)
	client := newPrefixedClient(t, fmt.Sprintf("grpc://%s/boost/", addr))

	require.NoError(t, chainIdWithin(client, 2*time.Second))
	require.Equal(t, []string{"/boost" + evm.RPCQueryService_ChainId_FullMethodName}, seen())
}

// No path keeps the canonical method, so every existing root-mounted
// upstream is unchanged.
func TestGrpcBdsClient_NoURLPathCallsCanonicalMethod(t *testing.T) {
	addr, seen := startMountedServer(t, "", 1)
	client := newPrefixedClient(t, fmt.Sprintf("grpc://%s", addr))

	require.NoError(t, chainIdWithin(client, 2*time.Second))
	require.Equal(t, []string{evm.RPCQueryService_ChainId_FullMethodName}, seen())
}

// grpcs:// is the documented TLS scheme for a BDS upstream, so the registry
// must build the BDS client for it rather than refuse the scheme.
func TestClientRegistry_GrpcsSchemeBuildsBdsClient(t *testing.T) {
	logger := zerolog.New(io.Discard)
	ups := common.NewFakeUpstream("bds-tls")
	ups.Config().Type = common.UpstreamTypeEvm
	ups.Config().Endpoint = "grpcs://127.0.0.1:1/boost"
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	client, err := NewClientRegistry(&logger, "test-project", nil, nil).CreateClient(ctx, ups)
	require.NoError(t, err)
	require.Equal(t, ClientTypeGrpcBds, client.GetType())
}
