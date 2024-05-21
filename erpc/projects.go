package erpc

import (
	"context"

	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/config"
	"github.com/flair-sdk/erpc/data"
	"github.com/flair-sdk/erpc/resiliency"
	"github.com/flair-sdk/erpc/upstream"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

type PreparedProject struct {
	Config   *config.ProjectConfig
	Networks map[string]*PreparedNetwork
	Logger   *zerolog.Logger
}

type ProjectsRegistry struct {
	upstreamsRegistry    *upstream.UpstreamsRegistry
	rateLimitersRegistry *resiliency.RateLimitersRegistry
	preparedProjects     map[string]*PreparedProject
	store                data.Store
}

func NewProjectsRegistry(
	store data.Store,
	upstreamsRegistry *upstream.UpstreamsRegistry,
	rateLimitersRegistry *resiliency.RateLimitersRegistry,
	staticProjects []*config.ProjectConfig,
) (*ProjectsRegistry, error) {
	reg := &ProjectsRegistry{
		store:                store,
		upstreamsRegistry:    upstreamsRegistry,
		rateLimitersRegistry: rateLimitersRegistry,
		preparedProjects:     make(map[string]*PreparedProject),
	}

	for _, project := range staticProjects {
		err := reg.NewProject(project)
		if err != nil {
			return nil, err
		}
	}

	return reg, nil
}

func (r *ProjectsRegistry) GetProject(projectId string) (*PreparedProject, error) {
	project, ok := r.preparedProjects[projectId]
	if !ok {
		// TODO when implementing dynamic db-based project loading, this should be a DB query
		return nil, common.NewErrProjectNotFound(projectId)
	}
	return project, nil
}

func (r *ProjectsRegistry) NewProject(project *config.ProjectConfig) error {
	if _, ok := r.preparedProjects[project.Id]; ok {
		return common.NewErrProjectAlreadyExists(project.Id)
	}
	ptr := log.Logger.With().Str("project", project.Id).Logger()
	pp := &PreparedProject{
		Config: project,
		Logger: &ptr,
	}

	var preparedUpstreams []*upstream.PreparedUpstream
	ups, err := r.upstreamsRegistry.GetUpstreamsByProject(project.Id)
	if err != nil {
		return err
	}
	preparedUpstreams = append(preparedUpstreams, ups...)

	var preparedNetworks map[string]*PreparedNetwork = make(map[string]*PreparedNetwork)
	for _, network := range project.Networks {
		if _, ok := preparedNetworks[network.NetworkId]; !ok {
			if network.Architecture == "" {
				network.Architecture = upstream.ArchitectureEvm
			}
			pn, err := r.NewNetwork(pp.Logger, r.store, project, network)
			if err != nil {
				return err
			}
			preparedNetworks[network.NetworkId] = pn
		}
	}

	for _, ups := range preparedUpstreams {
		arch := ups.Architecture
		if arch == "" {
			arch = upstream.ArchitectureEvm
		}
		for _, networkId := range ups.NetworkIds {
			if _, ok := preparedNetworks[networkId]; !ok {
				pn, err := r.NewNetwork(
					pp.Logger,
					r.store,
					project,
					&config.NetworkConfig{
						NetworkId:    networkId,
						Architecture: arch,
					},
				)
				if err != nil {
					return err
				}
				preparedNetworks[networkId] = pn
			}
		}
	}

	for _, pu := range preparedUpstreams {
		for _, networkId := range pu.NetworkIds {
			if _, ok := preparedNetworks[networkId]; !ok {
				return common.NewErrNetworkNotFound(networkId)
			}
			if preparedNetworks[networkId].Upstreams == nil {
				preparedNetworks[networkId].Upstreams = make([]*upstream.PreparedUpstream, 0)
			}
			preparedNetworks[networkId].Upstreams = append(preparedNetworks[networkId].Upstreams, pu)
		}
	}

	pp.Networks = preparedNetworks

	r.preparedProjects[project.Id] = pp

	return nil
}

func (p *PreparedProject) GetNetwork(networkId string) (*PreparedNetwork, error) {
	network, ok := p.Networks[networkId]
	if !ok {
		return nil, common.NewErrNetworkNotFound(networkId)
	}
	return network, nil
}

func (p *PreparedProject) Forward(ctx context.Context, networkId string, nq *common.NormalizedRequest, w common.ResponseWriter) error {
	network, err := p.GetNetwork(networkId)
	if err != nil {
		return err
	}

	m, _ := nq.Method()
	p.Logger.Debug().Str("method", m).Msgf("forwarding request to network")
	err = network.Forward(ctx, nq, w)

	if err == nil {
		p.Logger.Info().Msgf("successfully forward request for network")
		return nil
	} else {
		p.Logger.Warn().Err(err).Msgf("failed to forward request for network")
	}

	return err
}
