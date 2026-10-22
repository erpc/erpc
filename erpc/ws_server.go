package erpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/auth"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/indexer"
	"github.com/erpc/erpc/indexer/adapters/wsclient"
	"github.com/erpc/erpc/telemetry"
	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
)

var wsConnCounter int64

// WsConnection is a client WebSocket connection, bound to one network.
type WsConnection struct {
	id     string
	conn   *websocket.Conn
	ctx    context.Context // cancelled on teardown; parent of every request
	cancel context.CancelFunc
	logger *zerolog.Logger

	server    *HttpServer
	project   *PreparedProject
	networkId string
	// networkLabel labels the connection's subscription metrics; set by
	// SubscriptionManager.getOrCreateConn before the first subscription.
	networkLabel string
	// architecture is the URL's, for error bodies built before the request
	// is bound to its network (as on the HTTP path).
	architecture common.NetworkArchitecture
	httpReq      *http.Request // the upgrade request, for auth and headers
	// costFrames: every text message starts with a cost header block (see
	// writeMessage); set at upgrade from headerCostFrames and costHeaders.
	costFrames bool
	// notifyCostBlocks caches each subscription kind's notification block.
	notifyCostBlocks [indexer.KindPendingTx + 1]atomic.Pointer[[]byte]

	// Write synchronization (gorilla/websocket requires synchronized writes)
	writeMu sync.Mutex

	// sem bounds concurrent requests; inflight lets teardown wait for them.
	sem      chan struct{}
	inflight sync.WaitGroup

	// stop records how the connection should end and stops the read loop;
	// teardown (run by the read loop's goroutine) carries it out.
	stopOnce    sync.Once
	stopped     chan struct{}
	closeCode   int
	closeReason string
	closeGrace  time.Duration
	done        chan struct{}

	closed     atomic.Bool // no more writes once set
	peerClosed atomic.Bool // the peer's close frame was already answered

	// subsClosed is guarded by SubscriptionManager.connMu.
	subsClosed bool

	lastDropLogAt atomic.Int64
}

// handleWebSocket upgrades an HTTP connection to WebSocket and runs the
// read/write loops for the lifetime of the connection.
func (s *HttpServer) handleWebSocket(
	w http.ResponseWriter,
	r *http.Request,
	lg *zerolog.Logger,
	project *PreparedProject,
	architecture string,
	chainId string,
) {
	wsCfg := s.serverCfg.WebSocket

	// Checked before the upgrade so a refused client gets a plain 429.
	clientIP := s.resolveRealClientIP(r)
	if reason, ok := s.wsConnLimiter.acquire(clientIP); !ok {
		lg.Warn().Str("reason", reason).Msg("websocket connection refused: connection limit reached")
		telemetry.CounterHandle(telemetry.MetricWebsocketRejectedTotal, project.Config.Id, reason).Inc()
		http.Error(w, "too many websocket connections", http.StatusTooManyRequests)
		return
	}
	defer s.wsConnLimiter.release(clientIP)

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

	// Not derived from the app context: shutdown ends connections via stop()
	// so in-flight requests still get their response.
	connCtx, connCancel := context.WithCancel(context.Background())
	wsc := &WsConnection{
		id:           fmt.Sprintf("ws-%d", atomic.AddInt64(&wsConnCounter, 1)),
		conn:         wsConn,
		ctx:          connCtx,
		cancel:       connCancel,
		logger:       lg,
		server:       s,
		project:      project,
		networkId:    fmt.Sprintf("%s:%s", architecture, chainId),
		architecture: common.NetworkArchitecture(architecture),
		httpReq:      r,
		costFrames:   s.costHeadersEnabled() && r.Header.Get(headerCostFrames) == "1",
		sem:          make(chan struct{}, wsCfg.MaxConcurrentRequestsPerConnection),
		stopped:      make(chan struct{}),
		done:         make(chan struct{}),
	}

	lg.Info().Str("connId", wsc.id).Str("remoteAddr", r.RemoteAddr).Msg("websocket connection established")

	// Store before checking draining so shutdownWebSockets either sees this
	// connection or we see its flag.
	s.activeWsConns.Store(wsc.id, wsc)
	telemetry.GaugeHandle(telemetry.MetricWebsocketConnectionsActive, project.Config.Id).Inc()
	if s.draining.Load() {
		wsc.stop(websocket.CloseGoingAway, "server shutting down", 0)
	}

	wsConn.SetReadLimit(wsCfg.MaxMessageSize)

	// A peer that sends neither a pong nor a request within pongWait is
	// dropped; the read loop arms the deadline before every read.
	pingInterval := wsCfg.PingInterval.Duration()
	pongWait := 2 * pingInterval
	wsConn.SetPongHandler(func(string) error {
		lg.Trace().Str("connId", wsc.id).Msg("websocket pong received")
		return wsConn.SetReadDeadline(time.Now().Add(pongWait))
	})

	// As gorilla's default handler (echo the close frame), plus logging the
	// peer's close code and recording that it was answered.
	wsConn.SetCloseHandler(func(code int, text string) error {
		lg.Info().
			Str("connId", wsc.id).
			Int("closeCode", code).
			Str("closeReason", text).
			Msg("websocket close frame received from peer")
		message := websocket.FormatCloseMessage(code, "")
		_ = wsConn.WriteControl(websocket.CloseMessage, message, time.Now().Add(wsWriteDeadline))
		wsc.peerClosed.Store(true)
		return nil
	})

	go wsc.pingLoop(pingInterval)

	wsc.readLoop(pongWait)
	wsc.teardown()
}

