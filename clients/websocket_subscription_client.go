package clients

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
)

const (
	// WebSocket connection parameters
	wsWriteWait      = 10 * time.Second
	wsPongWait       = 60 * time.Second
	wsPingPeriod     = 30 * time.Second
	wsMaxMessageSize = 1024 * 1024 // 1MB

	// Reconnection parameters
	wsInitialBackoff = 1 * time.Second
	wsMaxBackoff     = 60 * time.Second
	wsBackoffFactor  = 2.0
)

// BlockHeader represents an Ethereum block header from newHeads subscription
type BlockHeader struct {
	Number    *HexBigInt `json:"number"`
	Hash      string     `json:"hash"`
	Timestamp *HexBigInt `json:"timestamp"`
}

// HexBigInt is a big.Int that marshals/unmarshals as a hex string
type HexBigInt big.Int

func (h *HexBigInt) UnmarshalJSON(data []byte) error {
	var hexStr string
	if err := json.Unmarshal(data, &hexStr); err != nil {
		return err
	}

	// Remove 0x prefix if present
	if len(hexStr) >= 2 && hexStr[:2] == "0x" {
		hexStr = hexStr[2:]
	}

	i := new(big.Int)
	if _, ok := i.SetString(hexStr, 16); !ok {
		return fmt.Errorf("invalid hex number: %s", hexStr)
	}
	*h = HexBigInt(*i)
	return nil
}

func (h *HexBigInt) Int64() int64 {
	if h == nil {
		return 0
	}
	return (*big.Int)(h).Int64()
}

