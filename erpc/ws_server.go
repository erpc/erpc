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
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/headcache"
	"github.com/erpc/erpc/telemetry"
	"github.com/rs/zerolog"
)

// WebSocket JSON-RPC endpoint.
//
// Served on the same paths as HTTP (/<project>/<architecture>/<chainId>, or
// aliased) when the client sends an Upgrade request and server.webSocket is
// enabled. A connection is bound to one project+network:
//
//   - Ordinary JSON-RPC calls go through the exact same pipeline as HTTP
//     (method allow/ignore lists, per-message consumer auth, project and
//     network rate limits, project.Forward).
//   - eth_subscribe("newHeads") and eth_subscribe("logs", {address, topics})
//     are served from the network's head cache. Reorgs are emitted as the
//     orphaned logs with "removed":true (highest block first) followed by the
//     new canonical logs. When the network has no head cache, eth_subscribe
//     fails with a JSON-RPC error; eRPC never proxies upstream WebSockets.
//   - eth_unsubscribe only sees subscription ids created on the same
//     connection.
//
// Bounds: server.webSocket.maxConnections (HTTP 503 at upgrade),
// maxSubscriptionsPerConnection, maxMessageBytes (inbound frame, 1009 close),
// sendQueueSize (a client that falls this far behind, or whose head cache
// subscription overflows, is closed with 1008), writeTimeout per frame.
// Connections close with 1001 when the server shuts down.

var wsPingInterval = 30 * time.Second

const (
	wsMaxInflightPerConn = 16
	wsCloseSlowConsumer  = "slow consumer"
)

type wsServer struct {
	s     *HttpServer
	cfg   *common.WebSocketServerConfig
	conns atomic.Int64
}

func newWsServer(s *HttpServer, cfg *common.WebSocketServerConfig) *wsServer {
	if cfg == nil || !cfg.Enabled {
		return nil
	}
	return &wsServer{s: s, cfg: cfg}
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
	defer func() {
		if rec := recover(); rec != nil {
			telemetry.MetricUnexpectedPanicTotal.WithLabelValues("ws-handler", "", common.ErrorFingerprint(rec)).Inc()
			s.logger.Error().Interface("panic", rec).Str("stack", string(debug.Stack())).Msg("unexpected panic in websocket handler")
		}
	}()
	if s.draining != nil && s.draining.Load() {
		wsHttpError(w, http.StatusServiceUnavailable, "server is shutting down")
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
		wsHttpError(w, http.StatusBadRequest, "websocket requires /<project>/<architecture>/<chainId>")
		return
	}
	project, err := s.erpc.GetProject(projectId)
	if err != nil || project == nil {
		wsHttpError(w, http.StatusNotFound, "project not found")
		return
	}
	networkId := architecture + ":" + chainId
	nw, err := project.GetNetwork(r.Context(), networkId)
	if err != nil {
		wsHttpError(w, http.StatusNotFound, "network not found")
		return
	}

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
			wsHttpError(w, http.StatusForbidden, "origin not allowed")
			return
		}
	}

	// Credentials are checked at upgrade so unauthenticated clients never get
	// a connection. Each message is authenticated again with its own method.
	if _, err := ws.authenticate(r.Context(), project, r, "eth_subscribe", nil); err != nil {
		wsHttpError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	if n := ws.conns.Add(1); n > int64(ws.cfg.MaxConnections) {
		ws.conns.Add(-1)
		wsHttpError(w, http.StatusServiceUnavailable, "too many websocket connections")
		return
	}
	defer ws.conns.Add(-1)

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Origin already enforced above against project CORS.
		InsecureSkipVerify: true,
	})
	if err != nil {
		return
	}
	conn.SetReadLimit(ws.cfg.MaxMessageBytes)

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
	c.run()
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
	sub    *headcache.Subscription
	logs   bool
	filter *headcache.LogFilter
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

	ctx    context.Context
	cancel context.CancelCauseFunc
	out    chan []byte

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