// isWebSocketUpgradeRequest reports whether r is a WebSocket opening
// handshake (RFC 6455 section 4.1) that this server serves. Every
// upgrade-specific code path uses this one check, so with WebSocket off, or
// for a request that merely resembles an upgrade, it is ordinary HTTP
// everywhere.
func isWebSocketUpgradeRequest(r *http.Request, wsEnabled bool) bool {
	return wsEnabled && r.Method == http.MethodGet && websocket.IsWebSocketUpgrade(r)
}

func (s *HttpServer) webSocketEnabled() bool {
	return s.serverCfg != nil && s.serverCfg.WebSocket != nil &&
		s.serverCfg.WebSocket.Enabled != nil && *s.serverCfg.WebSocket.Enabled
}

// checkWsOrigin checks the upgrade request's origin against the project's
// CORS allowed origins; without a CORS config every origin is allowed.
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

func (wsc *WsConnection) readLoop(pongWait time.Duration) {
	for {
		// Backpressure: don't read the next request until a slot is free.
		select {
		case wsc.sem <- struct{}{}:
		case <-wsc.stopped:
			return
		}

		_ = wsc.conn.SetReadDeadline(time.Now().Add(pongWait))
		select {
		case <-wsc.stopped:
			// stop() may have set its deadline before ours.
			<-wsc.sem
			return
		default:
		}

		_, message, err := wsc.conn.ReadMessage()
		if err != nil {
			<-wsc.sem
			select {
			case <-wsc.stopped:
				return
			default:
			}
			// The error type tells a clean close, a dropped connection and
			// an expired read deadline apart.
			ev := wsc.logger.Info().Str("connId", wsc.id).Err(err).
				Str("errType", fmt.Sprintf("%T", err))
			if ce, ok := err.(*websocket.CloseError); ok {
				ev = ev.Int("closeCode", ce.Code).Str("closeReason", ce.Text)
			}
			if websocket.IsUnexpectedCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				ev.Msg("websocket read ended: unexpected close")
			} else {
				ev.Msg("websocket read ended")
			}
			return
		}

		wsc.inflight.Add(1)
		go func() {
			defer func() {
				<-wsc.sem
				wsc.inflight.Done()
			}()
			wsc.handleMessage(message)
		}()
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
				Msg("unexpected panic in websocket message handler")
		}
	}()

	startedAt := time.Now()
	ctx, cancel := context.WithTimeoutCause(wsc.ctx, wsc.server.reqMaxTimeout, ErrHandlerTimeout)
	defer cancel()

	raw = bytes.TrimLeft(raw, " \t\r\n")
	if len(raw) > 0 && raw[0] == '[' {
		wsc.handleBatch(ctx, raw, &startedAt)
		return
	}

	out, notification := wsc.handleRequest(ctx, raw, &startedAt, false)
	if notification {
		wsc.writeMeteringOnly(wsc.costBlock(ctx, []interface{}{out}))
		if r, ok := out.(*common.NormalizedResponse); ok {
			go r.Release()
		}
		return
	}
	switch v := out.(type) {
	case *common.NormalizedResponse:
		wsc.writeNormalizedResponse(v)
	default:
		if err := wsc.writeJSON(wsc.costBlock(ctx, []interface{}{v}), v); err != nil {
			wsc.logger.Debug().Err(err).Str("connId", wsc.id).Msg("failed to write error response")
		}
	}
}

