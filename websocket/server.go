package websocket

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
)

// Server manages WebSocket connections and ConnectionManagers
type Server struct {
	config       *Config
	upgrader     websocket.Upgrader
	connManagers sync.Map // networkId → *ConnectionManager
	logger       *zerolog.Logger
	mu           sync.RWMutex
}

// NewServer creates a new WebSocket server
func NewServer(logger *zerolog.Logger, config *Config) *Server {
	if config == nil {
		config = DefaultConfig()
	}

	return &Server{
		config: config,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  config.ReadBufferSize,
			WriteBufferSize: config.WriteBufferSize,
			CheckOrigin: func(r *http.Request) bool {
				// CORS is handled at HTTP layer, always allow upgrade
				return true
			},
		},
		logger: logger,
	}
}

// IsWebSocketUpgrade checks if the HTTP request is a WebSocket upgrade request
func IsWebSocketUpgrade(r *http.Request) bool {
	return websocket.IsWebSocketUpgrade(r)
}

// Upgrade upgrades an HTTP connection to WebSocket
func (s *Server) Upgrade(
	w http.ResponseWriter,
	r *http.Request,
	networkInfo NetworkInfo,
	forwardFunc ForwardFunc,
) error {
	if !s.config.Enabled {
		http.Error(w, "WebSocket is not enabled", http.StatusServiceUnavailable)
		return fmt.Errorf("websocket is not enabled")
	}

	// Get or create ConnectionManager for this network
	manager := s.GetOrCreateManager(r.Context(), networkInfo, forwardFunc)

	// Check connection limit
	if manager.ConnectionCount() >= s.config.MaxConnectionsPerNetwork {
		s.logger.Warn().
			Str("networkId", networkInfo.Id()).
			Int("count", manager.ConnectionCount()).
			Msg("connection limit reached, rejecting new connection")
		http.Error(w, "Connection limit reached", http.StatusServiceUnavailable)
		return fmt.Errorf("connection limit reached for network %s", networkInfo.Id())
	}

	// Upgrade the connection
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.logger.Error().Err(err).Msg("failed to upgrade connection")
		return err
	}

	// Create and start the connection
	wsConn := NewConnection(conn, manager, s.logger, s.config)
	manager.AddConnection(wsConn)

	s.logger.Info().
		Str("connId", wsConn.ID()).
		Str("networkId", networkInfo.Id()).
		Str("projectId", networkInfo.ProjectId()).
		Str("remoteAddr", r.RemoteAddr).
		Msg("websocket connection established")

	// Start handling the connection (non-blocking)
	go wsConn.Start()

	return nil
}

// GetOrCreateManager gets or creates a ConnectionManager for a network
func (s *Server) GetOrCreateManager(
	ctx context.Context,
	networkInfo NetworkInfo,
	forwardFunc ForwardFunc,
) *ConnectionManager {
	networkId := networkInfo.Id()

	// Try to load existing manager
	if val, ok := s.connManagers.Load(networkId); ok {
		return val.(*ConnectionManager)
	}

	// Create new manager
	s.mu.Lock()
	defer s.mu.Unlock()

	// Double-check after acquiring lock
	if val, ok := s.connManagers.Load(networkId); ok {
		return val.(*ConnectionManager)
	}

	manager := NewConnectionManager(ctx, networkInfo, forwardFunc, s.logger, s.config)
	s.connManagers.Store(networkId, manager)

	s.logger.Info().
		Str("networkId", networkId).
		Msg("created connection manager for network")

	return manager
}

// Shutdown gracefully shuts down all connection managers
func (s *Server) Shutdown() {
	s.logger.Info().Msg("shutting down websocket server")

	s.connManagers.Range(func(key, value interface{}) bool {
		manager := value.(*ConnectionManager)
		manager.Shutdown()
		return true
	})
}
