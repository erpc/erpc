package erpc

import (
	"context"

	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/upstream"
	"github.com/flair-sdk/erpc/vendors"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

type ERPC struct {
	cfg                  *common.Config
	upstreamsRegistry    *upstream.UpstreamsRegistry
	rateLimitersRegistry *upstream.RateLimitersRegistry
	projectsRegistry     *ProjectsRegistry
}

func NewERPC(
	logger *zerolog.Logger,
	evmJsonRpcCache *EvmJsonRpcCache,
	cfg *common.Config,
) (*ERPC, error) {
	rateLimitersRegistry, err := upstream.NewRateLimitersRegistry(cfg.RateLimiters)
	if err != nil {
		return nil, err
	}

	vendorsRegistry := vendors.NewVendorsRegistry()

	upstreamsRegistry, err := upstream.NewUpstreamsRegistry(logger, cfg, rateLimitersRegistry, vendorsRegistry)
	if err != nil {
		return nil, err
	}

	networksRegistry := NewNetworksRegistry(rateLimitersRegistry)
	projectRegistry, err := NewProjectsRegistry(
		cfg.Projects,
		networksRegistry,
		upstreamsRegistry,
		rateLimitersRegistry,
		evmJsonRpcCache,
	)
	if err != nil {
		return nil, err
	}
	err = projectRegistry.Bootstrap(context.Background())
	if err != nil {
		return nil, err
	}

	upstreamsRegistry.OnUpstreamsPriorityChange = func(projectId string, networkId string) error {
		log.Info().Str("project", projectId).Str("network", networkId).Msgf("upstreams priority updated")
		prj, err := projectRegistry.GetProject(projectId)
		if err != nil {
			return err
		}

		ntw, err := prj.GetNetwork(networkId)
		if err != nil {
			return err
		}

		for i := 0; i < len(ntw.Upstreams); i++ {
			for j := i + 1; j < len(ntw.Upstreams); j++ {
				if ntw.Upstreams[i].Score < ntw.Upstreams[j].Score {
					ntw.Upstreams[i], ntw.Upstreams[j] = ntw.Upstreams[j], ntw.Upstreams[i]
				}
			}
		}

		var finalOrder string
		for _, u := range ntw.Upstreams {
			finalOrder += u.Config().Id + ", "
		}
		log.Info().Str("project", projectId).Str("network", networkId).Str("upstreams", finalOrder).Msgf("upstreams priority updated")

		return nil
	}

	return &ERPC{
		cfg:                  cfg,
		upstreamsRegistry:    upstreamsRegistry,
		rateLimitersRegistry: rateLimitersRegistry,
		projectsRegistry:     projectRegistry,
	}, nil
}

func (e *ERPC) GetNetwork(projectId string, networkId string) (*Network, error) {
	prj, err := e.GetProject(projectId)
	if err != nil {
		return nil, err
	}

	return prj.GetNetwork(networkId)
}

func (e *ERPC) GetProject(projectId string) (*PreparedProject, error) {
	return e.projectsRegistry.GetProject(projectId)
}

func (e *ERPC) Shutdown() error {
	// TODO: Implement
	return nil
}
