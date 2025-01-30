package erpc

import (
	"context"
	"fmt"

	"github.com/erpc/erpc/auth"
	"github.com/erpc/erpc/clients"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/thirdparty"
	"github.com/erpc/erpc/upstream"
	"github.com/rs/zerolog"
)

type ERPC struct {
	cfg               *common.Config
	projectsRegistry  *ProjectsRegistry
	adminAuthRegistry *auth.AuthRegistry
}

func NewERPC(
	appCtx context.Context,
	logger *zerolog.Logger,
	evmJsonRpcCache *EvmJsonRpcCache,
	cfg *common.Config,
) (*ERPC, error) {
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

	vendorsRegistry := thirdparty.NewVendorsRegistry()
	projectRegistry, err := NewProjectsRegistry(
		appCtx,
		logger,
		cfg.Projects,
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

	go func() {
		err := projectRegistry.Bootstrap(appCtx)
		if err != nil {
			logger.Error().Err(err).Msg("failed to bootstrap projects on first attempt (will keep retrying in the background)")
		}
	}()

	return &ERPC{
		cfg:               cfg,
		projectsRegistry:  projectRegistry,
		adminAuthRegistry: adminAuthRegistry,
	}, nil
}

func (e *ERPC) AdminAuthenticate(ctx context.Context, nq *common.NormalizedRequest, ap *auth.AuthPayload) error {
	if e.adminAuthRegistry != nil {
		err := e.adminAuthRegistry.Authenticate(ctx, nq, ap)
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
					Id:        n.NetworkId,
					Upstreams: []*taxonomyUpstream{},
				}
				upstreams := n.upstreamsRegistry.GetNetworkUpstreams(n.NetworkId)
				for _, u := range upstreams {
					ntw.Upstreams = append(ntw.Upstreams, &taxonomyUpstream{Id: u.Config().Id})
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
