package clients_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/erpc/erpc/clients"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/upstream"
	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// TestWsClientNormalizesJsonRpcErrorsLikeHttp: the same upstream error body
// must normalize to the same error code over WS as over HTTP.
func TestWsClientNormalizesJsonRpcErrorsLikeHttp(t *testing.T) {
	const errBody = `,"error":{"code":3,"message":"execution reverted","data":"0x08c379a0"}}`

	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1` + errBody))
	}))
	t.Cleanup(httpSrv.Close)

	upgrader := websocket.Upgrader{}
	wsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var req struct {
				ID json.RawMessage `json:"id"`
			}
			_ = common.SonicCfg.Unmarshal(msg, &req)
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"jsonrpc":"2.0","id":`+string(req.ID)+errBody))
		}
	}))
	t.Cleanup(wsSrv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	logger := zerolog.New(zerolog.NewTestWriter(t)).Level(zerolog.WarnLevel)
	up := common.NewFakeUpstream("test-upstream")
	extractor := upstream.NewCompositeJsonRpcErrorExtractor()

	httpURL, err := url.Parse(httpSrv.URL)
	require.NoError(t, err)
	httpClient, err := clients.NewGenericHttpJsonRpcClient(ctx, &logger, "test-project", up, httpURL, nil, nil, extractor)
	require.NoError(t, err)

	wsURL, err := url.Parse(wsSrv.URL)
	require.NoError(t, err)
	wsURL.Scheme = "ws"
	wsClient, err := clients.NewWsJsonRpcClient(ctx, &logger, "test-project", up, wsURL, nil, extractor)
	require.NoError(t, err)

	for name, c := range map[string]clients.ClientInterface{"http": httpClient, "ws": wsClient} {
		reqCtx, reqCancel := context.WithTimeout(ctx, 2*time.Second)
		_, err := c.SendRequest(reqCtx, common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[]}`)))
		reqCancel()
		require.Error(t, err, name)
		require.True(t, common.HasErrorCode(err, common.ErrCodeEndpointExecutionException), "%s: got %v", name, err)
	}
}
