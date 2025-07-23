package upstream

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"sync"
	"time"

	"github.com/erpc/erpc/clients"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/thirdparty"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
)

type UpstreamsRegistry struct {
	appCtx               context.Context
	prjId                string
	scoreRefreshInterval time.Duration
	logger               *zerolog.Logger
	metricsTracker       *health.Tracker
	sharedStateRegistry  data.SharedStateRegistry
	clientRegistry       *clients.ClientRegistry
	vendorsRegistry      *thirdparty.VendorsRegistry
	providersRegistry    *thirdparty.ProvidersRegistry
	rateLimitersRegistry *RateLimitersRegistry
	upsCfg               []*common.UpstreamConfig
	initializer          *util.Initializer
	projectConfig        *common.ProjectConfig

	allUpstreams []*Upstream
	upstreamsMu  *sync.RWMutex
	networkMu    *sync.Map // map[string]*sync.RWMutex for per-network locks
	// map of network => upstreams
	networkUpstreams       map[string][]*Upstream
	networkShadowUpstreams map[string][]*Upstream
	// map of network -> method (or *) => upstreams
	sortedUpstreams map[string]map[string][]*Upstream
	// map of upstream -> network (or *) -> method (or *) => score
	upstreamScores map[string]map[string]map[string]float64
	// map of network -> method -> current index for round-robin
	rrIndices map[string]map[string]int
	// map of network -> method -> current weights for weighted round-robin
	rrWeights map[string]map[string][]float64

	onUpstreamRegistered func(ups *Upstream) error
}

type UpstreamsHealth struct {
	Upstreams       []*Upstream                              `json:"upstreams"`
	SortedUpstreams map[string]map[string][]string           `json:"sortedUpstreams"`
	UpstreamScores  map[string]map[string]map[string]float64 `json:"upstreamScores"`
}

func NewUpstreamsRegistry(
	appCtx context.Context,
	logger *zerolog.Logger,
	prjId string,
	upsCfg []*common.UpstreamConfig,
	ssr data.SharedStateRegistry,
	rr *RateLimitersRegistry,
	vr *thirdparty.VendorsRegistry,
	pr *thirdparty.ProvidersRegistry,
	ppr *clients.ProxyPoolRegistry,
	mt *health.Tracker,
	scoreRefreshInterval time.Duration,
	projectConfig *common.ProjectConfig,
) *UpstreamsRegistry {
	lg := logger.With().Str("component", "upstreams").Logger()
	return &UpstreamsRegistry{
		appCtx:                 appCtx,
		prjId:                  prjId,
		scoreRefreshInterval:   scoreRefreshInterval,
		logger:                 logger,
		sharedStateRegistry:    ssr,
		clientRegistry:         clients.NewClientRegistry(logger, prjId, ppr),
		rateLimitersRegistry:   rr,
		vendorsRegistry:        vr,
		providersRegistry:      pr,
		metricsTracker:         mt,
		upsCfg:                 upsCfg,
		projectConfig:          projectConfig,
		networkUpstreams:       make(map[string][]*Upstream),
		networkShadowUpstreams: make(map[string][]*Upstream),
		sortedUpstreams:        make(map[string]map[string][]*Upstream),
		upstreamScores:         make(map[string]map[string]map[string]float64),
		upstreamsMu:            &sync.RWMutex{},
		networkMu:              &sync.Map{},
		rrIndices:              make(map[string]map[string]int),
		rrWeights:              make(map[string]map[string][]float64),
		initializer:            util.NewInitializer(appCtx, &lg, nil),
	}
}

func (u *UpstreamsRegistry) Bootstrap(ctx context.Context) error {
	err := u.scheduleScoreCalculationTimers(ctx)
	if err != nil {
		return err
	}

	return u.registerUpstream(u.appCtx, u.upsCfg...)
}

func (u *UpstreamsRegistry) OnUpstreamRegistered(fn func(ups *Upstream) error) {
	u.onUpstreamRegistered = fn
}

func (u *UpstreamsRegistry) NewUpstream(cfg *common.UpstreamConfig) (*Upstream, error) {
	// Set the project config reference
	cfg.ProjectConfig = u.projectConfig
	return NewUpstream(
		u.appCtx,
		u.prjId,
		cfg,
		u.clientRegistry,
		u.rateLimitersRegistry,
		u.vendorsRegistry,
		u.logger,
		u.metricsTracker,
		u.sharedStateRegistry,
	)
}

func (u *UpstreamsRegistry) GetInitializer() *util.Initializer {
	return u.initializer
}

func (u *UpstreamsRegistry) getNetworkMutex(networkId string) *sync.RWMutex {
	mutex, _ := u.networkMu.LoadOrStore(networkId, &sync.RWMutex{})
	return mutex.(*sync.RWMutex)
}

func (u *UpstreamsRegistry) GetProvidersRegistry() *thirdparty.ProvidersRegistry {
	return u.providersRegistry
}