// handleRequest runs one JSON-RPC request through the same steps as the
// HTTP handler and returns the response or error body to write.
// Subscription methods are rejected in batches. A panic becomes an error
// response carrying the request's id, since WebSocket clients match replies
// by id.
func (wsc *WsConnection) handleRequest(ctx context.Context, raw []byte, startedAt *time.Time, inBatch bool) (out interface{}, notification bool) {
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
			// A fresh request: the panic may have left nq locked.
			out = processErrorBody(wsc.logger, startedAt, common.NewNormalizedRequest(raw),
				fmt.Errorf("unexpected server panic: %v", rec), wsc.server.serverCfg.IncludeErrorDetails, wsc.architecture)
		}
	}()

	nq := common.NewNormalizedRequest(raw)
	nq.ForwardHeaders = make(http.Header)
	requestCtx := common.StartRequestSpan(ctx, nq)
	nq.SetClientIP(wsc.server.resolveRealClientIP(wsc.httpReq))

	fail := func(err error, includeDetails *bool) interface{} {
		common.EndRequestSpan(requestCtx, nil, err)
		return processErrorBody(wsc.logger, startedAt, nq, err, includeDetails, wsc.architecture)
	}
	unsupported := func(message string) interface{} {
		common.EndRequestSpan(requestCtx, nil, nil)
		return wsc.buildUnsupportedMethodResponse(nq, message)
	}

	if err := nq.Validate(); err != nil {
		return fail(err, &common.TRUE), notification
	}
	// A valid request without an id is a notification: it runs, but gets no
	// reply (JSON-RPC 2.0 §4.1). Invalid requests are still answered.
	if jrr, err := nq.JsonRpcRequest(); err == nil {
		notification = jrr.IsNotification()
	}

	project := wsc.project
	headers := wsc.httpReq.Header
	queryArgs := wsc.httpReq.URL.Query()
	if err := applyForwardHeaders(project, nq, headers); err != nil {
		return fail(err, &common.TRUE), notification
	}

	method, _ := nq.Method()
	allowed, err := isMethodAllowed(project, method)
	if err != nil {
		return fail(err, &common.TRUE), notification
	}
	if !allowed {
		return unsupported(fmt.Sprintf("method not supported: %s", method)), notification
	}
	if inBatch && IsSubscriptionMethod(method) {
		return unsupported("subscription methods (eth_subscribe, eth_unsubscribe) are not supported in batch requests"), notification
	}

	ap, err := auth.NewPayloadFromHttp(method, wsc.httpReq.RemoteAddr, headers, queryArgs)
	if err != nil {
		return fail(err, &common.TRUE), notification
	}
	user, err := project.AuthenticateConsumer(requestCtx, nq, method, ap)
	if err != nil {
		auth.RecordAuthFailed(project.Config.Id, err)
		return fail(err, wsc.server.serverCfg.IncludeErrorDetails), notification
	}
	nq.SetUser(user)
	if project.Config.TrustUserIdHeader && nq.User() == nil {
		nq.SetUserFromTrustedHeader(headers.Get(common.HeaderUserId))
	}

	nw, err := project.GetNetwork(requestCtx, wsc.networkId)
	if err != nil {
		return fail(err, wsc.server.serverCfg.IncludeErrorDetails), notification
	}
	nq.SetNetwork(nw)
	applyRequestDirectives(project, nw, nq, headers, queryArgs)

	var resp *common.NormalizedResponse
	switch {
	case IsSubscribeMethod(method):
		resp, err = wsc.server.subscriptionManager.Subscribe(requestCtx, wsc, nq, project, wsc.networkId)
	case IsSubscriptionMethod(method):
		resp, err = wsc.server.subscriptionManager.Unsubscribe(requestCtx, wsc, nq, project, wsc.networkId)
	default:
		resp, err = project.Forward(requestCtx, wsc.networkId, nq)
	}
	if err != nil {
		if resp != nil {
			go resp.Release()
		}
		return fail(err, wsc.server.serverCfg.IncludeErrorDetails), notification
	}

	common.EndRequestSpan(requestCtx, resp, nil)
	return resp, notification
}

