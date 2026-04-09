package erpc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/auth"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
)

// WsConnection represents a single client WebSocket connection to the proxy.
type WsConnection struct {
	id     string
	conn   *websocket.Conn
	appCtx context.Context
	cancel context.CancelFunc
	logger *zerolog.Logger

	server              *HttpServer
	project             *PreparedProject
	subscriptionManager *SubscriptionManager
	architecture        string
	chainId             string
	networkId           string
	httpReq             *http.Request // original upgrade request for auth/headers

	// Write synchronization (gorilla/websocket requires synchronized writes)
	writeMu sync.Mutex

	// Subscription tracking: clientSubID -> *SubscriptionEntry
	subscriptions sync.Map

	closed atomic.Bool
}

func (s *HttpServer) handleWebSocket(
	httpCtx context.Context,
	w http.ResponseWriter,
	r *http.Request,
	lg *zerolog.Logger,
	project *PreparedProject,
	architecture string,
	chainId string,
) {
	wsCfg := s.serverCfg.WebSocket

	upgrader := websocket.Upgrader{
		ReadBufferSize:  wsCfg.ReadBufferSize,
		WriteBufferSize: wsCfg.WriteBufferSize,
		CheckOrigin: func(r *http.Request) bool {
			// Mirror the project's CORS policy
			if project != nil && project.Config.CORS != nil {
				origin := r.Header.Get("Origin")
				if origin == "" {
					return true
				}
				for _, allowedOrigin := range project.Config.CORS.AllowedOrigins {
					match, err := common.WildcardMatch(allowedOrigin, origin)
					if err != nil {
						continue
					}
					if match {
						return true
					}
				}
				return false
			}
			// No CORS config means allow all
			return true
		},
	}

	wsConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		lg.Error().Err(err).Msg("websocket upgrade failed")
		return
	}

	networkId := fmt.Sprintf("%s:%s", architecture, chainId)

	connCtx, connCancel := context.WithCancel(s.appCtx)
	wsc := &WsConnection{
		id:                  fmt.Sprintf("ws-%d", time.Now().UnixNano()),
		conn:                wsConn,
		appCtx:              connCtx,
		cancel:              connCancel,
		logger:              lg,
		server:              s,
		project:             project,
		subscriptionManager: s.subscriptionManager,
		architecture:        architecture,
		chainId:             chainId,
		networkId:           networkId,
		httpReq:             r,
	}

	lg.Info().Str("connId", wsc.id).Str("remoteAddr", r.RemoteAddr).Msg("websocket connection established")

	// Track active connection for graceful shutdown
	s.activeWsConns.Store(wsc.id, wsc)

	// Set max message size
	wsConn.SetReadLimit(wsCfg.MaxMessageSize)

	// Set up pong handler to extend read deadline on pong receipt
	pingInterval := wsCfg.PingInterval.Duration()
	wsConn.SetPongHandler(func(string) error {
		return wsConn.SetReadDeadline(time.Now().Add(pingInterval * 2))
	})

	go wsc.pingLoop(pingInterval)

	// Run read loop (blocks until connection closes)
	wsc.readLoop()

	// Cleanup
	s.activeWsConns.Delete(wsc.id)
	wsc.Close()
}

func (wsc *WsConnection) readLoop() {
	for {
		if wsc.appCtx.Err() != nil {
			return
		}

		_, message, err := wsc.conn.ReadMessage()
		if err != nil {
			if wsc.appCtx.Err() != nil {
				return
			}
			if websocket.IsUnexpectedCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				wsc.logger.Warn().Err(err).Str("connId", wsc.id).Msg("websocket read error")
			} else {
				wsc.logger.Info().Str("connId", wsc.id).Msg("websocket connection closed by client")
			}
			return
		}

		go wsc.handleMessage(message)
	}
}

func (wsc *WsConnection) handleMessage(raw []byte) {
	defer func() {
		if rec := recover(); rec != nil {
			telemetry.MetricUnexpectedPanicTotal.WithLabelValues(
				"ws-request-handler",
				fmt.Sprintf("project:%s network:%s", wsc.project.Config.Id, wsc.networkId),
				common.ErrorFingerprint(rec),
			).Inc()
			wsc.logger.Error().
				Interface("panic", rec).
				Str("stack", string(debug.Stack())).
				Str("connId", wsc.id).
				Msg("unexpected panic in websocket request handler")
		}
	}()

	startedAt := time.Now()

	// Detect batch requests
	isBatch := len(raw) > 0 && raw[0] == '['
	if isBatch {
		wsc.handleBatch(raw, &startedAt)
		return
	}

	wsc.handleSingleRequest(raw, &startedAt)
}

