package erpc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

func init() { util.ConfigureTestLogger() }

// wsRaw sends a raw frame and returns the next raw frame.
func wsRaw(t *testing.T, c *websocket.Conn, msg string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, c.Write(ctx, websocket.MessageText, []byte(msg)))
	_, b, err := c.Read(ctx)
	require.NoError(t, err)
	return b
}

func dialRaw(t *testing.T, base string) *websocket.Conn {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL(base, ""), nil)
	require.NoError(t, err)
	c.SetReadLimit(8 << 20)
	t.Cleanup(func() { _ = c.CloseNow() })
	return c
}

// The WS endpoint is subscription-only: batches get one -32600 reply, any
// method other than eth_subscribe/eth_unsubscribe gets -32601, and neither
// reaches an upstream or creates a subscription.
func TestWs_UnsupportedContract(t *testing.T) {
	up := newScriptedEvmUpstream(123, 20)
	defer up.Close()
	cfg := wsHeadCacheCfg(up, &common.WebSocketServerConfig{Enabled: true})
	cfg.Projects[0].IgnoreMethods = []string{"eth_unsubscribe"}
	_, _, base, shutdown, e := createServerTestFixtures(cfg, t)
	defer shutdown()
	waitHead(t, e, 20)
	c := dialRaw(t, base)
	callsBefore := up.Calls("eth_call")

	var r wsMsg
	for _, batch := range []string{
		`[{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}]`,
		`[{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["newHeads"]}]`,
		`[]`,
		`  [1,2]`,
	} {
		require.NoError(t, json.Unmarshal(wsRaw(t, c, batch), &r), batch)
		require.Equal(t, -32600, r.Error.Code, batch)
		require.Contains(t, r.Error.Message, "batch")
	}
	for _, m := range []string{"eth_chainId", "eth_getBlockByNumber", "eth_call"} {
		require.NoError(t, json.Unmarshal(wsRaw(t, c, fmt.Sprintf(`{"jsonrpc":"2.0","id":"x","method":%q,"params":[]}`, m)), &r))
		require.Equal(t, `"x"`, string(r.ID))
		require.Equal(t, -32601, r.Error.Code, m)
	}
	// Project method lists still apply to the supported methods.
	require.NoError(t, json.Unmarshal(wsRaw(t, c, `{"jsonrpc":"2.0","id":5,"method":"eth_unsubscribe","params":["0x1"]}`), &r))
	require.Equal(t, -32601, r.Error.Code)

	require.Equal(t, 0, subCount(t, e))
	require.Equal(t, callsBefore, up.Calls("eth_call"), "unsupported calls must not reach upstreams")
	// The connection stays usable after rejections.
	r = wsMsg{}
	require.NoError(t, json.Unmarshal(wsRaw(t, c, `{"jsonrpc":"2.0","id":9,"method":"eth_subscribe","params":["newHeads"]}`), &r))
	require.Nil(t, r.Error)
}

func TestWs_SingleInvalidRequestCodes(t *testing.T) {
	c, _ := testWsConn(t, 4)
	for msg, code := range map[string]int{`17`: -32600, `{"foo":1}`: -32600, `"x"`: -32600, `{"jsonrpc"`: -32700, `[{"jsonrpc":"2.0","id":1,"method":"eth_subscribe"}]`: -32600} {
		reply, _ := c.handleOne([]byte(msg))
		var r wsMsg
		require.NoError(t, json.Unmarshal(reply, &r))
		require.Equal(t, code, r.Error.Code, msg)
	}
}

func TestWs_IdlessNotificationsHaveNoSubscriptionEffects(t *testing.T) {
	up := newScriptedEvmUpstream(123, 20)
	defer up.Close()
	_, _, base, shutdown, e := createServerTestFixtures(wsHeadCacheCfg(up, &common.WebSocketServerConfig{Enabled: true}), t)
	defer shutdown()
	waitHead(t, e, 20)
	w, _, err := dialWs(t, wsURL(base, ""), nil)
	require.NoError(t, err)

	write := func(msg string) {
		t.Helper()
		require.NoError(t, w.c.Write(context.Background(), websocket.MessageText, []byte(msg)))
	}
	assertSilent := func() {
		t.Helper()
		select {
		case m, ok := <-w.msgs:
			if !ok {
				t.Fatal("connection closed while waiting for notification silence")
			}
			t.Fatalf("unexpected websocket response: %+v", m)
		case err := <-w.done:
			t.Fatalf("connection closed while waiting for notification silence: %v", err)
		case <-time.After(150 * time.Millisecond):
		}
	}

	write(`{"jsonrpc":"2.0","method":"eth_subscribe","params":["newHeads"]}`)
	assertSilent()
	// This reply is a barrier after the id-less frame has been handled.
	require.Equal(t, -32601, w.call("eth_chainId", `[]`).Error.Code)
	require.Equal(t, 0, subCount(t, e))

	subReply := w.call("eth_subscribe", `["newHeads"]`)
	require.Nil(t, subReply.Error)
	var subID string
	require.NoError(t, json.Unmarshal(subReply.Result, &subID))
	require.Equal(t, 1, subCount(t, e))
	write(fmt.Sprintf(`{"jsonrpc":"2.0","method":"eth_unsubscribe","params":[%q]}`, subID))
	assertSilent()
	require.Equal(t, -32601, w.call("eth_chainId", `[]`).Error.Code)
	require.Equal(t, 1, subCount(t, e), "id-less unsubscribe must not remove the subscription")
	require.Equal(t, "true", string(w.call("eth_unsubscribe", fmt.Sprintf(`[%q]`, subID)).Result))
	require.Eventually(t, func() bool { return subCount(t, e) == 0 }, 5*time.Second, 20*time.Millisecond)
}

