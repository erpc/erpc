package erpc

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"
)

// TestHttpServer_WebSocketUpgradeReachesDispatch pins that a WebSocket upgrade
// on the bare aliased root "/" reaches the WS dispatch. The empty-path
// classifier used to mark any non-POST root request as a healthcheck, so
// wss://host/ (common with domain aliasing) was served as health and never
// upgraded.
func TestHttpServer_WebSocketUpgradeReachesDispatch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logger := log.Logger
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		cfg := &common.Config{
			Server: &common.ServerConfig{
				ListenV4:           util.BoolPtr(true),
				MaxTimeout:         common.Duration(5 * time.Second).Ptr(),
				WaitBeforeShutdown: common.Duration(0).Ptr(),
				WebSocket:          &common.WebSocketServerConfig{Enabled: util.BoolPtr(true)},
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

		ts := serveInMemory(t, httpServer)
		t.Cleanup(func() {
			cancel()
			<-httpServer.drained
		})
		base := "ws" + strings.TrimPrefix(ts.URL, "http")

		assertReachedWS := func(path string, hdr http.Header) {
			dctx, dcancel := context.WithTimeout(ctx, 5*time.Second)
			defer dcancel()
			c, resp, derr := testWsDialer.DialContext(dctx, base+path, hdr)
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
	})
}