func (wsc *WsConnection) handleSingleRequest(raw []byte, startedAt *time.Time) {
	nq := common.NewNormalizedRequest(raw)
	nq.ForwardHeaders = make(http.Header)

	requestCtx := common.StartRequestSpan(wsc.appCtx, nq)

	// Resolve and set real client IP
	clientIP := wsc.server.resolveRealClientIP(wsc.httpReq)
	nq.SetClientIP(clientIP)

	// Validate the raw JSON-RPC payload early
	if err := nq.Validate(); err != nil {
		wsc.writeErrorResponse(nq, err, startedAt, &common.TRUE)
		common.EndRequestSpan(requestCtx, nil, err)
		return
	}

	// Apply header forwarding rules
	if wsc.project != nil {
		for _, matchKey := range wsc.project.Config.ForwardHeaders {
			for key, values := range wsc.httpReq.Header {
				matches, err := common.WildcardMatch(matchKey, key)
				if err != nil {
					continue
				}
				if matches {
					for _, value := range values {
						nq.ForwardHeaders.Add(matchKey, value)
					}
				}
			}
		}
	}

	method, _ := nq.Method()

	// Check method allowlist/denylist
	if !wsc.isMethodAllowed(method) {
		jsonrpcVersion := "2.0"
		var reqId interface{}
		if jrr, err := nq.JsonRpcRequest(); err == nil {
			jsonrpcVersion = jrr.JSONRPC
			reqId = jrr.ID
		}
		resp := map[string]interface{}{
			"jsonrpc": jsonrpcVersion,
			"id":      reqId,
			"error": map[string]interface{}{
				"code":    int(common.JsonRpcErrorUnsupportedException),
				"message": fmt.Sprintf("method not supported: %s", method),
			},
		}
		_ = wsc.writeJSON(resp)
		common.EndRequestSpan(requestCtx, nil, nil)
		return
	}

	// Authentication
	if wsc.project != nil {
		ap, err := auth.NewPayloadFromHttp(method, wsc.httpReq.RemoteAddr, wsc.httpReq.Header, wsc.httpReq.URL.Query())
		if err != nil {
			wsc.writeErrorResponse(nq, err, startedAt, &common.TRUE)
			common.EndRequestSpan(requestCtx, nil, err)
			return
		}
		user, err := wsc.project.AuthenticateConsumer(requestCtx, nq, method, ap)
		if err != nil {
			wsc.writeErrorResponse(nq, err, startedAt, wsc.server.serverCfg.IncludeErrorDetails)
			common.EndRequestSpan(requestCtx, nil, err)
			return
		}
		nq.SetUser(user)
	}

	// Resolve network
	nw, err := wsc.project.GetNetwork(wsc.appCtx, wsc.networkId)
	if err != nil {
		wsc.writeErrorResponse(nq, err, startedAt, wsc.server.serverCfg.IncludeErrorDetails)
		common.EndRequestSpan(requestCtx, nil, err)
		return
	}
	nq.SetNetwork(nw)

	nq.ApplyDirectiveDefaults(nw.Config().DirectiveDefaults)
	uaMode := common.UserAgentTrackingModeSimplified
	if wsc.project != nil && wsc.project.Config.UserAgentMode != "" {
		uaMode = wsc.project.Config.UserAgentMode
	}
	nq.EnrichFromHttp(wsc.httpReq.Header, wsc.httpReq.URL.Query(), uaMode)

	// Handle subscription methods specially
	if IsSubscriptionMethod(method) {
		var resp *common.NormalizedResponse
		var err error

		if method == "eth_subscribe" {
			resp, err = wsc.subscriptionManager.Subscribe(requestCtx, wsc, nq, wsc.project, wsc.networkId)
		} else {
			resp, err = wsc.subscriptionManager.Unsubscribe(requestCtx, wsc, nq, wsc.project, wsc.networkId)
		}

		if err != nil {
			if resp != nil {
				go resp.Release()
			}
			wsc.writeErrorResponse(nq, err, startedAt, wsc.server.serverCfg.IncludeErrorDetails)
			common.EndRequestSpan(requestCtx, nil, err)
			return
		}

		wsc.writeNormalizedResponse(resp)
		common.EndRequestSpan(requestCtx, resp, nil)
		return
	}

	// Forward the request through the normal chain
	resp, err := wsc.project.Forward(requestCtx, wsc.networkId, nq)
	if err != nil {
		if resp != nil {
			go resp.Release()
		}
		wsc.writeErrorResponse(nq, err, startedAt, wsc.server.serverCfg.IncludeErrorDetails)
		common.EndRequestSpan(requestCtx, nil, err)
		return
	}

	wsc.writeNormalizedResponse(resp)
	common.EndRequestSpan(requestCtx, resp, nil)
}