func (c *wsConn) run() {
	// Tied to the application context, not the request: hijacked
	// connections are not tracked by http.Server.Shutdown.
	c.ctx, c.cancel = context.WithCancelCause(c.ws.s.appCtx)
	defer c.cleanup()

	writerDone := make(chan struct{})
	go c.writeLoop(writerDone)
	go c.pingLoop()

	sem := make(chan struct{}, wsMaxInflightPerConn)
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
		select {
		case sem <- struct{}{}:
		case <-c.ctx.Done():
		}
		if c.ctx.Err() != nil {
			break
		}
		c.wg.Add(1)
		go func(data []byte) {
			defer c.wg.Done()
			defer func() { <-sem }()
			defer func() {
				if rec := recover(); rec != nil {
					telemetry.MetricUnexpectedPanicTotal.WithLabelValues("ws-message", c.networkId, common.ErrorFingerprint(rec)).Inc()
					c.lg.Error().Interface("panic", rec).Str("stack", string(debug.Stack())).Msg("unexpected panic handling websocket message")
					c.closeWith(websocket.StatusInternalError, "internal error")
				}
			}()
			c.handleMessage(data)
		}(data)
	}
	<-writerDone
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
			cancel()
			if err != nil {
				c.closeWith(websocket.StatusPolicyViolation, "write timeout")
			}
		}
	}
}

