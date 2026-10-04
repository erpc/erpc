package erpc

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/erpc/erpc/auth"
	"github.com/erpc/erpc/blockstore"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
)

// WebSocket subscription endpoint.
//
// Served on the same paths as HTTP (/<project>/<architecture>/<chainId>, or
// aliased) when the client sends an Upgrade request and server.webSocket is
// enabled. The upgrade is rejected (HTTP 503) unless the network has a
// Redis-backed head cache: notifications are rendered only from cached
// canonical block/log events and never trigger upstream calls.
//
// Contract (subscriptions only):
//   - eth_subscribe("newHeads") and eth_subscribe("logs", {address, topics}).
//     Reorgs emit orphaned logs with "removed":true (highest block first)
//     followed by the new canonical logs. The subscription id reply always
//     precedes its notifications.
//   - eth_unsubscribe only sees ids created on the same connection.
//   - Any other method gets -32601 and batch frames get -32600; ordinary
//     JSON-RPC calls belong on HTTP.
//
// The logs filter accepts only address and topics; any other member
// (blockHash, fromBlock, toBlock, unknown) is -32602 before registration.
// A panic in a connection goroutine is recovered, counted in
// erpc_unexpected_panic_total (scope ws-write/ws-ping/ws-pump/ws-message) and
// terminates the affected connection ("internal_error" close reason).
//
// Frames are handled sequentially per connection. Bounds:
// server.webSocket.maxConnections / maxConnectionsPerProject (HTTP 503 at
// upgrade), maxSubscriptionsPerConnection, maxMessageBytes (1009 close),
// sendQueueSize (a client that falls this far behind, or whose head cache
// subscription overflows or skips heights, is closed with 1008),
// writeTimeout per frame. Credentials are re-checked every pingInterval.
// Connections close with 1001 when the server shuts down.

const (
	wsCloseSlowConsumer = "slow consumer"
	// Fallback for configs that bypassed SetDefaults. Ping never becomes
	// disabled: re-auth rides on it.
	wsDefaultPingInterval = 30 * time.Second
)

func (ws *wsServer) pingInterval() time.Duration {
	if d := ws.cfg.PingInterval.Duration(); d > 0 {
		return d
	}
	return wsDefaultPingInterval
}

type wsServer struct {
	s     *HttpServer
	cfg   *common.WebSocketServerConfig
	conns atomic.Int64

	mu         sync.Mutex
	perProject map[string]int
}

func newWsServer(s *HttpServer, cfg *common.WebSocketServerConfig) *wsServer {
	if cfg == nil || !cfg.Enabled {
		return nil
	}
	return &wsServer{s: s, cfg: cfg, perProject: map[string]int{}}
}

func isWebSocketUpgrade(r *http.Request) bool {
	return r.Method == http.MethodGet &&
		strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}

