package erpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	promUtil "github.com/prometheus/client_golang/prometheus/testutil"
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

func byId(t *testing.T, b []byte) map[string]wsMsg {
	var replies []wsMsg
	require.NoError(t, json.Unmarshal(b, &replies), string(b))
	m := map[string]wsMsg{}
	for _, r := range replies {
		m[string(r.ID)] = r
	}
	require.Len(t, m, len(replies), "duplicate ids")
	return m
}

func TestWs_Batch(t *testing.T) {
	up := newScriptedEvmUpstream(123, 20)
	defer up.Close()
	cfg := wsHeadCacheCfg(up, &common.WebSocketServerConfig{Enabled: true, MaxBatchSize: 4, MaxSubscriptionsPerConnection: 1})
	cfg.Projects[0].IgnoreMethods = []string{"eth_blocked"}
	_, _, base, shutdown, e := createServerTestFixtures(cfg, t)
	defer shutdown()
	waitHead(t, e, 20)
	c := dialRaw(t, base)

	t.Run("valid", func(t *testing.T) {
		m := byId(t, wsRaw(t, c, `[{"jsonrpc":"2.0","id":101,"method":"eth_chainId","params":[]},{"jsonrpc":"2.0","id":"x","method":"eth_chainId","params":[]}]`))
		require.Len(t, m, 2)
		for _, id := range []string{"101", `"x"`} {
			require.Nil(t, m[id].Error)
			require.JSONEq(t, `"0x7b"`, string(m[id].Result))
		}
	})
	t.Run("mixed invalid and method-denied items", func(t *testing.T) {
		b := wsRaw(t, c, `[1,{"jsonrpc":"2.0","id":2,"method":"eth_blocked","params":[]},{"jsonrpc":"2.0","id":3,"method":"eth_chainId","params":[]},{"foo":"bar"}]`)
		var arr []wsMsg
		require.NoError(t, json.Unmarshal(b, &arr))
		require.Len(t, arr, 4)
		nulls := 0
		for _, r := range arr {
			switch string(r.ID) {
			case "null", "":
				nulls++
				require.Equal(t, -32600, r.Error.Code, "valid JSON non-request items are Invalid Request")
			case "2":
				require.Equal(t, -32601, r.Error.Code)
			case "3":
				require.Nil(t, r.Error)
			}
		}
		require.Equal(t, 2, nulls)
	})
	t.Run("empty, oversize, nested and invalid json", func(t *testing.T) {
		var r wsMsg
		require.NoError(t, json.Unmarshal(wsRaw(t, c, `[]`), &r))
		require.Equal(t, -32600, r.Error.Code)
		big := "[" + strings.TrimSuffix(strings.Repeat(`{"jsonrpc":"2.0","id":1,"method":"eth_chainId"},`, 5), ",") + "]"
		require.NoError(t, json.Unmarshal(wsRaw(t, c, big), &r))
		require.Equal(t, -32600, r.Error.Code)
		require.Contains(t, r.Error.Message, "batch too large")
		m := byId(t, wsRaw(t, c, `[[{"jsonrpc":"2.0","id":1,"method":"eth_chainId"}]]`))
		require.Equal(t, -32600, m["null"].Error.Code)
		require.NoError(t, json.Unmarshal(wsRaw(t, c, `[{"jsonrpc"`), &r))
		require.Equal(t, -32700, r.Error.Code)
	})
	t.Run("subscribe in batch respects cap and orders id before events", func(t *testing.T) {
		m := byId(t, wsRaw(t, c, `[{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["newHeads"]},{"jsonrpc":"2.0","id":2,"method":"eth_subscribe","params":["newHeads"]},{"jsonrpc":"2.0","id":3,"method":"eth_subscribe","params":["nope"]}]`))
		require.Equal(t, -32601, m["3"].Error.Code)
		ok, over := m["1"], m["2"]
		if ok.Error != nil {
			ok, over = over, ok
		}
		require.Nil(t, ok.Error)
		require.Equal(t, int(common.JsonRpcErrorCapacityExceeded), over.Error.Code)
		require.Equal(t, 1, subCount(t, e))
		var sub string
		require.NoError(t, json.Unmarshal(ok.Result, &sub))
		up.Mine(1)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, n, err := c.Read(ctx)
		require.NoError(t, err)
		require.Contains(t, string(n), sub)
		require.Contains(t, string(n), `"number":"0x15"`)
		require.Equal(t, 1.0, promUtil.ToFloat64(telemetry.MetricWsSubscriptions.WithLabelValues("test_project", "evm:123", "newHeads")))
		require.GreaterOrEqual(t, promUtil.ToFloat64(telemetry.MetricWsNotificationsTotal.WithLabelValues("test_project", "evm:123", "newHeads")), 1.0)
		require.GreaterOrEqual(t, promUtil.ToFloat64(telemetry.MetricWsConnections.WithLabelValues("test_project", "evm:123")), 1.0)
		_ = c.Close(websocket.StatusNormalClosure, "")
		require.Eventually(t, func() bool {
			return promUtil.ToFloat64(telemetry.MetricWsSubscriptions.WithLabelValues("test_project", "evm:123", "newHeads")) == 0 &&
				promUtil.ToFloat64(telemetry.MetricWsClosedTotal.WithLabelValues("test_project", "evm:123", "client")) >= 1
		}, 5*time.Second, 20*time.Millisecond)
	})
}

