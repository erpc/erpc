package websocket

import (
	"testing"
	"time"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	if !cfg.Enabled {
		t.Error("expected Enabled to be true by default")
	}

	if cfg.MaxConnectionsPerNetwork != 10000 {
		t.Errorf("expected MaxConnectionsPerNetwork to be 10000, got %d", cfg.MaxConnectionsPerNetwork)
	}

	if cfg.MaxSubscriptionsPerConnection != 100 {
		t.Errorf("expected MaxSubscriptionsPerConnection to be 100, got %d", cfg.MaxSubscriptionsPerConnection)
	}

	if cfg.PingInterval != 30*time.Second {
		t.Errorf("expected PingInterval to be 30s, got %v", cfg.PingInterval)
	}

	if cfg.PongTimeout != 60*time.Second {
		t.Errorf("expected PongTimeout to be 60s, got %v", cfg.PongTimeout)
	}

	if cfg.ReadBufferSize != 4096 {
		t.Errorf("expected ReadBufferSize to be 4096, got %d", cfg.ReadBufferSize)
	}

	if cfg.WriteBufferSize != 4096 {
		t.Errorf("expected WriteBufferSize to be 4096, got %d", cfg.WriteBufferSize)
	}
}