func (u *UpstreamsRegistry) PrepareUpstreamsForNetwork(ctx context.Context, networkId string) error {
	networkMu := u.getNetworkMutex(networkId)
	networkMu.Lock()
	defer networkMu.Unlock()

	allProviders := u.providersRegistry.GetAllProviders()

	var tasks []*util.BootstrapTask
	for _, p := range allProviders {
		// Capture loop variable in local scope
		provider := p
		t := u.buildProviderBootstrapTask(provider, networkId)
		tasks = append(tasks, t)
	}
	errCh := make(chan error, 1)
	go func() {
		if err := u.initializer.ExecuteTasks(ctx, tasks...); err != nil {
			u.logger.Error().
				Err(err).
				Str("networkId", networkId).
				Msg("failed to execute provider bootstrap tasks")
			errCh <- err
		}
		close(errCh)
	}()

	// Wait for at least one upstream, completion, or error
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errCh:
			return err
		case <-ticker.C:
			// check if the initializer has failed
			status := u.initializer.Status()

			if status.State == util.StateFailed {
				return fmt.Errorf("initialization failed with state: %s", status.State.String())
			}

			// Check if we have at least one upstream for this network
			u.upstreamsMu.RLock()
			upstreamsCount := len(u.networkUpstreams[networkId])
			u.upstreamsMu.RUnlock()

			if upstreamsCount == 0 {
				continue
			}

			if upstreamsCount > 0 {
				u.logger.Info().
					Str("networkId", networkId).
					Int("upstreamsCount", upstreamsCount).
					Msg("at least one upstream is available for network, continuing initialization")
				return nil
			}
		}
	}
}

func (u *UpstreamsRegistry) GetNetworkShadowUpstreams(networkId string) []*Upstream {
	u.upstreamsMu.RLock()
	defer u.upstreamsMu.RUnlock()
	return u.networkShadowUpstreams[networkId]
}

func (u *UpstreamsRegistry) GetNetworkUpstreams(ctx context.Context, networkId string) []*Upstream {
	if ctx == nil {
		ctx = u.appCtx
	}
	_, span := common.StartDetailSpan(ctx, "UpstreamsRegistry.GetNetworkUpstreams")
	defer span.End()

	u.upstreamsMu.RLock()
	defer u.upstreamsMu.RUnlock()
	return u.networkUpstreams[networkId]
}

func (u *UpstreamsRegistry) GetAllUpstreams() []*Upstream {
	u.upstreamsMu.RLock()
	defer u.upstreamsMu.RUnlock()
	return u.allUpstreams
}

func (u *UpstreamsRegistry) GetSortedUpstreams(ctx context.Context, networkId, method string) ([]common.Upstream, error) {
	_, span := common.StartDetailSpan(ctx, "UpstreamsRegistry.GetSortedUpstreams")
	defer span.End()

	u.upstreamsMu.RLock()
	upsList := u.sortedUpstreams[networkId][method]
	u.upstreamsMu.RUnlock()

	if upsList == nil {
		networkMu := u.getNetworkMutex(networkId)
		networkMu.Lock()
		defer networkMu.Unlock()

		u.upstreamsMu.RLock()
		upsList = u.sortedUpstreams[networkId]["*"]
		if upsList == nil {
			upsList = u.networkUpstreams[networkId]
			if upsList == nil {
				u.upstreamsMu.RUnlock()
				return nil, common.NewErrNoUpstreamsFound(u.prjId, networkId)
			}
		}
		u.upstreamsMu.RUnlock()

		u.upstreamsMu.Lock()
		// Create a copy of the default upstreams list for this method
		methodUpsList := make([]*Upstream, len(upsList))
		copy(methodUpsList, upsList)

		if _, ok := u.sortedUpstreams[networkId]; !ok {
			u.sortedUpstreams[networkId] = make(map[string][]*Upstream)
		}
		u.sortedUpstreams[networkId][method] = methodUpsList

		if u.sortedUpstreams[networkId]["*"] == nil {
			cpUps := make([]*Upstream, len(methodUpsList))
			copy(cpUps, methodUpsList)
			u.sortedUpstreams[networkId]["*"] = cpUps
		}

		if u.sortedUpstreams["*"][method] == nil {
			cpUps := make([]*Upstream, len(methodUpsList))
			copy(cpUps, methodUpsList)
			u.sortedUpstreams["*"][method] = cpUps
		}

		// Initialize scores for this method on this network and "any" network
		for _, ups := range methodUpsList {
			upid := ups.Id()
			if _, ok := u.upstreamScores[upid]; !ok {
				u.upstreamScores[upid] = make(map[string]map[string]float64)
			}
			if _, ok := u.upstreamScores[upid][networkId]; !ok {
				u.upstreamScores[upid][networkId] = make(map[string]float64)
			}
			if _, ok := u.upstreamScores[upid][networkId][method]; !ok {
				u.upstreamScores[upid][networkId][method] = 0
			}
			if _, ok := u.upstreamScores[upid]["*"][method]; !ok {
				u.upstreamScores[upid]["*"][method] = 0
			}
		}
		u.upstreamsMu.Unlock()

		return castToCommonUpstreams(methodUpsList), nil
	}

	return castToCommonUpstreams(upsList), nil
}

func (u *UpstreamsRegistry) RLockUpstreams() {
	u.upstreamsMu.RLock()
}

func (u *UpstreamsRegistry) RUnlockUpstreams() {
	u.upstreamsMu.RUnlock()
}