// The notification-only case sends nothing; prove it by checking the very
// next frame belongs to a follow-up single call.
func TestWs_BatchNotificationOnlyNoReply(t *testing.T) {
	up := newScriptedEvmUpstream(123, 20)
	defer up.Close()
	_, _, base, shutdown, e := createServerTestFixtures(wsHeadCacheCfg(up, &common.WebSocketServerConfig{Enabled: true}), t)
	defer shutdown()
	waitHead(t, e, 20)
	c := dialRaw(t, base)
	ctx := context.Background()
	require.NoError(t, c.Write(ctx, websocket.MessageText, []byte(`[{"jsonrpc":"2.0","method":"eth_chainId","params":[]},{"jsonrpc":"2.0","method":"eth_chainId"}]`)))
	time.Sleep(300 * time.Millisecond)
	var r wsMsg
	require.NoError(t, json.Unmarshal(wsRaw(t, c, `{"jsonrpc":"2.0","id":9,"method":"eth_chainId"}`), &r))
	require.Equal(t, "9", string(r.ID))
	// Mixed: only the id'd item is in the array.
	var arr []wsMsg
	require.NoError(t, json.Unmarshal(wsRaw(t, c, `[{"jsonrpc":"2.0","method":"eth_chainId"},{"jsonrpc":"2.0","id":7,"method":"eth_chainId"}]`), &arr))
	require.Len(t, arr, 1)
	require.Equal(t, "7", string(arr[0].ID))
}

// Batch items share the connection inflight bound.
func TestWs_BatchRespectsInflight(t *testing.T) {
	c, _ := testWsConn(t, 64)
	c.ws.cfg.MaxBatchSize = 50
	const limit = 3
	sem := make(chan struct{}, limit)
	var held, peak atomic.Int32
	acquire := func() bool {
		sem <- struct{}{}
		n := held.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		return true
	}
	release := func() { held.Add(-1); <-sem }
	items := make([]string, 40)
	for i := range items {
		items[i] = fmt.Sprint(i) // invalid items, handled without a project
	}
	require.True(t, c.dispatchBatch([]byte("["+strings.Join(items, ",")+"]"), acquire, release))
	select {
	case b := <-c.out:
		var arr []wsMsg
		require.NoError(t, json.Unmarshal(b, &arr))
		require.Len(t, arr, 40)
	case <-time.After(5 * time.Second):
		t.Fatal("no batch reply")
	}
	require.LessOrEqual(t, peak.Load(), int32(limit))
	c.wg.Wait()
}

