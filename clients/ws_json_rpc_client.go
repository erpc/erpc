package clients

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"encoding/json"

	"github.com/erpc/erpc/common"
	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

const (
	wsPingInterval    = 30 * time.Second
	wsWriteWait       = 10 * time.Second
	wsReconnectMin    = 1 * time.Second
	wsReconnectMax    = 30 * time.Second
	wsReconnectFactor = 2.0
)

// WsJsonRpcClient implements ClientInterface for WebSocket-based JSON-RPC upstream connections.
type WsJsonRpcClient struct {
	Url     *url.URL
	headers http.Header

	projectId string
	upstream  common.Upstream
	appCtx    context.Context
	logger    *zerolog.Logger

	// Connection state
	connMu sync.Mutex
	conn   *websocket.Conn

	// connWake is pulsed by reconnect() once a new connection is in c.conn,
	// so readLoop can wake up without polling. Capacity 1 coalesces bursts.
	connWake chan struct{}

	// Write synchronization (gorilla/websocket requires synchronized writes)
	writeMu sync.Mutex

	// Pending request tracking: JSON-RPC ID -> response channel.
	// Uses RWMutex because the hot path (handleMessage dispatching responses)
	// only needs a read lock, while writes (register/deregister) are less frequent.
	pendingMu sync.RWMutex
	pending   map[string]chan *wsPendingResult

	// Signalled when the first connection is established (or app shutdown).
	// readLoop blocks on this before entering its main loop.
	connReady chan struct{}
	connOnce  sync.Once

	// Subscription notification callbacks: upstreamSubID -> handler
	subHandlersMu sync.RWMutex
	subHandlers   map[string]func(params []byte)

	// Disconnect/reconnect callbacks are keyed by caller-supplied IDs so
	// subscribers can replace (on re-subscribe) and remove (on teardown)
	// their hooks, preventing the callback slices from growing unbounded
	// over long-lived connections with subscription churn.
	onDisconnectMu  sync.RWMutex
	onDisconnectCbs map[string]func()

	onReconnectMu  sync.RWMutex
	onReconnectCbs map[string]func()

	// Error extractor for architecture-specific error normalization
	errorExtractor common.JsonRpcErrorExtractor

	connected atomic.Bool

	// wireIDCounter generates unique JSON-RPC ids on the WS wire so that
	// concurrent SendRequest calls with the same caller-supplied id do not
	// collide on the pending response map. The original caller id is
	// restored on the response before returning.
	wireIDCounter atomic.Uint64
}

type wsPendingResult struct {
	resp *common.NormalizedResponse
	err  error
}

// wsMessage is a minimal struct for parsing incoming WS messages to determine if they are
// responses (have "id") or notifications (have "method").
type wsMessage struct {
	JSONRPC string                              `json:"jsonrpc"`
	ID      interface{}                         `json:"id,omitempty"`
	Method  string                              `json:"method,omitempty"`
	Result  json.RawMessage                     `json:"result,omitempty"`
	Error   *common.ErrJsonRpcExceptionExternal `json:"error,omitempty"`
	Params  json.RawMessage                     `json:"params,omitempty"`
}

// wsNotificationParams is the structure of subscription notification params.
type wsNotificationParams struct {
	Subscription string          `json:"subscription"`
	Result       json.RawMessage `json:"result"`
}

func NewWsJsonRpcClient(
	appCtx context.Context,
	logger *zerolog.Logger,
	projectId string,
	upstream common.Upstream,
	parsedUrl *url.URL,
	jsonRpcCfg *common.JsonRpcUpstreamConfig,
	extractor common.JsonRpcErrorExtractor,
) (ClientInterface, error) {
	headers := http.Header{}
	if jsonRpcCfg != nil && jsonRpcCfg.Headers != nil {
		for k, v := range jsonRpcCfg.Headers {
			headers.Set(k, v)
		}
	}

	client := &WsJsonRpcClient{
		Url:             parsedUrl,
		headers:         headers,
		projectId:       projectId,
		upstream:        upstream,
		appCtx:          appCtx,
		logger:          logger,
		pending:         make(map[string]chan *wsPendingResult),
		connReady:       make(chan struct{}),
		connWake:        make(chan struct{}, 1),
		subHandlers:     make(map[string]func(params []byte)),
		onDisconnectCbs: make(map[string]func()),
		onReconnectCbs:  make(map[string]func()),
		errorExtractor:  extractor,
	}

	if err := client.connect(); err != nil {
		// Don't fail on initial connection — start reconnect loop in background.
		// The upstream may not be available at startup but will be retried.
		logger.Warn().Err(err).Str("url", parsedUrl.String()).Msg("initial websocket connection failed, will retry in background")
		go client.reconnect()
	} else {
		client.connOnce.Do(func() { close(client.connReady) })
	}

	go client.readLoop()
	go client.pingLoop()
	go func() {
		<-appCtx.Done()
		client.shutdown()
	}()

	return client, nil
}

