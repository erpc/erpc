package clients

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
	wsMaxPendingDrain = 5 * time.Second
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

	// Write synchronization (gorilla/websocket requires synchronized writes)
	writeMu sync.Mutex

	// Pending request tracking: JSON-RPC ID -> response channel
	pendingMu sync.Mutex
	pending   map[string]chan *wsPendingResult

	// Read loop lifecycle
	readDone chan struct{}

	// Subscription notification callbacks: upstreamSubID -> handler
	subHandlersMu sync.RWMutex
	subHandlers   map[string]func(params []byte)

	// Reconnect callback (for subscription recovery)
	onReconnectMu sync.RWMutex
	onReconnect   func()

	// Error extractor for architecture-specific error normalization
	errorExtractor common.JsonRpcErrorExtractor

	connected atomic.Bool
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
		Url:            parsedUrl,
		headers:        headers,
		projectId:      projectId,
		upstream:       upstream,
		appCtx:         appCtx,
		logger:         logger,
		pending:        make(map[string]chan *wsPendingResult),
		readDone:       make(chan struct{}),
		subHandlers:    make(map[string]func(params []byte)),
		errorExtractor: extractor,
	}

	if err := client.connect(); err != nil {
		// Don't fail on initial connection — start reconnect loop in background.
		// The upstream may not be available at startup but will be retried.
		logger.Warn().Err(err).Str("url", parsedUrl.String()).Msg("initial websocket connection failed, will retry in background")
		go client.reconnect()
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

	// Serialize the JSON-RPC request
	jrReq.RLock()
	requestBody, err := common.SonicCfg.Marshal(map[string]interface{}{
		"jsonrpc": jrReq.JSONRPC,
		"id":      jrReq.ID,
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

	// Create a unique string key for the pending map from the request ID.
	// Must use normalizeIDKey to handle JSON number parsing (float64 scientific notation).
	idKey := normalizeIDKey(jrReq.ID)

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

// SetOnReconnect sets a callback that fires after a successful reconnection.
// Used by the subscription manager to re-subscribe after upstream reconnect.
func (c *WsJsonRpcClient) SetOnReconnect(callback func()) {
	c.onReconnectMu.Lock()
	c.onReconnect = callback
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
	defer close(c.readDone)

	for {
		if c.appCtx.Err() != nil {
			return
		}

		c.connMu.Lock()
		conn := c.conn
		c.connMu.Unlock()

		if conn == nil {
			// Connection is being re-established
			time.Sleep(100 * time.Millisecond)
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
			c.reconnect()
			continue
		}

		c.handleMessage(message)
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

		c.pendingMu.Lock()
		ch, ok := c.pending[idKey]
		c.pendingMu.Unlock()

		if !ok {
			c.logger.Debug().Str("id", idKey).Msg("received response for unknown request ID")
			return
		}

		nr := common.NewNormalizedResponse().WithBody(io.NopCloser(strings.NewReader(string(message))))

		if msg.Error != nil {
			// Parse the error properly through the response's JsonRpcResponse
			ch <- &wsPendingResult{resp: nr}
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

		// Notify subscription manager for re-subscription
		c.onReconnectMu.RLock()
		cb := c.onReconnect
		c.onReconnectMu.RUnlock()
		if cb != nil {
			go cb()
		}

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
