package erpc

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/util"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// selfHealMockUpstream is a WS upstream whose connections can be killed
// WITHOUT a WebSocket close handshake, and which keeps accepting fresh
// connections afterwards — the moral equivalent of a fullnode pod being
// deleted and recreated behind a still-healthy gateway.
type selfHealMockUpstream struct {
	mu        sync.Mutex
	conns     []*selfHealConn
	connSeen  chan *selfHealConn
	subSeen   chan string // upstream-assigned sub id per eth_subscribe served
	subCount  atomic.Int64
	lastSubID atomic.Value // string
}

type selfHealConn struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
}

func (sc *selfHealConn) writeJSON(v interface{}) {
	sc.writeMu.Lock()
	defer sc.writeMu.Unlock()
	_ = sc.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_ = sc.conn.WriteJSON(v)
}

func (sc *selfHealConn) sendHead(subID string, num int64) {
	sc.writeJSON(map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "eth_subscription",
		"params": map[string]interface{}{
			"subscription": subID,
			"result": map[string]interface{}{
				"number":     fmt.Sprintf("0x%x", num),
				"hash":       fmt.Sprintf("0x%064x", num),
				"parentHash": fmt.Sprintf("0x%064x", num-1),
			},
		},
	})
}

func (m *selfHealMockUpstream) handle(conn *websocket.Conn) {
	sc := &selfHealConn{conn: conn}
	m.mu.Lock()
	m.conns = append(m.conns, sc)
	m.mu.Unlock()
	select {
	case m.connSeen <- sc:
	default:
	}

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var req map[string]interface{}
		if err := json.Unmarshal(msg, &req); err != nil {
			continue
		}
		method, _ := req["method"].(string)
		id := req["id"]
		switch method {
		case "eth_chainId":
			sc.writeJSON(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": "0x7b"})
		case "eth_getBlockByNumber":
			sc.writeJSON(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": map[string]interface{}{"number": "0x100", "timestamp": "0x6702a8f0"}})
		case "eth_syncing":
			sc.writeJSON(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": false})
		case "eth_subscribe":
			subID := fmt.Sprintf("0xupsub%02x", m.subCount.Add(1))
			m.lastSubID.Store(subID)
			sc.writeJSON(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": subID})
			select {
			case m.subSeen <- subID:
			default:
			}
		case "eth_unsubscribe":
			sc.writeJSON(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": true})
		default:
			sc.writeJSON(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": "0x1"})
		}
	}
}

// TestWebSocket_UpstreamDiesUngracefully_SelfHeals is the end-to-end
// regression test for the 2026-06-12 zkSync chain-324 incident: the single
// WS upstream's connection dies with NO close handshake; eRPC must — with
// no process restart —
//
//  1. detect the dead connection and re-dial,
//  2. re-subscribe newHeads upstream (retrying through transient failures),
//  3. resume delivering heads to the ALREADY-CONNECTED client subscription,
//  4. accept NEW client subscriptions afterwards.
func TestWebSocket_UpstreamDiesUngracefully_SelfHeals(t *testing.T) {
	mock := &selfHealMockUpstream{
		connSeen: make(chan *selfHealConn, 16),
		subSeen:  make(chan string, 16),
	}
	mockSrv := mockWsUpstream(t, mock.handle)
	defer mockSrv.Close()

	setupGock()
	defer util.ResetGock()

	wsUpstreamURL := "ws" + strings.TrimPrefix(mockSrv.URL, "http")
	addr, cleanup := setupTestERPCServer(t, standardWsConfig(wsUpstreamURL))
	defer cleanup()

	// eRPC's upstream WS client connects at upstream registration.
	var upConn1 *selfHealConn
	select {
	case upConn1 = <-mock.connSeen:
	case <-time.After(5 * time.Second):
		t.Fatal("eRPC never connected to the WS upstream")
	}

	// Client subscribes to newHeads (this lazily bootstraps the network's
	// indexer ingress, which issues the upstream eth_subscribe).
	client := dialWs(t, addr)
	defer client.Close()
	resp := sendAndReceive(t, client, `{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["newHeads"]}`)
	require.Nil(t, resp["error"], "subscribe must succeed while the upstream is healthy: %v", resp["error"])
	clientSubID, ok := resp["result"].(string)
	require.True(t, ok, "subscription ID should be a string, got: %v", resp["result"])

	var sub1 string
	select {
	case sub1 = <-mock.subSeen:
	case <-time.After(5 * time.Second):
		t.Fatal("eRPC never subscribed newHeads on the upstream")
	}

	// pushHeadsUntilReceived repeatedly emits fresh heads on the upstream
	// connection until one reaches the given client (the upstream subscribe
	// response and eRPC's handler registration race the first push — real
	// chains emit heads continuously, so a retry loop models reality).
	headNum := int64(0x200)
	pushHeadsUntilReceived := func(c *websocket.Conn, up *selfHealConn, subID string, within time.Duration) map[string]interface{} {
		t.Helper()
		deadline := time.Now().Add(within)
		notifCh := make(chan map[string]interface{}, 1)
		errCh := make(chan error, 1)
		go func() {
			_ = c.SetReadDeadline(deadline)
			_, msg, err := c.ReadMessage()
			if err != nil {
				errCh <- err
				return
			}
			var notif map[string]interface{}
			if err := json.Unmarshal(msg, &notif); err != nil {
				errCh <- err
				return
			}
			notifCh <- notif
		}()
		for {
			headNum++
			up.sendHead(subID, headNum)
			select {
			case notif := <-notifCh:
				return notif
			case err := <-errCh:
				t.Fatalf("client read failed while waiting for a head: %v", err)
				return nil
			case <-time.After(250 * time.Millisecond):
				if time.Now().After(deadline) {
					t.Fatalf("client never received a head within %s", within)
					return nil
				}
			}
		}
	}

	// Sanity: a head flows end-to-end before the failure.
	notif := pushHeadsUntilReceived(client, upConn1, sub1, 5*time.Second)
	require.Equal(t, "eth_subscription", notif["method"])
	require.Equal(t, clientSubID, notif["params"].(map[string]interface{})["subscription"])

	// ── The incident ──────────────────────────────────────────────────
	// Kill the upstream TCP connection with NO WebSocket close handshake.
	_ = upConn1.conn.UnderlyingConn().Close()

	// eRPC must re-dial (the gateway/httptest server is still up, exactly
	// like the incident topology) ...
	var upConn2 *selfHealConn
	select {
	case upConn2 = <-mock.connSeen:
	case <-time.After(10 * time.Second):
		t.Fatal("eRPC never re-dialed the upstream after the ungraceful kill")
	}

	// ... and re-subscribe newHeads with a fresh upstream subscription.
	var sub2 string
	select {
	case sub2 = <-mock.subSeen:
	case <-time.After(10 * time.Second):
		t.Fatal("eRPC never re-subscribed newHeads after reconnecting")
	}
	assert.NotEqual(t, sub1, sub2, "the re-subscribe must be a fresh upstream subscription")

	// The ALREADY-CONNECTED client must resume receiving heads on its
	// ORIGINAL subscription ID, with no client-side action.
	notif = pushHeadsUntilReceived(client, upConn2, sub2, 10*time.Second)
	assert.Equal(t, "eth_subscription", notif["method"])
	assert.Equal(t, clientSubID, notif["params"].(map[string]interface{})["subscription"],
		"recovered heads must arrive on the client's original subscription ID")

	// And a NEW client must be able to subscribe and receive heads.
	client2 := dialWs(t, addr)
	defer client2.Close()
	resp2 := sendAndReceive(t, client2, `{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["newHeads"]}`)
	require.Nil(t, resp2["error"], "new client subscribe must succeed after recovery: %v", resp2["error"])
	client2SubID, _ := resp2["result"].(string)

	notif2 := pushHeadsUntilReceived(client2, upConn2, sub2, 10*time.Second)
	assert.Equal(t, client2SubID, notif2["params"].(map[string]interface{})["subscription"])
}