func (c *WsJsonRpcClient) GetType() ClientType {
	return ClientTypeWsJsonRpc
}

// IsConnected returns true if the upstream WebSocket connection is currently established.
func (c *WsJsonRpcClient) IsConnected() bool {
	return c.connected.Load()
}

func (c *WsJsonRpcClient) SendRequest(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	ctx, span := common.StartDetailSpan(ctx, "WsJsonRpcClient.SendRequest",
		trace.WithAttributes(
			attribute.String("upstream.id", c.upstream.Id()),
		),
	)
	defer span.End()

	startedAt := time.Now()

	jrReq, err := req.JsonRpcRequest()
	if err != nil {
		return nil, common.NewErrUpstreamRequest(
			err,
			c.upstream,
			req.NetworkId(),
			"",
			0, 0, 0, 0,
		)
	}

	// Use a unique outbound wire id so concurrent SendRequest calls with the
	// same caller-supplied JSON-RPC id do not collide in c.pending. The
	// original id is restored on the response below before returning.
	wireID := c.wireIDCounter.Add(1)
	idKey := strconv.FormatUint(wireID, 10)

	// Serialize the JSON-RPC request with the rewritten wire id
	jrReq.RLock()
	originalID := jrReq.ID
	requestBody, err := common.SonicCfg.Marshal(map[string]interface{}{
		"jsonrpc": jrReq.JSONRPC,
		"id":      wireID,
		"method":  jrReq.Method,
		"params":  jrReq.Params,
	})
	jrReq.RUnlock()
	if err != nil {
		common.SetTraceSpanError(span, err)
		return nil, common.NewErrUpstreamRequest(
			err,
			c.upstream,
			req.NetworkId(),
			jrReq.Method,
			0, 0, 0, 0,
		)
	}

	// Register a response channel
	respCh := make(chan *wsPendingResult, 1)
	c.pendingMu.Lock()
	c.pending[idKey] = respCh
	c.pendingMu.Unlock()

	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, idKey)
		c.pendingMu.Unlock()
	}()

	// Write to the WebSocket connection
	if err := c.writeMessage(websocket.TextMessage, requestBody); err != nil {
		common.SetTraceSpanError(span, err)
		return nil, common.NewErrEndpointTransportFailure(c.Url, err)
	}

	c.logger.Debug().
		Str("host", c.Url.Host).
		RawJSON("request", requestBody).
		Msg("sent json rpc websocket request")

	// Wait for response
	select {
	case result := <-respCh:
		if result.err != nil {
			common.SetTraceSpanError(span, result.err)
			return nil, result.err
		}
		// Restore the caller's original JSON-RPC id on the response, since
		// the on-wire id was rewritten to our unique counter above.
		if result.resp != nil {
			if jrr, perr := result.resp.JsonRpcResponse(ctx); perr == nil && jrr != nil {
				_ = jrr.SetID(originalID)
			}
		}
		return result.resp, nil
	case <-ctx.Done():
		err := ctx.Err()
		if errors.Is(err, context.DeadlineExceeded) {
			err = common.NewErrEndpointRequestTimeout(time.Since(startedAt), err)
		} else if errors.Is(err, context.Canceled) {
			err = common.NewErrEndpointRequestCanceled(err)
		}
		common.SetTraceSpanError(span, err)
		return nil, err
	case <-c.appCtx.Done():
		return nil, common.NewErrEndpointRequestCanceled(c.appCtx.Err())
	}
}

// RegisterSubscriptionHandler registers a callback for a specific upstream subscription ID.
// When the upstream sends a notification for this subscription, the handler is called with the raw params bytes.
func (c *WsJsonRpcClient) RegisterSubscriptionHandler(upstreamSubID string, handler func(params []byte)) {
	c.subHandlersMu.Lock()
	c.subHandlers[upstreamSubID] = handler
	c.subHandlersMu.Unlock()
}

// UnregisterSubscriptionHandler removes the callback for a specific upstream subscription ID.
func (c *WsJsonRpcClient) UnregisterSubscriptionHandler(upstreamSubID string) {
	c.subHandlersMu.Lock()
	delete(c.subHandlers, upstreamSubID)
	c.subHandlersMu.Unlock()
}