func (wsc *WsConnection) handleBatch(ctx context.Context, raw []byte, startedAt *time.Time) {
	var requests []json.RawMessage
	if err := common.SonicCfg.Unmarshal(raw, &requests); err != nil {
		wsc.writeError(int(common.JsonRpcErrorParseException), "parse error")
		return
	}
	// JSON-RPC 2.0 section 6: an empty batch gets a single Invalid Request error.
	if len(requests) == 0 {
		wsc.writeError(int(common.JsonRpcErrorClientSideException), "invalid request: empty batch")
		return
	}

	responses := make([]interface{}, len(requests))
	notifications := make([]bool, len(requests))
	var wg sync.WaitGroup

	for i, reqBody := range requests {
		wg.Add(1)
		go func(index int, reqRaw json.RawMessage) {
			defer wg.Done()
			responses[index], notifications[index] = wsc.handleRequest(ctx, reqRaw, startedAt, true)
		}(i, reqBody)
	}

	wg.Wait()

	// Notifications get no entry; a batch of only notifications gets no reply.
	replies := make([]interface{}, 0, len(responses))
	for i, resp := range responses {
		if !notifications[i] {
			replies = append(replies, resp)
		}
	}
	// Costs cover notifications too, as on HTTP: they ran.
	costs := wsc.costBlock(ctx, responses)
	if len(replies) > 0 {
		wsc.writeBatchResponse(costs, replies)
	} else {
		wsc.writeMeteringOnly(costs)
	}

	for _, resp := range responses {
		if r, ok := resp.(*common.NormalizedResponse); ok {
			go r.Release()
		}
	}
}

