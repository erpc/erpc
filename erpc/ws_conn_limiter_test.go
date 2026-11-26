package erpc

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWsConnLimiter(t *testing.T) {
	t.Run("NilAdmitsEverything", func(t *testing.T) {
		l := newWsConnLimiter(0, 0)
		require.Nil(t, l)
		for i := 0; i < 3; i++ {
			_, ok := l.acquire("1.2.3.4")
			assert.True(t, ok)
		}
		l.release("1.2.3.4")
	})

	t.Run("GlobalCap", func(t *testing.T) {
		l := newWsConnLimiter(2, 0)
		_, ok := l.acquire("1.1.1.1")
		require.True(t, ok)
		_, ok = l.acquire("2.2.2.2")
		require.True(t, ok)
		reason, ok := l.acquire("3.3.3.3")
		assert.False(t, ok)
		assert.Equal(t, wsRejectConnGlobal, reason)
		l.release("1.1.1.1")
		_, ok = l.acquire("3.3.3.3")
		assert.True(t, ok)
	})

	t.Run("PerIPCap", func(t *testing.T) {
		l := newWsConnLimiter(0, 1)
		_, ok := l.acquire("1.1.1.1")
		require.True(t, ok)
		reason, ok := l.acquire("1.1.1.1")
		assert.False(t, ok)
		assert.Equal(t, wsRejectConnPerIP, reason)
		_, ok = l.acquire("2.2.2.2")
		assert.True(t, ok)
	})

	t.Run("UnresolvedIPOnlyCountsTowardsTotal", func(t *testing.T) {
		l := newWsConnLimiter(3, 1)
		for i := 0; i < 3; i++ {
			_, ok := l.acquire("n/a")
			require.True(t, ok)
		}
		reason, ok := l.acquire("n/a")
		assert.False(t, ok)
		assert.Equal(t, wsRejectConnGlobal, reason)
		assert.Empty(t, l.perIP)
	})

	t.Run("ReleaseReclaimsPerIPEntries", func(t *testing.T) {
		l := newWsConnLimiter(0, 2)
		for i := 0; i < 100; i++ {
			ip := fmt.Sprintf("10.0.0.%d", i)
			_, ok := l.acquire(ip)
			require.True(t, ok)
			l.release(ip)
		}
		assert.Empty(t, l.perIP)
		assert.Zero(t, l.total)
	})
}

func dialWsExpectStatus(t *testing.T, addr string) (*websocket.Conn, int) {
	t.Helper()
	conn, resp, err := websocket.DefaultDialer.Dial(fmt.Sprintf("ws://%s/test_ws/evm/123", addr), nil)
	if err != nil {
		require.NotNil(t, resp, "dial failed without an HTTP response: %v", err)
		return nil, resp.StatusCode
	}
	return conn, resp.StatusCode
}

func TestWebSocket_ConnectionCaps(t *testing.T) {
	setupGock()
	t.Cleanup(util.ResetGock)

	cases := []struct {
		name   string
		ws     *common.WebSocketServerConfig
		reason string
	}{
		{"Global", &common.WebSocketServerConfig{MaxConnections: 1}, wsRejectConnGlobal},
		{"PerIP", &common.WebSocketServerConfig{MaxConnectionsPerIP: 1}, wsRejectConnPerIP},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := httpOnlyConfig()
			cfg.Server.WebSocket = tc.ws
			addr, cleanup := setupTestERPCServer(t, cfg)
			defer cleanup()

			rejected := telemetry.MetricWebsocketRejectedTotal.WithLabelValues("test_ws", tc.reason)
			base := testutil.ToFloat64(rejected)

			first, status := dialWsExpectStatus(t, addr)
			require.NotNil(t, first)
			assert.Equal(t, http.StatusSwitchingProtocols, status)

			second, status := dialWsExpectStatus(t, addr)
			assert.Nil(t, second)
			assert.Equal(t, http.StatusTooManyRequests, status)
			assert.Equal(t, base+1, testutil.ToFloat64(rejected))

			// Closing frees the slot.
			first.Close()
			require.Eventually(t, func() bool {
				c, _ := dialWsExpectStatus(t, addr)
				if c == nil {
					return false
				}
				c.Close()
				return true
			}, 5*time.Second, 50*time.Millisecond)
		})
	}
}

func TestWebSocket_InstanceSubscriptionCap(t *testing.T) {
	mockUpstream := mockWsUpstream(t, func(conn *websocket.Conn) {
		n := 0
		standardMockWsHandler(conn, func(method string, id interface{}, req map[string]interface{}) {
			switch method {
			case "eth_subscribe":
				n++
				mockWriteJSON(conn, map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": fmt.Sprintf("0xsub%d", n)})
			default:
				mockWriteJSON(conn, map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": true})
			}
		})
	})
	defer mockUpstream.Close()

	setupGock()
	t.Cleanup(util.ResetGock)

	cfg := standardWsConfig("ws" + strings.TrimPrefix(mockUpstream.URL, "http"))
	cfg.Server.WebSocket = &common.WebSocketServerConfig{MaxSubscriptions: 1}
	addr, cleanup := setupTestERPCServer(t, cfg)
	defer cleanup()
	time.Sleep(2 * time.Second)

	rejected := telemetry.MetricWebsocketRejectedTotal.WithLabelValues("test_ws", wsRejectSubGlobal)
	base := testutil.ToFloat64(rejected)

	conn1 := dialWs(t, addr)
	defer conn1.Close()
	conn2 := dialWs(t, addr)
	defer conn2.Close()

	resp := sendAndReceive(t, conn1, `{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["newHeads"]}`)
	require.Nil(t, resp["error"])
	subID := resp["result"].(string)

	resp = sendAndReceive(t, conn2, `{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["newHeads"]}`)
	require.NotNil(t, resp["error"], "second subscription on the instance must be refused")
	assert.Contains(t, fmt.Sprint(resp["error"]), "per instance")
	assert.Equal(t, base+1, testutil.ToFloat64(rejected))

	resp = sendAndReceive(t, conn1, fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"eth_unsubscribe","params":["%s"]}`, subID))
	require.Equal(t, true, resp["result"])

	resp = sendAndReceive(t, conn2, `{"jsonrpc":"2.0","id":2,"method":"eth_subscribe","params":["newHeads"]}`)
	assert.Nil(t, resp["error"], "unsubscribe must free the slot")

	// A closed connection gives its subscriptions back.
	conn2.Close()
	require.Eventually(t, func() bool {
		resp := sendAndReceive(t, conn1, `{"jsonrpc":"2.0","id":3,"method":"eth_subscribe","params":["newHeads"]}`)
		return resp["error"] == nil
	}, 5*time.Second, 100*time.Millisecond)
}