func (u *UpstreamsRegistry) sortAndFilterUpstreams(networkId, method string, upstreams []*Upstream) []*Upstream {
	activeUpstreams := make([]*Upstream, 0)
	for _, ups := range upstreams {
		if !u.metricsTracker.IsCordoned(ups, method) {
			activeUpstreams = append(activeUpstreams, ups)
		}
	}

	// Get load balancer type from project's upstreamDefaults
	lbType := common.LoadBalancerTypeHighestScore // default to original behavior
	if len(activeUpstreams) > 0 {
		projectConfig := activeUpstreams[0].Config().ProjectConfig
		if projectConfig != nil && projectConfig.UpstreamDefaults != nil && projectConfig.UpstreamDefaults.LoadBalancer != nil {
			lbType = projectConfig.UpstreamDefaults.LoadBalancer.Type
		}
	}

	// Only sort by score if using highestScore or weightedRoundRobin
	if lbType == common.LoadBalancerTypeHighestScore || lbType == common.LoadBalancerTypeWeightedRoundRobin {
		// Calculate total score
		totalScore := 0.0
		for _, ups := range activeUpstreams {
			score := u.upstreamScores[ups.Id()][networkId][method]
			if score > 0 {
				totalScore += score
			}
		}

		// If all scores are 0, fall back to random shuffle
		if totalScore == 0 {
			rand.Shuffle(len(activeUpstreams), func(i, j int) {
				activeUpstreams[i], activeUpstreams[j] = activeUpstreams[j], activeUpstreams[i]
			})
			return activeUpstreams
		}

		sort.Slice(activeUpstreams, func(i, j int) bool {
			scoreI := u.upstreamScores[activeUpstreams[i].Id()][networkId][method]
			scoreJ := u.upstreamScores[activeUpstreams[j].Id()][networkId][method]

			if scoreI < 0 {
				scoreI = 0
			}
			if scoreJ < 0 {
				scoreJ = 0
			}

			if scoreI != scoreJ {
				return scoreI > scoreJ
			}

			// If values are equal, sort by upstream ID for consistency
			return activeUpstreams[i].Id() < activeUpstreams[j].Id()
		})
	}

	return activeUpstreams
}

func (u *UpstreamsRegistry) RefreshUpstreamNetworkMethodScores() error {
	ctx, span := common.StartDetailSpan(u.appCtx, "UpstreamsRegistry.RefreshUpstreamNetworkMethodScores")
	defer span.End()

	u.upstreamsMu.Lock()
	defer u.upstreamsMu.Unlock()

	if len(u.allUpstreams) == 0 {
		u.logger.Trace().Str("projectId", u.prjId).Msgf("no upstreams yet to refresh scores")
		return nil
	}

	ln := len(u.sortedUpstreams)

	allNetworks := make([]string, 0, ln)
	for networkId := range u.sortedUpstreams {
		allNetworks = append(allNetworks, networkId)
	}

	for _, networkId := range allNetworks {
		for method := range u.sortedUpstreams[networkId] {
			// Create a copy of all the the upstreams so we can re-add
			// previously cordoned upstreams that might have become healthy and uncordoned.
			var upsList []*Upstream
			if networkId == "*" {
				// This branch means we want to sort and score all upstreams for all their networks
				upsList = append([]*Upstream{}, u.allUpstreams...)
			} else {
				upsList = append([]*Upstream{}, u.networkUpstreams[networkId]...)
			}
			u.updateScoresAndSort(ctx, networkId, method, upsList)
		}
	}

	return nil
}

func (u *UpstreamsRegistry) registerUpstream(ctx context.Context, upsCfgs ...*common.UpstreamConfig) error {
	tasks := make([]*util.BootstrapTask, 0)
	for _, c := range upsCfgs {
		upsCfg := c
		tasks = append(tasks, u.buildUpstreamBootstrapTask(upsCfg))
	}
	return u.initializer.ExecuteTasks(ctx, tasks...)
}

func (u *UpstreamsRegistry) buildUpstreamBootstrapTask(upsCfg *common.UpstreamConfig) *util.BootstrapTask {
	cfg := new(common.UpstreamConfig)
	*cfg = *upsCfg
	return util.NewBootstrapTask(
		fmt.Sprintf("upstream/%s", cfg.Id),
		func(ctx context.Context) error {
			_, span := common.StartDetailSpan(ctx, "UpstreamsRegistry.buildUpstreamBootstrapTask")
			defer span.End()

			u.logger.Debug().Str("upstreamId", cfg.Id).Msg("attempt to bootstrap upstream")

			u.upstreamsMu.RLock()
			var ups *Upstream
			for _, up := range u.allUpstreams {
				if up.Id() == cfg.Id {
					ups = up
					break
				}
			}
			u.upstreamsMu.RUnlock()

			var err error
			if ups == nil {
				ups, err = u.NewUpstream(cfg)
				if err != nil {
					return err
				}
			}

			err = ups.Bootstrap(ctx)
			if err != nil {
				return err
			}
			u.doRegisterBootstrappedUpstream(ups)

			if u.onUpstreamRegistered != nil {
				// TODO Refactor the upstream<->network relationship to avoid circular dependency. Then we can remove this goroutine.
				// We need this now at the moment so that lazy-loaded networks and lazy-loaded upstreams (from Providers) can work together.
				go func() {
					err = u.onUpstreamRegistered(ups)
					if err != nil {
						u.logger.Error().Err(err).Str("upstreamId", cfg.Id).Msg("failed to call onUpstreamRegistered")
					}
				}()
			}

			u.logger.Debug().Str("upstreamId", cfg.Id).Msg("upstream bootstrap completed")
			return nil
		},
	)
}

