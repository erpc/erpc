package erpc

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/auth"
	"github.com/erpc/erpc/clients"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/thirdparty"
	"github.com/erpc/erpc/upstream"
	"github.com/rs/zerolog"
)

type ProjectsRegistry struct {
	logger *zerolog.Logger
	appCtx context.Context

	rateLimitersRegistry *upstream.RateLimitersRegistry
	sharedState          data.SharedStateRegistry
	evmJsonRpcCache      *evm.EvmJsonRpcCache
	preparedProjects     map[string]*PreparedProject
	staticProjects       []*common.ProjectConfig
	vendorsRegistry      *thirdparty.VendorsRegistry
	proxyPoolRegistry    *clients.ProxyPoolRegistry
}

func NewProjectsRegistry(
	appCtx context.Context,
	logger *zerolog.Logger,
	staticProjects []*common.ProjectConfig,
	sharedState data.SharedStateRegistry,
	evmJsonRpcCache *evm.EvmJsonRpcCache,
	rateLimitersRegistry *upstream.RateLimitersRegistry,
	vendorsRegistry *thirdparty.VendorsRegistry,
	proxyPoolRegistry *clients.ProxyPoolRegistry,
) (*ProjectsRegistry, error) {
	reg := &ProjectsRegistry{
		appCtx:               appCtx,
		logger:               logger,
		staticProjects:       staticProjects,
		preparedProjects:     make(map[string]*PreparedProject),
		sharedState:          sharedState,
		rateLimitersRegistry: rateLimitersRegistry,
		evmJsonRpcCache:      evmJsonRpcCache,
		vendorsRegistry:      vendorsRegistry,
		proxyPoolRegistry:    proxyPoolRegistry,
	}

	for _, prjCfg := range staticProjects {
		_, err := reg.RegisterProject(prjCfg)
		if err != nil {
			return nil, err
		}
	}

	return reg, nil
}

func (r *ProjectsRegistry) Bootstrap(appCtx context.Context) error {
	wg := sync.WaitGroup{}
	wg.Add(len(r.preparedProjects))
	var errs []error
	for _, prj := range r.preparedProjects {
		go func(prj *PreparedProject) {
			defer wg.Done()
			err := prj.Bootstrap(appCtx)
			if err != nil {
				errs = append(errs, err)
			}
		}(prj)
	}
	wg.Wait()
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
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

	var wsDuration time.Duration
	if prjCfg.ScoreMetricsWindowSize > 0 {
		wsDuration = prjCfg.ScoreMetricsWindowSize.Duration()
	} else {
		wsDuration = 30 * time.Minute
	}
	metricsTracker := health.NewTracker(&lg, prjCfg.Id, wsDuration)
	providersRegistry, err := thirdparty.NewProvidersRegistry(
		&lg,
		r.vendorsRegistry,
		prjCfg.Providers,
		prjCfg.UpstreamDefaults,
	)
	if err != nil {
		return nil, err
	}
	upstreamsRegistry := upstream.NewUpstreamsRegistry(
		r.appCtx,
		&lg,
		prjCfg.Id,
		prjCfg.Upstreams,
		r.sharedState,
		r.rateLimitersRegistry,
		r.vendorsRegistry,
		providersRegistry,
		r.proxyPoolRegistry,
		metricsTracker,
		1*time.Second,
	)

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
		cfgMu:                sync.RWMutex{},
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

	// TODO can we refactor the architecture so this relation is more straightforward?
	// The main challenge is for some upstreams we are detecting network (chainId) lazily therefore we can't set it before initializing the upstream.
	// Should we eliminate chainid lazy detection (that would be a bummer and a breaking change :D)?
	upstreamsRegistry.OnUpstreamRegistered(func(ups *upstream.Upstream) error {
		ntwId := ups.NetworkId()
		if ntwId == "" {
			return fmt.Errorf("upstream %s has no network id set yet", ups.Config().Id)
		}
		ntw, err := pp.networksRegistry.GetNetwork(ntwId)
		if err != nil {
			return err
		}
		ups.SetNetworkConfig(ntw.cfg)
		return nil
	})

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
