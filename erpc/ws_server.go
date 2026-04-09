package erpc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/auth"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
)

// wsConnCounter is an atomic counter for generating unique WebSocket connection IDs.
var wsConnCounter int64

// WsConnection represents a single client WebSocket connection to the proxy.
// Each connection tracks its own subscriptions and enforces per-connection limits.
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

	// Subscription tracking: clientSubId -> *SubscriptionEntry
	subscriptions sync.Map

	// subscriptionCount tracks active subscriptions atomically for fast limit checks,
	// avoiding a full sync.Map.Range() scan on every subscribe call.
	subscriptionCount atomic.Int64

	closed atomic.Bool
}

// handleWebSocket upgrades an HTTP connection to WebSocket and runs the
// read/write loops for the lifetime of the connection.
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
			return checkWsOrigin(r, project)
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
		id:                  fmt.Sprintf("ws-%d", atomic.AddInt64(&wsConnCounter, 1)),
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

// checkWsOrigin validates the WebSocket upgrade request origin against
// the project's CORS policy. If no CORS config is set, all origins are allowed.
func checkWsOrigin(r *http.Request, project *PreparedProject) bool {
	if project == nil || project.Config.CORS == nil {
		return true
	}

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

//
// --- Read loop and message dispatch ---
//

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

	isBatch := len(raw) > 0 && raw[0] == '['
	if isBatch {
		wsc.handleBatch(raw, &startedAt)
		return
	}

	wsc.handleSingleRequest(raw, &startedAt)
}

//
// --- Single request handling ---
//

func (wsc *WsConnection) handleSingleRequest(raw []byte, startedAt *time.Time) {
	nq := common.NewNormalizedRequest(raw)
	nq.ForwardHeaders = make(http.Header)

	requestCtx := common.StartRequestSpan(wsc.appCtx, nq)

	clientIP := wsc.server.resolveRealClientIP(wsc.httpReq)
	nq.SetClientIP(clientIP)

	if err := nq.Validate(); err != nil {
		wsc.writeErrorResponse(nq, err, startedAt, &common.TRUE)
		common.EndRequestSpan(requestCtx, nil, err)
		return
	}

	wsc.applyForwardHeaders(nq)

	method, _ := nq.Method()

	if !wsc.isMethodAllowed(method) {
		wsc.writeMethodNotSupportedError(nq, method)
		common.EndRequestSpan(requestCtx, nil, nil)
		return
	}

	if err := wsc.authenticate(requestCtx, nq, method); err != nil {
		wsc.writeErrorResponse(nq, err, startedAt, wsc.server.serverCfg.IncludeErrorDetails)
		common.EndRequestSpan(requestCtx, nil, err)
		return
	}

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

	// Subscription methods have their own dedicated handling path
	if IsSubscriptionMethod(method) {
		wsc.handleSubscriptionMethod(requestCtx, nq, method, startedAt)
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

// handleSubscriptionMethod routes eth_subscribe and eth_unsubscribe to the
// subscription manager.
func (wsc *WsConnection) handleSubscriptionMethod(requestCtx context.Context, nq *common.NormalizedRequest, method string, startedAt *time.Time) {
	var resp *common.NormalizedResponse
	var err error

	if IsSubscribeMethod(method) {
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

		// If subscribe failed, close the connection so the client reconnects
		// to a potentially healthier endpoint rather than sitting idle.
		if IsSubscribeMethod(method) {
			wsc.closeWithCode(websocket.CloseGoingAway, "subscription failed")
		}
		return
	}

	wsc.writeNormalizedResponse(resp)
	common.EndRequestSpan(requestCtx, resp, nil)
}

// applyForwardHeaders copies matching headers from the original upgrade
// request to the normalized request per the project's ForwardHeaders config.
func (wsc *WsConnection) applyForwardHeaders(nq *common.NormalizedRequest) {
	if wsc.project == nil {
		return
	}
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

// authenticate validates the request against the project's auth config.
// Returns nil if authentication succeeds or no auth is configured.
func (wsc *WsConnection) authenticate(requestCtx context.Context, nq *common.NormalizedRequest, method string) error {
	if wsc.project == nil {
		return nil
	}

	ap, err := auth.NewPayloadFromHttp(method, wsc.httpReq.RemoteAddr, wsc.httpReq.Header, wsc.httpReq.URL.Query())
	if err != nil {
		return err
	}
	user, err := wsc.project.AuthenticateConsumer(requestCtx, nq, method, ap)
	if err != nil {
		return err
	}
	nq.SetUser(user)
	return nil
}

// writeMethodNotSupportedError writes a JSON-RPC error response for
// methods that are blocked by the project's allowlist/denylist.
func (wsc *WsConnection) writeMethodNotSupportedError(nq *common.NormalizedRequest, method string) {
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
}

//
// --- Batch request handling ---
//

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

			wsc.handleBatchItem(index, reqRaw, startedAt, responses)
		}(i, reqBody)
	}

	wg.Wait()

	wsc.writeBatchResponse(responses)

	for _, resp := range responses {
		if r, ok := resp.(*common.NormalizedResponse); ok {
			go r.Release()
		}
	}
}

