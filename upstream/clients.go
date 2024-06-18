package upstream

import (
	"fmt"
	"net/url"
	"sync"

	"github.com/flair-sdk/erpc/common"
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

func (manager *ClientRegistry) CreateClient(ups *PreparedUpstream) (ClientInterface, error) {
	// Create a new client for the endpoint if not already present
	var once sync.Once
	var newClient ClientInterface
	var clientErr error

	once.Do(func() {
		switch ups.Architecture {
		case common.ArchitectureEvm:
			parsedUrl, err := url.Parse(ups.Endpoint)
			if err != nil {
				clientErr = fmt.Errorf("failed to parse URL for upstream: %v", ups.Id)
			} else {
				if parsedUrl.Scheme == "http" || parsedUrl.Scheme == "https" {
					newClient, err = NewHttpJsonRpcClient(ups, parsedUrl)
					if err != nil {
						clientErr = fmt.Errorf("failed to create HTTP client for upstream: %v", ups.Id)
					}
				} else if parsedUrl.Scheme == "ws" || parsedUrl.Scheme == "wss" {
					clientErr = fmt.Errorf("websocket client not implemented yet")
				} else {
					clientErr = fmt.Errorf("unsupported EVM scheme: %v for upstream: %v", parsedUrl.Scheme, ups.Id)
				}
			}
		default:
			clientErr = fmt.Errorf("unsupported architecture: %v for upstream: %v", ups.Architecture, ups.Id)
		}

		if clientErr != nil {
			manager.clients.Store(ups.Endpoint, newClient)
		}
	})

	return newClient, clientErr
}