func (u *UpstreamsRegistry) buildProviderBootstrapTask(
	provider *thirdparty.Provider,
	networkId string,
) *util.BootstrapTask {
	taskName := fmt.Sprintf("provider/%s/network/%s", provider.Id(), networkId)
	return util.NewBootstrapTask(
		taskName,
		func(ctx context.Context) error {
			_, span := common.StartDetailSpan(ctx, "UpstreamsRegistry.buildProviderBootstrapTask")
			defer span.End()

			lg := u.logger.With().Str("provider", provider.Id()).Str("networkId", networkId).Logger()
			lg.Debug().Msg("attempting to create upstream(s) from provider")

			if ok, err := provider.SupportsNetwork(ctx, networkId); err == nil && !ok {
				lg.Debug().Msg("provider does not support network; skipping upstream creation")
				return nil
			} else if err != nil {
				return err
			}

			upsCfgs, err := provider.GenerateUpstreamConfigs(ctx, &lg, networkId)
			if err != nil {
				return err
			}
			if lg.GetLevel() <= zerolog.DebugLevel {
				lg.Debug().Interface("upstreams", upsCfgs).Msgf("created %d upstream(s) from provider", len(upsCfgs))
			} else {
				lg.Info().Msgf("registering %d upstream(s) from provider", len(upsCfgs))
			}
			err = u.registerUpstream(ctx, upsCfgs...)
			if err != nil {
				lg.Error().Err(err).Msg("failed to bootstrap upstreams from provider")
				return err
			}
			return nil
		},
	)
}

func (u *UpstreamsRegistry) doRegisterBootstrappedUpstream(ups *Upstream) {
	networkId := ups.NetworkId()
	cfg := ups.Config()

	u.upstreamsMu.Lock()
	defer u.upstreamsMu.Unlock()

	u.allUpstreams = append(u.allUpstreams, ups)

	// Initialize the upstream's score maps
	if _, ok := u.upstreamScores[cfg.Id]; !ok {
		u.upstreamScores[cfg.Id] = make(map[string]map[string]float64)
	}

	// Initialize wildcard network scores
	if _, ok := u.upstreamScores[cfg.Id]["*"]; !ok {
		u.upstreamScores[cfg.Id]["*"] = make(map[string]float64)
	}
	if _, ok := u.upstreamScores[cfg.Id]["*"]["*"]; !ok {
		u.upstreamScores[cfg.Id]["*"]["*"] = 0
	}

	// Initialize specific network scores
	if _, ok := u.upstreamScores[cfg.Id][networkId]; !ok {
		u.upstreamScores[cfg.Id][networkId] = make(map[string]float64)
	}
	if _, ok := u.upstreamScores[cfg.Id][networkId]["*"]; !ok {
		u.upstreamScores[cfg.Id][networkId]["*"] = 0
	}

	// Initialize wildcard sorted upstreams
	if _, ok := u.sortedUpstreams["*"]; !ok {
		u.sortedUpstreams["*"] = make(map[string][]*Upstream)
	}
	if _, ok := u.sortedUpstreams["*"]["*"]; !ok {
		u.sortedUpstreams["*"]["*"] = []*Upstream{}
	}

	// Initialize network-specific sorted upstreams
	if _, ok := u.sortedUpstreams[networkId]; !ok {
		u.sortedUpstreams[networkId] = make(map[string][]*Upstream)
	}
	if _, ok := u.sortedUpstreams[networkId]["*"]; !ok {
		u.sortedUpstreams[networkId]["*"] = []*Upstream{}
	}

	// Add to network upstreams map
	isShadow := ups.Config() != nil && ups.Config().Shadow != nil && ups.Config().Shadow.Enabled
	exists := false
	if isShadow {
		for _, existingUps := range u.networkShadowUpstreams[networkId] {
			if existingUps.Id() == cfg.Id {
				exists = true
				break
			}
		}
		if !exists {
			u.networkShadowUpstreams[networkId] = append(u.networkShadowUpstreams[networkId], ups)
		}
	} else {
		for _, existingUps := range u.networkUpstreams[networkId] {
			if existingUps.Id() == cfg.Id {
				exists = true
				break
			}
		}
		if !exists {
			u.networkUpstreams[networkId] = append(u.networkUpstreams[networkId], ups)
		}
	}

	// Add to wildcard sorted upstreams if not already present
	found := false
	for _, existingUps := range u.sortedUpstreams["*"]["*"] {
		if existingUps.Id() == cfg.Id {
			found = true
			break
		}
	}
	if !found {
		u.sortedUpstreams["*"]["*"] = append(u.sortedUpstreams["*"]["*"], ups)
	}

	for method, netUps := range u.sortedUpstreams["*"] {
		methodUpsFound := false
		for _, existingUps := range netUps {
			if existingUps.Id() == cfg.Id {
				methodUpsFound = true
				break
			}
		}
		if !methodUpsFound {
			u.sortedUpstreams["*"][method] = append(u.sortedUpstreams["*"][method], ups)
		}
	}

	// Add to network-specific sorted upstreams if not already present
	found = false
	for _, existingUps := range u.sortedUpstreams[networkId]["*"] {
		if existingUps.Id() == cfg.Id {
			found = true
			break
		}
	}
	if !found {
		u.sortedUpstreams[networkId]["*"] = append(u.sortedUpstreams[networkId]["*"], ups)
	} else {
		for method, netUps := range u.sortedUpstreams[networkId] {
			methodUpsFound := false
			for _, existingUps := range netUps {
				if existingUps.Id() == cfg.Id {
					methodUpsFound = true
					break
				}
			}
			if !methodUpsFound {
				u.sortedUpstreams[networkId][method] = append(u.sortedUpstreams[networkId][method], ups)
			}
		}
	}

	u.logger.Debug().
		Str("upstreamId", cfg.Id).
		Str("networkId", networkId).
		Msg("upstream registered and initialized in registry")
}

