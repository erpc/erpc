package upstream

import (
	"fmt"
	"net/url"
	"sync"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
)

type ClientType string

const (
	ClientTypeHttpJsonRpc          ClientType = "HttpJsonRpc"
	ClientTypeAlchemyHttpJsonRpc   ClientType = "AlchemyHttpJsonRpc"
	ClientTypeEnvioHttpJsonRpc     ClientType = "EnvioHttpJsonRpc"
	ClientTypePimlicoHttpJsonRpc   ClientType = "PimlicoHttpJsonRpc"
	ClientTypeEtherspotHttpJsonRpc ClientType = "EtherspotHttpJsonRpc"
	ClientTypeThirdwebHttpJsonRpc  ClientType = "ThirdwebHttpJsonRpc"
)

// Define a shared interface for all types of Clients
type ClientInterface interface {
	GetType() ClientType
	SupportsNetwork(networkId string) (bool, error)
}

type Client struct {
	Upstream *Upstream
}

// ClientRegistry manages client instances
type ClientRegistry struct {
	logger  *zerolog.Logger
	clients sync.Map
}

// NewClientRegistry creates a new client registry
func NewClientRegistry(logger *zerolog.Logger) *ClientRegistry {
	return &ClientRegistry{logger: logger}
}

// GetOrCreateClient retrieves an existing client for a given endpoint or creates a new one if it doesn't exist
func (manager *ClientRegistry) GetOrCreateClient(ups *Upstream) (ClientInterface, error) {
	// Attempt to load an existing client
	if client, ok := manager.clients.Load(ups.Config().Endpoint); ok {
		return client.(ClientInterface), nil
	}

	return manager.CreateClient(ups)
}

func (manager *ClientRegistry) CreateClient(ups *Upstream) (ClientInterface, error) {
	// Create a new client for the endpoint if not already present
	var once sync.Once
	var newClient ClientInterface
	var clientErr error

	cfg := ups.Config()
	parsedUrl, err := url.Parse(cfg.Endpoint)
	if err != nil {
		clientErr = fmt.Errorf("failed to parse URL for upstream: %v", cfg.Id)
	} else {
		once.Do(func() {
			switch cfg.Type {
			case common.UpstreamTypeEvm:
				if parsedUrl.Scheme == "http" || parsedUrl.Scheme == "https" {
					newClient, err = NewGenericHttpJsonRpcClient(manager.logger, ups, parsedUrl)
					if err != nil {
						clientErr = fmt.Errorf("failed to create HTTP client for upstream: %v", cfg.Id)
					}
				} else if parsedUrl.Scheme == "ws" || parsedUrl.Scheme == "wss" {
					clientErr = fmt.Errorf("websocket client not implemented yet")
				} else {
					clientErr = fmt.Errorf("unsupported endpoint scheme: %v for upstream: %v", parsedUrl.Scheme, cfg.Id)
				}

			case common.UpstreamTypeEvmAlchemy:
				newClient, err = NewAlchemyHttpJsonRpcClient(ups, parsedUrl)
				if err != nil {
					clientErr = fmt.Errorf("failed to create Alchemy client for upstream: %v", cfg.Id)
				}

			case common.UpstreamTypeEvmThirdweb:
				newClient, err = NewThirdwebHttpJsonRpcClient(ups, parsedUrl)
				if err != nil {
					clientErr = fmt.Errorf("failed to create Thirdweb client for upstream: %v", cfg.Id)
				}

			case common.UpstreamTypeEvmEnvio:
				newClient, err = NewEnvioHttpJsonRpcClient(ups, parsedUrl)
				if err != nil {
					clientErr = fmt.Errorf("failed to create Envio client for upstream: %v", cfg.Id)
				}

			case common.UpstreamTypeEvmPimlico:
				newClient, err = NewPimlicoHttpJsonRpcClient(ups, parsedUrl)
				if err != nil {
					clientErr = fmt.Errorf("failed to create Pimlico client for upstream: %v", cfg.Id)
				}

			case common.UpstreamTypeEvmEtherspot:
				newClient, err = NewEtherspotHttpJsonRpcClient(ups, parsedUrl)
				if err != nil {
					clientErr = fmt.Errorf("failed to create Etherspot client for upstream: %v", cfg.Id)
				}

			default:
				clientErr = fmt.Errorf("unsupported upstream type: %v for upstream: %v", cfg.Type, cfg.Id)
			}

			if clientErr == nil {
				manager.clients.Store(cfg.Endpoint, newClient)
			}
		})
	}

	return newClient, clientErr
}
