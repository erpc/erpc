package clients

import (
	"context"
	"fmt"
	"net/url"
	"sync"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
)

type ClientType string

const (
	ClientTypeHttpJsonRpc ClientType = "HttpJsonRpc"
)

type ClientInterface interface {
	GetType() ClientType
}

type Client struct {
	Upstream common.Upstream
}

type ClientRegistry struct {
	logger    *zerolog.Logger
	projectId string
	clients   sync.Map
}

func NewClientRegistry(logger *zerolog.Logger, projectId string) *ClientRegistry {
	return &ClientRegistry{
		logger:    logger,
		projectId: projectId,
	}
}

func (manager *ClientRegistry) GetOrCreateClient(appCtx context.Context, ups common.Upstream) (ClientInterface, error) {
	if client, ok := manager.clients.Load(ups.Config().Endpoint); ok {
		return client.(ClientInterface), nil
	}

	return manager.CreateClient(appCtx, ups)
}

func (manager *ClientRegistry) CreateClient(appCtx context.Context, ups common.Upstream) (ClientInterface, error) {
	var once sync.Once
	var newClient ClientInterface
	var clientErr error

	cfg := ups.Config()

	parsedUrl, err := url.Parse(cfg.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("failed to parse URL for upstream: %v", cfg.Id)
	}

	proxyPoolRegistry, err := NewProxyPoolRegistry(common.GetConfig().ProxyPools, manager.logger)
	if err != nil {
		return nil, fmt.Errorf("failed to create proxy pool registry: %v. will use fallback http client", err)
	}

	if err != nil {
		clientErr = fmt.Errorf("failed to parse URL for upstream: %v", cfg.Id)
	} else {
		once.Do(func() {
			switch cfg.Type {
			case common.UpstreamTypeEvm:
				if parsedUrl.Scheme == "http" || parsedUrl.Scheme == "https" {
					lg := manager.logger.With().Str("upstreamId", cfg.Id).Logger()
					newClient, err = NewGenericHttpJsonRpcClient(
						appCtx,
						&lg,
						manager.projectId,
						cfg.Id,
						parsedUrl,
						cfg.JsonRpc,
						proxyPoolRegistry,
					)
					if err != nil {
						clientErr = fmt.Errorf("failed to create HTTP client for upstream: %v", cfg.Id)
					}
				} else if parsedUrl.Scheme == "ws" || parsedUrl.Scheme == "wss" {
					clientErr = fmt.Errorf("websocket client not implemented yet")
				} else {
					clientErr = fmt.Errorf("unsupported endpoint scheme: %v for upstream: %v", parsedUrl.Scheme, cfg.Id)
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