func (u *UpstreamsRegistry) scheduleScoreCalculationTimers(ctx context.Context) error {
	if u.scoreRefreshInterval == 0 {
		return nil
	}

	go func() {
		ticker := time.NewTicker(u.scoreRefreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				err := u.RefreshUpstreamNetworkMethodScores()
				if err != nil {
					u.logger.Warn().Err(err).Msgf("failed to refresh upstream network method scores")
				}
			}
		}
	}()

	return nil
}

func (u *UpstreamsRegistry) updateScoresAndSort(ctx context.Context, networkId, method string, upsList []*Upstream) {
	_, span := common.StartDetailSpan(ctx, "UpstreamsRegistry.UpdateScoresAndSort")
	defer span.End()

	// Get load balancer type from project's upstreamDefaults
	lbType := common.LoadBalancerTypeHighestScore // default to original behavior
	if len(upsList) > 0 {
		projectConfig := upsList[0].Config().ProjectConfig
		if projectConfig != nil && projectConfig.UpstreamDefaults != nil && projectConfig.UpstreamDefaults.LoadBalancer != nil {
			lbType = projectConfig.UpstreamDefaults.LoadBalancer.Type
		}
	}

	// Only update scores if using highestScore or weightedRoundRobin
	if lbType == common.LoadBalancerTypeHighestScore || lbType == common.LoadBalancerTypeWeightedRoundRobin {
		var respLatencies, errorRates, totalRequests, throttledRates, blockHeadLags, finalizationLags []float64

		for _, ups := range upsList {
			qn := 0.70
			cfg := ups.Config()
			if cfg != nil && cfg.Routing != nil && cfg.Routing.ScoreLatencyQuantile != 0 {
				qn = cfg.Routing.ScoreLatencyQuantile
			}
			metrics := u.metricsTracker.GetUpstreamMethodMetrics(ups, method)
			latency := metrics.ResponseQuantiles.GetQuantile(qn).Seconds()

			// Handle zero latency values: if an upstream has zero latency, it likely means
			// it has no successful requests (100% error rate), so we should treat this as
			// invalid data rather than the best possible latency.
			// We'll use a sentinel value that will be handled in normalization.
			if latency == 0.0 && metrics.ErrorRate() > 0.5 {
				// If latency is zero and error rate is high, treat as invalid latency
				latency = -1.0 // Sentinel value for invalid latency
			}

			respLatencies = append(respLatencies, latency)
			blockHeadLags = append(blockHeadLags, float64(metrics.BlockHeadLag.Load()))
			finalizationLags = append(finalizationLags, float64(metrics.FinalizationLag.Load()))
			errorRates = append(errorRates, metrics.ErrorRate())
			throttledRates = append(throttledRates, metrics.ThrottledRate())
			totalRequests = append(totalRequests, float64(metrics.RequestsTotal.Load()))
		}

		normRespLatencies := normalizeValuesLogWithInvalid(respLatencies)
		normErrorRates := normalizeValues(errorRates)
		normThrottledRates := normalizeValues(throttledRates)
		normTotalRequests := normalizeValues(totalRequests)
		normBlockHeadLags := normalizeValuesLog(blockHeadLags)
		normFinalizationLags := normalizeValuesLog(finalizationLags)
		for i, ups := range upsList {
			upsId := ups.Id()
			score := u.calculateScore(
				ups,
				networkId,
				method,
				normTotalRequests[i],
				normRespLatencies[i],
				normErrorRates[i],
				normThrottledRates[i],
				normBlockHeadLags[i],
				normFinalizationLags[i],
			)
			// Upstream might not have scores initialized yet (especially when networkId is *)
			// TODO add a test case to send request to network A when network B is defined in config but no requests sent yet
			if upsc, ok := u.upstreamScores[upsId]; ok {
				if _, ok := upsc[networkId]; ok {
					upsc[networkId][method] = score
				}
			}
			ups.logger.Trace().
				Str("method", method).
				Float64("score", score).
				Float64("normalizedTotalRequests", normTotalRequests[i]).
				Float64("normalizedRespLatency", normRespLatencies[i]).
				Float64("normalizedErrorRate", normErrorRates[i]).
				Float64("normalizedThrottledRate", normThrottledRates[i]).
				Float64("normalizedBlockHeadLag", normBlockHeadLags[i]).
				Float64("normalizedFinalizationLag", normFinalizationLags[i]).
				Msg("score updated")
			telemetry.MetricUpstreamScoreOverall.WithLabelValues(u.prjId, ups.VendorName(), networkId, upsId, method).Set(score)
		}
	}

	// Filter out cordoned upstreams
	activeUpstreams := make([]*Upstream, 0)
	for _, ups := range upsList {
		if !u.metricsTracker.IsCordoned(ups, method) {
			activeUpstreams = append(activeUpstreams, ups)
		}
	}

	// Only sort if using highestScore or weightedRoundRobin
	if lbType == common.LoadBalancerTypeHighestScore || lbType == common.LoadBalancerTypeWeightedRoundRobin {
		// Calculate total score
		totalScore := 0.0
		for _, ups := range activeUpstreams {
			score := u.upstreamScores[ups.Id()][networkId][method]
			if score > 0 {
				totalScore += score
			}
		}

		// If all scores are 0, fall back to random shuffle
		if totalScore == 0 {
			rand.Shuffle(len(activeUpstreams), func(i, j int) {
				activeUpstreams[i], activeUpstreams[j] = activeUpstreams[j], activeUpstreams[i]
			})
		} else {
			sort.Slice(activeUpstreams, func(i, j int) bool {
				scoreI := u.upstreamScores[activeUpstreams[i].Id()][networkId][method]
				scoreJ := u.upstreamScores[activeUpstreams[j].Id()][networkId][method]

				if scoreI < 0 {
					scoreI = 0
				}
				if scoreJ < 0 {
					scoreJ = 0
				}

				if scoreI != scoreJ {
					return scoreI > scoreJ
				}

				// If values are equal, sort by upstream ID for consistency
				return activeUpstreams[i].Id() < activeUpstreams[j].Id()
			})
		}
	}

	u.sortedUpstreams[networkId][method] = activeUpstreams
}

