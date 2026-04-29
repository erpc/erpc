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
	ClientTypeGrpcBds     ClientType = "GrpcBds"
	ClientTypeWsJsonRpc   ClientType = "WsJsonRpc"
)

type ClientInterface interface {
	GetType() ClientType
	SendRequest(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error)
}

type Client struct {
	Upstream common.Upstream
}

// clientCreation memoises the once-per-upstream client construction. Sharing
// the sync.Once across CreateClient calls is the correctness-critical part:
// previously `var once sync.Once` was declared locally so every call ran the
// body, and two concurrent callers that both missed the cache could each
// spawn a client and its goroutines, with only the last winning Store — the
// losing client (and its <-appCtx.Done() shutdown waiter, ping/read loops,
// etc.) leaked for the lifetime of the process.
type clientCreation struct {
	once   sync.Once
	client ClientInterface
	err    error
}

type ClientRegistry struct {
	logger            *zerolog.Logger
	projectId         string
	clients           sync.Map // upstream key -> ClientInterface (read-fast path)
	clientCreations   sync.Map // upstream key -> *clientCreation (build coordination)
	proxyPoolRegistry *ProxyPoolRegistry
	evmExtractor      common.JsonRpcErrorExtractor
}

func NewClientRegistry(logger *zerolog.Logger, projectId string, proxyPoolRegistry *ProxyPoolRegistry, evmExtractor common.JsonRpcErrorExtractor) *ClientRegistry {
	cr := &ClientRegistry{
		logger:            logger,
		projectId:         projectId,
		proxyPoolRegistry: proxyPoolRegistry,
		evmExtractor:      evmExtractor,
	}
	return cr
}

func (manager *ClientRegistry) GetOrCreateClient(appCtx context.Context, ups common.Upstream) (ClientInterface, error) {
	if client, ok := manager.clients.Load(common.UniqueUpstreamKey(ups)); ok {
		return client.(ClientInterface), nil
	}

	return manager.CreateClient(appCtx, ups)
}

func (manager *ClientRegistry) CreateClient(appCtx context.Context, ups common.Upstream) (ClientInterface, error) {
	cfg := ups.Config()

	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("upstream endpoint is required")
	}

	parsedUrl, err := url.Parse(cfg.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("failed to parse URL for upstream: %v", cfg.Id)
	}

	var proxyPool *ProxyPool
	if cfg.JsonRpc != nil && cfg.JsonRpc.ProxyPool != "" {
		proxyPool, err = manager.proxyPoolRegistry.GetPool(cfg.JsonRpc.ProxyPool)
		if err != nil {
			return nil, fmt.Errorf("failed to get proxy pool: %v", cfg.Id)
		}
	}

	upstreamKey := common.UniqueUpstreamKey(ups)
	cv, _ := manager.clientCreations.LoadOrStore(upstreamKey, &clientCreation{})
	creation := cv.(*clientCreation)

	creation.once.Do(func() {
		lg := manager.logger.With().Str("upstreamId", cfg.Id).Logger()
		var c ClientInterface
		var cerr error
		switch cfg.Type {
		case common.UpstreamTypeEvm:
			switch parsedUrl.Scheme {
			case "http", "https":
				c, cerr = NewGenericHttpJsonRpcClient(
					appCtx,
					&lg,
					manager.projectId,
					ups,
					parsedUrl,
					cfg.JsonRpc,
					proxyPool,
					manager.evmExtractor,
				)
				if cerr != nil {
					cerr = fmt.Errorf("failed to create HTTP client for upstream: %v: %w", cfg.Id, cerr)
				}
			case "ws", "wss":
				c, cerr = NewWsJsonRpcClient(
					appCtx,
					&lg,
					manager.projectId,
					ups,
					parsedUrl,
					cfg.JsonRpc,
					manager.evmExtractor,
				)
				if cerr != nil {
					cerr = fmt.Errorf("failed to create WebSocket client for upstream %v: %w", cfg.Id, cerr)
				}
			case "grpc", "grpc+bds":
				c, cerr = NewGrpcBdsClient(
					appCtx,
					&lg,
					manager.projectId,
					ups,
					parsedUrl,
				)
				if cerr != nil {
					cerr = fmt.Errorf("failed to create gRPC BDS client for upstream: %v: %w", cfg.Id, cerr)
				}
			default:
				cerr = fmt.Errorf("unsupported endpoint scheme: %v for upstream: %v", parsedUrl.Scheme, cfg.Id)
			}
		default:
			cerr = fmt.Errorf("unsupported upstream type: %v for upstream: %v", cfg.Type, cfg.Id)
		}
		creation.client = c
		creation.err = cerr
		if cerr == nil {
			manager.clients.Store(upstreamKey, c)
		}
	})

	return creation.client, creation.err
}
