package erpc

import (
	"context"
	"strconv"
	"strings"

	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/config"
	"github.com/flair-sdk/erpc/upstream"
	"github.com/rs/zerolog/log"
)

type ProjectsRegistry struct {
	networksRegistry     *NetworksRegistry
	upstreamsRegistry    *upstream.UpstreamsRegistry
	rateLimitersRegistry *upstream.RateLimitersRegistry
	evmJsonRpcCache      *EvmJsonRpcCache
	preparedProjects     map[string]*PreparedProject
}

func NewProjectsRegistry(
	staticProjects []*config.ProjectConfig,
	networksRegistry *NetworksRegistry,
	upstreamsRegistry *upstream.UpstreamsRegistry,
	rateLimitersRegistry *upstream.RateLimitersRegistry,
	evmJsonRpcCache *EvmJsonRpcCache,
) (*ProjectsRegistry, error) {
	reg := &ProjectsRegistry{
		preparedProjects:     make(map[string]*PreparedProject),
		networksRegistry:     networksRegistry,
		upstreamsRegistry:    upstreamsRegistry,
		rateLimitersRegistry: rateLimitersRegistry,
		evmJsonRpcCache:      evmJsonRpcCache,
	}

	for _, project := range staticProjects {
		err := reg.RegisterProject(project)
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

func (r *ProjectsRegistry) GetProject(projectId string) (*PreparedProject, error) {
	project, ok := r.preparedProjects[projectId]
	if !ok {
		// TODO when implementing dynamic db-based project loading, this should be a DB query
		return nil, common.NewErrProjectNotFound(projectId)
	}
	return project, nil
}

func (r *ProjectsRegistry) RegisterProject(prjCfg *config.ProjectConfig) error {
	if _, ok := r.preparedProjects[prjCfg.Id]; ok {
		return common.NewErrProjectAlreadyExists(prjCfg.Id)
	}
	ptr := log.Logger.With().Str("project", prjCfg.Id).Logger()
	pp := &PreparedProject{
		Config: prjCfg,
		Logger: &ptr,
	}

	preparedUpstreams, err := r.upstreamsRegistry.GetUpstreamsByProject(prjCfg.Id)
	if err != nil {
		return err
	}

	// Initialize networks based on configuration
	var preparedNetworks map[string]*PreparedNetwork = make(map[string]*PreparedNetwork)
	for _, nwCfg := range prjCfg.Networks {
		nt, err := r.networksRegistry.RegisterNetwork(pp.Logger, r.evmJsonRpcCache, prjCfg, nwCfg)
		if err != nil {
			return err
		}
		preparedNetworks[nt.NetworkId] = nt
	}

	log.Info().Msgf("registered %d networks for project %s", len(preparedNetworks), prjCfg.Id)

	// Initialize networks based on upstreams supported networks
	for _, ups := range preparedUpstreams {
		arch := ups.Architecture
		if arch == "" {
			arch = common.ArchitectureEvm
		}
		for _, networkId := range ups.NetworkIds {
			ncfg := &config.NetworkConfig{
				Architecture: arch,
			}

			switch arch {
			case common.ArchitectureEvm:
				chainId, err := strconv.Atoi(strings.Replace(networkId, "eip155:", "", 1))
				if err != nil {
					return common.NewErrInvalidEvmChainId(networkId)
				}
				ncfg.Evm = &config.EvmNetworkConfig{
					ChainId: chainId,
				}
			default:
				return common.NewErrUnknownNetworkArchitecture(arch)
			}
			nt, err := r.networksRegistry.RegisterNetwork(
				pp.Logger,
				r.evmJsonRpcCache,
				prjCfg,
				ncfg,
			)
			if err != nil {
				return err
			}
			preparedNetworks[nt.NetworkId] = nt
		}
	}

	log.Info().Msgf("registered %d upstreams for project %s", len(preparedUpstreams), prjCfg.Id)

	// Populate networks with upstreams
	for _, pu := range preparedUpstreams {
		for _, networkId := range pu.NetworkIds {
			nt := r.networksRegistry.GetNetwork(prjCfg.Id, networkId)
			if nt == nil {
				return common.NewErrNetworkNotFound(networkId)
			}
			nt.upstreamsMutex.Lock()
			if nt.Upstreams == nil {
				nt.Upstreams = make([]*upstream.PreparedUpstream, 0)
			}
			nt.Upstreams = append(nt.Upstreams, pu)
			nt.upstreamsMutex.Unlock()
		}
	}

	pp.Networks = preparedNetworks

	r.preparedProjects[prjCfg.Id] = pp

	log.Info().Msgf("registered project %s", prjCfg.Id)

	return nil
}
