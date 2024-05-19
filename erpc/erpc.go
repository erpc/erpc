package erpc

import (
	"github.com/flair-sdk/erpc/config"
	"github.com/flair-sdk/erpc/resiliency"
	"github.com/flair-sdk/erpc/upstream"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

type ERPC struct {
	cfg                  *config.Config
	upstreamsRegistry    *upstream.UpstreamsRegistry
	rateLimitersRegistry *resiliency.RateLimitersRegistry
	projectsRegistry     *ProjectsRegistry
}

func NewERPC(logger zerolog.Logger, cfg *config.Config) (*ERPC, error) {
	rateLimitersRegistry, err := resiliency.NewRateLimitersRegistry(cfg.RateLimiters)
	if err != nil {
		return nil, err
	}

	upstreamsRegistry, err := upstream.NewUpstreamsRegistry(logger, cfg, rateLimitersRegistry)
	if err != nil {
		return nil, err
	}

	projectRegistry, err := NewProjectsRegistry(upstreamsRegistry, rateLimitersRegistry, cfg.Projects)
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
			finalOrder += u.Id + ", "
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

func (e *ERPC) GetNetwork(projectId string, networkId string) (*PreparedNetwork, error) {
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
