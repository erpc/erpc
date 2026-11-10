package clients

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blockchain-data-standards/manifesto/evm"
	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/resolver"
)

const bdsHealthService = "bds.evm.RPCQueryService"

// startHealthReportingServer serves the BDS happy server for chainID plus
// grpc.health, with bdsHealthService registered at `initial`.
func startHealthReportingServer(t *testing.T, chainID uint64, initial healthpb.HealthCheckResponse_ServingStatus) (string, *health.Server) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := grpc.NewServer()
	evm.RegisterRPCQueryServiceServer(srv, &happyRPCServer{chainID: chainID})
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
	addr, hs := startHealthReportingServer(t, 1, healthpb.HealthCheckResponse_NOT_SERVING)
	client := newHealthCheckedClient(t, addr, bdsHealthService)

	require.Error(t, chainIdWithin(client, 500*time.Millisecond), "a NOT_SERVING reader received the request")

	hs.SetServingStatus(bdsHealthService, healthpb.HealthCheckResponse_SERVING)
	require.Eventually(t, func() bool { return chainIdWithin(client, 500*time.Millisecond) == nil },
		5*time.Second, 50*time.Millisecond, "the pool never used the reader after SERVING")
}

// Unset keeps today's behaviour: health status is not consulted.
func TestGrpcBdsClient_HealthCheckService_UnsetIgnoresHealth(t *testing.T) {
	addr, _ := startHealthReportingServer(t, 1, healthpb.HealthCheckResponse_NOT_SERVING)
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

// A deploy replaces a reader: the old one reports NOT_SERVING while it drains
// and keeps its listener open, and the endpoint now leads to the new one. Calls
// must move to the new reader before the old one closes, both where the
// endpoint is a DNS name repointed at the new reader and where it is a load
// balancer that routes new connections to it.
func TestGrpcBdsClient_HealthCheckService_FollowsReplacementReader(t *testing.T) {
	window := bdsReplacementDedupWindow
	bdsReplacementDedupWindow = 100 * time.Millisecond
	t.Cleanup(func() { bdsReplacementDedupWindow = window })

	routes := map[string]func(t *testing.T) (endpoint string, routeTo func(addr string)){
		"dns name": func(t *testing.T) (string, func(string)) {
			name := repointableDNSName(t)
			return name.host + ":50051", name.pointAt
		},
		"load balancer": func(t *testing.T) (string, func(string)) {
			lb := startSwitchingProxy(t)
			return lb.addr, lb.routeTo
		},
	}
	for kind, route := range routes {
		t.Run(kind, func(t *testing.T) {
			endpoint, routeTo := route(t)
			oldAddr, oldHealth := startHealthReportingServer(t, 1, healthpb.HealthCheckResponse_SERVING)
			routeTo(oldAddr)
			client := newHealthCheckedClient(t, endpoint, bdsHealthService)
			// Round-robin over the pool: every connection has dialed the old reader.
			calls := 2 * bdsPoolSize
			for range calls {
				require.Equal(t, `"0x1"`, answeredChainId(client, 2*time.Second))
			}

			newAddr, _ := startHealthReportingServer(t, 2, healthpb.HealthCheckResponse_SERVING)
			routeTo(newAddr)
			oldHealth.SetServingStatus(bdsHealthService, healthpb.HealthCheckResponse_NOT_SERVING)

			require.Eventually(t, func() bool {
				for range calls {
					if answeredChainId(client, 500*time.Millisecond) != `"0x2"` {
						return false
					}
				}
				return true
			}, 5*time.Second, 50*time.Millisecond, "calls stayed on the draining reader")
		})
	}
}

// answeredChainId is the eth_chainId result the client returns within d, or
// "" when the call fails.
func answeredChainId(client GrpcBdsClient, d time.Duration) string {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`))
	resp, err := client.SendRequest(ctx, req)
	if err != nil {
		return ""
	}
	jrr, err := resp.JsonRpcResponse()
	if err != nil || jrr == nil {
		return ""
	}
	return jrr.GetResultString()
}

// repointableName is a host the "dns" resolver answers with one address the
// test sets, as a DNS record a deploy repoints. A lookup returns the current
// address; like grpc-go's DNS resolver, nothing looks it up again until a
// channel asks (ResolveNow).
type repointableName struct {
	host string
	addr atomic.Pointer[string]
}

func (n *repointableName) pointAt(addr string) { n.addr.Store(&addr) }

// repointableDNSName registers, for this test only, a "dns" resolver that
// answers the returned name itself and passes every other name to grpc-go's.
func repointableDNSName(t *testing.T) *repointableName {
	t.Helper()
	dnsBuilder := resolver.Get("dns")
	require.NotNil(t, dnsBuilder)
	name := &repointableName{host: "reader.repointable.test"}
	resolver.Register(&repointableBuilder{dns: dnsBuilder, name: name})
	t.Cleanup(func() { resolver.Register(dnsBuilder) })
	return name
}

type repointableBuilder struct {
	dns  resolver.Builder
	name *repointableName
}

func (b *repointableBuilder) Scheme() string { return "dns" }

func (b *repointableBuilder) Build(target resolver.Target, cc resolver.ClientConn, opts resolver.BuildOptions) (resolver.Resolver, error) {
	if host, _, err := net.SplitHostPort(target.Endpoint()); err != nil || host != b.name.host {
		return b.dns.Build(target, cc, opts)
	}
	r := &repointableResolver{name: b.name, cc: cc}
	r.ResolveNow(resolver.ResolveNowOptions{})
	return r, nil
}

type repointableResolver struct {
	name *repointableName
	cc   resolver.ClientConn
}

func (r *repointableResolver) ResolveNow(resolver.ResolveNowOptions) {
	_ = r.cc.UpdateState(resolver.State{Addresses: []resolver.Address{{Addr: *r.name.addr.Load()}}})
}

func (r *repointableResolver) Close() {}

// switchingProxy is a TCP load balancer: each new connection goes to the
// backend set last; open connections keep theirs and close with it.
type switchingProxy struct {
	addr    string
	backend atomic.Pointer[string]
}

func (p *switchingProxy) routeTo(addr string) { p.backend.Store(&addr) }

func startSwitchingProxy(t *testing.T) *switchingProxy {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = lis.Close() })
	p := &switchingProxy{addr: lis.Addr().String()}
	go func() {
		for {
			front, err := lis.Accept()
			if err != nil {
				return
			}
			back, err := net.Dial("tcp", *p.backend.Load())
			if err != nil {
				_ = front.Close()
				continue
			}
			go func() { _, _ = io.Copy(back, front); _ = back.Close() }()
			go func() { _, _ = io.Copy(front, back); _ = front.Close() }()
		}
	}()
	return p
}
