package erpc

import (
	"context"
	"net/http"
	"sync"

	"github.com/failsafe-go/failsafe-go"
	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/config"
	"github.com/flair-sdk/erpc/resiliency"
	"github.com/flair-sdk/erpc/upstream"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

type PreparedProject struct {
	Config   *config.ProjectConfig
	Networks map[string]*PreparedNetwork
	Logger   zerolog.Logger
}

type ProjectsRegistry struct {
	upstreamsRegistry    *upstream.UpstreamsRegistry
	rateLimitersRegistry *resiliency.RateLimitersRegistry
	preparedProjects     map[string]*PreparedProject
}

func NewProjectsRegistry(
	upstreamsRegistry *upstream.UpstreamsRegistry,
	rateLimitersRegistry *resiliency.RateLimitersRegistry,
	staticProjects []*config.ProjectConfig,
) (*ProjectsRegistry, error) {
	reg := &ProjectsRegistry{
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
	pp := &PreparedProject{
		Config: project,
		Logger: log.Logger.With().Str("project", project.Id).Logger(),
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
			pn, err := r.NewNetwork(pp.Logger, project, network)
			if err != nil {
				return err
			}
			preparedNetworks[network.NetworkId] = pn
		}
	}

	for _, upstream := range preparedUpstreams {
		for _, networkId := range upstream.NetworkIds {
			if _, ok := preparedNetworks[networkId]; !ok {
				pn, err := r.NewNetwork(pp.Logger, project, &config.NetworkConfig{NetworkId: networkId})
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

func (p *PreparedProject) Forward(ctx context.Context, networkId string, nq *upstream.NormalizedRequest, w http.ResponseWriter, wmu *sync.Mutex) error {
	network, err := p.GetNetwork(networkId)
	if err != nil {
		return err
	}

	if network.FailsafePolicies == nil || len(network.FailsafePolicies) == 0 {
		p.Logger.Debug().Msgf("forwarding request to network with no retries")
		err = network.Forward(ctx, nq, w, wmu)
	} else {
		_, execErr := network.Executor().WithContext(ctx).GetWithExecution(func(exec failsafe.Execution[interface{}]) (interface{}, error) {
			p.Logger.Debug().Int("attempts", exec.Attempts()).Msgf("forwarding request to network")
			err = network.Forward(exec.Context(), nq, w, wmu)
			return nil, err
		})
		if execErr != nil {
			err = resiliency.TranslateFailsafeError(execErr)
		}
	}

	if err == nil {
		p.Logger.Info().Msgf("successfully forward request for network")
		return nil
	} else {
		p.Logger.Warn().Err(err).Msgf("failed to forward request for network")
	}

	return err
}
