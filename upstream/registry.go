package upstream

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/clients"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/thirdparty"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
)

// ScoringConfig bundles all project-level scoring/routing parameters.
// Pass nil to NewUpstreamsRegistry to use defaults.
type ScoringConfig struct {
	RoutingStrategy   string
	ScoreGranularity  string
	PenaltyDecayRate  float64
	SwitchHysteresis  float64
	MinSwitchInterval time.Duration
}

func (c *ScoringConfig) withDefaults() *ScoringConfig {
	out := &ScoringConfig{}
	if c != nil {
		*out = *c
	}
	if out.RoutingStrategy == "" {
		out.RoutingStrategy = "score-based"
	}
	if out.ScoreGranularity == "" {
		out.ScoreGranularity = "upstream"
	}
	if out.PenaltyDecayRate == 0 {
		out.PenaltyDecayRate = 0.95
	}
	// Negative → disabled (no stickiness). Zero → use default.
	if out.SwitchHysteresis == 0 {
		out.SwitchHysteresis = 0.10
	} else if out.SwitchHysteresis < 0 {
		out.SwitchHysteresis = 0
	}
	if out.MinSwitchInterval == 0 {
		out.MinSwitchInterval = 2 * time.Minute
	} else if out.MinSwitchInterval < 0 {
		out.MinSwitchInterval = 0
	}
	return out
}

type UpstreamsRegistry struct {
	appCtx               context.Context
	prjId                string
	scoreRefreshInterval time.Duration
	scoringCfg           *ScoringConfig
	refreshMu            sync.Mutex
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
	networkMu    *sync.Map
	// map of network => upstreams
	networkUpstreams       map[string][]*Upstream
	networkShadowUpstreams map[string][]*Upstream
	networkUpstreamsAtomic sync.Map
	// map of network -> method (or *) => upstreams
	sortedUpstreams map[string]map[string][]*Upstream
	// map of upstream -> network (or *) -> method (or *) => score (displayed as 1/(1+penalty))
	upstreamScores map[string]map[string]map[string]float64

	// penalty state: upstream -> network -> method -> decayed penalty
	penaltyState map[string]map[string]map[string]float64
	// last switch time per (network, method) for stickiness guard
	lastSwitchTime map[string]map[string]time.Time
	// round-robin rotation counter per (network, method)
	rotationCounters map[string]map[string]uint64

	onUpstreamRegistered func(ups *Upstream) error
	scoreMetricsMode     telemetry.ScoreMetricsMode
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
	scoringCfg *ScoringConfig,
	onUpstreamRegistered func(*Upstream) error,
) *UpstreamsRegistry {
	lg := logger.With().Str("component", "upstreams").Logger()
	return &UpstreamsRegistry{
		appCtx:               appCtx,
		prjId:                prjId,
		scoreRefreshInterval: scoreRefreshInterval,
		scoringCfg:           scoringCfg.withDefaults(),
		logger:               logger,
		sharedStateRegistry:  ssr,
		clientRegistry: clients.NewClientRegistry(
			logger,
			prjId,
			ppr,
			evm.NewJsonRpcErrorExtractor(),
		),
		rateLimitersRegistry:   rr,
		vendorsRegistry:        vr,
		providersRegistry:      pr,
		metricsTracker:         mt,
		upsCfg:                 upsCfg,
		networkUpstreams:       make(map[string][]*Upstream),
		networkShadowUpstreams: make(map[string][]*Upstream),
		sortedUpstreams:        make(map[string]map[string][]*Upstream),
		upstreamScores:         make(map[string]map[string]map[string]float64),
		penaltyState:           make(map[string]map[string]map[string]float64),
		lastSwitchTime:         make(map[string]map[string]time.Time),
		rotationCounters:       make(map[string]map[string]uint64),
		upstreamsMu:            &sync.RWMutex{},
		networkMu:              &sync.Map{},
		initializer:            util.NewInitializer(appCtx, &lg, nil),
		onUpstreamRegistered:   onUpstreamRegistered,
		scoreMetricsMode:       telemetry.ScoreModeCompact,
	}
}