func (wsc *WsConnection) buildUnsupportedMethodResponse(nq *common.NormalizedRequest, message string) *HttpJsonRpcErrorResponse {
	jsonrpcVersion := "2.0"
	var reqId interface{}
	if jrr, err := nq.JsonRpcRequest(); err == nil {
		jsonrpcVersion = jrr.JSONRPC
		reqId = jrr.ResponseID(isSvmRequest(nq))
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

// wsWriteDeadline bounds every write, so a stalled client cannot hold
// writeMu and starve the connection's other writers.
const wsWriteDeadline = 10 * time.Second

var errWsConnClosed = errors.New("connection closed")

// headerCostFrames on the upgrade request opts the connection into cost
// frames, when server.costHeaders is also on.
const headerCostFrames = "X-ERPC-Cost-Frames"

var crlf = []byte("\r\n")

// costBlock is the cost frame header block for a response to items: the
// HTTP cost header set in HTTP/1.1 wire form, without the closing blank
// line. nil when cost frames are off or nothing was routed.
func (wsc *WsConnection) costBlock(ctx context.Context, items []interface{}) []byte {
	if !wsc.costFrames {
		return nil
	}
	h := wsc.server.costHeaders(ctx, items)
	if len(h) == 0 {
		return nil
	}
	var b bytes.Buffer
	_ = h.Write(&b)
	return b.Bytes()
}

// notificationCostBlock is the cost frame header block of one subscription
// notification: one billable eth_subscription call on the connection's
// network. sub.NetworkID is the indexer's project-scoped key, not the
// network id the response headers carry, so the connection's is used.
func (wsc *WsConnection) notificationCostBlock(sub wsclient.Subscription) []byte {
	if !wsc.costFrames {
		return nil
	}
	kind := int(sub.Kind)
	cacheable := kind >= 0 && kind < len(wsc.notifyCostBlocks)
	if cacheable {
		if b := wsc.notifyCostBlocks[kind].Load(); b != nil {
			return *b
		}
	}
	h := http.Header{}
	h.Set("X-ERPC-Calls", "1")
	h.Set("X-ERPC-Billable", "1")
	h.Set("X-ERPC-Methods", "eth_subscription")
	h.Set("X-ERPC-Subscription", sub.Kind.String())
	h.Set("X-ERPC-Network-Id", wsc.networkId)
	nw, err := wsc.project.GetNetwork(wsc.ctx, wsc.networkId)
	if err == nil {
		h.Set("X-ERPC-Network-Alias", nw.Label())
	}
	var buf bytes.Buffer
	_ = h.Write(&buf)
	b := buf.Bytes()
	// Retry the alias next time if the network lookup failed.
	if cacheable && err == nil {
		wsc.notifyCostBlocks[kind].Store(&b)
	}
	return b
}

// writeMessage writes one text message produced by write. With cost frames
// on, the message starts with costs (a header block, possibly empty) and the
// blank line ending it, so the block and its body are one message.
func (wsc *WsConnection) writeMessage(costs []byte, write func(w io.Writer) error) error {
	wsc.writeMu.Lock()
	defer wsc.writeMu.Unlock()

	if wsc.closed.Load() {
		return errWsConnClosed
	}
	if err := wsc.conn.SetWriteDeadline(time.Now().Add(wsWriteDeadline)); err != nil {
		return err
	}
	defer wsc.conn.SetWriteDeadline(time.Time{})

	w, err := wsc.conn.NextWriter(websocket.TextMessage)
	if err != nil {
		return err
	}
	if wsc.costFrames {
		if _, err = w.Write(costs); err == nil {
			_, err = w.Write(crlf)
		}
	}
	if err == nil {
		err = write(w)
	}
	if closeErr := w.Close(); err == nil {
		err = closeErr
	}
	return err
}

func (wsc *WsConnection) writeJSON(costs []byte, v interface{}) error {
	return wsc.writeMessage(costs, func(w io.Writer) error {
		return json.NewEncoder(w).Encode(v)
	})
}

// writeError writes an error for a message that routed no call.
func (wsc *WsConnection) writeError(code int, message string) {
	_ = wsc.writeJSON(nil, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      nil,
		"error": map[string]interface{}{
			"code":    code,
			"message": message,
		},
	})
}

func (wsc *WsConnection) writeNormalizedResponse(resp *common.NormalizedResponse) {
	// Release even when the write never starts (connection closed or broken).
	defer func() { go resp.Release() }()
	err := wsc.writeMessage(wsc.costBlock(wsc.ctx, []interface{}{resp}), func(w io.Writer) error {
		_, err := resp.WriteTo(w)
		return err
	})
	if err != nil {
		wsc.logger.Debug().Err(err).Str("connId", wsc.id).Msg("failed to write websocket response")
	}
}

func (wsc *WsConnection) writeBatchResponse(costs []byte, responses []interface{}) {
	_ = wsc.writeMessage(costs, func(w io.Writer) error {
		_, err := NewBatchResponseWriter(responses).WriteTo(w)
		return err
	})
}

// writeMeteringOnly writes a cost block with an empty body for calls that
// ran but get no reply (id-less notifications), so a metering proxy still
// sees them. Nothing is written without cost frames or routed items.
func (wsc *WsConnection) writeMeteringOnly(costs []byte) {
	if len(costs) == 0 {
		return
	}
	_ = wsc.writeMessage(costs, func(io.Writer) error { return nil })
}

func (wsc *WsConnection) pingLoop(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// WriteControl is safe alongside other writers, so pings don't
			// wait behind a slow data write holding writeMu.
			err := wsc.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(wsWriteDeadline))
			wsc.logger.Trace().Str("connId", wsc.id).Msg("websocket ping sent")
			if err != nil {
				wsc.logger.Info().Err(err).Str("connId", wsc.id).
					Str("errType", fmt.Sprintf("%T", err)).
					Msg("websocket ping failed, closing connection")
				wsc.stop(websocket.CloseGoingAway, "", 0)
				return
			}
		case <-wsc.stopped:
			return
		}
	}
}

