package websocket

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
)

// ConnectionManager manages all WebSocket connections for a specific network
type ConnectionManager struct {
	networkInfo NetworkInfo
	forwardFunc ForwardFunc
	config      *Config

	connections sync.Map // *Connection → bool
	connCount   atomic.Int32

	// TODO: Will be initialized in Phase 2
	// subManager    *subscription.Manager

	logger *zerolog.Logger
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.RWMutex
}

// NewConnectionManager creates a new connection manager for a network
func NewConnectionManager(
	ctx context.Context,
	networkInfo NetworkInfo,
	forwardFunc ForwardFunc,
	logger *zerolog.Logger,
	config *Config,
) *ConnectionManager {
	ctx, cancel := context.WithCancel(ctx)

	lg := logger.With().
		Str("component", "connection_manager").
		Str("networkId", networkInfo.Id()).
		Logger()

	return &ConnectionManager{
		networkInfo: networkInfo,
		forwardFunc: forwardFunc,
		config:      config,
		logger:      &lg,
		ctx:         ctx,
		cancel:      cancel,
	}
}

// AddConnection registers a new connection
func (cm *ConnectionManager) AddConnection(conn *Connection) {
	cm.connections.Store(conn, true)
	cm.connCount.Add(1)

	cm.logger.Debug().
		Str("connId", conn.ID()).
		Int("totalConnections", int(cm.connCount.Load())).
		Msg("connection added")
}

// RemoveConnection unregisters a connection
func (cm *ConnectionManager) RemoveConnection(conn *Connection) {
	cm.connections.Delete(conn)
	cm.connCount.Add(-1)

	cm.logger.Debug().
		Str("connId", conn.ID()).
		Int("totalConnections", int(cm.connCount.Load())).
		Msg("connection removed")

	// TODO: Phase 2 - Remove all subscriptions for this connection
	// if cm.subManager != nil {
	//     cm.subManager.UnregisterCallback(conn.ID())
	// }
}

// ConnectionCount returns the current number of active connections
func (cm *ConnectionManager) ConnectionCount() int {
	return int(cm.connCount.Load())
}

// Shutdown gracefully shuts down all connections
func (cm *ConnectionManager) Shutdown() {
	cm.logger.Info().Msg("shutting down connection manager")

	// Cancel context to stop all operations
	cm.cancel()

	// TODO: Phase 2 - Stop subscription manager
	// if cm.subManager != nil {
	//     cm.subManager.Shutdown()
	// }

	// Close all connections
	var wg sync.WaitGroup
	cm.connections.Range(func(key, value interface{}) bool {
		conn := key.(*Connection)
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn.Close()
		}()
		return true
	})

	// Wait for all connections to close (with timeout)
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		cm.logger.Info().Msg("all connections closed gracefully")
	case <-time.After(5 * time.Second):
		cm.logger.Warn().Msg("timeout waiting for connections to close")
	}
}

// Context returns the manager's context
func (cm *ConnectionManager) Context() context.Context {
	return cm.ctx
}