// SetScoreMetricsMode sets the emission mode for upstream score metrics.
// Any value other than ScoreModeDetailed or ScoreModeNone is treated as ScoreModeCompact.
func (u *UpstreamsRegistry) SetScoreMetricsMode(mode telemetry.ScoreMetricsMode) {
	switch mode {
	case telemetry.ScoreModeDetailed:
		u.scoreMetricsMode = telemetry.ScoreModeDetailed
	case telemetry.ScoreModeNone:
		u.scoreMetricsMode = telemetry.ScoreModeNone
	default:
		u.scoreMetricsMode = telemetry.ScoreModeCompact
	}
}

func (u *UpstreamsRegistry) Bootstrap(ctx context.Context) {
	u.scheduleScoreCalculationTimers(ctx)

	// Fire-and-forget: register upstreams in background to avoid blocking service startup
	go func() {
		if err := u.registerUpstreams(u.appCtx, u.upsCfg...); err != nil {
			u.logger.Error().Err(err).Msg("failed to register upstreams in background")
		} else {
			u.logger.Info().Msg("upstreams registration completed")
		}
	}()
}

func (u *UpstreamsRegistry) NewUpstream(cfg *common.UpstreamConfig) (*Upstream, error) {
	// Warn about deprecated upstream-level eth_getLogs hard limits that are ignored now
	if cfg != nil && cfg.Evm != nil {
		if cfg.Evm.DeprecatedGetLogsMaxAllowedRange > 0 || cfg.Evm.DeprecatedGetLogsMaxAllowedAddresses > 0 || cfg.Evm.DeprecatedGetLogsMaxAllowedTopics > 0 {
			u.logger.Warn().
				Str("upstreamId", cfg.Id).
				Msg("deprecated upstream-level getLogs maxAllowed* configs detected; they are ignored. Configure limits at network-level evm.* instead")
		}
		if cfg.Evm.DeprecatedGetLogsSplitOnError != nil {
			u.logger.Warn().
				Str("upstreamId", cfg.Id).
				Msg("deprecated upstream-level getLogsSplitOnError detected; it is ignored. Configure at network-level evm.getLogsSplitOnError instead")
		}
	}
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

	// 1) Static upstreams are expected to be registered via Bootstrap() already.

	// 2) Start execution of provider-based upstreams tasks in background; errors are logged but do not fail the caller
	allProviders := u.providersRegistry.GetAllProviders()
	var tasks []*util.BootstrapTask
	for _, p := range allProviders {
		provider := p
		t := u.buildProviderBootstrapTask(provider, networkId)
		tasks = append(tasks, t)
	}
	go func() {
		if err := u.initializer.ExecuteTasks(ctx, tasks...); err != nil {
			u.logger.Error().
				Err(err).
				Str("networkId", networkId).
				Interface("status", u.initializer.Status()).
				Msg("failed to execute provider bootstrap tasks")
		} else {
			u.logger.Info().
				Str("networkId", networkId).
				Interface("status", u.initializer.Status()).
				Msg("provider bootstrap tasks executed successfully")
		}
	}()

	// Require only one ready upstream to start handling requests.
	// Use adaptive timeout: default to 30s, but cap to the caller's context deadline if sooner.
	minReady := 1
	waitTimeout := 30 * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		until := time.Until(deadline)
		if until < waitTimeout {
			if until <= 0 {
				// Ensure a small positive wait in case the deadline is already due
				until = 50 * time.Millisecond
			}
			waitTimeout = until
		}
	}

	// 3) Wait until either static upstreams or provider-based upstreams are ready, or timeout/context ends
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	timeoutCtx, cancel := context.WithTimeout(ctx, waitTimeout)
	defer cancel()
	for {
		select {
		case <-timeoutCtx.Done():
			err := timeoutCtx.Err()
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				// Timeout reached, check if we have any upstreams ready
				u.upstreamsMu.RLock()
				upstreamsCount := len(u.networkUpstreams[networkId])
				u.upstreamsMu.RUnlock()

				if upstreamsCount > 0 {
					u.logger.Info().
						Str("networkId", networkId).
						Int("upstreamsCount", upstreamsCount).
						Int("minReady", minReady).
						Dur("waitTimeout", waitTimeout).
						Msg("timeout reached but some upstreams are ready for network initialization")
					return nil
				}
				// Consider per-network and unknown upstream/provider activity
				summary := u.summarizeNetworkTasks(networkId)
				if summary.hasOngoing {
					return common.NewErrNetworkInitializing(u.prjId, networkId)
				}
				if summary.providersAllTerminal {
					// Grace period to allow in-flight upstream registrations to complete
					time.Sleep(100 * time.Millisecond)
					u.upstreamsMu.RLock()
					latest := len(u.networkUpstreams[networkId])
					u.upstreamsMu.RUnlock()
					if latest >= minReady {
						u.logger.Info().
							Str("networkId", networkId).
							Int("upstreamsCount", latest).
							Msg("upstreams became ready during grace period after providers terminal state")
						return nil
					}
					// Return a retryable error so the auto-retry loop can
					// re-attempt when upstreams may have recovered.
					return common.NewErrNetworkNotSupported(u.prjId, networkId)
				}
				// Default: initializing
				return common.NewErrNetworkInitializing(u.prjId, networkId)
			}
			return timeoutCtx.Err()
		case <-ticker.C:
			// Check how many upstreams are ready for this network
			u.upstreamsMu.RLock()
			upstreamsCount := len(u.networkUpstreams[networkId])
			u.upstreamsMu.RUnlock()

			if upstreamsCount < minReady {
				// Keep waiting if there is any ongoing per-network or unknown upstream work
				summary := u.summarizeNetworkTasks(networkId)
				if summary.hasOngoing {
					continue
				}
				if summary.providersAllTerminal {
					// Grace period to allow in-flight upstream registrations to complete
					time.Sleep(100 * time.Millisecond)
					u.upstreamsMu.RLock()
					latest := len(u.networkUpstreams[networkId])
					u.upstreamsMu.RUnlock()
					if latest >= minReady {
						u.logger.Info().
							Str("networkId", networkId).
							Int("upstreamsCount", latest).
							Msg("upstreams became ready during grace period after providers terminal state")
						return nil
					}
					// Return a retryable error so the auto-retry loop can
					// re-attempt when upstreams may have recovered.
					return common.NewErrNetworkNotSupported(u.prjId, networkId)
				}
				continue
			}

			u.logger.Info().
				Str("networkId", networkId).
				Int("upstreamsCount", upstreamsCount).
				Int("minReady", minReady).
				Msg("upstreams ready for network initialization")
			return nil
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

	// Fast path: lock-free atomic snapshot
	if v, ok := u.networkUpstreamsAtomic.Load(networkId); ok {
		if arr, ok2 := v.([]*Upstream); ok2 {
			return arr
		}
	}

	// Fallback: read under lock and populate snapshot for future reads
	u.upstreamsMu.RLock()
	ups := u.networkUpstreams[networkId]
	if ups == nil {
		u.upstreamsMu.RUnlock()
		return nil
	}
	cp := make([]*Upstream, len(ups))
	copy(cp, ups)
	// Populate snapshot while still holding RLock to avoid stale overwrite after a writer runs
	u.networkUpstreamsAtomic.Store(networkId, cp)
	u.upstreamsMu.RUnlock()
	return cp
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

		// Ensure wildcard map exists before writing into it
		if _, ok := u.sortedUpstreams["*"]; !ok {
			u.sortedUpstreams["*"] = make(map[string][]*Upstream)
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
			if _, ok := u.upstreamScores[upid]["*"]; !ok {
				u.upstreamScores[upid]["*"] = make(map[string]float64)
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

func (u *UpstreamsRegistry) RefreshUpstreamNetworkMethodScores() error {
	u.refreshMu.Lock()
	defer u.refreshMu.Unlock()

	_, span := common.StartDetailSpan(u.appCtx, "UpstreamsRegistry.RefreshUpstreamNetworkMethodScores")
	defer span.End()

	if u.scoringCfg.RoutingStrategy == "round-robin" {
		return u.refreshRoundRobin()
	}
	return u.refreshScoreBased()
}

func (u *UpstreamsRegistry) refreshRoundRobin() error {
	u.upstreamsMu.RLock()
	if len(u.allUpstreams) == 0 {
		u.upstreamsMu.RUnlock()
		return nil
	}
	type key struct{ network, method string }
	work := make(map[key][]*Upstream)
	for networkId, methods := range u.sortedUpstreams {
		for method := range methods {
			var src []*Upstream
			if networkId == "*" {
				src = u.allUpstreams
			} else {
				src = u.networkUpstreams[networkId]
			}
			cp := make([]*Upstream, len(src))
			copy(cp, src)
			work[key{networkId, method}] = cp
		}
	}
	u.upstreamsMu.RUnlock()

	type pairResult struct {
		network, method string
		sorted          []*Upstream
	}
	var results []pairResult
	for km, upsList := range work {
		if len(upsList) == 0 {
			continue
		}
		active := u.filterCordoned(upsList, km.method)
		if len(active) == 0 {
			continue
		}
		if _, ok := u.rotationCounters[km.network]; !ok {
			u.rotationCounters[km.network] = make(map[string]uint64)
		}
		u.rotationCounters[km.network][km.method]++
		offset := int(u.rotationCounters[km.network][km.method] % uint64(len(active))) // #nosec G115
		rotated := make([]*Upstream, len(active))
		for i := range active {
			rotated[i] = active[(i+offset)%len(active)]
		}
		results = append(results, pairResult{km.network, km.method, rotated})
	}

	u.upstreamsMu.Lock()
	for _, res := range results {
		if _, ok := u.sortedUpstreams[res.network]; !ok {
			u.sortedUpstreams[res.network] = make(map[string][]*Upstream)
		}
		u.sortedUpstreams[res.network][res.method] = res.sorted
		u.emitMetrics(res.sorted, res.network, res.method, nil)
	}
	u.upstreamsMu.Unlock()
	return nil
}

func (u *UpstreamsRegistry) refreshScoreBased() error {
	u.upstreamsMu.RLock()
	if len(u.allUpstreams) == 0 {
		u.upstreamsMu.RUnlock()
		u.logger.Trace().Str("projectId", u.prjId).Msgf("no upstreams yet to refresh scores")
		return nil
	}
	type key struct{ network, method string }
	work := make(map[key][]*Upstream)
	for networkId, methods := range u.sortedUpstreams {
		for method := range methods {
			var src []*Upstream
			if networkId == "*" {
				src = u.allUpstreams
			} else {
				src = u.networkUpstreams[networkId]
			}
			cp := make([]*Upstream, len(src))
			copy(cp, src)
			work[key{networkId, method}] = cp
		}
	}
	// Snapshot previous sorted order for stickiness
	prevPrimary := make(map[key]string)
	for networkId, methods := range u.sortedUpstreams {
		for method, ups := range methods {
			if len(ups) > 0 {
				prevPrimary[key{networkId, method}] = ups[0].Id()
			}
		}
	}
	u.upstreamsMu.RUnlock()

	cfg := u.scoringCfg
	type pairResult struct {
		network, method string
		penalties       map[string]float64
		sorted          []*Upstream
	}
	var results []pairResult

	if cfg.ScoreGranularity == "upstream" {
		// Compute ONE penalty per upstream using method="*" metrics, then broadcast
		upstreamPenalties := make(map[string]map[string]float64) // network -> upstreamId -> penalty
		networkUpstreams := make(map[string][]*Upstream)         // network -> upstreams list
		for km, upsList := range work {
			networkUpstreams[km.network] = upsList
		}
		for networkId, upsList := range networkUpstreams {
			penalties := u.computePenalties(upsList, networkId, "*")
			upstreamPenalties[networkId] = penalties
		}
		// Apply the same penalty-sorted order to all methods for each network
		for km, upsList := range work {
			penalties, ok := upstreamPenalties[km.network]
			if !ok {
				penalties = make(map[string]float64)
			}
			active := u.filterCordoned(upsList, km.method)
			sorted := u.stickySort(active, penalties, km.network, km.method, prevPrimary[km])
			results = append(results, pairResult{km.network, km.method, penalties, sorted})
		}
	} else {
		// Per-method: compute penalty per (upstream, method) pair
		for km, upsList := range work {
			penalties := u.computePenalties(upsList, km.network, km.method)
			active := u.filterCordoned(upsList, km.method)
			sorted := u.stickySort(active, penalties, km.network, km.method, prevPrimary[km])
			results = append(results, pairResult{km.network, km.method, penalties, sorted})
		}
	}

	// Commit under write lock
	u.upstreamsMu.Lock()
	for _, res := range results {
		if _, ok := u.sortedUpstreams[res.network]; !ok {
			u.sortedUpstreams[res.network] = make(map[string][]*Upstream)
		}
		u.sortedUpstreams[res.network][res.method] = res.sorted
		u.emitMetrics(res.sorted, res.network, res.method, res.penalties)
	}
	u.upstreamsMu.Unlock()
	return nil
}

// computePenalties calculates the EMA-smoothed penalty for each upstream.
// Uses absolute metric values (no peer-relative baseline) for stability.
func (u *UpstreamsRegistry) computePenalties(upsList []*Upstream, networkId, metricsMethod string) map[string]float64 {
	cfg := u.scoringCfg
	n := len(upsList)
	if n == 0 {
		return nil
	}

	penalties := make(map[string]float64, n)
	for _, ups := range upsList {
		upsId := ups.Id()
		mt := u.metricsTracker.GetUpstreamMethodMetrics(ups, metricsMethod)
		mul := ups.getScoreMultipliers(networkId, metricsMethod, nil)

		qn := 0.70
		upsCfg := ups.Config()
		if upsCfg != nil && upsCfg.Routing != nil && upsCfg.Routing.ScoreLatencyQuantile != 0 {
			qn = upsCfg.Routing.ScoreLatencyQuantile
		}

		var instant float64

		if mul.ErrorRate != nil && *mul.ErrorRate > 0 {
			instant += mt.ErrorRate() * *mul.ErrorRate
		}

		if mul.RespLatency != nil && *mul.RespLatency > 0 {
			instant += mt.ResponseQuantiles.GetQuantile(qn).Seconds() * *mul.RespLatency
		}

		if mul.ThrottledRate != nil && *mul.ThrottledRate > 0 {
			instant += mt.ThrottledRate() * *mul.ThrottledRate
		}

		if mul.BlockHeadLag != nil && *mul.BlockHeadLag > 0 {
			instant += math.Max(0, float64(mt.BlockHeadLag.Load())) * *mul.BlockHeadLag
		}

		if mul.FinalizationLag != nil && *mul.FinalizationLag > 0 {
			instant += math.Max(0, float64(mt.FinalizationLag.Load())) * *mul.FinalizationLag
		}

		if mul.Misbehaviors != nil && *mul.Misbehaviors > 0 {
			instant += mt.MisbehaviorRate() * *mul.Misbehaviors
		}

		if math.IsNaN(instant) || math.IsInf(instant, 0) {
			instant = 0
		}

		stored := u.getPenalty(upsId, networkId, metricsMethod)
		if math.IsNaN(stored) || math.IsInf(stored, 0) {
			stored = 0
		}
		decayed := stored*cfg.PenaltyDecayRate + instant*(1.0-cfg.PenaltyDecayRate)
		u.setPenalty(upsId, networkId, metricsMethod, decayed)
		penalties[upsId] = decayed
	}
	return penalties
}

func (u *UpstreamsRegistry) getPenalty(upsId, networkId, method string) float64 {
	if nw, ok := u.penaltyState[upsId]; ok {
		if meth, ok := nw[networkId]; ok {
			return meth[method]
		}
	}
	return 0
}

func (u *UpstreamsRegistry) setPenalty(upsId, networkId, method string, val float64) {
	if _, ok := u.penaltyState[upsId]; !ok {
		u.penaltyState[upsId] = make(map[string]map[string]float64)
	}
	if _, ok := u.penaltyState[upsId][networkId]; !ok {
		u.penaltyState[upsId][networkId] = make(map[string]float64)
	}
	u.penaltyState[upsId][networkId][method] = val
}

// stickySort sorts upstreams by penalty ascending, preserving the current
// primary unless a challenger is significantly better (hysteresis) and the
// minimum cooldown has elapsed.
func (u *UpstreamsRegistry) stickySort(
	active []*Upstream,
	penalties map[string]float64,
	networkId, method string,
	prevPrimaryId string,
) []*Upstream {
	if len(active) == 0 {
		return active
	}
	cfg := u.scoringCfg

	sort.Slice(active, func(i, j int) bool {
		pi := penalties[active[i].Id()]
		pj := penalties[active[j].Id()]
		if pi != pj {
			return pi < pj
		}
		return active[i].Id() < active[j].Id()
	})

	if cfg.SwitchHysteresis <= 0 || prevPrimaryId == "" || active[0].Id() == prevPrimaryId {
		if prevPrimaryId == "" || (len(active) > 0 && active[0].Id() != prevPrimaryId) {
			u.setLastSwitchTime(networkId, method, time.Now())
		}
		return active
	}

	prevIdx := -1
	for i, ups := range active {
		if ups.Id() == prevPrimaryId {
			prevIdx = i
			break
		}
	}
	if prevIdx < 0 {
		u.setLastSwitchTime(networkId, method, time.Now())
		return active
	}

	primaryP := penalties[prevPrimaryId]
	challengerP := penalties[active[0].Id()]

	shouldSwitch := primaryP > 0 && challengerP < primaryP*(1.0-cfg.SwitchHysteresis)
	if shouldSwitch && cfg.MinSwitchInterval > 0 {
		if time.Since(u.getLastSwitchTime(networkId, method)) < cfg.MinSwitchInterval {
			shouldSwitch = false
		}
	}

	if shouldSwitch {
		u.setLastSwitchTime(networkId, method, time.Now())
		return active
	}

	prev := active[prevIdx]
	copy(active[prevIdx:], active[prevIdx+1:])
	copy(active[1:], active[:len(active)-1])
	active[0] = prev
	return active
}

func (u *UpstreamsRegistry) getLastSwitchTime(networkId, method string) time.Time {
	if nw, ok := u.lastSwitchTime[networkId]; ok {
		return nw[method]
	}
	return time.Time{}
}

func (u *UpstreamsRegistry) setLastSwitchTime(networkId, method string, t time.Time) {
	if _, ok := u.lastSwitchTime[networkId]; !ok {
		u.lastSwitchTime[networkId] = make(map[string]time.Time)
	}
	u.lastSwitchTime[networkId][method] = t
}

func (u *UpstreamsRegistry) filterCordoned(upsList []*Upstream, method string) []*Upstream {
	active := make([]*Upstream, 0, len(upsList))
	for _, ups := range upsList {
		if !u.metricsTracker.IsCordoned(ups, method) {
			active = append(active, ups)
		}
	}
	return active
}

// emitMetrics emits routing priority (always) and score (detailed mode only).
func (u *UpstreamsRegistry) emitMetrics(sorted []*Upstream, networkId, method string, penalties map[string]float64) {
	if u.scoreMetricsMode == telemetry.ScoreModeNone {
		return
	}
	detailed := u.scoreMetricsMode == telemetry.ScoreModeDetailed
	for i, ups := range sorted {
		upsId := ups.Id()
		penalty := 0.0
		if penalties != nil {
			penalty = penalties[upsId]
		}
		score := 1.0 / (1.0 + penalty)

		if _, ok := u.upstreamScores[upsId]; !ok {
			u.upstreamScores[upsId] = make(map[string]map[string]float64)
		}
		if _, ok := u.upstreamScores[upsId][networkId]; !ok {
			u.upstreamScores[upsId][networkId] = make(map[string]float64)
		}
		u.upstreamScores[upsId][networkId][method] = score

		upLabel := "n/a"
		catLabel := "n/a"
		if detailed {
			upLabel = upsId
			catLabel = method
		}

		telemetry.MetricUpstreamRoutingPriority.WithLabelValues(
			u.prjId, ups.VendorName(), ups.NetworkLabel(), upLabel, catLabel,
		).Set(float64(i + 1))

		if detailed {
			telemetry.MetricUpstreamScoreOverall.WithLabelValues(
				u.prjId, ups.VendorName(), ups.NetworkLabel(), upLabel, catLabel,
			).Set(score)
		}
	}
}

func (u *UpstreamsRegistry) registerUpstreams(ctx context.Context, upsCfgs ...*common.UpstreamConfig) error {
	tasks := make([]*util.BootstrapTask, 0)
	for _, c := range upsCfgs {
		upsCfg := c
		tasks = append(tasks, u.buildUpstreamBootstrapTask(upsCfg))
	}
	return u.initializer.ExecuteTasks(ctx, tasks...)
}

func (u *UpstreamsRegistry) buildUpstreamBootstrapTask(upsCfg *common.UpstreamConfig) *util.BootstrapTask {
	// Deep copy to avoid race conditions when detectFeatures modifies the config
	cfg := upsCfg.Copy()
	// Name: network/<networkId>/upstream/<id> if chainId configured; else upstream/<id>
	taskName := fmt.Sprintf("upstream/%s", cfg.Id)
	if cfg.Evm != nil && cfg.Evm.ChainId > 0 {
		taskName = fmt.Sprintf("network/%s/upstream/%s", util.EvmNetworkId(cfg.Evm.ChainId), cfg.Id)
	}
	return util.NewBootstrapTask(
		taskName,
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
	taskName := fmt.Sprintf("network/%s/provider/%s", networkId, provider.Id())
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
			return u.registerUpstreams(ctx, upsCfgs...)
		},
	)
}

// providerTasksCompletionAndFatal inspects tasks in the initializer and returns
// (allDone, anyFatal) for provider tasks associated with the given networkId.
// We infer membership by task name prefix: "provider/<id>/network/<networkId>".
type networkTaskSummary struct {
	providersAllTerminal bool
	hasOngoing           bool
}

// summarizeNetworkTasks computes provider completion/fatality and presence of any ongoing
// per-network or unknown upstream tasks in a single pass.
func (u *UpstreamsRegistry) summarizeNetworkTasks(networkId string) networkTaskSummary {
	provPrefix := "network/" + networkId + "/provider/"
	upsPrefix := "network/" + networkId + "/upstream/"
	unknownUpsPrefix := "upstream/"

	providersAllTerminal := true
	hasOngoing := false

	st := u.initializer.Status()
	for _, ts := range st.Tasks {
		name := ts.Name
		if strings.HasPrefix(name, provPrefix) {
			switch ts.State {
			case util.TaskSucceeded, util.TaskFatal:
				// terminal
			case util.TaskPending, util.TaskRunning, util.TaskFailed, util.TaskTimedOut:
				providersAllTerminal = false
				hasOngoing = true
			default:
				providersAllTerminal = false
			}
			continue
		}
		if strings.HasPrefix(name, upsPrefix) || strings.HasPrefix(name, unknownUpsPrefix) {
			switch ts.State {
			case util.TaskPending, util.TaskRunning, util.TaskFailed, util.TaskTimedOut:
				hasOngoing = true
			}
		}
	}
	return networkTaskSummary{
		providersAllTerminal: providersAllTerminal,
		hasOngoing:           hasOngoing,
	}
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
	// (removed: avoid mutating sortedUpstreams during registration; refresh owns ordering)

	// Initialize network-specific sorted upstreams
	// (removed: avoid mutating sortedUpstreams during registration; refresh owns ordering)

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

	// Refresh atomic snapshot for this network's upstreams
	if !isShadow {
		cp := make([]*Upstream, len(u.networkUpstreams[networkId]))
		copy(cp, u.networkUpstreams[networkId])
		u.networkUpstreamsAtomic.Store(networkId, cp)
	}

	u.logger.Debug().
		Str("upstreamId", cfg.Id).
		Str("networkId", networkId).
		Msg("upstream registered and initialized in registry")
}

func (u *UpstreamsRegistry) scheduleScoreCalculationTimers(ctx context.Context) {
	if u.scoreRefreshInterval == 0 {
		return
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

func castToCommonUpstreams(upstreams []*Upstream) []common.Upstream {
	commonUpstreams := make([]common.Upstream, len(upstreams))
	for i, ups := range upstreams {
		commonUpstreams[i] = ups
	}
	return commonUpstreams
}