// wrap routes upgrade requests to the WS server before the timeout/gzip
// wrappers (which cannot hijack). All other requests are untouched.
func (ws *wsServer) wrap(next http.Handler) http.Handler {
	if ws == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isWebSocketUpgrade(r) {
			ws.serve(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func wsHttpError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	b, _ := json.Marshal(msg)
	_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","error":{"code":-32600,"message":%s}}`, b)
}

func (ws *wsServer) serve(w http.ResponseWriter, r *http.Request) {
	s := ws.s
	traceCtx, serverSpan := common.StartHTTPServerSpan(r.Context(), r)
	defer serverSpan.End()
	reject := func(status int, msg string) {
		common.SetTraceSpanError(serverSpan, errors.New(msg))
		wsHttpError(w, status, msg)
	}
	defer func() {
		if rec := recover(); rec != nil {
			telemetry.MetricUnexpectedPanicTotal.WithLabelValues("ws-handler", "", common.ErrorFingerprint(rec)).Inc()
			s.logger.Error().Interface("panic", rec).Str("stack", string(debug.Stack())).Msg("unexpected panic in websocket handler")
		}
	}()
	if s.draining != nil && s.draining.Load() {
		reject(http.StatusServiceUnavailable, "server is shutting down")
		return
	}

	projectId, architecture, chainId := ws.resolveAlias(r)
	projectId, architecture, chainId, isAdmin, isHealth, err := s.parseUrlPath(r, projectId, architecture, chainId)
	// parseUrlPath classifies every non-POST project path as a healthcheck;
	// only an explicit /healthcheck suffix is one for upgrades.
	if isHealth && !strings.HasSuffix(strings.TrimRight(r.URL.Path, "/"), "/healthcheck") {
		isHealth = false
	}
	if err != nil || isAdmin || isHealth || projectId == "" || architecture == "" || chainId == "" {
		reject(http.StatusBadRequest, "websocket requires /<project>/<architecture>/<chainId>")
		return
	}
	project, err := s.erpc.GetProject(projectId)
	if err != nil || project == nil {
		reject(http.StatusNotFound, "project not found")
		return
	}
	networkId := architecture + ":" + chainId

	// Origin, credentials and connection caps are all checked before the
	// network is resolved: GetNetwork may lazily create and hydrate a network
	// (upstream calls), which unauthenticated clients must never trigger.

	// Browsers do not apply CORS to WebSockets, so the project's allowed
	// origins are enforced here. Without a CORS config any origin is accepted,
	// matching HTTP behavior (eRPC does not rely on cookies).
	if origin := r.Header.Get("Origin"); origin != "" && project.Config.CORS != nil {
		allowed := false
		for _, o := range project.Config.CORS.AllowedOrigins {
			if ok, _ := common.WildcardMatch(o, origin); ok {
				allowed = true
				break
			}
		}
		if !allowed {
			reject(http.StatusForbidden, "origin not allowed")
			return
		}
	}

	// Credentials are checked at upgrade so unauthenticated clients never get
	// a connection. Each message is authenticated again with its own method.
	if _, err := ws.authenticate(traceCtx, project, r, "eth_subscribe", nil); err != nil {
		reject(http.StatusUnauthorized, "unauthorized")
		return
	}

	if n := ws.conns.Add(1); n > int64(ws.cfg.MaxConnections) {
		ws.conns.Add(-1)
		reject(http.StatusServiceUnavailable, "too many websocket connections")
		return
	}
	defer ws.conns.Add(-1)
	if !ws.acquireProject(projectId) {
		reject(http.StatusServiceUnavailable, "too many websocket connections for this project")
		return
	}
	defer ws.releaseProject(projectId)

	nw, err := project.GetNetwork(traceCtx, networkId)
	if err != nil {
		reject(http.StatusNotFound, "network not found")
		return
	}
	serverSpan.SetAttributes(attribute.String("project.id", projectId), attribute.String("network.id", networkId))
	if nw.BlockStore() == nil {
		reject(http.StatusServiceUnavailable, "websocket subscriptions require evm.blockStore for this network")
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Origin already enforced above against project CORS.
		InsecureSkipVerify: true,
	})
	if err != nil {
		common.SetTraceSpanError(serverSpan, err)
		return
	}
	conn.SetReadLimit(ws.cfg.MaxMessageBytes)
	telemetry.MetricWebSocketConnections.WithLabelValues(projectId, networkId).Inc()
	closeReason := "other"
	defer func() {
		serverSpan.SetAttributes(attribute.String("websocket.close_reason", closeReason))
		telemetry.MetricWebSocketConnections.WithLabelValues(projectId, networkId).Dec()
		telemetry.MetricWebSocketClosuresTotal.WithLabelValues(projectId, networkId, closeReason).Inc()
	}()

	lg := s.logger.With().Str("component", "ws").Str("projectId", projectId).Str("networkId", networkId).Logger()
	c := &wsConn{
		ws:        ws,
		conn:      conn,
		project:   project,
		network:   nw,
		networkId: networkId,
		req:       r,
		clientIP:  s.resolveRealClientIP(r),
		lg:        &lg,
		out:       make(chan []byte, ws.cfg.SendQueueSize),
		subs:      map[string]*wsSub{},
	}
	closeReason = c.run()
}

func (ws *wsServer) acquireProject(id string) bool {
	if ws.cfg.MaxConnectionsPerProject <= 0 {
		return true
	}
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if ws.perProject[id] >= ws.cfg.MaxConnectionsPerProject {
		return false
	}
	ws.perProject[id]++
	return true
}

func (ws *wsServer) releaseProject(id string) {
	if ws.cfg.MaxConnectionsPerProject <= 0 {
		return
	}
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if ws.perProject[id]--; ws.perProject[id] <= 0 {
		delete(ws.perProject, id)
	}
}

func (ws *wsServer) resolveAlias(r *http.Request) (string, string, string) {
	cfg := ws.s.serverCfg
	if cfg == nil || cfg.Aliasing == nil {
		return "", "", ""
	}
	host := r.Host
	if i := strings.Index(host, ":"); i != -1 {
		host = host[:i]
	}
	for _, rule := range cfg.Aliasing.Rules {
		if ok, err := common.WildcardMatch(rule.MatchDomain, host); err == nil && ok {
			return rule.ServeProject, rule.ServeArchitecture, rule.ServeChain
		}
	}
	return "", "", ""
}

func (ws *wsServer) authenticate(ctx context.Context, project *PreparedProject, r *http.Request, method string, nq *common.NormalizedRequest) (*common.User, error) {
	ap, err := auth.NewPayloadFromHttp(method, r.RemoteAddr, r.Header, r.URL.Query())
	if err != nil {
		return nil, err
	}
	if nq == nil {
		nq = common.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":0,"method":%q,"params":[]}`, method)))
		nq.SetClientIP(ws.s.resolveRealClientIP(r))
	}
	return project.AuthenticateConsumer(ctx, nq, method, ap)
}

type wsSub struct {
	id     string
	sub    *blockstore.Subscription
	logs   bool
	kind   string
	filter *blockstore.LogFilter
	// unsubscribed distinguishes client eth_unsubscribe from overflow closes.
	unsubscribed atomic.Bool
	// last is the highest block number emitted (0 = nothing yet); used to
	// detect discontinuities so clients never see a silent gap.
	last int64
}

type wsConn struct {
	ws        *wsServer
	conn      *websocket.Conn
	project   *PreparedProject
	network   *Network
	networkId string
	req       *http.Request
	clientIP  string
	lg        *zerolog.Logger
	ctx       context.Context
	cancel    context.CancelCauseFunc
	out       chan []byte

	mu   sync.Mutex
	subs map[string]*wsSub
	wg   sync.WaitGroup
}

type wsCloseErr struct {
	code   websocket.StatusCode
	reason string
}

func (e *wsCloseErr) Error() string { return e.reason }

func (c *wsConn) closeWith(code websocket.StatusCode, reason string) {
	c.cancel(&wsCloseErr{code: code, reason: reason})
}

func (c *wsConn) run() string {
	// Tied to the application context, not the request: hijacked
	// connections are not tracked by http.Server.Shutdown.
	c.ctx, c.cancel = context.WithCancelCause(c.ws.s.appCtx)
	defer c.cleanup()

	writerDone := make(chan struct{})
	go c.writeLoop(writerDone)
	go c.pingLoop()

	for {
		// Not c.ctx: coder/websocket tears the connection down without a
		// close frame when a Read context is cancelled, which would hide
		// the 1008/1001 status from the client. writeLoop's Close/CloseNow
		// unblocks this Read instead.
		_, data, err := c.conn.Read(context.Background())
		if err != nil {
			if websocket.CloseStatus(err) == websocket.StatusMessageTooBig {
				c.closeWith(websocket.StatusMessageTooBig, "message too big")
			} else {
				c.cancel(err)
			}
			break
		}
		c.safeHandle(data)
	}
	<-writerDone
	return websocketCloseReason(context.Cause(c.ctx), c.ws.s.appCtx)
}

func websocketCloseReason(cause error, appCtx context.Context) string {
	if appCtx != nil && appCtx.Err() != nil {
		return "shutdown"
	}
	var closeErr *wsCloseErr
	if errors.As(cause, &closeErr) {
		switch closeErr.reason {
		case "unauthorized":
			return "unauthorized"
		case "write timeout":
			return "write_timeout"
		case "ping timeout":
			return "ping_timeout"
		case wsCloseSlowConsumer:
			return "slow_consumer"
		case "subscription gap: resubscribe":
			return "gap"
		case "subscription closed: " + wsCloseSlowConsumer + " or head cache stopped":
			return "subscription_closed"
		case "message too big":
			return "message_too_big"
		case "failed to render subscription event", "internal error":
			return "internal_error"
		}
		switch closeErr.code {
		case websocket.StatusGoingAway:
			return "shutdown"
		case websocket.StatusMessageTooBig:
			return "message_too_big"
		case websocket.StatusInternalError:
			return "internal_error"
		}
		return "other"
	}
	if cause != nil && websocket.CloseStatus(cause) >= 0 {
		return "peer"
	}
	return "other"
}

func (c *wsConn) safeHandle(data []byte) {
	defer func() {
		if rec := recover(); rec != nil {
			c.recovered("ws-message", rec)
		}
	}()
	c.handleMessage(data)
}

// recovered records a panic caught in one of the connection's goroutines and
// marks the connection for internal-error closure. The caller must have called recover().
func (c *wsConn) recovered(scope string, rec interface{}) {
	telemetry.MetricUnexpectedPanicTotal.WithLabelValues(scope, c.networkId, common.ErrorFingerprint(rec)).Inc()
	c.lg.Error().Interface("panic", rec).Str("stack", string(debug.Stack())).Str("scope", scope).Msg("unexpected panic in websocket connection")
	c.closeWith(websocket.StatusInternalError, "internal error")
}

func (c *wsConn) cleanup() {
	c.cancel(nil)
	c.mu.Lock()
	for id, s := range c.subs {
		s.unsubscribed.Store(true)
		s.sub.Close()
		delete(c.subs, id)
	}
	c.mu.Unlock()
	c.wg.Wait()
}

func (c *wsConn) writeLoop(done chan struct{}) {
	defer close(done)
	defer func() {
		if rec := recover(); rec != nil {
			c.recovered("ws-write", rec)
			// No writer is left to close the connection, and run() is blocked
			// in Read until it is closed.
			_ = c.conn.CloseNow()
		}
	}()
	wt := c.ws.cfg.WriteTimeout.Duration()
	for {
		select {
		case <-c.ctx.Done():
			code, reason := websocket.StatusNormalClosure, ""
			var ce *wsCloseErr
			cause := context.Cause(c.ctx)
			switch {
			case errors.As(cause, &ce):
				code, reason = ce.code, ce.reason
			case websocket.CloseStatus(cause) != -1:
				// Peer closed; the library echoes the close frame.
				_ = c.conn.CloseNow()
				return
			case c.ws.s.appCtx.Err() != nil:
				code, reason = websocket.StatusGoingAway, "server shutting down"
			default:
				// Read failed without a close frame (dropped TCP etc).
			}
			go func() {
				time.Sleep(wt)
				_ = c.conn.CloseNow()
			}()
			_ = c.conn.Close(code, reason)
			return
		case msg := <-c.out:
			wctx, cancel := context.WithTimeout(c.ctx, wt)
			err := c.conn.Write(wctx, websocket.MessageText, msg)
			timedOut := errors.Is(wctx.Err(), context.DeadlineExceeded)
			cancel()
			if err != nil {
				if timedOut {
					// A genuinely slow consumer: tell it why (1008).
					c.closeWith(websocket.StatusPolicyViolation, "write timeout")
				} else {
					// Peer reset/close or another transport error: ordinary
					// churn, not a slow consumer, so it is not labeled write_timeout.
					c.cancel(err)
				}
			}
		}
	}
}

// pingLoop detects dead peers (which would otherwise hold a connection slot
// until TCP gives up) and keeps idle connections alive behind LB idle
// timeouts. Pong handling requires the concurrent reader in run().
func (c *wsConn) pingLoop() {
	defer func() {
		if rec := recover(); rec != nil {
			c.recovered("ws-ping", rec)
		}
	}()
	t := time.NewTicker(c.ws.pingInterval())
	defer t.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-t.C:
			// Ping waits for the reader to consume a pong; frame handling can
			// occupy that reader longer than an outbound write timeout.
			pctx, cancel := context.WithTimeout(c.ctx, max(c.ws.cfg.WriteTimeout.Duration(), c.ws.pingInterval()))
			err := c.conn.Ping(pctx)
			cancel()
			if err != nil && c.ctx.Err() == nil {
				c.closeWith(websocket.StatusPolicyViolation, "ping timeout")
				return
			}
			// Re-check the upgrade credentials so revoked keys and expired
			// JWTs stop streaming within one ping interval.
			// A rate-limit rejection means the credentials were valid (the
			// limit is applied after a strategy succeeds), so it must not
			// close a healthy stream.
			if _, err := c.ws.authenticate(c.ctx, c.project, c.req, "eth_subscribe", nil); err != nil && c.ctx.Err() == nil &&
				!common.HasErrorCode(err, common.ErrCodeAuthRateLimitRuleExceeded) {
				c.closeWith(websocket.StatusPolicyViolation, "unauthorized")
				return
			}
		}
	}
}

