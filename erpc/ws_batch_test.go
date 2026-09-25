package erpc

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	promUtil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/erpc/erpc/util"
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
				require.NotNil(t, r.Error)
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
