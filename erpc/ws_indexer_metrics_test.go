package erpc

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pushingHeadsUpstream answers eth_subscribe and then pushes one head.
func pushingHeadsUpstream(t *testing.T) *httptest.Server {
	return mockWsUpstream(t, func(conn *websocket.Conn) {
		standardMockWsHandler(conn, func(method string, id interface{}, req map[string]interface{}) {
			if method != "eth_subscribe" {
				mockWriteJSON(conn, map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": true})
				return
			}
			subID := fmt.Sprintf("0xheads%v", id)
			mockWriteJSON(conn, map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": subID})
			go func() {
				time.Sleep(300 * time.Millisecond)
				mockWriteJSON(conn, map[string]interface{}{
					"jsonrpc": "2.0",
					"method":  "eth_subscription",
					"params": map[string]interface{}{
						"subscription": subID,
						"result":       map[string]interface{}{"number": "0x101", "hash": "0xaaa"},
					},
				})
			}()
		})
	})
}

func TestWebSocket_LastHeadTimestampCarriesProjectAndNetwork(t *testing.T) {
	mockUpstream := pushingHeadsUpstream(t)
	defer mockUpstream.Close()
	setupGock()
	defer util.ResetGock()

	addr, cleanup := setupTestERPCServer(t, standardWsConfig("ws"+strings.TrimPrefix(mockUpstream.URL, "http")))
	defer cleanup()

	conn := dialWs(t, addr)
	defer conn.Close()
	resp := sendAndReceive(t, conn, `{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["newHeads"]}`)
	require.Nil(t, resp["error"])

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, _, err := conn.ReadMessage()
	require.NoError(t, err, "head notification")

	assert.Positive(t, testutil.ToFloat64(telemetry.MetricNetworkSubscriptionLastHeadTimestamp.WithLabelValues("test_ws", "evm:123")))
}