func TestWs_ConfigFallbacks(t *testing.T) {
	ws := &wsServer{cfg: &common.WebSocketServerConfig{}}
	require.Equal(t, 16, ws.inflight())
	require.Equal(t, 30*time.Second, ws.pingInterval(), "ping (and re-auth) must never be disabled")
	require.Equal(t, 100, ws.maxBatchSize())
	ws.cfg.MaxInflightPerConnection, ws.cfg.PingInterval, ws.cfg.MaxBatchSize = 4, common.Duration(time.Second), 7
	require.Equal(t, 4, ws.inflight())
	require.Equal(t, time.Second, ws.pingInterval())
	require.Equal(t, 7, ws.maxBatchSize())
}

// Regression: WS survives past the HTTP server WriteTimeout (1s).
func TestWs_OutlivesHTTPWriteTimeout(t *testing.T) {
	for _, mode := range []string{"eth_chainId", "newHeads"} {
		t.Run(mode, func(t *testing.T) {
			up := newScriptedEvmUpstream(123, 20)
			defer up.Close()
			cfg := wsHeadCacheCfg(up, &common.WebSocketServerConfig{Enabled: true})
			cfg.Server.WriteTimeout = common.Duration(time.Second).Ptr()
			_, _, base, shutdown, e := createServerTestFixtures(cfg, t)
			defer shutdown()
			waitHead(t, e, 20)
			w, _, err := dialWs(t, wsURL(base, ""), nil)
			require.NoError(t, err)
			require.Nil(t, w.call("eth_chainId", `[]`).Error)
			var sub string
			if mode == "newHeads" {
				r := w.call("eth_subscribe", `["newHeads"]`)
				require.Nil(t, r.Error)
				require.NoError(t, json.Unmarshal(r.Result, &sub))
			}
			time.Sleep(1500 * time.Millisecond)
			if mode == "eth_chainId" {
				after := w.call("eth_chainId", `[]`)
				require.Nil(t, after.Error)
				require.JSONEq(t, `"0x7b"`, string(after.Result))
			} else {
				up.Mine(1)
				require.Contains(t, string(w.next(sub)), `"number":"0x15"`)
			}
		})
	}
}

func TestWs_BatchResponseSizeCap(t *testing.T) {
	old := wsMaxBatchResponseBytes
	wsMaxBatchResponseBytes = 64
	defer func() { wsMaxBatchResponseBytes = old }()
	c, _ := testWsConn(t, 4)
	sem := make(chan struct{}, 4)
	acquire := func() bool { sem <- struct{}{}; return true }
	release := func() { <-sem }
	require.True(t, c.dispatchBatch([]byte(`[1,2,3]`), acquire, release))
	select {
	case b := <-c.out:
		var r wsMsg
		require.NoError(t, json.Unmarshal(b, &r), string(b))
		require.Equal(t, int(common.JsonRpcErrorCapacityExceeded), r.Error.Code)
	case <-time.After(5 * time.Second):
		t.Fatal("no reply")
	}
	c.wg.Wait()
}