// sendStream enqueues a subscription notification, waiting up to
// WriteTimeout for queue space. Backpressure thus lands on the head cache's
// per-subscription queue (which closes on overflow) instead of dropping a
// client that is keeping up with a large block.
func (c *wsConn) sendStream(msg []byte) bool {
	select {
	case c.out <- msg:
		return true
	case <-c.ctx.Done():
		return false
	default:
	}
	t := time.NewTimer(c.ws.cfg.WriteTimeout.Duration())
	defer t.Stop()
	select {
	case c.out <- msg:
		return true
	case <-c.ctx.Done():
		return false
	case <-t.C:
		c.closeWith(websocket.StatusPolicyViolation, wsCloseSlowConsumer)
		return false
	}
}

// send enqueues an RPC reply without blocking. A full queue disconnects the client.
func (c *wsConn) send(msg []byte) bool {
	if c.ctx.Err() != nil {
		return false
	}
	select {
	case c.out <- msg:
		return true
	default:
		c.closeWith(websocket.StatusPolicyViolation, wsCloseSlowConsumer)
		return false
	}
}

type wsRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func errorReply(id json.RawMessage, code int, msg string) []byte {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	b, _ := json.Marshal(msg)
	return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":{"code":%d,"message":%s}}`, id, code, b))
}