// jsonRpcMessage represents a JSON-RPC message
type jsonRpcMessage struct {
	Version string          `json:"jsonrpc"`
	ID      interface{}     `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonRpcError   `json:"error,omitempty"`
}

type jsonRpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// subscriptionNotification is the notification from a subscription
type subscriptionNotification struct {
	Subscription string          `json:"subscription"`
	Result       json.RawMessage `json:"result"`
}

// WebsocketSubscriptionClient manages WebSocket connections for Ethereum subscriptions
type WebsocketSubscriptionClient struct {
	endpoint string
	logger   *zerolog.Logger

	conn      *websocket.Conn
	connMu    sync.Mutex
	connected atomic.Bool

	subscriptionID string
	blockChan      chan *BlockHeader

	// Reconnection state
	currentBackoff time.Duration

	// Shutdown coordination
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewWebsocketSubscriptionClient creates a new WebSocket subscription client
func NewWebsocketSubscriptionClient(endpoint string, logger *zerolog.Logger) *WebsocketSubscriptionClient {
	lg := logger.With().Str("component", "wsSubscriptionClient").Str("endpoint", endpoint).Logger()
	ctx, cancel := context.WithCancel(context.Background())

	return &WebsocketSubscriptionClient{
		endpoint:       endpoint,
		logger:         &lg,
		blockChan:      make(chan *BlockHeader, 100),
		currentBackoff: wsInitialBackoff,
		ctx:            ctx,
		cancel:         cancel,
	}
}

// Connect establishes a WebSocket connection to the endpoint
func (c *WebsocketSubscriptionClient) Connect(ctx context.Context) error {
	c.connMu.Lock()
	defer c.connMu.Unlock()

	if c.connected.Load() {
		return nil
	}

	c.logger.Debug().Msg("connecting to websocket endpoint")

	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}

	conn, _, err := dialer.DialContext(ctx, c.endpoint, nil)
	if err != nil {
		c.logger.Warn().Err(err).Msg("failed to connect to websocket endpoint")
		return fmt.Errorf("websocket dial failed: %w", err)
	}

	conn.SetReadLimit(wsMaxMessageSize)
	conn.SetReadDeadline(time.Now().Add(wsPongWait))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(wsPongWait))
		return nil
	})

	c.conn = conn
	c.connected.Store(true)
	c.currentBackoff = wsInitialBackoff

	c.logger.Info().Msg("connected to websocket endpoint")

	// Start ping goroutine
	c.wg.Add(1)
	go c.pingLoop()

	return nil
}

// SubscribeNewHeads subscribes to newHeads and returns a channel of block headers
func (c *WebsocketSubscriptionClient) SubscribeNewHeads(ctx context.Context) (<-chan *BlockHeader, error) {
	c.connMu.Lock()
	if !c.connected.Load() {
		c.connMu.Unlock()
		return nil, fmt.Errorf("not connected")
	}

	// Send subscription request
	req := jsonRpcMessage{
		Version: "2.0",
		ID:      1,
		Method:  "eth_subscribe",
		Params:  json.RawMessage(`["newHeads"]`),
	}

	c.conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
	if err := c.conn.WriteJSON(req); err != nil {
		c.connMu.Unlock()
		return nil, fmt.Errorf("failed to send subscription request: %w", err)
	}

	// Read subscription response
	var resp jsonRpcMessage
	if err := c.conn.ReadJSON(&resp); err != nil {
		c.connMu.Unlock()
		return nil, fmt.Errorf("failed to read subscription response: %w", err)
	}

	if resp.Error != nil {
		c.connMu.Unlock()
		return nil, fmt.Errorf("subscription error: %s (code %d)", resp.Error.Message, resp.Error.Code)
	}

	var subResult string
	if err := json.Unmarshal(resp.Result, &subResult); err != nil {
		c.connMu.Unlock()
		return nil, fmt.Errorf("failed to parse subscription ID: %w", err)
	}

	c.subscriptionID = subResult
	c.connMu.Unlock()

	c.logger.Info().Str("subscriptionId", c.subscriptionID).Msg("subscribed to newHeads")

	// Start read loop
	c.wg.Add(1)
	go c.readLoop()

	return c.blockChan, nil
}

// readLoop reads messages from the WebSocket connection
func (c *WebsocketSubscriptionClient) readLoop() {
	defer c.wg.Done()
	defer c.handleDisconnect()

	for {
		select {
		case <-c.ctx.Done():
			return
		default:
		}

		c.connMu.Lock()
		conn := c.conn
		c.connMu.Unlock()

		if conn == nil {
			return
		}

		var msg jsonRpcMessage
		if err := conn.ReadJSON(&msg); err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				c.logger.Warn().Err(err).Msg("websocket read error")
			}
			return
		}

		// Handle subscription notification
		if msg.Method == "eth_subscription" {
			var notification subscriptionNotification
			if err := json.Unmarshal(msg.Params, &notification); err != nil {
				c.logger.Warn().Err(err).Msg("failed to parse subscription notification")
				continue
			}

			if notification.Subscription != c.subscriptionID {
				continue
			}

			var header BlockHeader
			if err := json.Unmarshal(notification.Result, &header); err != nil {
				c.logger.Warn().Err(err).Msg("failed to parse block header")
				continue
			}

			select {
			case c.blockChan <- &header:
				c.logger.Debug().
					Int64("blockNumber", header.Number.Int64()).
					Str("hash", header.Hash).
					Msg("received new block header")
			default:
				c.logger.Warn().Msg("block channel full, dropping block header")
			}
		}
	}
}

// pingLoop sends periodic ping messages to keep the connection alive
func (c *WebsocketSubscriptionClient) pingLoop() {
	defer c.wg.Done()

	ticker := time.NewTicker(wsPingPeriod)
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			c.connMu.Lock()
			conn := c.conn
			c.connMu.Unlock()

			if conn == nil {
				return
			}

			conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				c.logger.Warn().Err(err).Msg("ping failed")
				return
			}
		}
	}
}

// handleDisconnect handles connection disconnection
func (c *WebsocketSubscriptionClient) handleDisconnect() {
	c.connMu.Lock()
	defer c.connMu.Unlock()

	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
	c.connected.Store(false)
	c.subscriptionID = ""

	c.logger.Info().Msg("websocket disconnected")
}

// Close closes the WebSocket connection
func (c *WebsocketSubscriptionClient) Close() error {
	c.cancel()

	c.connMu.Lock()
	if c.conn != nil {
		// Send close message
		c.conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
		c.conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		c.conn.Close()
		c.conn = nil
	}
	c.connected.Store(false)
	c.connMu.Unlock()

	c.wg.Wait()
	close(c.blockChan)

	c.logger.Info().Msg("websocket client closed")
	return nil
}

// IsConnected returns whether the client is currently connected
func (c *WebsocketSubscriptionClient) IsConnected() bool {
	return c.connected.Load()
}

// GetBackoff returns the current backoff duration for reconnection
func (c *WebsocketSubscriptionClient) GetBackoff() time.Duration {
	backoff := c.currentBackoff
	c.currentBackoff = time.Duration(float64(c.currentBackoff) * wsBackoffFactor)
	if c.currentBackoff > wsMaxBackoff {
		c.currentBackoff = wsMaxBackoff
	}
	return backoff
}

// ResetBackoff resets the backoff to the initial value
func (c *WebsocketSubscriptionClient) ResetBackoff() {
	c.currentBackoff = wsInitialBackoff
}
