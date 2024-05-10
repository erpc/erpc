package upstream

import "sync"

type Client struct {
	Upstream *PreparedUpstream
}

// ClientManager manages client instances
type ClientManager struct {
	clients sync.Map
}

// NewClientManager creates a new client manager
func NewClientManager() *ClientManager {
	return &ClientManager{}
}

// GetOrCreateClient retrieves an existing client for a given endpoint or creates a new one if it doesn't exist
func (manager *ClientManager) GetOrCreateClient(upstream *PreparedUpstream) *Client {
	// Attempt to load an existing client
	if client, ok := manager.clients.Load(upstream.Endpoint); ok {
		return client.(*Client)
	}

	// Create a new client for the endpoint if not already present
	var once sync.Once
	var client *Client
	once.Do(func() {
		client = &Client{Upstream: upstream}
		manager.clients.Store(upstream.Endpoint, client)
	})

	return client
}
