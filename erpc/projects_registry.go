package erpc

import (
	"context"

	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/upstream"
	"github.com/rs/zerolog/log"
)

type ProjectsRegistry struct {
	networksRegistry     *NetworksRegistry
	upstreamsRegistry    *upstream.UpstreamsRegistry
	rateLimitersRegistry *upstream.RateLimitersRegistry
	evmJsonRpcCache      *EvmJsonRpcCache
	preparedProjects     map[string]*PreparedProject
	staticProjects       []*common.ProjectConfig
}

func NewProjectsRegistry(
	staticProjects []*common.ProjectConfig,
	networksRegistry *NetworksRegistry,
	upstreamsRegistry *upstream.UpstreamsRegistry,
	rateLimitersRegistry *upstream.RateLimitersRegistry,
	evmJsonRpcCache *EvmJsonRpcCache,
) (*ProjectsRegistry, error) {
	reg := &ProjectsRegistry{
		staticProjects:       staticProjects,
		preparedProjects:     make(map[string]*PreparedProject),
		networksRegistry:     networksRegistry,
		upstreamsRegistry:    upstreamsRegistry,
		rateLimitersRegistry: rateLimitersRegistry,
		evmJsonRpcCache:      evmJsonRpcCache,
	}

	for _, prjCfg := range staticProjects {
		_, err := reg.RegisterProject(prjCfg)
		if err != nil {
			return nil, err
		}
	}

	return reg, nil
}

func (p *ProjectsRegistry) Bootstrap(ctx context.Context) error {
	for _, pp := range p.preparedProjects {
		err := pp.Bootstrap(ctx)
		if err != nil {
			return err
		}
	}

	return nil
}

func (r *ProjectsRegistry) GetProject(projectId string) (project *PreparedProject, err error) {
	project, exists := r.preparedProjects[projectId]
	if !exists {
		project, err = r.loadProject(projectId)
	}
	return
}

func (r *ProjectsRegistry) RegisterProject(prjCfg *common.ProjectConfig) (*PreparedProject, error) {
	if _, ok := r.preparedProjects[prjCfg.Id]; ok {
		return nil, common.NewErrProjectAlreadyExists(prjCfg.Id)
	}
	ptr := log.Logger.With().Str("project", prjCfg.Id).Logger()
	pp := &PreparedProject{
		Config: prjCfg,
		Logger: &ptr,
	}

	// preparedUpstreams, err := r.upstreamsRegistry.GetUpstreamsByProject(prjCfg.Id)
	// if err != nil {
	// 	return nil, err
	// }

	// // Initialize networks based on configuration
	// var preparedNetworks map[string]*Network = make(map[string]*Network)
	// for _, nwCfg := range prjCfg.Networks {
	// 	nt, err := r.networksRegistry.RegisterNetwork(pp.Logger, r.evmJsonRpcCache, prjCfg, nwCfg)
	// 	if err != nil {
	// 		return nil, err
	// 	}
	// 	preparedNetworks[nt.NetworkId] = nt
	// }

	// log.Info().Msgf("registered %d networks for project %s", len(preparedNetworks), prjCfg.Id)

	// // Initialize networks based on upstreams supported networks
	// for _, ups := range preparedUpstreams {
	// 	tp := ups.Config().Type
	// 	if tp == "" {
	// 		tp, err = common.UpstreamTypeEvm
	// 	}
	// 	for _, networkId := range ups.NetworkIds {
	// 		ncfg := &common.NetworkConfig{
	// 			Architecture: tp,
	// 		}

	// 		switch tp {
	// 		case common.ArchitectureEvm:
	// 			chainId, err := strconv.Atoi(strings.Replace(networkId, "eip155:", "", 1))
	// 			if err != nil {
	// 				return common.NewErrInvalidEvmChainId(networkId)
	// 			}
	// 			ncfg.Evm = &common.EvmNetworkConfig{
	// 				ChainId: chainId,
	// 			}
	// 		default:
	// 			return common.NewErrUnknownNetworkArchitecture(tp)
	// 		}
	// 		nt, err := r.networksRegistry.RegisterNetwork(
	// 			pp.Logger,
	// 			r.evmJsonRpcCache,
	// 			prjCfg,
	// 			ncfg,
	// 		)
	// 		if err != nil {
	// 			return err
	// 		}
	// 		preparedNetworks[nt.NetworkId] = nt
	// 	}
	// }

	// log.Info().Msgf("registered %d upstreams for project %s", len(preparedUpstreams), prjCfg.Id)

	// // Populate networks with upstreams
	// for _, pu := range preparedUpstreams {
	// 	for _, networkId := range pu.NetworkIds {
	// 		nt := r.networksRegistry.GetNetwork(prjCfg.Id, networkId)
	// 		if nt == nil {
	// 			return common.NewErrNetworkNotFound(networkId)
	// 		}
	// 		nt.upstreamsMutex.Lock()
	// 		if nt.Upstreams == nil {
	// 			nt.Upstreams = make([]*upstream.Upstream, 0)
	// 		}
	// 		nt.Upstreams = append(nt.Upstreams, pu)
	// 		nt.upstreamsMutex.Unlock()
	// 	}
	// }

	// pp.Networks = preparedNetworks
	pp.Networks = make(map[string]*Network)

	r.preparedProjects[prjCfg.Id] = pp

	log.Info().Msgf("registered project %s", prjCfg.Id)

	return pp, nil
}

func (r *ProjectsRegistry) loadProject(projectId string) (*PreparedProject, error) {
	for _, prj := range r.staticProjects {
		if prj.Id == projectId {
			return r.RegisterProject(prj)
		}
	}

	// TODO implement dynamic project config loading from DB

	return nil, common.NewErrProjectNotFound(projectId)
}