func (u *UpstreamsRegistry) calculateScore(
	ups *Upstream,
	networkId,
	method string,
	normTotalRequests,
	normRespLatency,
	normErrorRate,
	normThrottledRate,
	normBlockHeadLag,
	normFinalizationLag float64,
) float64 {
	mul := ups.getScoreMultipliers(networkId, method)

	score := 0.0

	// Higher score for lower total requests (to balance the load)
	if mul.TotalRequests != nil && *mul.TotalRequests > 0 {
		score += expCurve(1-normTotalRequests) * *mul.TotalRequests
	}

	// Higher score for lower latency
	if mul.RespLatency != nil && *mul.RespLatency > 0 {
		score += expCurve(1-normRespLatency) * *mul.RespLatency
	}

	// Higher score for lower error rate
	if mul.ErrorRate != nil && *mul.ErrorRate > 0 {
		score += expCurve(1-normErrorRate) * *mul.ErrorRate
	}

	// Higher score for lower throttled rate
	if mul.ThrottledRate != nil && *mul.ThrottledRate > 0 {
		score += expCurve(1-normThrottledRate) * *mul.ThrottledRate
	}

	// Higher score for lower block head lag
	if mul.BlockHeadLag != nil && *mul.BlockHeadLag > 0 {
		score += expCurve(1-normBlockHeadLag) * *mul.BlockHeadLag
	}

	// Higher score for lower finalization lag
	if mul.FinalizationLag != nil && *mul.FinalizationLag > 0 {
		score += expCurve(1-normFinalizationLag) * *mul.FinalizationLag
	}

	if mul.Overall != nil && *mul.Overall > 0 {
		score *= *mul.Overall
	}
	return score
}

func expCurve(x float64) float64 {
	return math.Pow(x, 2.0)
}

func normalizeValues(values []float64) []float64 {
	if len(values) == 0 {
		return []float64{}
	}
	max := values[0]
	for _, value := range values {
		if value > max {
			max = value
		}
	}
	normalized := make([]float64, len(values))
	for i, value := range values {
		if max > 0 {
			normalized[i] = value / max
		} else {
			normalized[i] = 0
		}
	}
	return normalized
}

func normalizeValuesLog(values []float64) []float64 {
	if len(values) == 0 {
		return []float64{}
	}

	// Find the true min and max values in the input slice.
	// Assumes values are non-negative based on typical use for latencies, lags etc.
	dataMin := values[0]
	dataMax := values[0]
	for i := 1; i < len(values); i++ {
		if values[i] < dataMin {
			dataMin = values[i]
		}
		if values[i] > dataMax {
			dataMax = values[i]
		}
	}

	normalized := make([]float64, len(values))

	if dataMin == dataMax {
		// If all values are the same
		if dataMin == 0.0 {
			// All values are 0, result is all 0s (implicitly done by make).
		} else {
			// All values are a positive constant C, result is all 1s.
			// This makes behavior consistent with normalizeValues.
			for i := range normalized {
				normalized[i] = 1.0
			}
		}
		return normalized
	}

	// Apply log(v+1) transformation and scale to [0, 1]
	// log(dataMin+1) will be the minimum of the log-transformed values.
	// log(dataMax+1) will be the maximum of the log-transformed values.
	logMinOffset := math.Log(dataMin + 1.0)
	logMaxOffset := math.Log(dataMax + 1.0)
	denom := logMaxOffset - logMinOffset

	// Since dataMin < dataMax, dataMin+1 < dataMax+1,
	// so logMinOffset < logMaxOffset, and denom > 0.
	// Thus, no division by zero here.

	for i, v := range values {
		if v < 0 {
			// This case should ideally not happen for metrics like latency.
			// If it can, specific handling might be needed.
			// For now, map negative values to 0 to avoid math.Log errors with v+1.0 <= 0.
			normalized[i] = 0.0
			continue
		}
		logVOffset := math.Log(v + 1.0)
		norm := (logVOffset - logMinOffset) / denom

		// Clamp to [0, 1] as a safeguard against potential floating-point inaccuracies.
		if norm < 0.0 {
			norm = 0.0
		} else if norm > 1.0 {
			norm = 1.0
		}
		normalized[i] = norm
	}

	return normalized
}

