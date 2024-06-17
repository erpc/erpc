package upstream

import (
	"fmt"
	"net/url"
	"sync"
)

// Define a shared interface for all types of Clients
type ClientInterface interface {
	GetType() string
}

type Client struct {
	Upstream *PreparedUpstream
}

// ClientRegistry manages client instances
type ClientRegistry struct {
	clients sync.Map
}

// NewClientRegistry creates a new client registry
func NewClientRegistry() *ClientRegistry {
	return &ClientRegistry{}
}

// GetOrCreateClient retrieves an existing client for a given endpoint or creates a new one if it doesn't exist
func (manager *ClientRegistry) GetOrCreateClient(upstream *PreparedUpstream) (ClientInterface, error) {
	// Attempt to load an existing client
	if client, ok := manager.clients.Load(upstream.Endpoint); ok {
		return client.(ClientInterface), nil
	}

	return manager.CreateClient(upstream)
}

func (manager *ClientRegistry) CreateClient(upstream *PreparedUpstream) (ClientInterface, error) {
	// Create a new client for the endpoint if not already present
	var once sync.Once
	var newClient ClientInterface
	var clientErr error

	once.Do(func() {
		switch upstream.Architecture {
		case ArchitectureEvm:
			parsedUrl, err := url.Parse(upstream.Endpoint)
			if err != nil {
				clientErr = fmt.Errorf("failed to parse URL for upstream: %v", upstream.Id)
			} else {
				if parsedUrl.Scheme == "http" || parsedUrl.Scheme == "https" {
					newClient, err = NewHttpJsonRpcClient(upstream, parsedUrl)
					if err != nil {
						clientErr = fmt.Errorf("failed to create HTTP client for upstream: %v", upstream.Id)
					}
				} else if parsedUrl.Scheme == "ws" || parsedUrl.Scheme == "wss" {
					clientErr = fmt.Errorf("websocket client not implemented yet")
				} else {
					clientErr = fmt.Errorf("unsupported EVM scheme: %v for upstream: %v", parsedUrl.Scheme, upstream.Id)
				}
			}
		default:
			clientErr = fmt.Errorf("unsupported architecture: %v for upstream: %v", upstream.Architecture, upstream.Id)
		}

		if clientErr != nil {
			manager.clients.Store(upstream.Endpoint, newClient)
		}
	})

	return newClient, clientErr
}