func TestWs_ConfigFallbacks(t *testing.T) {
	ws := &wsServer{cfg: &common.WebSocketServerConfig{}}
	require.Equal(t, 30*time.Second, ws.pingInterval(), "ping (and re-auth) must never be disabled")
	ws.cfg.PingInterval = common.Duration(time.Second)
	require.Equal(t, time.Second, ws.pingInterval())
}

// Regression: WS survives past the HTTP server WriteTimeout (1s).
func TestWs_OutlivesHTTPWriteTimeout(t *testing.T) {
	up := newScriptedEvmUpstream(123, 20)
	defer up.Close()
	cfg := wsHeadCacheCfg(up, &common.WebSocketServerConfig{Enabled: true})
	cfg.Server.WriteTimeout = common.Duration(time.Second).Ptr()
	_, _, base, shutdown, e := createServerTestFixtures(cfg, t)
	defer shutdown()
	waitHead(t, e, 20)
	w, _, err := dialWs(t, wsURL(base, ""), nil)
	require.NoError(t, err)
	r := w.call("eth_subscribe", `["newHeads"]`)
	require.Nil(t, r.Error)
	var sub string
	require.NoError(t, json.Unmarshal(r.Result, &sub))
	time.Sleep(1500 * time.Millisecond)
	up.Mine(1)
	require.Contains(t, string(w.next(sub)), `"number":"0x15"`)
	require.Nil(t, w.call("eth_subscribe", `["newHeads"]`).Error)
}

// Each eth_subscribe consumes project rate-limit budget.
func TestWs_SubscribeRateLimit(t *testing.T) {
	up := newScriptedEvmUpstream(123, 20)
	defer up.Close()
	cfg := wsHeadCacheCfg(up, &common.WebSocketServerConfig{Enabled: true, MaxSubscriptionsPerConnection: 10})
	cfg.RateLimiters = &common.RateLimiterConfig{Budgets: []*common.RateLimitBudgetConfig{{
		Id: "ws-subs", Rules: []*common.RateLimitRuleConfig{{Method: "eth_subscribe", MaxCount: 2, Period: common.RateLimitPeriodMinute}},
	}}}
	cfg.Projects[0].RateLimitBudget = "ws-subs"
	_, _, base, shutdown, e := createServerTestFixtures(cfg, t)
	defer shutdown()
	waitHead(t, e, 20)
	w, _, err := dialWs(t, wsURL(base, ""), nil)
	require.NoError(t, err)
	ok, limited := 0, 0
	for i := 0; i < 5; i++ {
		if w.call("eth_subscribe", `["newHeads"]`).Error == nil {
			ok++
		} else {
			limited++
		}
	}
	require.Equal(t, 2, ok)
	require.Equal(t, 3, limited)
	require.Equal(t, 2, subCount(t, e))
}

