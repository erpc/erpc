package websocket

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"

	"github.com/bytedance/sonic"
	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
)

// Connection represents a single WebSocket client connection
type Connection struct {
	id      string
	conn    *websocket.Conn
	manager *ConnectionManager
	config  *Config

	subscriptions sync.Map // subId → bool (placeholder for Phase 2)

	send   chan []byte
	done   chan struct{}
	logger *zerolog.Logger
	mu     sync.RWMutex
}

// NewConnection creates a new WebSocket connection
func NewConnection(
	conn *websocket.Conn,
	manager *ConnectionManager,
	logger *zerolog.Logger,
	config *Config,
) *Connection {
	id := generateConnectionId()

	lg := logger.With().
		Str("component", "ws_connection").
		Str("connId", id).
		Logger()

	return &Connection{
		id:      id,
		conn:    conn,
		manager: manager,
		config:  config,
		send:    make(chan []byte, 256),
		done:    make(chan struct{}),
		logger:  &lg,
	}
}

// ID returns the connection ID
func (c *Connection) ID() string {
	return c.id
}

// Start begins reading and writing to the connection
func (c *Connection) Start() {
	defer func() {
		c.manager.RemoveConnection(c)
		c.cleanup()
	}()

	// Start writer goroutine
	go c.writer()

	// Start reader (blocking)
	c.reader()
}

// reader reads messages from the WebSocket connection
func (c *Connection) reader() {
	defer func() {
		close(c.done)
	}()

	// Set pong handler for ping/pong
	c.conn.SetReadDeadline(time.Now().Add(c.config.PongTimeout))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(c.config.PongTimeout))
		return nil
	})

	// Set max message size (1MB)
	c.conn.SetReadLimit(1024 * 1024)

	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure, websocket.CloseNormalClosure) {
				c.logger.Warn().Err(err).Msg("websocket read error")
			} else {
				c.logger.Debug().Err(err).Msg("websocket connection closed")
			}
			return
		}

		// Handle the message
		if err := c.handleMessage(message); err != nil {
			c.logger.Error().Err(err).RawJSON("message", message).Msg("failed to handle message")
		}
	}
}

// writer writes messages to the WebSocket connection
func (c *Connection) writer() {
	ticker := time.NewTicker(c.config.PingInterval)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()

	for {
		select {
		case message, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if !ok {
				// Channel closed
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			if err := c.conn.WriteMessage(websocket.TextMessage, message); err != nil {
				c.logger.Error().Err(err).Msg("failed to write message")
				return
			}

		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				c.logger.Debug().Err(err).Msg("failed to write ping")
				return
			}

		case <-c.done:
			return
		}
	}
}

// handleMessage processes incoming JSON-RPC messages
func (c *Connection) handleMessage(data []byte) error {
	var req JsonRpcRequest
	if err := sonic.Unmarshal(data, &req); err != nil {
		c.logger.Error().Err(err).RawJSON("data", data).Msg("failed to parse message")
		resp := NewErrorResponse(nil, ErrCodeParseError, "Parse error", err.Error())
		return c.sendResponse(resp)
	}

	c.logger.Debug().
		Str("method", req.Method).
		Interface("id", req.Id).
		Msg("received message")

	// Route based on method
	switch req.Method {
	case "eth_subscribe":
		return c.handleSubscribe(&req)
	case "eth_unsubscribe":
		return c.handleUnsubscribe(&req)
	default:
		// Unknown method
		resp := NewErrorResponse(req.Id, ErrCodeMethodNotFound, "Method not found",
			"Only eth_subscribe and eth_unsubscribe are supported over WebSocket")
		return c.sendResponse(resp)
	}
}

// handleSubscribe handles eth_subscribe requests
// TODO: Phase 2 - Implement full subscription logic
func (c *Connection) handleSubscribe(req *JsonRpcRequest) error {
	c.logger.Debug().Interface("params", req.Params).Msg("subscribe request")

	// For now, return not implemented
	resp := NewErrorResponse(req.Id, ErrCodeInternalError,
		"Subscriptions not yet implemented",
		"Phase 2 implementation pending")
	return c.sendResponse(resp)
}

// handleUnsubscribe handles eth_unsubscribe requests
// TODO: Phase 2 - Implement full unsubscribe logic
func (c *Connection) handleUnsubscribe(req *JsonRpcRequest) error {
	c.logger.Debug().Interface("params", req.Params).Msg("unsubscribe request")

	// For now, return not implemented
	resp := NewErrorResponse(req.Id, ErrCodeInternalError,
		"Subscriptions not yet implemented",
		"Phase 2 implementation pending")
	return c.sendResponse(resp)
}

// sendResponse sends a JSON-RPC response to the client
func (c *Connection) sendResponse(resp *JsonRpcResponse) error {
	data, err := sonic.Marshal(resp)
	if err != nil {
		return err
	}

	select {
	case c.send <- data:
		return nil
	case <-c.done:
		return websocket.ErrCloseSent
	}
}

// SendNotification sends a subscription notification to the client
// TODO: Phase 2 - Use this for actual notifications
func (c *Connection) SendNotification(subId string, result interface{}) error {
	notification := NewNotification(subId, result)
	data, err := json.Marshal(notification)
	if err != nil {
		return err
	}

	select {
	case c.send <- data:
		return nil
	case <-c.done:
		return websocket.ErrCloseSent
	}
}

// Close gracefully closes the connection
func (c *Connection) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()

	select {
	case <-c.done:
		// Already closed
		return
	default:
		c.logger.Info().Msg("closing websocket connection")

		// Send close message
		c.conn.SetWriteDeadline(time.Now().Add(time.Second))
		c.conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))

		// Close send channel to stop writer
		close(c.send)

		// Wait a bit for graceful close
		time.Sleep(time.Second)

		// Force close
		c.conn.Close()
	}
}

// cleanup cleans up connection resources
func (c *Connection) cleanup() {
	c.logger.Debug().Msg("cleaning up connection")
	// TODO: Phase 2 - Clean up subscriptions
}

// generateConnectionId generates a unique connection ID
func generateConnectionId() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
