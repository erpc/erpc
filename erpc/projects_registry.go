package erpc

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/auth"
	"github.com/erpc/erpc/clients"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/internal/policy"
	"github.com/erpc/erpc/internal/policy/stdlib"
	"github.com/erpc/erpc/thirdparty"
	"github.com/erpc/erpc/upstream"
	"github.com/rs/zerolog"
)

// ScoreMetricsWindowSize is the tumbling window the score-metrics
// tracker uses for per-upstream rolling counters (errorRate, p95, …).
// At each tick the counters reset to zero and start re-accumulating —
// so this knob also caps how fast a knob/config change is reflected
// in the observed metrics that drive `keepHealthy` and `sortByScore`.
//
// Production default is 10 minutes (stable averages, immune to spikes).
// The erpc-simulator overrides this in its init() to ~15s so that
// twiddling a fake-upstream knob produces a visible shift in routing
// within a few seconds. Package-level so callers can override it
// before NewERPC without plumbing it through every constructor.
var ScoreMetricsWindowSize = 10 * time.Minute

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

func (r *ProjectsRegistry) Bootstrap(appCtx context.Context) {
	for _, prj := range r.preparedProjects {
		prj.Bootstrap(appCtx)
	}
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

	// Score-metrics window default; Phase 7 will replace with explicit config knob.
	// Read from the package-level override so tooling (notably the
	// erpc-simulator) can shorten it for fast feedback loops.
	metricsTracker := health.NewTracker(&lg, prjCfg.Id, ScoreMetricsWindowSize)
	providersRegistry, err := thirdparty.NewProvidersRegistry(
		&lg,
		r.vendorsRegistry,
		prjCfg.Providers,
		prjCfg.UpstreamDefaults,
	)
	if err != nil {
		return nil, err
	}
	pp := &PreparedProject{
		Config:               prjCfg,
		Logger:               &lg,
		rateLimitersRegistry: r.rateLimitersRegistry,
		cfgMu:                sync.RWMutex{},
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
		func(ups *upstream.Upstream) error {
			ntwId := ups.NetworkId()
			if ntwId == "" {
				return fmt.Errorf("upstream %s has no network id set yet", ups.Id())
			}
			ntw, err := pp.networksRegistry.GetNetwork(r.appCtx, ntwId)
			if err != nil {
				return err
			}
			ups.SetNetworkConfig(ntw.cfg)
			return nil
		},
	)

	if prjCfg.Auth != nil {
		consumerAuthRegistry, err := auth.NewAuthRegistry(r.appCtx, &lg, prjCfg.Id, prjCfg.Auth, r.rateLimitersRegistry)
		if err != nil {
			return nil, err
		}
		pp.consumerAuthRegistry = consumerAuthRegistry
	}

	pp.upstreamsRegistry = upstreamsRegistry
	pp.policyEngine = policy.NewEngine(
		r.appCtx,
		&lg,
		prjCfg.Id,
		metricsTracker,
		stdlib.Install,
	)
	pp.networksRegistry = NewNetworksRegistry(
		pp,
		r.appCtx,
		upstreamsRegistry,
		metricsTracker,
		r.evmJsonRpcCache,
		r.rateLimitersRegistry,
		pp.policyEngine,
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
