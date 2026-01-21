package upstream

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
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

// Scoring tunables (kept local to this file for now)
const (
	// Number of requests after which we fully trust an upstream's per-method metrics
	scoreMinSamplesForConfidence = 10.0
	// Exponential moving average factor for previous score weight (0..1).
	// Higher means more inertia (favor stability over reactivity).
	scoreEMAPreviousWeight = 0.7
	// Neutral fallback latency (seconds) when no peers have valid samples; chosen conservatively
	scoreNeutralLatencySeconds = 0.2
)

type UpstreamsRegistry struct {
	appCtx               context.Context
	prjId                string
	scoreRefreshInterval time.Duration
	// Serializes score refresh to avoid concurrent refresh goroutines racing
	// on shared maps even when read-write locks are held in different phases.
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
	networkMu    *sync.Map // map[string]*sync.RWMutex for per-network locks
	// map of network => upstreams
	networkUpstreams       map[string][]*Upstream
	networkShadowUpstreams map[string][]*Upstream
	// lock-free snapshot for hot path reads (key: networkId, val: []*Upstream copy)
	networkUpstreamsAtomic sync.Map
	// map of network -> method (or *) => upstreams
	sortedUpstreams map[string]map[string][]*Upstream
	// map of upstream -> network (or *) -> method (or *) => score
	upstreamScores map[string]map[string]map[string]float64

	onUpstreamRegistered func(ups *Upstream) error
	// scoreMetricsMode controls the labeling granularity for score metrics for this registry.
	scoreMetricsMode telemetry.ScoreMetricsMode
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
	onUpstreamRegistered func(*Upstream) error,
) *UpstreamsRegistry {
	lg := logger.With().Str("component", "upstreams").Logger()
	return &UpstreamsRegistry{
		appCtx:               appCtx,
		prjId:                prjId,
		scoreRefreshInterval: scoreRefreshInterval,
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
					return common.NewTaskFatal(common.NewErrNetworkNotSupported(u.prjId, networkId))
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
					return common.NewTaskFatal(common.NewErrNetworkNotSupported(u.prjId, networkId))
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
	// Avoid concurrent refreshers racing on shared maps by serializing this function.
	u.refreshMu.Lock()
	defer u.refreshMu.Unlock()

	_, span := common.StartDetailSpan(u.appCtx, "UpstreamsRegistry.RefreshUpstreamNetworkMethodScores")
	defer span.End()

	// Snapshot current workset under read lock
	u.upstreamsMu.RLock()
	if len(u.allUpstreams) == 0 {
		u.upstreamsMu.RUnlock()
		u.logger.Trace().Str("projectId", u.prjId).Msgf("no upstreams yet to refresh scores")
		return nil
	}
	type key struct{ network, method string }
	work := make(map[key][]*Upstream)
	prevScores := make(map[key]map[string]float64) // per (network,method) -> upstreamId -> prev score
	for networkId, methods := range u.sortedUpstreams {
		for method := range methods {
			if networkId == "*" {
				cp := make([]*Upstream, len(u.allUpstreams))
				copy(cp, u.allUpstreams)
				k := key{networkId, method}
				work[k] = cp
				// capture previous scores under lock
				if _, ok := prevScores[k]; !ok {
					prevScores[k] = make(map[string]float64)
				}
				for _, ups := range cp {
					if upsc, ok := u.upstreamScores[ups.Id()]; ok {
						if nwsc, ok := upsc[networkId]; ok {
							if p, ok := nwsc[method]; ok {
								prevScores[k][ups.Id()] = p
							}
						}
					}
				}
			} else {
				src := u.networkUpstreams[networkId]
				cp := make([]*Upstream, len(src))
				copy(cp, src)
				k := key{networkId, method}
				work[k] = cp
				// capture previous scores under lock
				if _, ok := prevScores[k]; !ok {
					prevScores[k] = make(map[string]float64)
				}
				for _, ups := range cp {
					if upsc, ok := u.upstreamScores[ups.Id()]; ok {
						if nwsc, ok := upsc[networkId]; ok {
							if p, ok := nwsc[method]; ok {
								prevScores[k][ups.Id()] = p
							}
						}
					}
				}
			}
		}
	}
	u.upstreamsMu.RUnlock()

	// Compute scores and ordering outside the lock
	type pairResult struct {
		network string
		method  string
		scores  map[string]float64
		sorted  []*Upstream
	}
	results := make([]pairResult, 0, len(work))
	for km, upsList := range work {
		// Gather metrics (measured) and per-upstream quantiles
		var (
			respLatencies    = make([]float64, len(upsList))
			errorRates       = make([]float64, len(upsList))
			totalRequests    = make([]float64, len(upsList))
			throttledRates   = make([]float64, len(upsList))
			blockHeadLags    = make([]float64, len(upsList))
			finalizationLags = make([]float64, len(upsList))
			misbehaviorRates = make([]float64, len(upsList))
		)
		for i, ups := range upsList {
			qn := 0.70
			cfg := ups.Config()
			if cfg != nil && cfg.Routing != nil && cfg.Routing.ScoreLatencyQuantile != 0 {
				qn = cfg.Routing.ScoreLatencyQuantile
			}
			mt := u.metricsTracker.GetUpstreamMethodMetrics(ups, km.method)
			respLatencies[i] = mt.ResponseQuantiles.GetQuantile(qn).Seconds()
			errorRates[i] = mt.ErrorRate()
			throttledRates[i] = mt.ThrottledRate()
			totalRequests[i] = float64(mt.RequestsTotal.Load())
			blockHeadLags[i] = float64(mt.BlockHeadLag.Load())
			finalizationLags[i] = float64(mt.FinalizationLag.Load())
			misbehaviorRates[i] = mt.MisbehaviorRate()
		}

		// Compute peer baselines for confidence-weighted mixing
		baselineLatency := medianPositive(respLatencies)
		if baselineLatency == 0 {
			baselineLatency = scoreNeutralLatencySeconds
		}
		baselineError := median(errorRates)
		baselineThrottle := median(throttledRates)

		// Build effective metrics using confidence weighting by sample size
		effLat := make([]float64, len(upsList))
		effErr := make([]float64, len(upsList))
		effThr := make([]float64, len(upsList))
		for i := range upsList {
			w := clamp01(totalRequests[i] / scoreMinSamplesForConfidence)
			lat := respLatencies[i]
			if lat <= 0 {
				// No successful samples for latency quantile: trust baseline only.
				effLat[i] = baselineLatency
			} else {
				effLat[i] = w*lat + (1.0-w)*baselineLatency
			}
			effErr[i] = w*errorRates[i] + (1.0-w)*baselineError
			effThr[i] = w*throttledRates[i] + (1.0-w)*baselineThrottle
		}

		// Normalize effective metrics for scoring
		normRespLatencies := normalizeValuesLog(effLat)
		normErrorRates := normalizeValues(effErr)
		normThrottledRates := normalizeValues(effThr)
		normTotalRequests := normalizeValues(totalRequests)
		normBlockHeadLags := normalizeValuesLog(blockHeadLags)
		normFinalizationLags := normalizeValuesLog(finalizationLags)
		normMisbehaviorRates := normalizeValues(misbehaviorRates)

		// Calculate instantaneous scores, then apply EMA smoothing using previous scores
		scores := make(map[string]float64, len(upsList))
		for i, ups := range upsList {
			upsId := ups.Id()
			instant := u.calculateScore(
				ups,
				km.network,
				km.method,
				nil,
				normTotalRequests[i],
				normRespLatencies[i],
				normErrorRates[i],
				normThrottledRates[i],
				normBlockHeadLags[i],
				normFinalizationLags[i],
				normMisbehaviorRates[i],
			)
			// Previous smoothed score captured during snapshot
			prev := prevScores[km][upsId]
			// Guard against NaN/Inf propagation in EMA smoothing.
			// NaN can occur from metrics collection edge cases (e.g., division by zero)
			// and once present will propagate indefinitely through EMA calculations.
			if math.IsNaN(prev) || math.IsInf(prev, 0) {
				u.logger.Warn().
					Str("upstreamId", upsId).
					Str("network", km.network).
					Str("method", km.method).
					Float64("prev", prev).
					Msg("previous EMA score was NaN/Inf, resetting to 0")
				prev = 0
			}
			if math.IsNaN(instant) || math.IsInf(instant, 0) {
				u.logger.Warn().
					Str("upstreamId", upsId).
					Str("network", km.network).
					Str("method", km.method).
					Float64("instant", instant).
					Msg("instant score calculation returned NaN/Inf, using 0")
				instant = 0
			}
			// Apply EMA smoothing even when previous score is zero.
			// Using prev==0 for first-time entries only scales all scores uniformly,
			// preserving ordering while ensuring consistent EMA application.
			smoothed := scoreEMAPreviousWeight*prev + (1.0-scoreEMAPreviousWeight)*instant
			scores[upsId] = smoothed
			// Emit score metric according to configured mode.
			// compact: collapse upstream and category into 'n/a' to reduce cardinality
			// detailed: include full upstream and category
			// Respect emission mode
			if u.scoreMetricsMode == telemetry.ScoreModeNone {
				// Do not emit anything in 'none' mode
				// Continue to compute and store scores, but skip metric emission
			} else if !math.IsNaN(smoothed) && !math.IsInf(smoothed, 0) {
				// Only emit valid scores to Prometheus (defense-in-depth against NaN/Inf)
				upLabel := "n/a"
				catLabel := "n/a"
				if u.scoreMetricsMode == telemetry.ScoreModeDetailed {
					upLabel = upsId
					catLabel = km.method
				}
				telemetry.MetricUpstreamScoreOverall.WithLabelValues(u.prjId, ups.VendorName(), ups.NetworkLabel(), upLabel, catLabel).Set(smoothed)
			} else {
				// Defense-in-depth triggered: smoothed score is NaN/Inf despite input guards
				u.logger.Error().
					Str("upstreamId", upsId).
					Str("network", km.network).
					Str("method", km.method).
					Float64("prev", prev).
					Float64("instant", instant).
					Float64("smoothed", smoothed).
					Msg("smoothed score is NaN/Inf despite input guards - not emitting to Prometheus")
			}
		}

		// Filter disabled by cordon and sort by computed scores desc
		active := make([]*Upstream, 0, len(upsList))
		for _, ups := range upsList {
			if !u.metricsTracker.IsCordoned(ups, km.method) {
				active = append(active, ups)
			}
		}
		// If all scores are 0, shuffle randomly
		total := 0.0
		for _, ups := range active {
			if s, ok := scores[ups.Id()]; ok && s > 0 {
				total += s
			}
		}
		if total == 0 {
			rand.Shuffle(len(active), func(i, j int) {
				active[i], active[j] = active[j], active[i]
			})
		} else {
			sort.Slice(active, func(i, j int) bool {
				si := scores[active[i].Id()]
				sj := scores[active[j].Id()]
				if si < 0 {
					si = 0
				}
				if sj < 0 {
					sj = 0
				}
				if si != sj {
					return si > sj
				}
				return active[i].Id() < active[j].Id()
			})
		}
		results = append(results, pairResult{
			network: km.network,
			method:  km.method,
			scores:  scores,
			sorted:  active,
		})
	}

	// Commit under a short write lock
	u.upstreamsMu.Lock()
	for _, res := range results {
		// Ensure maps exist
		if _, ok := u.sortedUpstreams[res.network]; !ok {
			u.sortedUpstreams[res.network] = make(map[string][]*Upstream)
		}
		u.sortedUpstreams[res.network][res.method] = res.sorted

		for upsId, sc := range res.scores {
			if _, ok := u.upstreamScores[upsId]; !ok {
				u.upstreamScores[upsId] = make(map[string]map[string]float64)
			}
			if _, ok := u.upstreamScores[upsId][res.network]; !ok {
				u.upstreamScores[upsId][res.network] = make(map[string]float64)
			}
			u.upstreamScores[upsId][res.network][res.method] = sc
		}
	}
	u.upstreamsMu.Unlock()

	return nil
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

// updateScoresAndSort has been removed in favor of a single scoring path
// implemented in RefreshUpstreamNetworkMethodScores, which computes and commits
// scores and ordering for all (network, method) pairs in a batch.

func (u *UpstreamsRegistry) calculateScore(
	ups *Upstream,
	networkId,
	method string,
	finalities []common.DataFinalityState,
	normTotalRequests,
	normRespLatency,
	normErrorRate,
	normThrottledRate,
	normBlockHeadLag,
	normFinalizationLag,
	normMisbehaviorRate float64,
) float64 {
	mul := ups.getScoreMultipliers(networkId, method, finalities)

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

	// Higher score for lower misbehavior rate
	if mul.Misbehaviors != nil && *mul.Misbehaviors > 0 {
		score += expCurve(1-normMisbehaviorRate) * *mul.Misbehaviors
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
	// Find max while skipping NaN/Inf values
	max := 0.0
	for _, value := range values {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			continue
		}
		if value > max {
			max = value
		}
	}
	normalized := make([]float64, len(values))
	for i, value := range values {
		// Handle NaN/Inf values by mapping to 0
		if math.IsNaN(value) || math.IsInf(value, 0) {
			normalized[i] = 0
			continue
		}
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

	// Find the true min and max values in the input slice, skipping NaN/Inf values.
	// Assumes values are non-negative based on typical use for latencies, lags etc.
	// NaN comparisons always return false, so we need explicit checks.
	dataMin := math.MaxFloat64
	dataMax := 0.0
	hasValidValue := false
	for _, v := range values {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			continue
		}
		hasValidValue = true
		if v < dataMin {
			dataMin = v
		}
		if v > dataMax {
			dataMax = v
		}
	}

	// If no valid values, return all zeros
	if !hasValidValue {
		return make([]float64, len(values))
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
		// Handle invalid values (NaN, Inf, negative)
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			// Map invalid values to 0 to avoid propagating NaN/Inf
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

// normalizeValuesLogWithInvalid was removed; effective metrics avoid generating
// invalid latencies and are normalized via normalizeValuesLog instead.

// clamp01 clamps a float64 to the [0,1] interval.
func clamp01(x float64) float64 {
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}

// median computes median of a slice (copying and sorting).
// Returns 0.0 for empty input.
func median(vals []float64) float64 {
	if len(vals) == 0 {
		return 0.0
	}
	cp := make([]float64, len(vals))
	copy(cp, vals)
	sort.Float64s(cp)
	n := len(cp)
	if n%2 == 1 {
		return cp[n/2]
	}
	return (cp[n/2-1] + cp[n/2]) / 2.0
}

// medianPositive computes the median of strictly positive values.
// If no positive values exist, returns 0.0.
func medianPositive(vals []float64) float64 {
	pos := make([]float64, 0, len(vals))
	for _, v := range vals {
		if v > 0 {
			pos = append(pos, v)
		}
	}
	return median(pos)
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