func resultReply(id json.RawMessage, result interface{}) []byte {
	rb, _ := json.Marshal(result)
	return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":%s}`, id, rb))
}

// handleMessage processes one frame. The reply is enqueued before any
// subscription pump starts so the id always precedes its notifications.
func (c *wsConn) handleMessage(data []byte) {
	reply, after := c.handleOne(bytes.TrimSpace(data))
	delivered := false
	if reply != nil {
		delivered = c.send(reply)
	}
	if after != nil {
		after(delivered)
	}
}

// handleOne serves the subscription-only contract: eth_subscribe and
// eth_unsubscribe. Batches and every other method are rejected; ordinary
// JSON-RPC calls belong on HTTP.
func (c *wsConn) handleOne(trimmed []byte) ([]byte, func(bool)) {
	if len(trimmed) > 0 && trimmed[0] == '[' {
		return errorReply(nil, int(common.JsonRpcErrorClientSideException), "batch requests are not supported over websocket"), nil
	}
	if !json.Valid(trimmed) {
		return errorReply(nil, int(common.JsonRpcErrorParseException), "parse error"), nil
	}
	var req wsRequest
	// Syntactically valid JSON that is not a request object is -32600.
	if err := json.Unmarshal(trimmed, &req); err != nil || req.Method == "" {
		return errorReply(nil, int(common.JsonRpcErrorClientSideException), "invalid request"), nil
	}
	if len(req.ID) == 0 {
		if req.JSONRPC == "2.0" {
			return nil, nil
		}
		req.ID = json.RawMessage("null")
	}
	if req.Method != "eth_subscribe" && req.Method != "eth_unsubscribe" {
		return errorReply(req.ID, int(common.JsonRpcErrorUnsupportedException), fmt.Sprintf("method not supported over websocket: %s (use HTTP)", req.Method)), nil
	}
	nq := common.NewNormalizedRequest(trimmed)
	nq.SetClientIP(c.clientIP)
	if err := c.admit(c.ctx, nq, req.Method); errors.Is(err, errWsMethodDenied) {
		return errorReply(req.ID, int(common.JsonRpcErrorUnsupportedException), "method not supported: "+req.Method), nil
	} else if err != nil {
		now := time.Now()
		return c.render(processErrorBody(c.lg, &now, nq, err, c.ws.s.serverCfg.IncludeErrorDetails, common.NetworkArchitecture(c.network.Architecture()))), nil
	}
	if req.Method == "eth_unsubscribe" {
		return c.unsubscribe(&req), nil
	}
	return c.subscribe(c.ctx, nq, &req)
}

