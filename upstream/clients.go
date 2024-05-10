package upstream

import (
	"net/url"
	"sync"

	"github.com/rs/zerolog/log"
)

// Define a shared interface for all types of Clients
type ClientInterface interface {
	GetType() string
}

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
func (manager *ClientManager) GetOrCreateClient(upstream *PreparedUpstream) ClientInterface {
	// Attempt to load an existing client
	if client, ok := manager.clients.Load(upstream.Endpoint); ok {
		return client.(ClientInterface)
	}

	// Create a new client for the endpoint if not already present
	var once sync.Once
	var newClient ClientInterface

	once.Do(func() {
		switch upstream.Architecture {
		case ArchitectureEvm:
			parsedUrl, err := url.Parse(upstream.Endpoint)
			if err != nil {
				log.Fatal().Msgf("failed to parse URL for upstream: %v", upstream.Id)
			}

			if parsedUrl.Scheme == "http" || parsedUrl.Scheme == "https" {
				newClient, err = NewHttpJsonRpcClient(parsedUrl)
				if err != nil {
					log.Fatal().Msgf("failed to create HTTP client for upstream: %v", upstream.Id)
				}
			} else if parsedUrl.Scheme == "ws" || parsedUrl.Scheme == "wss" {
				// newClient = &WebSocketEvmRpcClient{Url: parsedUrl}
				log.Fatal().Msgf("websocket client not implemented yet")
			} else {
				log.Fatal().Msgf("unsupported EVM scheme: %v for upstream: %v", parsedUrl.Scheme, upstream.Id)
			}
		default:
			log.Fatal().Msgf("unsupported architecture: %v for upstream: %v", upstream.Architecture, upstream.Id)
		}

		manager.clients.Store(upstream.Endpoint, newClient)
	})

	return newClient
}
