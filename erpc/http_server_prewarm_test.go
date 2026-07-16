package erpc

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPrewarmUpstreamSubscriptions_Guards pins the reconciler's lifecycle
// contract: it must no-op safely when its dependencies are absent, and it must
// return promptly once the context is cancelled rather than ticking forever.
// The core per-network bootstrap is exercised through the client-subscribe path
// (SubscriptionManager.Subscribe -> bootstrapNetwork).
func TestPrewarmUpstreamSubscriptions_Guards(t *testing.T) {
	logger := zerolog.New(zerolog.NewTestWriter(t))

	t.Run("returns immediately when dependencies are nil", func(t *testing.T) {
		s := &HttpServer{logger: &logger} // no erpc, no subscriptionManager
		done := make(chan struct{})
		go func() {
			s.PrewarmUpstreamSubscriptions(context.Background())
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("PrewarmUpstreamSubscriptions did not return despite nil dependencies")
		}
	})

	t.Run("returns when context is cancelled", func(t *testing.T) {
		// A server with an empty ERPC: the first reconcile pass iterates zero
		// projects, then the loop blocks on the ticker until ctx is cancelled.
		s := &HttpServer{
			logger:              &logger,
			serverCfg:           &common.ServerConfig{WebSocket: &common.WebSocketServerConfig{Enabled: util.BoolPtr(true)}},
			erpc:                &ERPC{projectsRegistry: &ProjectsRegistry{}},
			subscriptionManager: NewSubscriptionManager(&logger, nil),
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			s.PrewarmUpstreamSubscriptions(ctx)
			close(done)
		}()
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("PrewarmUpstreamSubscriptions did not return after context cancellation")
		}
	})
}

// Two projects serving the same chain each own an indexer network, so each
// must be prewarmed on its own upstream.
func TestPrewarmUpstreamSubscriptions_ProjectsSharingANetwork(t *testing.T) {
	setupGock()
	defer util.ResetGock()

	prev := prewarmInterval
	prewarmInterval = 50 * time.Millisecond
	defer func() { prewarmInterval = prev }()

	var subscribesA, subscribesB atomic.Int32
	mockWith := func(count *atomic.Int32, subID string) *httptest.Server {
		return mockWsUpstream(t, func(conn *websocket.Conn) {
			standardMockWsHandler(conn, func(method string, id interface{}, _ map[string]interface{}) {
				result := interface{}("0x1")
				if method == "eth_subscribe" {
					count.Add(1)
					result = subID
				}
				mockWriteJSON(conn, map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": result})
			})
		})
	}
	upA, upB := mockWith(&subscribesA, "0xa"), mockWith(&subscribesB, "0xb")
	defer upA.Close()
	defer upB.Close()

	cfg := standardWsConfig("ws" + strings.TrimPrefix(upA.URL, "http"))
	second := *cfg.Projects[0]
	second.Id = "test_ws2"
	second.Networks = []*common.NetworkConfig{{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}}}
	second.Upstreams = []*common.UpstreamConfig{
		{Id: "http-upstream", Type: common.UpstreamTypeEvm, Endpoint: "http://rpc1.localhost", Evm: &common.EvmUpstreamConfig{ChainId: 123}},
		{Id: "ws-upstream", Type: common.UpstreamTypeEvm, Endpoint: "ws" + strings.TrimPrefix(upB.URL, "http"), Evm: &common.EvmUpstreamConfig{ChainId: 123}},
	}
	cfg.Projects = append(cfg.Projects, &second)
	srv, _, cancel := startTestERPCServer(t, cfg)
	defer func() {
		_ = srv.Shutdown(&log.Logger)
		cancel()
	}()

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go srv.PrewarmUpstreamSubscriptions(ctx)

	require.Eventually(t, func() bool { return subscribesA.Load() > 0 }, 5*time.Second, 20*time.Millisecond,
		"first project must be prewarmed")
	assert.Eventually(t, func() bool { return subscribesB.Load() > 0 }, 5*time.Second, 20*time.Millisecond,
		"second project on the same chain must be prewarmed on its own upstream")
}

// With the WebSocket server disabled nothing consumes upstream subscriptions,
// so prewarm must not open them.
func TestPrewarmUpstreamSubscriptions_SkippedWhenWebSocketDisabled(t *testing.T) {
	setupGock()
	defer util.ResetGock()

	prev := prewarmInterval
	prewarmInterval = 50 * time.Millisecond
	defer func() { prewarmInterval = prev }()

	var subscribes atomic.Int32
	up := mockWsUpstream(t, func(conn *websocket.Conn) {
		standardMockWsHandler(conn, func(method string, id interface{}, _ map[string]interface{}) {
			if method == "eth_subscribe" {
				subscribes.Add(1)
			}
			mockWriteJSON(conn, map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": "0x1"})
		})
	})
	defer up.Close()

	cfg := standardWsConfig("ws" + strings.TrimPrefix(up.URL, "http"))
	cfg.Server.WebSocket = &common.WebSocketServerConfig{Enabled: util.BoolPtr(false)}
	srv, _, cancel := startTestERPCServer(t, cfg)
	defer func() {
		_ = srv.Shutdown(&log.Logger)
		cancel()
	}()

	ctx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	srv.PrewarmUpstreamSubscriptions(ctx)

	assert.Zero(t, subscribes.Load(), "prewarm must not subscribe upstream while the WebSocket server is disabled")
}