func (c *wsConn) render(v interface{}) []byte {
	var buf bytes.Buffer
	var err error
	if r, ok := v.(*HttpJsonRpcErrorResponse); ok {
		_, err = writeJsonRpcError(&buf, r)
	} else {
		err = common.SonicCfg.NewEncoder(&buf).Encode(v)
	}
	if err != nil {
		c.lg.Warn().Err(err).Msg("failed to serialize websocket response")
		return nil
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// admit applies the HTTP gates relevant to subscriptions: validation,
// ignore/allow methods and per-message consumer auth.
func (c *wsConn) admit(ctx context.Context, nq *common.NormalizedRequest, method string) error {
	if err := nq.Validate(); err != nil {
		return err
	}
	pc := c.project.Config
	handle := true
	for _, m := range pc.IgnoreMethods {
		if ok, _ := common.WildcardMatch(m, method); ok {
			handle = false
			break
		}
	}
	for _, m := range pc.AllowMethods {
		if ok, _ := common.WildcardMatch(m, method); ok {
			handle = true
			break
		}
	}
	if !handle {
		return errWsMethodDenied
	}
	user, err := c.ws.authenticate(ctx, c.project, c.req, method, nq)
	if err != nil {
		return err
	}
	nq.SetUser(user)
	if pc.TrustUserIdHeader && nq.User() == nil {
		nq.SetUserFromTrustedHeader(c.req.Header.Get(common.HeaderUserId))
	}
	nq.SetNetwork(c.network)
	return nil
}

var errWsMethodDenied = errors.New("ws: method denied by project method lists")

func newSubscriptionId() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "0x" + hex.EncodeToString(b[:])
}

func (c *wsConn) subscribe(ctx context.Context, nq *common.NormalizedRequest, req *wsRequest) ([]byte, func(bool)) {
	var params []json.RawMessage
	if err := json.Unmarshal(req.Params, &params); err != nil || len(params) == 0 {
		return errorReply(req.ID, int(common.JsonRpcErrorInvalidArgument), "eth_subscribe requires a subscription type"), nil
	}
	var kind string
	if err := json.Unmarshal(params[0], &kind); err != nil {
		return errorReply(req.ID, int(common.JsonRpcErrorInvalidArgument), "invalid subscription type"), nil
	}
	s := &wsSub{id: newSubscriptionId(), kind: kind}
	switch kind {
	case "newHeads":
		if len(params) > 1 {
			return errorReply(req.ID, int(common.JsonRpcErrorInvalidArgument), "newHeads takes no filter"), nil
		}
	case "logs":
		s.logs = true
		obj := map[string]interface{}{}
		if len(params) > 1 {
			if err := json.Unmarshal(params[1], &obj); err != nil || obj == nil {
				return errorReply(req.ID, int(common.JsonRpcErrorInvalidArgument), "logs filter must be an object"), nil
			}
		}
		// Only address and topics are streamed; block-range and blockHash
		// members belong to eth_getLogs, and unknown members are not guessed at.
		for k := range obj {
			if k != "address" && k != "topics" {
				return errorReply(req.ID, int(common.JsonRpcErrorInvalidArgument), fmt.Sprintf("unsupported logs filter member %q (supported: address, topics)", k)), nil
			}
		}
		f, err := blockstore.ParseLogFilter(obj)
		if err != nil {
			return errorReply(req.ID, int(common.JsonRpcErrorInvalidArgument), err.Error()), nil
		}
		s.filter = f
	default:
		return errorReply(req.ID, int(common.JsonRpcErrorUnsupportedException), fmt.Sprintf("unsupported subscription type %q (supported: newHeads, logs)", kind)), nil
	}

	hc := c.network.BlockStore()
	if hc == nil {
		return errorReply(req.ID, int(common.JsonRpcErrorUnsupportedException), "subscriptions unavailable: head cache is not enabled for this network"), nil
	}
	// Subscription setup consumes project rate-limit budget like any call.
	if err := c.project.AcquireRateLimitPermit(ctx, nq); err != nil {
		now := time.Now()
		return c.render(processErrorBody(c.lg, &now, nq, err, c.ws.s.serverCfg.IncludeErrorDetails)), nil
	}

	c.mu.Lock()
	if c.ctx.Err() != nil {
		c.mu.Unlock()
		return nil, nil
	}
	if len(c.subs) >= c.ws.cfg.MaxSubscriptionsPerConnection {
		c.mu.Unlock()
		return errorReply(req.ID, int(common.JsonRpcErrorCapacityExceeded), fmt.Sprintf("too many subscriptions on this connection (max %d)", c.ws.cfg.MaxSubscriptionsPerConnection)), nil
	}
	s.sub = hc.Subscribe(c.ws.cfg.SendQueueSize)
	c.subs[s.id] = s
	// Reserve the pump in the wait group while still registered so cleanup
	// cannot finish before the deferred start runs.
	c.wg.Add(1)
	c.mu.Unlock()
	// The caller enqueues the id reply before starting the pump so no
	// notification can precede it.
	return resultReply(req.ID, s.id), func(delivered bool) {
		if delivered {
			go c.pump(s)
			return
		}
		c.mu.Lock()
		if c.subs[s.id] == s {
			delete(c.subs, s.id)
		}
		c.mu.Unlock()
		s.unsubscribed.Store(true)
		s.sub.Close()
		c.wg.Done()
	}
}

func (c *wsConn) unsubscribe(req *wsRequest) []byte {
	var params []string
	if err := json.Unmarshal(req.Params, &params); err != nil || len(params) != 1 {
		return errorReply(req.ID, int(common.JsonRpcErrorInvalidArgument), "eth_unsubscribe requires a subscription id")
	}
	c.mu.Lock()
	s, ok := c.subs[params[0]]
	if ok {
		delete(c.subs, params[0])
	}
	c.mu.Unlock()
	if ok {
		s.unsubscribed.Store(true)
		s.sub.Close()
	}
	return resultReply(req.ID, ok)
}

func (c *wsConn) notify(s *wsSub, result json.RawMessage) bool {
	return c.sendStream([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","method":"eth_subscription","params":{"subscription":%q,"result":%s}}`, s.id, result)))
}

func (c *wsConn) pump(s *wsSub) {
	defer c.wg.Done()
	defer s.sub.Close()
	defer func() {
		if rec := recover(); rec != nil {
			c.recovered("ws-pump", rec)
		}
	}()
	for {
		select {
		case <-c.ctx.Done():
			return
		case ev, ok := <-s.sub.C:
			if !ok {
				if !s.unsubscribed.Load() && c.ctx.Err() == nil {
					// Overflow or cache stop: the client missed events and
					// must reconnect rather than see a silent gap.
					c.closeWith(websocket.StatusPolicyViolation, "subscription closed: "+wsCloseSlowConsumer+" or head cache stopped")
				}
				return
			}
			if err := c.emit(s, ev); errors.Is(err, errWsGap) {
				// Missing heights (evicted records, window reset, recovery
				// after staleness): the client must resync, never skip.
				c.closeWith(websocket.StatusPolicyViolation, "subscription gap: resubscribe")
				return
			} else if err != nil {
				c.lg.Warn().Err(err).Str("subscription", s.id).Msg("failed to render subscription event")
				c.closeWith(websocket.StatusInternalError, "failed to render subscription event")
				return
			}
		}
	}
}

var errWsGap = errors.New("head discontinuity")

// checkContinuity verifies an event extends what the subscriber has seen:
// Added must be contiguous and start right after the last emitted block, or
// right at the lowest Removed block for a reorg rewind. The first event of a
// subscription establishes the baseline.
func checkContinuity(last int64, ev blockstore.Event) error {
	for i := 1; i < len(ev.Added); i++ {
		if ev.Added[i].Number != ev.Added[i-1].Number+1 {
			return errWsGap
		}
	}
	for i := 1; i < len(ev.Removed); i++ {
		if ev.Removed[i].Number != ev.Removed[i-1].Number-1 {
			return errWsGap
		}
	}
	if last == 0 {
		return nil
	}
	next := last + 1
	if len(ev.Removed) > 0 {
		lowest := ev.Removed[len(ev.Removed)-1].Number
		if ev.Removed[0].Number > last || lowest > next {
			return errWsGap
		}
		next = lowest
	}
	if len(ev.Added) > 0 && ev.Added[0].Number != next {
		return errWsGap
	}
	return nil
}

func (c *wsConn) emit(s *wsSub, ev blockstore.Event) error {
	if err := checkContinuity(s.last, ev); err != nil {
		return err
	}
	if len(ev.Added) > 0 {
		s.last = ev.Added[len(ev.Added)-1].Number
	} else if len(ev.Removed) > 0 {
		s.last = ev.Removed[len(ev.Removed)-1].Number - 1
	}
	if !s.logs {
		for _, rec := range ev.Added {
			h, err := rec.HeaderJSON()
			if err != nil {
				return err
			}
			if !c.notify(s, h) {
				return nil
			}
		}
		return nil
	}
	for _, rec := range ev.Removed {
		logs, err := rec.FilterLogs(s.filter, true)
		if err != nil {
			return err
		}
		for _, l := range logs {
			if !c.notify(s, l) {
				return nil
			}
		}
	}
	for _, rec := range ev.Added {
		logs, err := rec.FilterLogs(s.filter, false)
		if err != nil {
			return err
		}
		for _, l := range logs {
			if !c.notify(s, l) {
				return nil
			}
		}
	}
	return nil
}
