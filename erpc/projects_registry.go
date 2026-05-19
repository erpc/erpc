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
	"github.com/grafana/sobek"
	"github.com/rs/zerolog"
)

// ScoreMetricsWindowSize is the FALLBACK rolling window the
// score-metrics tracker uses for per-upstream counters (errorRate,
// p95, throttle, lag, …) when a project's YAML doesn't set the
// canonical `scoreMetricsWindowSize` field. The tracker keeps a
// 10-bucket sliding window of this duration; one bucket rotates
// every windowSize/10, so the window is also the upper bound on how
// long a freshly-degraded upstream can keep its old healthy score
// before `excludeIf` / `sortByScore` see the new reality.
//
// 10s is the default. Two competing concerns:
//   - reaction time: should be well under operator attention span
//     (humans notice incidents within tens of seconds, not minutes);
//   - rotation overhead at scale: a rotation iterates EVERY
//     TrackedMetrics (per (upstream, method) tuple) and re-inits one
//     DDSketch each — at a few hundred upstreams × ~20 methods that's
//     thousands of small allocations per second. Halving the window
//     halves both the rotation rate and the GC churn.
//
// 10s sits at the knee: a freshly-degraded upstream still rolls into
// view within ~1 s on a busy network (the rolling buckets fill almost
// immediately), but the rotation goroutine only ticks once per second
// instead of twice. Hot deployments narrow per-project to 5s; cold /
// huge-fan-out deployments widen to 30s+.
var ScoreMetricsWindowSize = 10 * time.Second

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
	// userScript is the compiled program of the user's TS config file
	// (when LoadConfig went through the .ts path). nil for YAML. Passed
	// into every project's policy.Engine so its runtime pool can
	// evaluate the user's whole module in each runtime — keeping
	// closure variables + module-level helpers live in the same scope
	// as the `evalFunc` arrow functions the user wrote.
	userScript *sobek.Program
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
	userScript *sobek.Program,
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
		userScript:           userScript,
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

	// Score-metrics window: prefer the project's explicit YAML value;
	// fall back to the package-level default (10m in production, 30s
	// for the erpc-simulator which sets it in init()).
	metricsWindow := ScoreMetricsWindowSize
	if d := prjCfg.ScoreMetricsWindowSize.Duration(); d > 0 {
		metricsWindow = d
	}
	metricsTracker := health.NewTracker(&lg, prjCfg.Id, metricsWindow)
	// Start the rotation goroutine that advances the rolling window
	// every `metricsWindow / rollingBuckets`. Without this the
	// per-upstream counters + quantile sketch accumulate forever,
	// which silently turns the rolling window into a "since process
	// start" window — degradations get diluted by lifetime traffic
	// and the score chip barely budges no matter how the upstream
	// behaves now.
	metricsTracker.Bootstrap(r.appCtx)
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
		r.userScript,
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
