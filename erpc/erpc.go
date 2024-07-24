package erpc

import (
	"context"

	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/upstream"
	"github.com/flair-sdk/erpc/vendors"
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
	rateLimitersRegistry, err := upstream.NewRateLimitersRegistry(cfg.RateLimiters)
	if err != nil {
		return nil, err
	}

	vendorsRegistry := vendors.NewVendorsRegistry()
	projectRegistry, err := NewProjectsRegistry(
		ctx,
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

// func (e *ERPC) Shutdown() error {
// 	if err := e.projectsRegistry.Shutdown(); err != nil {
// 		return err
// 	}
// 	if err := e.evmJsonRpcCache.Shutdown(); err != nil {
// 		return err
// 	}
// 	return nil
// }