func TestWs_BatchOverflowAbortsSubscriptions(t *testing.T) {
	up := newScriptedEvmUpstream(123, 20)
	defer up.Close()
	cfg := wsHeadCacheCfg(up, &common.WebSocketServerConfig{
		Enabled: true, MaxConnections: 1, MaxSubscriptionsPerConnection: 10,
	})
	_, _, base, shutdown, e := createServerTestFixtures(cfg, t)
	defer shutdown()
	waitHead(t, e, 20)
	old := wsMaxBatchResponseBytes
	wsMaxBatchResponseBytes = 64
	defer func() { wsMaxBatchResponseBytes = old }()
	baseline := promUtil.ToFloat64(telemetry.MetricWsSubscriptions.WithLabelValues("test_project", "evm:123", "newHeads"))
	c := dialRaw(t, base)
	var r wsMsg
	require.NoError(t, json.Unmarshal(wsRaw(t, c, `[{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["newHeads"]},{"jsonrpc":"2.0","id":2,"method":"eth_subscribe","params":["newHeads"]}]`), &r))
	require.Equal(t, int(common.JsonRpcErrorCapacityExceeded), r.Error.Code)
	require.Eventually(t, func() bool {
		return subCount(t, e) == 0 && promUtil.ToFloat64(telemetry.MetricWsSubscriptions.WithLabelValues("test_project", "evm:123", "newHeads")) == baseline
	}, 5*time.Second, 20*time.Millisecond, "overflow must release subscribers and their pumps")
	// The same connection must remain usable after aborting the reserved pumps.
	// A successful subscribe here also proves the subscription cap was freed.
	wsMaxBatchResponseBytes = old
	var subscribed wsMsg
	require.NoError(t, json.Unmarshal(wsRaw(t, c, `{"jsonrpc":"2.0","id":3,"method":"eth_subscribe","params":["newHeads"]}`), &subscribed))
	require.Nil(t, subscribed.Error)
	require.NotEmpty(t, subscribed.Result)
	_ = c.CloseNow()
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		next, _, err := websocket.Dial(ctx, wsURL(base, ""), nil)
		if err == nil {
			_ = next.CloseNow()
		}
		return err == nil
	}, 5*time.Second, 50*time.Millisecond, "connection slot must be reusable")
}

// Each batch item consumes project rate-limit budget on its own.
func TestWs_BatchPerItemRateLimit(t *testing.T) {
	up := newScriptedEvmUpstream(123, 20)
	defer up.Close()
	cfg := wsHeadCacheCfg(up, &common.WebSocketServerConfig{Enabled: true})
	cfg.RateLimiters = &common.RateLimiterConfig{Budgets: []*common.RateLimitBudgetConfig{{
		Id: "ws-batch", Rules: []*common.RateLimitRuleConfig{{Method: "eth_chainId", MaxCount: 2, Period: common.RateLimitPeriodMinute}},
	}}}
	cfg.Projects[0].RateLimitBudget = "ws-batch"
	_, _, base, shutdown, e := createServerTestFixtures(cfg, t)
	defer shutdown()
	waitHead(t, e, 20)
	c := dialRaw(t, base)
	items := make([]string, 5)
	for i := range items {
		items[i] = fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_chainId","params":[]}`, i+1)
	}
	m := byId(t, wsRaw(t, c, "["+strings.Join(items, ",")+"]"))
	require.Len(t, m, 5)
	ok, limited := 0, 0
	for _, r := range m {
		if r.Error == nil {
			ok++
		} else {
			limited++
		}
	}
	require.Equal(t, 2, ok)
	require.Equal(t, 3, limited)
}

func TestWs_SingleInvalidRequestCodes(t *testing.T) {
	c, _ := testWsConn(t, 4)
	for msg, code := range map[string]int{`17`: -32600, `{"foo":1}`: -32600, `"x"`: -32600, `{"jsonrpc"`: -32700} {
		reply, _ := c.handleOne([]byte(msg))
		var r wsMsg
		require.NoError(t, json.Unmarshal(reply, &r))
		require.Equal(t, code, r.Error.Code, msg)
	}
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
	upCalls := func() int64 {
		var n int64
		for _, m := range []string{"eth_chainId", "eth_blockNumber", "eth_getBlockByNumber", "eth_syncing"} {
			n += up.Calls(m)
		}
		return n
	}

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
	// not an unrelated failure, kept it from being created above).
	_, _, _ = dialWs(t, unseenURL("secret=s3cret"), http.Header{"Origin": {"https://good.example"}})
	require.Eventually(t, created, 5*time.Second, 20*time.Millisecond)

	// Authorized success on a configured network is unchanged.
	before := upCalls()
	w, _, err := dialWs(t, wsURL(base, "secret=s3cret"), http.Header{"Origin": {"https://good.example"}})
	require.NoError(t, err)
	r := w.call("eth_chainId", `[]`)
	require.Nil(t, r.Error)
	require.JSONEq(t, `"0x7b"`, string(r.Result))
	require.GreaterOrEqual(t, upCalls(), before)
}