// handleBatchItem processes a single request within a batch. Subscription
// methods are rejected in batch requests since they require a persistent
// connection context.
func (wsc *WsConnection) handleBatchItem(index int, reqRaw json.RawMessage, startedAt *time.Time, responses []interface{}) {
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

	// Subscription methods are not supported in batch requests
	if IsSubscriptionMethod(method) {
		responses[index] = wsc.buildUnsupportedMethodResponse(nq, "subscription methods (eth_subscribe, eth_unsubscribe) are not supported in batch requests")
		common.EndRequestSpan(requestCtx, nil, nil)
		return
	}

	if !wsc.isMethodAllowed(method) {
		responses[index] = wsc.buildUnsupportedMethodResponse(nq, fmt.Sprintf("method not supported: %s", method))
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
}

// buildUnsupportedMethodResponse constructs a JSON-RPC error response for
// methods that cannot be used in the current context.
func (wsc *WsConnection) buildUnsupportedMethodResponse(nq *common.NormalizedRequest, message string) *HttpJsonRpcErrorResponse {
	jsonrpcVersion := "2.0"
	var reqId interface{}
	if jrr, err := nq.JsonRpcRequest(); err == nil {
		jsonrpcVersion = jrr.JSONRPC
		reqId = jrr.ID
	}
	return &HttpJsonRpcErrorResponse{
		Jsonrpc: jsonrpcVersion,
		Id:      reqId,
		Error: map[string]interface{}{
			"code":    int(common.JsonRpcErrorUnsupportedException),
			"message": message,
		},
	}
}

//
// --- Method filtering ---
//

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

//
// --- Write helpers ---
//

// wsWriteDeadline is the timeout applied to all WebSocket write operations.
const wsWriteDeadline = 10 * time.Second

func (wsc *WsConnection) writeJSON(v interface{}) error {
	wsc.writeMu.Lock()
	defer wsc.writeMu.Unlock()

	if wsc.closed.Load() {
		return fmt.Errorf("connection closed")
	}

	if err := wsc.conn.SetWriteDeadline(time.Now().Add(wsWriteDeadline)); err != nil {
		return err
	}
	defer wsc.conn.SetWriteDeadline(time.Time{})

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

//
// --- Keepalive ---
//

func (wsc *WsConnection) pingLoop(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			wsc.writeMu.Lock()
			err := wsc.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(wsWriteDeadline))
			wsc.writeMu.Unlock()
			if err != nil {
				wsc.logger.Debug().Err(err).Str("connId", wsc.id).Msg("websocket ping failed, closing connection")
				wsc.cancel()
				return
			}
		case <-wsc.appCtx.Done():
			return
		}
	}
}

//
// --- Connection lifecycle ---
//

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

	if wsc.subscriptionManager != nil && wsc.project != nil {
		wsc.subscriptionManager.CleanupConnection(wsc, wsc.project)
	}

	_ = wsc.conn.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(code, reason),
		time.Now().Add(unsubscribeTimeout),
	)
	_ = wsc.conn.Close()

	wsc.logger.Info().Str("connId", wsc.id).Int("closeCode", code).Msg("websocket connection closed")
}

// WriteSubscriptionNotification sends a subscription notification to the client.
// Used by the subscription manager to route upstream events to clients.
func (wsc *WsConnection) WriteSubscriptionNotification(clientSubId string, result json.RawMessage) error {
	notification := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "eth_subscription",
		"params": map[string]interface{}{
			"subscription": clientSubId,
			"result":       result,
		},
	}
	return wsc.writeJSON(notification)
}