func normalizeValuesLogWithInvalid(values []float64) []float64 {
	if len(values) == 0 {
		return []float64{}
	}

	// Separate valid and invalid values
	var validValues []float64
	var invalidIndices []int

	for i, v := range values {
		if v < 0 {
			// Negative values are our sentinel for invalid latency
			invalidIndices = append(invalidIndices, i)
		} else {
			validValues = append(validValues, v)
		}
	}

	normalized := make([]float64, len(values))

	// If all values are invalid, return all 1.0 (worst possible score)
	if len(validValues) == 0 {
		for i := range normalized {
			normalized[i] = 1.0
		}
		return normalized
	}

	// If all values are valid, use standard log normalization
	if len(invalidIndices) == 0 {
		return normalizeValuesLog(values)
	}

	// Mixed case: normalize valid values and assign worst score to invalid ones
	dataMin := validValues[0]
	dataMax := validValues[0]
	for i := 1; i < len(validValues); i++ {
		if validValues[i] < dataMin {
			dataMin = validValues[i]
		}
		if validValues[i] > dataMax {
			dataMax = validValues[i]
		}
	}

	// If all valid values are the same
	if dataMin == dataMax {
		for i, v := range values {
			if v < 0 {
				normalized[i] = 1.0 // Worst score for invalid values
			} else if dataMin == 0.0 {
				normalized[i] = 0.0 // All valid values are 0
			} else {
				normalized[i] = 0.5 // All valid values are the same positive value
			}
		}
		return normalized
	}

	// Apply log(v+1) transformation and scale to [0, 1] for valid values
	logMinOffset := math.Log(dataMin + 1.0)
	logMaxOffset := math.Log(dataMax + 1.0)
	denom := logMaxOffset - logMinOffset

	for i, v := range values {
		if v < 0 {
			// Invalid latency gets worst possible score
			normalized[i] = 1.0
		} else {
			logVOffset := math.Log(v + 1.0)
			norm := (logVOffset - logMinOffset) / denom

			// Clamp to [0, 1] as a safeguard
			if norm < 0.0 {
				norm = 0.0
			} else if norm > 1.0 {
				norm = 1.0
			}
			normalized[i] = norm
		}
	}

	return normalized
}

func (u *UpstreamsRegistry) GetUpstreamsHealth() (*UpstreamsHealth, error) {
	u.upstreamsMu.RLock()
	defer u.upstreamsMu.RUnlock()

	sortedUpstreams := make(map[string]map[string][]string)
	upstreamScores := make(map[string]map[string]map[string]float64)

	for nw, methods := range u.sortedUpstreams {
		for method, ups := range methods {
			upstreamIds := make([]string, len(ups))
			for i, ups := range ups {
				upstreamIds[i] = ups.Id()
			}
			if _, ok := sortedUpstreams[nw]; !ok {
				sortedUpstreams[nw] = make(map[string][]string)
			}
			sortedUpstreams[nw][method] = upstreamIds
		}
	}

	for upsId, nwMethods := range u.upstreamScores {
		for nw, methods := range nwMethods {
			for method, score := range methods {
				if _, ok := upstreamScores[upsId]; !ok {
					upstreamScores[upsId] = make(map[string]map[string]float64)
				}
				if _, ok := upstreamScores[upsId][nw]; !ok {
					upstreamScores[upsId][nw] = make(map[string]float64)
				}
				upstreamScores[upsId][nw][method] = score
			}
		}
	}

	return &UpstreamsHealth{
		Upstreams:       u.allUpstreams,
		SortedUpstreams: sortedUpstreams,
		UpstreamScores:  upstreamScores,
	}, nil
}

func (u *UpstreamsRegistry) GetMetricsTracker() *health.Tracker {
	return u.metricsTracker
}

func (u *UpstreamsRegistry) GetNextUpstream(ctx context.Context, networkId, method string) (*Upstream, error) {
	u.upstreamsMu.Lock()
	defer u.upstreamsMu.Unlock()

	// Get sorted upstreams
	upsList := u.sortedUpstreams[networkId][method]
	if upsList == nil {
		upsList = u.sortedUpstreams[networkId]["*"]
		if upsList == nil {
			upsList = u.networkUpstreams[networkId]
			if upsList == nil {
				return nil, common.NewErrNoUpstreamsFound(u.prjId, networkId)
			}
		}

		// Create a copy of the default upstreams list for this method
		methodUpsList := make([]*Upstream, len(upsList))
		copy(methodUpsList, upsList)

		if _, ok := u.sortedUpstreams[networkId]; !ok {
			u.sortedUpstreams[networkId] = make(map[string][]*Upstream)
		}
		u.sortedUpstreams[networkId][method] = methodUpsList

		if u.sortedUpstreams[networkId]["*"] == nil {
			cpUps := make([]*Upstream, len(methodUpsList))
			copy(cpUps, methodUpsList)
			u.sortedUpstreams[networkId]["*"] = cpUps
		}

		if u.sortedUpstreams["*"][method] == nil {
			cpUps := make([]*Upstream, len(methodUpsList))
			copy(cpUps, methodUpsList)
			u.sortedUpstreams["*"][method] = cpUps
		}

		// Initialize scores for this method on this network and "any" network
		for _, ups := range methodUpsList {
			upid := ups.Id()
			if _, ok := u.upstreamScores[upid]; !ok {
				u.upstreamScores[upid] = make(map[string]map[string]float64)
			}
			if _, ok := u.upstreamScores[upid][networkId]; !ok {
				u.upstreamScores[upid][networkId] = make(map[string]float64)
			}
			if _, ok := u.upstreamScores[upid][networkId][method]; !ok {
				u.upstreamScores[upid][networkId][method] = 0
			}
			if _, ok := u.upstreamScores[upid]["*"][method]; !ok {
				u.upstreamScores[upid]["*"][method] = 0
			}
		}

		upsList = methodUpsList
	}

	if len(upsList) == 0 {
		return nil, common.NewErrNoUpstreamsFound(u.prjId, networkId)
	}

	// Get load balancer type from project's upstreamDefaults
	lbType := common.LoadBalancerTypeHighestScore
	if len(upsList) > 0 {
		projectConfig := upsList[0].Config().ProjectConfig
		if projectConfig != nil && projectConfig.UpstreamDefaults != nil && projectConfig.UpstreamDefaults.LoadBalancer != nil {
			lbType = projectConfig.UpstreamDefaults.LoadBalancer.Type
		}
	}

	// Filter out cordoned upstreams
	activeUpstreams := make([]*Upstream, 0)
	for _, ups := range upsList {
		if !u.metricsTracker.IsCordoned(ups, method) {
			activeUpstreams = append(activeUpstreams, ups)
		}
	}

	if len(activeUpstreams) == 0 {
		return nil, common.NewErrNoUpstreamsFound(u.prjId, networkId)
	}

	// Select next upstream based on load balancer type
	switch lbType {
	case common.LoadBalancerTypeRoundRobin:
		return u.getNextRoundRobin(networkId, method, activeUpstreams)
	case common.LoadBalancerTypeWeightedRoundRobin:
		return u.getNextWeightedRoundRobin(networkId, method, activeUpstreams)
	case common.LoadBalancerTypeLeastConnection:
		return u.getNextLeastConnection(networkId, method, activeUpstreams)
	default: // LoadBalancerTypeHighestScore
		return activeUpstreams[0], nil
	}
}