// Regression: a batch aborted mid-dispatch must release reserved subscriptions
// without starting pumps for IDs never delivered to the client.
func TestWs_BatchAbortAfterSubscribeDoesNotHangCleanup(t *testing.T) {
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

	c, cancel := testWsConn(t, 16)
	lg := zerolog.Nop()
	c.lg, c.project, c.network, c.networkId = &lg, prj, nw, "evm:123"
	c.req = httptest.NewRequest(http.MethodGet, "/test_project/evm/123", nil)
	c.subs = map[string]*wsSub{}
	c.ws.s = &HttpServer{serverCfg: &common.ServerConfig{}}
	c.ws.cfg.MaxSubscriptionsPerConnection = 10
	c.ws.cfg.SendQueueSize = 16

	calls := 0
	acquire := func() bool {
		calls++
		if calls == 1 {
			return true
		}
		// Second slot: wait for the subscribe to register, then close.
		require.Eventually(t, func() bool { return subCount(t, e) == 1 }, 5*time.Second, 10*time.Millisecond)
		cancel(errors.New("closing"))
		return false
	}
	batch := `[{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["newHeads"]},{"jsonrpc":"2.0","id":2,"method":"eth_chainId"}]`
	require.False(t, c.dispatchBatch([]byte(batch), acquire, func() {}))
	require.Empty(t, c.subs, "aborted batch must not retain undisclosed subscriptions")
	reservedDone := make(chan struct{})
	go func() { c.wg.Wait(); close(reservedDone) }()
	select {
	case <-reservedDone:
	case <-time.After(5 * time.Second):
		t.Fatal("aborted batch retained a reserved pump")
	}

	done := make(chan struct{})
	go func() { c.cleanup(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup hung: reserved subscription pump never started")
	}
	require.Eventually(t, func() bool { return subCount(t, e) == 0 }, 5*time.Second, 20*time.Millisecond)
}

// Public path: a client sends a batch larger than the inflight limit (with
// subscribes) and disconnects mid-batch. Connection slots, subscriptions and
// gauges must all be released.
func TestWs_BatchDisconnectReleasesResources(t *testing.T) {
	up := newScriptedEvmUpstream(123, 20)
	defer up.Close()
	cfg := wsHeadCacheCfg(up, &common.WebSocketServerConfig{
		Enabled: true, MaxConnections: 1, MaxInflightPerConnection: 1, MaxBatchSize: 50, MaxSubscriptionsPerConnection: 50,
	})
	_, _, base, shutdown, e := createServerTestFixtures(cfg, t)
	defer shutdown()
	waitHead(t, e, 20)
	conns := func() float64 {
		return promUtil.ToFloat64(telemetry.MetricWsConnections.WithLabelValues("test_project", "evm:123"))
	}
	subs := func() float64 {
		return promUtil.ToFloat64(telemetry.MetricWsSubscriptions.WithLabelValues("test_project", "evm:123", "newHeads"))
	}
	conns0, subs0 := conns(), subs()

	for round := 0; round < 3; round++ {
		var c *websocket.Conn
		require.Eventually(t, func() bool {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			var err error
			c, _, err = websocket.Dial(ctx, wsURL(base, ""), nil)
			return err == nil
		}, 10*time.Second, 50*time.Millisecond, "connection slot must be released (round %d)", round)
		items := make([]string, 40)
		for i := range items {
			if i%2 == 0 {
				items[i] = fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_subscribe","params":["newHeads"]}`, i)
			} else {
				items[i] = fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_getBlockByNumber","params":["0x13",true]}`, i)
			}
		}
		require.NoError(t, c.Write(context.Background(), websocket.MessageText, []byte("["+strings.Join(items, ",")+"]")))
		time.Sleep(time.Duration(round*5) * time.Millisecond)
		_ = c.CloseNow()
	}
	require.Eventually(t, func() bool {
		return subCount(t, e) == 0 && conns() == conns0 && subs() == subs0
	}, 10*time.Second, 20*time.Millisecond, "subs=%d conns=%v subsGauge=%v", subCount(t, e), conns(), subs())
}
