package erpc

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/util"
	"github.com/gorilla/websocket"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"
)

// TestHttpServer_WebSocketUpgradeReachesDispatch pins that a WebSocket upgrade
// on the bare aliased root "/" reaches the WS dispatch. The empty-path
// classifier used to mark any non-POST root request as a healthcheck, so
// wss://host/ (common with domain aliasing) was served as health and never
// upgraded.
func TestHttpServer_WebSocketUpgradeReachesDispatch(t *testing.T) {
	logger := log.Logger
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := &common.Config{
		Server: &common.ServerConfig{
			ListenV4:   util.BoolPtr(true),
			MaxTimeout: common.Duration(5 * time.Second).Ptr(),
			WebSocket:  &common.WebSocketServerConfig{Enabled: util.BoolPtr(true)},
			Aliasing: &common.AliasingConfig{
				Rules: []*common.AliasingRuleConfig{
					{MatchDomain: "*", ServeProject: "test_project", ServeArchitecture: "evm", ServeChain: "123"},
				},
			},
		},
		Projects: []*common.ProjectConfig{
			{
				Id: "test_project",
				Networks: []*common.NetworkConfig{
					{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}},
				},
				Upstreams: []*common.UpstreamConfig{
					{Type: common.UpstreamTypeEvm, Endpoint: "http://rpc1.localhost", Evm: &common.EvmUpstreamConfig{ChainId: 123}},
				},
			},
		},
		RateLimiters: &common.RateLimiterConfig{},
	}
	require.NoError(t, cfg.SetDefaults(nil))

	ssr, err := data.NewSharedStateRegistry(ctx, &logger, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: "memory",
			Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"},
		},
	})
	require.NoError(t, err)

	erpcInstance, err := NewERPC(ctx, &logger, ssr, nil, nil, cfg)
	require.NoError(t, err)
	erpcInstance.Bootstrap(ctx)

	httpServer, err := NewHttpServer(ctx, &logger, cfg.Server, cfg.HealthCheck, cfg.Admin, cfg.Indexer, erpcInstance)
	require.NoError(t, err)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = httpServer.serverV4.Serve(listener) }()
	defer httpServer.serverV4.Shutdown(context.Background())
	base := "ws://" + listener.Addr().String()

	assertReachedWS := func(path string, hdr http.Header) {
		dctx, dcancel := context.WithTimeout(ctx, 5*time.Second)
		defer dcancel()
		c, resp, derr := websocket.DefaultDialer.DialContext(dctx, base+path, hdr)
		if resp != nil {
			_ = resp.Body.Close()
		}
		require.NoErrorf(t, derr, "%s must upgrade to WebSocket", path)
		_ = c.Close()
	}

	assertReachedWS("/", nil)

	// A proxy (e.g. Traefik) that adds Accept-Encoding: gzip to the upgrade
	// request must not break the handshake: the gzip writer is not an
	// http.Hijacker, which used to 500 the upgrade.
	assertReachedWS("/", http.Header{"Accept-Encoding": []string{"gzip"}})
}