// stop ends the connection with the given close code: it stops reading new
// requests and lets teardown finish. Requests already in flight get up to
// grace to complete. Only the first call counts; safe from any goroutine.
func (wsc *WsConnection) stop(code int, reason string, grace time.Duration) {
	wsc.stopOnce.Do(func() {
		wsc.closeCode, wsc.closeReason, wsc.closeGrace = code, reason, grace
		close(wsc.stopped)
		_ = wsc.conn.SetReadDeadline(time.Now())
	})
}

// teardown runs on the read loop's goroutine once it exits, so no request
// can start while it waits for in-flight ones.
func (wsc *WsConnection) teardown() {
	// Server-initiated unless stop() said otherwise; never 1000, which
	// would tell the client nothing went wrong.
	wsc.stop(websocket.CloseGoingAway, "", 0)

	if wsc.closeGrace > 0 {
		wsc.waitInflight(wsc.closeGrace)
	}
	// Cancelled requests still write their JSON-RPC error before the close.
	wsc.cancel()
	wsc.waitInflight(wsWriteDeadline)
	wsc.closed.Store(true)

	wsc.server.subscriptionManager.CleanupConnection(wsc)

	if !wsc.peerClosed.Load() {
		_ = wsc.conn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(wsc.closeCode, wsc.closeReason),
			time.Now().Add(unsubscribeTimeout),
		)
	}
	_ = wsc.conn.Close()
	wsc.server.activeWsConns.Delete(wsc.id)
	telemetry.GaugeHandle(telemetry.MetricWebsocketConnectionsActive, wsc.project.Config.Id).Dec()
	close(wsc.done)

	wsc.logger.Info().Str("connId", wsc.id).Int("closeCode", wsc.closeCode).Msg("websocket connection closed")
}

func (wsc *WsConnection) waitInflight(timeout time.Duration) {
	idle := make(chan struct{})
	go func() {
		wsc.inflight.Wait()
		close(idle)
	}()
	select {
	case <-idle:
	case <-time.After(timeout):
	}
}

func (wsc *WsConnection) WriteSubscriptionNotification(sub wsclient.Subscription, result json.RawMessage, queued int) error {
	labels := []string{wsc.project.Config.Id, wsc.networkLabel, sub.Kind.String()}
	telemetry.ObserverHandle(telemetry.MetricWebsocketSendQueueLength, labels...).Observe(float64(queued))

	notification := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "eth_subscription",
		"params": map[string]interface{}{
			"subscription": sub.ClientSubID,
			"result":       result,
		},
	}
	body := countingWriter{}
	err := wsc.writeMessage(wsc.notificationCostBlock(sub), func(w io.Writer) error {
		body.w = w
		return json.NewEncoder(&body).Encode(notification)
	})
	if err != nil {
		return err
	}
	telemetry.CounterHandle(telemetry.MetricWebsocketNotificationsSentTotal, labels...).Inc()
	telemetry.CounterHandle(telemetry.MetricWebsocketNotificationBytesTotal, labels...).Add(float64(body.n))
	return nil
}

type countingWriter struct {
	w io.Writer
	n int
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += n
	return n, err
}

// NotificationDropped records a notification lost to a full subscription
// buffer. Losing one that is not lossy closes the connection with 1013 so
// the client reconnects knowing it missed data instead of silently
// diverging.
func (wsc *WsConnection) NotificationDropped(sub wsclient.Subscription, lossy bool) {
	telemetry.CounterHandle(telemetry.MetricWebsocketSubscriptionNotificationsDroppedTotal,
		wsc.project.Config.Id, wsc.networkLabel, sub.Kind.String(),
	).Inc()

	now := time.Now().UnixNano()
	if last := wsc.lastDropLogAt.Load(); now-last > int64(10*time.Second) && wsc.lastDropLogAt.CompareAndSwap(last, now) {
		wsc.logger.Warn().Str("connId", wsc.id).Str("clientSubId", sub.ClientSubID).
			Str("kind", sub.Kind.String()).Bool("closingConnection", !lossy).
			Msg("client is not keeping up; subscription notifications dropped")
	}

	if !lossy {
		wsc.stop(websocket.CloseTryAgainLater, "subscription buffer overflow: notifications were dropped", 0)
	}
}