func (u *UpstreamsRegistry) getNextRoundRobin(networkId, method string, upstreams []*Upstream) (*Upstream, error) {
	// Initialize index if needed
	if _, ok := u.rrIndices[networkId]; !ok {
		u.rrIndices[networkId] = make(map[string]int)
	}
	if _, ok := u.rrIndices[networkId][method]; !ok {
		u.rrIndices[networkId][method] = 0
	}

	// Get current index and update for next time
	idx := u.rrIndices[networkId][method]
	u.rrIndices[networkId][method] = (idx + 1) % len(upstreams)

	u.logger.Debug().
		Str("networkId", networkId).
		Str("method", method).
		Int("currentIndex", idx).
		Int("nextIndex", u.rrIndices[networkId][method]).
		Int("totalUpstreams", len(upstreams)).
		Str("selectedUpstream", upstreams[idx].Id()).
		Msg("round robin selection")

	return upstreams[idx], nil
}

func (u *UpstreamsRegistry) getNextWeightedRoundRobin(networkId, method string, upstreams []*Upstream) (*Upstream, error) {
	// Initialize weights map if not exists
	if _, ok := u.rrWeights[networkId]; !ok {
		u.rrWeights[networkId] = make(map[string][]float64)
	}
	if _, ok := u.rrWeights[networkId][method]; !ok {
		u.rrWeights[networkId][method] = make([]float64, len(upstreams))
	}

	// Calculate total weight based on scores
	totalWeight := 0.0
	for i, ups := range upstreams {
		score := u.upstreamScores[ups.Id()][networkId][method]
		if score < 0 {
			score = 0
		}
		u.rrWeights[networkId][method][i] = score
		totalWeight += score
	}

	// If all weights are zero, fall back to simple round-robin
	if totalWeight == 0 {
		u.logger.Debug().
			Str("networkId", networkId).
			Str("method", method).
			Msg("all weights are zero, falling back to simple round-robin")
		return u.getNextRoundRobin(networkId, method, upstreams)
	}

	// Normalize weights to sum to 1.0
	normalizedWeights := make([]float64, len(upstreams))
	for i := range upstreams {
		normalizedWeights[i] = u.rrWeights[networkId][method][i] / totalWeight
	}

	// Generate a random value between 0 and 1
	r := rand.Float64()

	// Find the first upstream whose cumulative weight exceeds the random value
	cumulativeWeight := 0.0
	for i, weight := range normalizedWeights {
		cumulativeWeight += weight
		if r <= cumulativeWeight {
			u.logger.Debug().
				Str("networkId", networkId).
				Str("method", method).
				Float64("randomValue", r).
				Float64("cumulativeWeight", cumulativeWeight).
				Float64("weight", weight).
				Int("selectedIndex", i).
				Str("selectedUpstream", upstreams[i].Id()).
				Msg("weighted round robin selection")
			return upstreams[i], nil
		}
	}

	// Fallback to first upstream (should never happen due to normalization)
	u.logger.Debug().
		Str("networkId", networkId).
		Str("method", method).
		Msg("falling back to first upstream in weighted round-robin")
	return upstreams[0], nil
}

func (u *UpstreamsRegistry) getNextLeastConnection(networkId, method string, upstreams []*Upstream) (*Upstream, error) {
	// Find upstream with least active connections
	minConnections := int64(math.MaxInt64)
	var selectedUpstream *Upstream

	for _, ups := range upstreams {
		metrics := u.metricsTracker.GetUpstreamMethodMetrics(ups, method)
		// Active connections = total requests - errors
		activeConnections := metrics.RequestsTotal.Load() - metrics.ErrorsTotal.Load()

		if activeConnections < minConnections {
			minConnections = activeConnections
			selectedUpstream = ups
		}
	}

	if selectedUpstream == nil {
		return nil, common.NewErrNoUpstreamsFound(u.prjId, networkId)
	}

	return selectedUpstream, nil
}

func castToCommonUpstreams(upstreams []*Upstream) []common.Upstream {
	commonUpstreams := make([]common.Upstream, len(upstreams))
	for i, ups := range upstreams {
		commonUpstreams[i] = ups
	}
	return commonUpstreams
}