// pingLoop detects dead peers (which would otherwise hold a connection slot
// until TCP gives up) and keeps idle connections alive behind LB idle
// timeouts. Pong handling requires the concurrent reader in run().
func (c *wsConn) pingLoop() {
	t := time.NewTicker(wsPingInterval)
	defer t.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-t.C:
			pctx, cancel := context.WithTimeout(c.ctx, c.ws.cfg.WriteTimeout.Duration())
			err := c.conn.Ping(pctx)
			cancel()
			if err != nil && c.ctx.Err() == nil {
				c.closeWith(websocket.StatusPolicyViolation, "ping timeout")
				return
			}
			// Re-check the upgrade credentials so revoked keys and expired
			// JWTs stop streaming within one ping interval.
			if _, err := c.ws.authenticate(c.ctx, c.project, c.req, "eth_subscribe", nil); err != nil && c.ctx.Err() == nil {
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
func (c *wsConn) send(msg []byte) {
	if c.ctx.Err() != nil {
		return
	}
	select {
	case c.out <- msg:
	default:
		c.closeWith(websocket.StatusPolicyViolation, wsCloseSlowConsumer)
	}
}

type wsRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func (c *wsConn) sendError(id json.RawMessage, code int, msg string) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	b, _ := json.Marshal(msg)
	c.send([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":{"code":%d,"message":%s}}`, id, code, b)))
}

func (c *wsConn) sendResult(id json.RawMessage, result interface{}) {
	rb, _ := json.Marshal(result)
	c.send([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":%s}`, id, rb)))
}

func (c *wsConn) handleMessage(data []byte) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		c.sendError(nil, -32600, "batch requests are not supported over websocket")
		return
	}
	var req wsRequest
	if err := json.Unmarshal(trimmed, &req); err != nil || req.Method == "" {
		c.sendError(nil, -32700, "invalid json-rpc request")
		return
	}
	if len(req.ID) == 0 {
		req.ID = json.RawMessage("null")
	}

	startedAt := time.Now()
	nq := common.NewNormalizedRequest(trimmed)
	nq.ForwardHeaders = make(http.Header)
	nq.SetClientIP(c.clientIP)
	ctx := common.StartRequestSpan(c.ctx, nq)

	res, err := c.admit(ctx, nq, req.Method)
	if err == nil {
		switch req.Method {
		case "eth_subscribe":
			c.subscribe(ctx, nq, &req)
			common.EndRequestSpan(ctx, nil, nil)
			return
		case "eth_unsubscribe":
			c.unsubscribe(&req)
			common.EndRequestSpan(ctx, nil, nil)
			return
		}
		fres, ferr := c.project.Forward(ctx, c.networkId, nq)
		if ferr != nil {
			if fres != nil {
				go fres.Release()
			}
			body := processErrorBody(c.lg, &startedAt, nq, ferr, c.ws.s.serverCfg.IncludeErrorDetails)
			c.writeAny(body)
			common.EndRequestSpan(ctx, nil, ferr)
			return
		}
		c.writeAny(fres)
		common.EndRequestSpan(ctx, fres, nil)
		return
	}
	if errors.Is(err, errSkipForward) {
		c.writeAny(res)
		common.EndRequestSpan(ctx, nil, nil)
		return
	}
	body := processErrorBody(c.lg, &startedAt, nq, err, c.ws.s.serverCfg.IncludeErrorDetails, common.NetworkArchitecture(c.network.Architecture()))
	c.writeAny(body)
	common.EndRequestSpan(ctx, nil, err)
}

func (c *wsConn) writeAny(v interface{}) {
	var buf bytes.Buffer
	var err error
	switch r := v.(type) {
	case *common.NormalizedResponse:
		if r == nil {
			return
		}
		_, err = r.WriteTo(&buf)
		go r.Release()
	case *HttpJsonRpcErrorResponse:
		_, err = writeJsonRpcError(&buf, r)
	default:
		err = common.SonicCfg.NewEncoder(&buf).Encode(v)
	}
	if err != nil {
		c.lg.Warn().Err(err).Msg("failed to serialize websocket response")
		return
	}
	c.send(bytes.TrimRight(buf.Bytes(), "\n"))
}

// admit applies the same gating as HTTP: validation, forward headers,
// ignore/allow methods, per-message consumer auth, trusted user header and
// request enrichment. Returning (res, err) mirrors Forward so errors share
// a single serialization path.
func (c *wsConn) admit(ctx context.Context, nq *common.NormalizedRequest, method string) (interface{}, error) {
	if err := nq.Validate(); err != nil {
		return nil, err
	}
	pc := c.project.Config
	headers := c.req.Header
	for _, matchKey := range pc.ForwardHeaders {
		for key, values := range headers {
			if ok, err := common.WildcardMatch(matchKey, key); err != nil {
				return nil, err
			} else if ok {
				for _, v := range values {
					nq.ForwardHeaders.Add(matchKey, v)
				}
			}
		}
	}
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
		return &HttpJsonRpcErrorResponse{
			Jsonrpc: "2.0",
			Id:      nq.ID(),
			Error: map[string]interface{}{
				"code":    int(common.JsonRpcErrorUnsupportedException),
				"message": fmt.Sprintf("method not supported: %s", method),
			},
			Request: nq,
		}, errSkipForward
	}
	user, err := c.ws.authenticate(ctx, c.project, c.req, method, nq)
	if err != nil {
		return nil, err
	}
	nq.SetUser(user)
	if pc.TrustUserIdHeader && nq.User() == nil {
		nq.SetUserFromTrustedHeader(headers.Get(common.HeaderUserId))
	}
	nq.SetNetwork(c.network)
	nq.ApplyDirectiveDefaults(c.network.Config().DirectiveDefaults)
	uaMode := common.UserAgentTrackingModeSimplified
	if pc.UserAgentMode != "" {
		uaMode = pc.UserAgentMode
	}
	nq.SetAllowClientDirectiveMatcher(c.project.clientDirectiveMatcherFor(nq.User()))
	nq.EnrichFromHttp(headers, c.req.URL.Query(), uaMode)
	return nil, nil
}

// errSkipForward carries a pre-built response through the error path.
var errSkipForward = errors.New("ws: response prepared without forwarding")

func newSubscriptionId() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "0x" + hex.EncodeToString(b[:])
}

func (c *wsConn) subscribe(ctx context.Context, nq *common.NormalizedRequest, req *wsRequest) {
	var params []json.RawMessage
	if err := json.Unmarshal(req.Params, &params); err != nil || len(params) == 0 {
		c.sendError(req.ID, int(common.JsonRpcErrorInvalidArgument), "eth_subscribe requires a subscription type")
		return
	}
	var kind string
	if err := json.Unmarshal(params[0], &kind); err != nil {
		c.sendError(req.ID, int(common.JsonRpcErrorInvalidArgument), "invalid subscription type")
		return
	}
	s := &wsSub{id: newSubscriptionId()}
	switch kind {
	case "newHeads":
		if len(params) > 1 {
			c.sendError(req.ID, int(common.JsonRpcErrorInvalidArgument), "newHeads takes no filter")
			return
		}
	case "logs":
		s.logs = true
		obj := map[string]interface{}{}
		if len(params) > 1 {
			if err := json.Unmarshal(params[1], &obj); err != nil || obj == nil {
				c.sendError(req.ID, int(common.JsonRpcErrorInvalidArgument), "logs filter must be an object")
				return
			}
		}
		f, err := headcache.ParseLogFilter(obj)
		if err != nil {
			c.sendError(req.ID, int(common.JsonRpcErrorInvalidArgument), err.Error())
			return
		}
		s.filter = f
	default:
		c.sendError(req.ID, int(common.JsonRpcErrorInvalidArgument), fmt.Sprintf("unsupported subscription type %q (supported: newHeads, logs)", kind))
		return
	}

	hc := c.network.HeadCache()
	if hc == nil {
		c.sendError(req.ID, int(common.JsonRpcErrorUnsupportedException), "subscriptions unavailable: head cache is not enabled for this network")
		return
	}
	// Subscription setup consumes project rate-limit budget like any call.
	if err := c.project.AcquireRateLimitPermit(ctx, nq); err != nil {
		now := time.Now()
		c.writeAny(processErrorBody(c.lg, &now, nq, err, c.ws.s.serverCfg.IncludeErrorDetails))
		return
	}

	c.mu.Lock()
	if c.ctx.Err() != nil {
		c.mu.Unlock()
		return
	}
	if len(c.subs) >= c.ws.cfg.MaxSubscriptionsPerConnection {
		c.mu.Unlock()
		c.sendError(req.ID, int(common.JsonRpcErrorCapacityExceeded), fmt.Sprintf("too many subscriptions on this connection (max %d)", c.ws.cfg.MaxSubscriptionsPerConnection))
		return
	}
	s.sub = hc.Subscribe(c.ws.cfg.SendQueueSize)
	c.subs[s.id] = s
	c.mu.Unlock()

	// The id reply is enqueued before the pump starts so no notification can
	// precede it.
	c.sendResult(req.ID, s.id)
	c.wg.Add(1)
	go c.pump(s)
}

func (c *wsConn) unsubscribe(req *wsRequest) {
	var params []string
	if err := json.Unmarshal(req.Params, &params); err != nil || len(params) != 1 {
		c.sendError(req.ID, int(common.JsonRpcErrorInvalidArgument), "eth_unsubscribe requires a subscription id")
		return
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
	c.sendResult(req.ID, ok)
}

func (c *wsConn) notify(id string, result json.RawMessage) bool {
	return c.sendStream([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","method":"eth_subscription","params":{"subscription":%q,"result":%s}}`, id, result)))
}

func (c *wsConn) pump(s *wsSub) {
	defer c.wg.Done()
	defer s.sub.Close()
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
func checkContinuity(last int64, ev headcache.Event) error {
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

func (c *wsConn) emit(s *wsSub, ev headcache.Event) error {
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
			if !c.notify(s.id, h) {
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
			if !c.notify(s.id, l) {
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
			if !c.notify(s.id, l) {
				return nil
			}
		}
	}
	return nil
}
