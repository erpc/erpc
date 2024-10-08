package erpc

import (
	"context"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/vendors"
	"github.com/rs/zerolog"
)

type ERPC struct {
	cfg              *common.Config
	projectsRegistry *ProjectsRegistry
}

func NewERPC(
	ctx context.Context,
	logger *zerolog.Logger,
	evmJsonRpcCache *EvmJsonRpcCache,
	cfg *common.Config,
) (*ERPC, error) {
	rateLimitersRegistry, err := upstream.NewRateLimitersRegistry(cfg.RateLimiters, logger)
	if err != nil {
		return nil, err
	}

	vendorsRegistry := vendors.NewVendorsRegistry()
	projectRegistry, err := NewProjectsRegistry(
		ctx,
		logger,
		cfg.Projects,
		evmJsonRpcCache,
		rateLimitersRegistry,
		vendorsRegistry,
	)
	if err != nil {
		return nil, err
	}

	return &ERPC{
		cfg:              cfg,
		projectsRegistry: projectRegistry,
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

func (e *ERPC) GetProjects() []*PreparedProject {
	return e.projectsRegistry.GetAll()
}