// SetOnDisconnect registers (or replaces) the callback keyed by id that fires
// when the upstream WS connection drops. Use RemoveOnDisconnect(id) to
// deregister on subscription teardown so long-lived connections don't
// accumulate dead callbacks.
func (c *WsJsonRpcClient) SetOnDisconnect(id string, callback func()) {
	c.onDisconnectMu.Lock()
	c.onDisconnectCbs[id] = callback
	c.onDisconnectMu.Unlock()
}

// RemoveOnDisconnect deregisters a disconnect callback previously set with
// SetOnDisconnect. A no-op if id is not registered.
func (c *WsJsonRpcClient) RemoveOnDisconnect(id string) {
	c.onDisconnectMu.Lock()
	delete(c.onDisconnectCbs, id)
	c.onDisconnectMu.Unlock()
}

// SetOnReconnect registers (or replaces) the callback keyed by id that fires
// after a successful reconnect.
func (c *WsJsonRpcClient) SetOnReconnect(id string, callback func()) {
	c.onReconnectMu.Lock()
	c.onReconnectCbs[id] = callback
	c.onReconnectMu.Unlock()
}

// RemoveOnReconnect deregisters a reconnect callback previously set with
// SetOnReconnect. A no-op if id is not registered.
func (c *WsJsonRpcClient) RemoveOnReconnect(id string) {
	c.onReconnectMu.Lock()
	delete(c.onReconnectCbs, id)
	c.onReconnectMu.Unlock()
}

func (c *WsJsonRpcClient) connect() error {
	c.connMu.Lock()
	defer c.connMu.Unlock()

	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}

	if c.Url.Scheme == "wss" {
		dialer.TLSClientConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
		}
	}

	conn, _, err := dialer.DialContext(c.appCtx, c.Url.String(), c.headers)
	if err != nil {
		return err
	}

	c.conn = conn
	c.connected.Store(true)

	c.logger.Info().Str("url", c.Url.String()).Msg("websocket connection established")
	return nil
}

func (c *WsJsonRpcClient) readLoop() {
	// Wait until the first connection is established (or the app shuts down)
	select {
	case <-c.connReady:
	case <-c.appCtx.Done():
		return
	}

	for {
		if c.appCtx.Err() != nil {
			return
		}

		c.connMu.Lock()
		conn := c.conn
		c.connMu.Unlock()

		if conn == nil {
			// Connection is being re-established after a disconnect; block
			// until reconnect() pulses connWake (or the app shuts down).
			select {
			case <-c.connWake:
			case <-c.appCtx.Done():
				return
			}
			continue
		}

		_, message, err := conn.ReadMessage()
		if err != nil {
			if c.appCtx.Err() != nil {
				return
			}
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				c.logger.Info().Msg("websocket connection closed normally")
			} else {
				c.logger.Warn().Err(err).Msg("websocket read error, will reconnect")
			}
			c.connected.Store(false)
			c.drainPending(common.NewErrEndpointTransportFailure(c.Url, fmt.Errorf("websocket connection lost: %w", err)))
			c.fireCallbacks(&c.onDisconnectMu, c.onDisconnectCbs)

			c.reconnect()
			continue
		}

		c.handleMessage(message)
	}
}

// fireCallbacks snapshots the callback map under rlock and dispatches each
// in its own goroutine. Snapshotting lets callbacks register/deregister
// other callbacks without deadlocking on the map's RWMutex.
func (c *WsJsonRpcClient) fireCallbacks(mu *sync.RWMutex, cbs map[string]func()) {
	mu.RLock()
	snapshot := make([]func(), 0, len(cbs))
	for _, cb := range cbs {
		snapshot = append(snapshot, cb)
	}
	mu.RUnlock()
	for _, cb := range snapshot {
		go cb()
	}
}

func (c *WsJsonRpcClient) handleMessage(message []byte) {
	var msg wsMessage
	if err := common.SonicCfg.Unmarshal(message, &msg); err != nil {
		c.logger.Warn().Err(err).Str("raw", string(message)).Msg("failed to parse websocket message")
		return
	}

	// Subscription notification: has "method" field (typically "eth_subscription")
	if msg.Method != "" && msg.ID == nil {
		c.handleNotification(msg.Method, msg.Params)
		return
	}

	// Response to a pending request: has "id" field
	if msg.ID != nil {
		idKey := normalizeIDKey(msg.ID)

		c.pendingMu.RLock()
		ch, ok := c.pending[idKey]
		c.pendingMu.RUnlock()

		if !ok {
			c.logger.Debug().Str("id", idKey).Msg("received response for unknown request ID")
			return
		}

		nr := common.NewNormalizedResponse().WithBody(io.NopCloser(strings.NewReader(string(message))))

		if msg.Error != nil {
			ch <- &wsPendingResult{resp: nr, err: msg.Error}
		} else {
			ch <- &wsPendingResult{resp: nr}
		}
		return
	}

	c.logger.Debug().Str("raw", string(message)).Msg("received unhandled websocket message")
}

