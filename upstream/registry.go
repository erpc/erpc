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

	allUpstreams []*Upstream
	upstreamsMu  *sync.RWMutex
	networkMu    *sync.Map // map[string]*sync.RWMutex for per-network locks
	// map of network => upstreams
	networkUpstreams map[string][]*Upstream
	// map of network -> method (or *) => upstreams
	sortedUpstreams map[string]map[string][]*Upstream
	// map of upstream -> network (or *) -> method (or *) => score
	upstreamScores map[string]map[string]map[string]float64

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
) *UpstreamsRegistry {
	lg := logger.With().Str("component", "upstreamsRegistry").Logger()
	return &UpstreamsRegistry{
		appCtx:               appCtx,
		prjId:                prjId,
		scoreRefreshInterval: scoreRefreshInterval,
		logger:               logger,
		sharedStateRegistry:  ssr,
		clientRegistry:       clients.NewClientRegistry(logger, prjId, ppr),
		rateLimitersRegistry: rr,
		vendorsRegistry:      vr,
		providersRegistry:    pr,
		metricsTracker:       mt,
		upsCfg:               upsCfg,
		networkUpstreams:     make(map[string][]*Upstream),
		sortedUpstreams:      make(map[string]map[string][]*Upstream),
		upstreamScores:       make(map[string]map[string]map[string]float64),
		upstreamsMu:          &sync.RWMutex{},
		networkMu:            &sync.Map{},
		initializer:          util.NewInitializer(appCtx, &lg, nil),
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

func (u *UpstreamsRegistry) GetSortedUpstreams(ctx context.Context, networkId, method string) ([]*Upstream, error) {
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

		return methodUpsList, nil
	}

	return upsList, nil
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

	if u.logger.Trace().Enabled() {
		ids := make([]string, len(activeUpstreams))
		for i, ups := range activeUpstreams {
			ids[i] = ups.Id()
		}
		scores := make([]float64, len(activeUpstreams))
		for i, ups := range activeUpstreams {
			scores[i] = u.upstreamScores[ups.Id()][networkId][method]
		}
		// u.logger.Trace().
		// 	Str("networkId", networkId).
		// 	Str("method", method).
		// 	Strs("upstreams", ids).
		// 	Floats64("scores", scores).
		// 	Msgf("sorted upstreams")
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

			upsCfgs, err := provider.GenerateUpstreamConfigs(networkId)
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
	exists := false
	for _, existingUps := range u.networkUpstreams[networkId] {
		if existingUps.Id() == cfg.Id {
			exists = true
			break
		}
	}
	if !exists {
		u.networkUpstreams[networkId] = append(u.networkUpstreams[networkId], ups)
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

	var respLatencies, errorRates, totalRequests, throttledRates, blockHeadLags, finalizationLags []float64

	for _, ups := range upsList {
		qn := 0.70
		cfg := ups.Config()
		if cfg != nil && cfg.Routing != nil && cfg.Routing.ScoreLatencyQuantile != 0 {
			qn = cfg.Routing.ScoreLatencyQuantile
		}
		metrics := u.metricsTracker.GetUpstreamMethodMetrics(ups, method)
		respLatencies = append(respLatencies, metrics.ResponseQuantiles.GetQuantile(qn).Seconds())
		blockHeadLags = append(blockHeadLags, float64(metrics.BlockHeadLag.Load()))
		finalizationLags = append(finalizationLags, float64(metrics.FinalizationLag.Load()))
		errorRates = append(errorRates, metrics.ErrorRate())
		throttledRates = append(throttledRates, metrics.ThrottledRate())
		totalRequests = append(totalRequests, float64(metrics.RequestsTotal.Load()))
	}

	normRespLatencies := normalizeValues(respLatencies)
	normErrorRates := normalizeValues(errorRates)
	normThrottledRates := normalizeValues(throttledRates)
	normTotalRequests := normalizeValues(totalRequests)
	normBlockHeadLags := normalizeValues(blockHeadLags)
	normFinalizationLags := normalizeValues(finalizationLags)
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

	upsList = u.sortAndFilterUpstreams(networkId, method, upsList)
	u.sortedUpstreams[networkId][method] = upsList
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
	if mul.TotalRequests > 0 {
		score += expCurve(1-normTotalRequests) * mul.TotalRequests
	}

	// Higher score for lower latency
	if mul.RespLatency > 0 {
		score += expCurve(1-normRespLatency) * mul.RespLatency
	}

	// Higher score for lower error rate
	if mul.ErrorRate > 0 {
		score += expCurve(1-normErrorRate) * mul.ErrorRate
	}

	// Higher score for lower throttled rate
	if mul.ThrottledRate > 0 {
		score += expCurve(1-normThrottledRate) * mul.ThrottledRate
	}

	// Higher score for lower block head lag
	if mul.BlockHeadLag > 0 {
		score += expCurve(1-normBlockHeadLag) * mul.BlockHeadLag
	}

	// Higher score for lower finalization lag
	if mul.FinalizationLag > 0 {
		score += expCurve(1-normFinalizationLag) * mul.FinalizationLag
	}

	return score * mul.Overall
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
