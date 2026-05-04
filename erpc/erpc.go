package erpc

import (
	"context"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/auth"
	"github.com/erpc/erpc/clients"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/thirdparty"
	"github.com/erpc/erpc/upstream"
	"github.com/rs/zerolog"
)

type ERPC struct {
	cfg               *common.Config
	projectsRegistry  *ProjectsRegistry
	adminAuthRegistry *auth.AuthRegistry
	logger            *zerolog.Logger
}

func NewERPC(
	appCtx context.Context,
	logger *zerolog.Logger,
	sharedState data.SharedStateRegistry,
	evmJsonRpcCache *evm.EvmJsonRpcCache,
	cfg *common.Config,
) (*ERPC, error) {
	if err := common.InitializeTracing(appCtx, logger, cfg.Tracing); err != nil {
		logger.Error().Err(err).Msg("failed to initialize tracing")
	}

	rateLimitersRegistry, err := upstream.NewRateLimitersRegistry(
		appCtx,
		cfg.RateLimiters,
		logger,
	)
	if err != nil {
		return nil, err
	}

	proxyPoolRegistry, err := clients.NewProxyPoolRegistry(cfg.ProxyPools, logger)
	if err != nil {
		return nil, err
	}

	if sharedState == nil {
		cfg := &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems: 100_000, MaxTotalSize: "1GB",
				},
			},
		}
		err = cfg.SetDefaults(cfg.ClusterKey)
		if err != nil {
			return nil, err
		}
		sharedState, err = data.NewSharedStateRegistry(appCtx, logger, cfg)
		if err != nil {
			return nil, err
		}
	}
	vendorsRegistry := thirdparty.NewVendorsRegistry()
	projectRegistry, err := NewProjectsRegistry(
		appCtx,
		logger,
		cfg.Projects,
		sharedState,
		evmJsonRpcCache,
		rateLimitersRegistry,
		vendorsRegistry,
		proxyPoolRegistry,
	)
	if err != nil {
		return nil, err
	}

	var adminAuthRegistry *auth.AuthRegistry
	if cfg.Admin != nil && cfg.Admin.Auth != nil {
		adminAuthRegistry, err = auth.NewAuthRegistry(appCtx, logger, "admin", cfg.Admin.Auth, rateLimitersRegistry)
		if err != nil {
			return nil, err
		}
	}

	// Shutdown tracing after appCtx is finished/cancelled
	go func() {
		<-appCtx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := common.ShutdownTracing(shutdownCtx); err != nil {
			logger.Error().Err(err).Msg("failed to shutdown tracer provider")
		}
	}()

	return &ERPC{
		cfg:               cfg,
		projectsRegistry:  projectRegistry,
		adminAuthRegistry: adminAuthRegistry,
		logger:            logger,
	}, nil
}

func (e *ERPC) Bootstrap(ctx context.Context) {
	e.projectsRegistry.Bootstrap(ctx)
}

func (e *ERPC) GetNetwork(ctx context.Context, projectId string, networkId string) (*Network, error) {
	prj, err := e.GetProject(projectId)
	if err != nil {
		return nil, err
	}

	return prj.GetNetwork(ctx, networkId)
}

func (e *ERPC) GetProject(projectId string) (*PreparedProject, error) {
	return e.projectsRegistry.GetProject(projectId)
}

func (e *ERPC) GetProjects() []*PreparedProject {
	return e.projectsRegistry.GetAll()
}
