package erpc

import (
	"context"
	"time"

	"github.com/erpc/erpc/auth"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/vendors"
	"github.com/rs/zerolog"
)

type ProjectsRegistry struct {
	logger *zerolog.Logger
	appCtx context.Context

	rateLimitersRegistry *upstream.RateLimitersRegistry
	evmJsonRpcCache      *EvmJsonRpcCache
	preparedProjects     map[string]*PreparedProject
	staticProjects       []*common.ProjectConfig
	vendorsRegistry      *vendors.VendorsRegistry
}

func NewProjectsRegistry(
	appCtx context.Context,
	logger *zerolog.Logger,
	staticProjects []*common.ProjectConfig,
	evmJsonRpcCache *EvmJsonRpcCache,
	rateLimitersRegistry *upstream.RateLimitersRegistry,
	vendorsRegistry *vendors.VendorsRegistry,
) (*ProjectsRegistry, error) {
	reg := &ProjectsRegistry{
		appCtx:               appCtx,
		logger:               logger,
		staticProjects:       staticProjects,
		preparedProjects:     make(map[string]*PreparedProject),
		rateLimitersRegistry: rateLimitersRegistry,
		evmJsonRpcCache:      evmJsonRpcCache,
		vendorsRegistry:      vendorsRegistry,
	}

	for _, prjCfg := range staticProjects {
		prj, err := reg.RegisterProject(prjCfg)
		if err != nil {
			return nil, err
		}
		prj.Bootstrap(appCtx)
	}

	return reg, nil
}

func (r *ProjectsRegistry) GetProject(projectId string) (project *PreparedProject, err error) {
	if projectId == "" {
		return nil, nil
	}
	project, exists := r.preparedProjects[projectId]
	if !exists {
		return nil, common.NewErrProjectNotFound(projectId)
	}
	return project, nil
}

func (r *ProjectsRegistry) RegisterProject(prjCfg *common.ProjectConfig) (*PreparedProject, error) {
	if _, ok := r.preparedProjects[prjCfg.Id]; ok {
		return nil, common.NewErrProjectAlreadyExists(prjCfg.Id)
	}

	lg := r.logger.With().Str("projectId", prjCfg.Id).Logger()

	ws := "30m"
	if prjCfg.HealthCheck != nil && prjCfg.HealthCheck.ScoreMetricsWindowSize != "" {
		ws = prjCfg.HealthCheck.ScoreMetricsWindowSize
	}
	wsDuration, err := time.ParseDuration(ws)
	if err != nil {
		return nil, err
	}
	metricsTracker := health.NewTracker(prjCfg.Id, wsDuration)
	upstreamsRegistry := upstream.NewUpstreamsRegistry(
		r.appCtx,
		&lg,
		prjCfg.Id,
		prjCfg.Upstreams,
		r.rateLimitersRegistry,
		r.vendorsRegistry,
		metricsTracker,
		1*time.Second,
	)
	err = upstreamsRegistry.Bootstrap(r.appCtx)
	if err != nil {
		return nil, err
	}

	var consumerAuthRegistry *auth.AuthRegistry
	if prjCfg.Auth != nil {
		consumerAuthRegistry, err = auth.NewAuthRegistry(&lg, prjCfg.Id, prjCfg.Auth, r.rateLimitersRegistry)
		if err != nil {
			return nil, err
		}
	}

	pp := &PreparedProject{
		Config:               prjCfg,
		Logger:               &lg,
		upstreamsRegistry:    upstreamsRegistry,
		consumerAuthRegistry: consumerAuthRegistry,
		rateLimitersRegistry: r.rateLimitersRegistry,
	}
	pp.networksRegistry = NewNetworksRegistry(
		pp,
		r.appCtx,
		upstreamsRegistry,
		metricsTracker,
		r.evmJsonRpcCache,
		r.rateLimitersRegistry,
		&lg,
	)
	r.preparedProjects[prjCfg.Id] = pp

	r.logger.Info().Msgf("registered project %s", prjCfg.Id)

	return pp, nil
}

func (r *ProjectsRegistry) GetAll() []*PreparedProject {
	projects := make([]*PreparedProject, 0, len(r.preparedProjects))
	for _, project := range r.preparedProjects {
		projects = append(projects, project)
	}
	return projects
}