func (wsc *WsConnection) handleBatch(raw []byte, startedAt *time.Time) {
	var requests []json.RawMessage
	if err := common.SonicCfg.Unmarshal(raw, &requests); err != nil {
		errResp := map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      nil,
			"error": map[string]interface{}{
				"code":    -32700,
				"message": "parse error",
			},
		}
		_ = wsc.writeJSON(errResp)
		return
	}

	responses := make([]interface{}, len(requests))
	var wg sync.WaitGroup

	for i, reqBody := range requests {
		wg.Add(1)
		go func(index int, reqRaw json.RawMessage) {
			defer wg.Done()
			defer func() {
				if rec := recover(); rec != nil {
					wsc.logger.Error().Interface("panic", rec).Msg("panic in batch websocket request")
					responses[index] = processErrorBody(wsc.logger, startedAt, nil, fmt.Errorf("%v", rec), wsc.server.serverCfg.IncludeErrorDetails)
				}
			}()

			nq := common.NewNormalizedRequest(reqRaw)
			nq.ForwardHeaders = make(http.Header)
			requestCtx := common.StartRequestSpan(wsc.appCtx, nq)

			clientIP := wsc.server.resolveRealClientIP(wsc.httpReq)
			nq.SetClientIP(clientIP)

			if err := nq.Validate(); err != nil {
				responses[index] = processErrorBody(wsc.logger, startedAt, nq, err, &common.TRUE)
				common.EndRequestSpan(requestCtx, nil, err)
				return
			}

			method, _ := nq.Method()
			if !wsc.isMethodAllowed(method) {
				jsonrpcVersion := "2.0"
				var reqId interface{}
				if jrr, err := nq.JsonRpcRequest(); err == nil {
					jsonrpcVersion = jrr.JSONRPC
					reqId = jrr.ID
				}
				responses[index] = &HttpJsonRpcErrorResponse{
					Jsonrpc: jsonrpcVersion,
					Id:      reqId,
					Error: map[string]interface{}{
						"code":    int(common.JsonRpcErrorUnsupportedException),
						"message": fmt.Sprintf("method not supported: %s", method),
					},
				}
				common.EndRequestSpan(requestCtx, nil, nil)
				return
			}

			if wsc.project != nil {
				ap, err := auth.NewPayloadFromHttp(method, wsc.httpReq.RemoteAddr, wsc.httpReq.Header, wsc.httpReq.URL.Query())
				if err != nil {
					responses[index] = processErrorBody(wsc.logger, startedAt, nq, err, &common.TRUE)
					common.EndRequestSpan(requestCtx, nil, err)
					return
				}
				user, err := wsc.project.AuthenticateConsumer(requestCtx, nq, method, ap)
				if err != nil {
					responses[index] = processErrorBody(wsc.logger, startedAt, nq, err, wsc.server.serverCfg.IncludeErrorDetails)
					common.EndRequestSpan(requestCtx, nil, err)
					return
				}
				nq.SetUser(user)
			}

			nw, err := wsc.project.GetNetwork(wsc.appCtx, wsc.networkId)
			if err != nil {
				responses[index] = processErrorBody(wsc.logger, startedAt, nq, err, wsc.server.serverCfg.IncludeErrorDetails)
				common.EndRequestSpan(requestCtx, nil, err)
				return
			}
			nq.SetNetwork(nw)
			nq.ApplyDirectiveDefaults(nw.Config().DirectiveDefaults)

			resp, err := wsc.project.Forward(requestCtx, wsc.networkId, nq)
			if err != nil {
				if resp != nil {
					go resp.Release()
				}
				responses[index] = processErrorBody(wsc.logger, startedAt, nq, err, wsc.server.serverCfg.IncludeErrorDetails)
				common.EndRequestSpan(requestCtx, nil, err)
				return
			}

			responses[index] = resp
			common.EndRequestSpan(requestCtx, resp, nil)
		}(i, reqBody)
	}

	wg.Wait()

	// Build batch response
	wsc.writeBatchResponse(responses)

	// Release responses
	for _, resp := range responses {
		if r, ok := resp.(*common.NormalizedResponse); ok {
			go r.Release()
		}
	}
}

func (wsc *WsConnection) isMethodAllowed(method string) bool {
	if wsc.project == nil {
		return true
	}

	shouldHandle := true

	if wsc.project.Config.IgnoreMethods != nil {
		for _, m := range wsc.project.Config.IgnoreMethods {
			match, _ := common.WildcardMatch(m, method)
			if match {
				shouldHandle = false
				break
			}
		}
	}

	if wsc.project.Config.AllowMethods != nil {
		for _, m := range wsc.project.Config.AllowMethods {
			match, _ := common.WildcardMatch(m, method)
			if match {
				shouldHandle = true
				break
			}
		}
	}

	return shouldHandle
}