// Unauthorized or disallowed-origin upgrades to a not-yet-created network must
// not create it: GetNetwork lazily bootstraps networks (upstream preparation,
// pollers, head cache), which only authorized clients may trigger.
func TestWs_UnauthorizedUpgradeDoesNotCreateNetwork(t *testing.T) {
	up := newScriptedEvmUpstream(123, 20)
	defer up.Close()
	cfg := wsHeadCacheCfg(up, &common.WebSocketServerConfig{Enabled: true})
	cfg.Projects[0].Auth = &common.AuthConfig{Strategies: []*common.AuthStrategyConfig{
		{Type: common.AuthTypeSecret, Secret: &common.SecretStrategyConfig{Id: "s1", Value: "s3cret"}},
	}}
	cfg.Projects[0].CORS = &common.CORSConfig{AllowedOrigins: []string{"https://good.example"}}
	_, _, base, shutdown, e := createServerTestFixtures(cfg, t)
	defer shutdown()
	waitHead(t, e, 20)
	unseenURL := func(q string) string {
		return strings.Replace(wsURL(base, q), "/evm/123", "/evm/999", 1)
	}
	prj, err := e.GetProject("test_project")
	require.NoError(t, err)
	created := func() bool {
		for _, n := range prj.GetNetworks() {
			if n.networkId == "evm:999" {
				return true
			}
		}
		return prj.FindNetworkConfig("evm:999") != nil
	}
	require.False(t, created())

	_, resp, err := dialWs(t, unseenURL(""), nil)
	require.Error(t, err)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	_, resp, err = dialWs(t, unseenURL("secret=wrong"), nil)
	require.Error(t, err)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	_, resp, err = dialWs(t, unseenURL("secret=s3cret"), http.Header{"Origin": {"https://evil.example"}})
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	require.False(t, created(), "rejected upgrade must not create or expose the network")

	// An authorized upgrade does reach network resolution (proving the gate,
	// not an unrelated failure, kept it from being created above). The
	// network has no head cache, so the upgrade itself is still refused.
	_, resp, err = dialWs(t, unseenURL("secret=s3cret"), http.Header{"Origin": {"https://good.example"}})
	require.Error(t, err)
	require.NotEqual(t, http.StatusSwitchingProtocols, resp.StatusCode)
	require.Eventually(t, created, 5*time.Second, 20*time.Millisecond)

	// Authorized success on a configured network is unchanged.
	w, _, err := dialWs(t, wsURL(base, "secret=s3cret"), http.Header{"Origin": {"https://good.example"}})
	require.NoError(t, err)
	require.Nil(t, w.call("eth_subscribe", `["newHeads"]`).Error)
}

// Regression: a subscribe whose id reply is never delivered must release the
// reserved subscription without starting a pump.
func TestWs_UndeliveredSubscribeReplyReleases(t *testing.T) {
	up := newScriptedEvmUpstream(123, 20)
	defer up.Close()
	cfg := wsHeadCacheCfg(up, &common.WebSocketServerConfig{Enabled: true})
	_, _, _, shutdown, e := createServerTestFixtures(cfg, t)
	defer shutdown()
	waitHead(t, e, 20)
	prj, err := e.GetProject("test_project")
	require.NoError(t, err)
	nw, err := prj.GetNetwork(t.Context(), "evm:123")
	require.NoError(t, err)

	// Queue of 1, pre-filled: the id reply cannot be enqueued.
	c, _ := testWsConn(t, 1)
	lg := zerolog.Nop()
	c.lg, c.project, c.network, c.networkId = &lg, prj, nw, "evm:123"
	c.req = httptest.NewRequest(http.MethodGet, "/test_project/evm/123", nil)
	c.subs = map[string]*wsSub{}
	c.ws.s = &HttpServer{serverCfg: &common.ServerConfig{}}
	c.ws.cfg.MaxSubscriptionsPerConnection = 10
	c.ws.cfg.SendQueueSize = 16
	c.out <- []byte("filler")

	c.handleMessage([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["newHeads"]}`))
	require.Error(t, c.ctx.Err(), "full queue closes the connection")
	require.Empty(t, c.subs, "undelivered id must not retain a subscription")
	done := make(chan struct{})
	go func() { c.cleanup(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup hung on a reserved pump")
	}
	require.Eventually(t, func() bool { return subCount(t, e) == 0 }, 5*time.Second, 20*time.Millisecond)
}

// Public path: a client pipelines many subscribes and disconnects abruptly.
// Connection slots and head-cache subscriptions must be released.
func TestWs_DisconnectReleasesResources(t *testing.T) {
	up := newScriptedEvmUpstream(123, 20)
	defer up.Close()
	cfg := wsHeadCacheCfg(up, &common.WebSocketServerConfig{
		Enabled: true, MaxConnections: 1, MaxSubscriptionsPerConnection: 50,
	})
	_, _, base, shutdown, e := createServerTestFixtures(cfg, t)
	defer shutdown()
	waitHead(t, e, 20)
	for round := 0; round < 3; round++ {
		var c *websocket.Conn
		require.Eventually(t, func() bool {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			var err error
			c, _, err = websocket.Dial(ctx, wsURL(base, ""), nil)
			return err == nil
		}, 10*time.Second, 50*time.Millisecond, "connection slot must be released (round %d)", round)
		for i := 0; i < 20; i++ {
			require.NoError(t, c.Write(context.Background(), websocket.MessageText,
				[]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_subscribe","params":["newHeads"]}`, i))))
		}
		time.Sleep(time.Duration(round*5) * time.Millisecond)
		_ = c.CloseNow()
	}
	require.Eventually(t, func() bool {
		return subCount(t, e) == 0
	}, 10*time.Second, 20*time.Millisecond, "head-cache subscriptions must be released")
}
