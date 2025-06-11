package erpc

import (
	"context"
	"fmt"
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
		adminAuthRegistry, err = auth.NewAuthRegistry(logger, "admin", cfg.Admin.Auth, rateLimitersRegistry)
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

func (e *ERPC) Bootstrap(ctx context.Context) error {
	err := e.projectsRegistry.Bootstrap(ctx)
	if err != nil {
		e.logger.Warn().Err(err).Msg("could not bootstrap projects on first attempt (will keep retrying in the background)")
	}

	return nil
}

func (e *ERPC) AdminAuthenticate(ctx context.Context, method string, ap *auth.AuthPayload) error {
	if e.adminAuthRegistry != nil {
		err := e.adminAuthRegistry.Authenticate(ctx, method, ap)
		if err != nil {
			return err
		}
	}
	return nil
}

func (e *ERPC) AdminHandleRequest(ctx context.Context, nq *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	method, err := nq.Method()
	if err != nil {
		return nil, err
	}

	switch method {
	case "erpc_taxonomy":
		jrr, err := nq.JsonRpcRequest()
		if err != nil {
			return nil, err
		}
		type taxonomyUpstream struct {
			Id string `json:"id"`
		}
		type taxonomyNetwork struct {
			Id        string              `json:"id"`
			Upstreams []*taxonomyUpstream `json:"upstreams"`
		}
		type taxonomyProject struct {
			Id       string             `json:"id"`
			Networks []*taxonomyNetwork `json:"networks"`
		}
		type taxonomyResult struct {
			Projects []*taxonomyProject `json:"projects"`
		}
		result := &taxonomyResult{}
		projects := e.GetProjects()
		for _, p := range projects {
			networks := []*taxonomyNetwork{}
			for _, n := range p.GetNetworks() {
				ntw := &taxonomyNetwork{
					Id:        n.Id(),
					Upstreams: []*taxonomyUpstream{},
				}
				upstreams := n.upstreamsRegistry.GetNetworkUpstreams(ctx, n.Id())
				for _, u := range upstreams {
					ntw.Upstreams = append(ntw.Upstreams, &taxonomyUpstream{Id: u.Id()})
				}
				networks = append(networks, ntw)
			}
			result.Projects = append(result.Projects, &taxonomyProject{
				Id:       p.Config.Id,
				Networks: networks,
			})
		}
		jrrs, err := common.NewJsonRpcResponse(
			jrr.ID,
			result,
			nil,
		)
		if err != nil {
			return nil, err
		}
		return common.NewNormalizedResponse().WithJsonRpcResponse(jrrs), nil

	case "erpc_config":
		jrr, err := nq.JsonRpcRequest()
		if err != nil {
			return nil, err
		}

		jrrs, err := common.NewJsonRpcResponse(
			jrr.ID,
			e.cfg,
			nil,
		)
		if err != nil {
			return nil, err
		}
		return common.NewNormalizedResponse().WithJsonRpcResponse(jrrs), nil

	case "erpc_project":
		jrr, err := nq.JsonRpcRequest()
		if err != nil {
			return nil, err
		}
		type configResult struct {
			Config *common.ProjectConfig `json:"config"`
			Health *ProjectHealthInfo    `json:"health"`
		}
		if len(jrr.Params) == 0 {
			return nil, common.NewErrInvalidRequest(fmt.Errorf("project id (params[0]) is required"))
		}
		pid, ok := jrr.Params[0].(string)
		if !ok {
			return nil, common.NewErrInvalidRequest(fmt.Errorf("project id (params[0]) must be a string"))
		}
		p, err := e.GetProject(pid)
		if err != nil {
			return nil, err
		}
		health, err := p.GatherHealthInfo()
		if err != nil {
			return nil, err
		}
		result := configResult{
			Config: p.Config,
			Health: health,
		}
		jrrs, err := common.NewJsonRpcResponse(
			jrr.ID,
			result,
			nil,
		)
		if err != nil {
			return nil, err
		}
		return common.NewNormalizedResponse().WithJsonRpcResponse(jrrs), nil
	default:
		return nil, common.NewErrEndpointUnsupported(
			fmt.Errorf("admin method %s is not supported", method),
		)
	}
}

func (e *ERPC) GetNetwork(ctx context.Context, projectId string, networkId string) (*Network, error) {
	prj, err := e.GetProject(projectId)
	if err != nil {
		return nil, err
	}

	return prj.GetNetwork(networkId)
}

func (e *ERPC) GetProject(projectId string) (*PreparedProject, error) {
	return e.projectsRegistry.GetProject(projectId)
}

func (e *ERPC) GetProjects() []*PreparedProject {
	return e.projectsRegistry.GetAll()
}