func (wsc *WsConnection) writeJSON(v interface{}) error {
	wsc.writeMu.Lock()
	defer wsc.writeMu.Unlock()

	if wsc.closed.Load() {
		return fmt.Errorf("connection closed")
	}

	return wsc.conn.WriteJSON(v)
}

func (wsc *WsConnection) writeMessage(messageType int, data []byte) error {
	wsc.writeMu.Lock()
	defer wsc.writeMu.Unlock()

	if wsc.closed.Load() {
		return fmt.Errorf("connection closed")
	}

	return wsc.conn.WriteMessage(messageType, data)
}

func (wsc *WsConnection) writeNormalizedResponse(resp *common.NormalizedResponse) {
	wsc.writeMu.Lock()
	defer wsc.writeMu.Unlock()

	if wsc.closed.Load() {
		return
	}

	w, err := wsc.conn.NextWriter(websocket.TextMessage)
	if err != nil {
		wsc.logger.Debug().Err(err).Str("connId", wsc.id).Msg("failed to get websocket writer")
		return
	}

	_, err = resp.WriteTo(w)
	if closeErr := w.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	if err != nil {
		wsc.logger.Debug().Err(err).Str("connId", wsc.id).Msg("failed to write websocket response")
	}

	go resp.Release()
}

func (wsc *WsConnection) writeBatchResponse(responses []interface{}) {
	wsc.writeMu.Lock()
	defer wsc.writeMu.Unlock()

	if wsc.closed.Load() {
		return
	}

	w, err := wsc.conn.NextWriter(websocket.TextMessage)
	if err != nil {
		return
	}

	bw := NewBatchResponseWriter(responses)
	_, _ = bw.WriteTo(w)
	_ = w.Close()
}

func (wsc *WsConnection) writeErrorResponse(nq *common.NormalizedRequest, origErr error, startedAt *time.Time, includeDetails *bool) {
	errBody := processErrorBody(wsc.logger, startedAt, nq, origErr, includeDetails)
	var err error
	switch v := errBody.(type) {
	case *HttpJsonRpcErrorResponse:
		err = wsc.writeJSON(v)
	default:
		err = wsc.writeJSON(errBody)
	}
	if err != nil {
		wsc.logger.Debug().Err(err).Str("connId", wsc.id).Msg("failed to write error response")
	}
}

func (wsc *WsConnection) pingLoop(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			wsc.writeMu.Lock()
			err := wsc.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second))
			wsc.writeMu.Unlock()
			if err != nil {
				wsc.logger.Debug().Err(err).Str("connId", wsc.id).Msg("websocket ping failed")
				return
			}
		case <-wsc.appCtx.Done():
			return
		}
	}
}

// Close cleans up the WebSocket connection and all associated subscriptions.
func (wsc *WsConnection) Close() {
	wsc.closeWithCode(websocket.CloseNormalClosure, "")
}

// CloseWithGoingAway closes the connection with a GoingAway status code,
// indicating the server is shutting down.
func (wsc *WsConnection) CloseWithGoingAway() {
	wsc.closeWithCode(websocket.CloseGoingAway, "server shutting down")
}

func (wsc *WsConnection) closeWithCode(code int, reason string) {
	if wsc.closed.Swap(true) {
		return // already closed
	}

	wsc.cancel()

	// Unsubscribe all active subscriptions for this connection
	if wsc.subscriptionManager != nil && wsc.project != nil {
		wsc.subscriptionManager.CleanupConnection(wsc, wsc.project)
	}

	_ = wsc.conn.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(code, reason),
		time.Now().Add(5*time.Second),
	)
	_ = wsc.conn.Close()

	wsc.logger.Info().Str("connId", wsc.id).Int("closeCode", code).Msg("websocket connection closed")
}

// WriteSubscriptionNotification sends a subscription notification to the client.
// Used by the subscription manager to route upstream events to clients.
func (wsc *WsConnection) WriteSubscriptionNotification(clientSubID string, result json.RawMessage) error {
	notification := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "eth_subscription",
		"params": map[string]interface{}{
			"subscription": clientSubID,
			"result":       result,
		},
	}
	return wsc.writeJSON(notification)
}

// resolveNetworkId extracts the network ID from the request body when not in the URL.
func resolveNetworkId(raw []byte, architecture, chainId string) (string, string, string) {
	if architecture != "" && chainId != "" {
		return fmt.Sprintf("%s:%s", architecture, chainId), architecture, chainId
	}

	var req map[string]interface{}
	if err := common.SonicCfg.Unmarshal(raw, &req); err == nil {
		if networkId, ok := req["networkId"].(string); ok {
			parts := strings.Split(networkId, ":")
			if len(parts) == 2 {
				return networkId, parts[0], parts[1]
			}
		}
	}

	return "", architecture, chainId
}