func (c *WsJsonRpcClient) handleNotification(method string, params []byte) {
	if method != "eth_subscription" {
		c.logger.Debug().Str("method", method).Msg("received non-subscription notification")
		return
	}

	var notifParams wsNotificationParams
	if err := common.SonicCfg.Unmarshal(params, &notifParams); err != nil {
		c.logger.Warn().Err(err).Msg("failed to parse subscription notification params")
		return
	}

	c.subHandlersMu.RLock()
	handler, ok := c.subHandlers[notifParams.Subscription]
	c.subHandlersMu.RUnlock()

	if !ok {
		c.logger.Debug().Str("subscriptionId", notifParams.Subscription).Msg("received notification for unknown subscription")
		return
	}

	handler(params)
}

func (c *WsJsonRpcClient) reconnect() {
	backoff := wsReconnectMin
	for {
		if c.appCtx.Err() != nil {
			return
		}

		c.logger.Info().Dur("backoff", backoff).Msg("attempting websocket reconnection")

		if err := c.connect(); err != nil {
			c.logger.Warn().Err(err).Dur("backoff", backoff).Msg("websocket reconnection failed")
			select {
			case <-time.After(backoff):
			case <-c.appCtx.Done():
				return
			}
			backoff = time.Duration(float64(backoff) * wsReconnectFactor)
			if backoff > wsReconnectMax {
				backoff = wsReconnectMax
			}
			continue
		}

		c.logger.Info().Msg("websocket reconnected successfully")

		// Signal readLoop if this is the first successful connection.
		c.connOnce.Do(func() { close(c.connReady) })

		// Wake readLoop if it's parked waiting for c.conn to be non-nil.
		// Buffered channel with cap 1 means we coalesce concurrent pulses.
		select {
		case c.connWake <- struct{}{}:
		default:
		}

		c.fireCallbacks(&c.onReconnectMu, c.onReconnectCbs)

		return
	}
}

func (c *WsJsonRpcClient) drainPending(err error) {
	c.pendingMu.Lock()
	pending := c.pending
	c.pending = make(map[string]chan *wsPendingResult)
	c.pendingMu.Unlock()

	for _, ch := range pending {
		select {
		case ch <- &wsPendingResult{err: err}:
		default:
		}
	}
}

func (c *WsJsonRpcClient) writeMessage(messageType int, data []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	c.connMu.Lock()
	conn := c.conn
	c.connMu.Unlock()

	if conn == nil {
		return fmt.Errorf("websocket connection not established")
	}

	if err := conn.SetWriteDeadline(time.Now().Add(wsWriteWait)); err != nil {
		return err
	}
	return conn.WriteMessage(messageType, data)
}

func (c *WsJsonRpcClient) pingLoop() {
	ticker := time.NewTicker(wsPingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if !c.connected.Load() {
				continue
			}
			if err := c.writeMessage(websocket.PingMessage, nil); err != nil {
				c.logger.Debug().Err(err).Msg("websocket ping failed")
			}
		case <-c.appCtx.Done():
			return
		}
	}
}

// normalizeIDKey converts a JSON-RPC ID to a stable string key.
// JSON unmarshalling turns integer IDs into float64, which can produce
// scientific notation with fmt.Sprintf (e.g., "1.51e+09" vs "1510000000").
// This function normalizes to avoid mismatches.
func normalizeIDKey(id interface{}) string {
	switch v := id.(type) {
	case float64:
		// Format without scientific notation
		return fmt.Sprintf("%.0f", v)
	case int:
		return fmt.Sprintf("%d", v)
	case int64:
		return fmt.Sprintf("%d", v)
	case string:
		return v
	default:
		return fmt.Sprintf("%v", v)
	}
}

func (c *WsJsonRpcClient) shutdown() {
	c.connected.Store(false)

	c.connMu.Lock()
	conn := c.conn
	c.conn = nil
	c.connMu.Unlock()

	if conn != nil {
		// Send close frame and close
		_ = conn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
			time.Now().Add(wsWriteWait),
		)
		_ = conn.Close()
	}

	c.drainPending(common.NewErrEndpointRequestCanceled(fmt.Errorf("websocket client shutting down")))
}
