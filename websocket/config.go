package websocket

import "time"

// Config holds WebSocket-specific configuration
type Config struct {
	Enabled                       bool
	MaxConnectionsPerNetwork      int
	MaxSubscriptionsPerConnection int
	PingInterval                  time.Duration
	PongTimeout                   time.Duration
	ReadBufferSize                int
	WriteBufferSize               int
}

// DefaultConfig returns default WebSocket configuration
func DefaultConfig() *Config {
	return &Config{
		Enabled:                       true,
		MaxConnectionsPerNetwork:      10000,
		MaxSubscriptionsPerConnection: 100,
		PingInterval:                  30 * time.Second,
		PongTimeout:                   60 * time.Second,
		ReadBufferSize:                4096,
		WriteBufferSize:               4096,
	}
}
