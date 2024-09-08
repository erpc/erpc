package upstream

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"sync"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/vendors"
	"github.com/rs/zerolog"
)

type UpstreamsRegistry struct {
	prjId                string
	scoreRefreshInterval time.Duration
	logger               *zerolog.Logger
	metricsTracker       *health.Tracker
	clientRegistry       *ClientRegistry
	vendorsRegistry      *vendors.VendorsRegistry
	rateLimitersRegistry *RateLimitersRegistry
	upsCfg               []*common.UpstreamConfig

	allUpstreams []*Upstream
	upstreamsMu  *sync.RWMutex
	// map of network -> method (or *) => upstreams
	sortedUpstreams map[string]map[string][]*Upstream
	// map of upstream -> network (or *) -> method (or *) => score
	upstreamScores map[string]map[string]map[string]float64
}

type UpstreamsHealth struct {
	Upstreams       []*Upstream                              `json:"upstreams"`
	SortedUpstreams map[string]map[string][]string           `json:"sortedUpstreams"`
	UpstreamScores  map[string]map[string]map[string]float64 `json:"upstreamScores"`
}

func NewUpstreamsRegistry(
	logger *zerolog.Logger,
	prjId string,
	upsCfg []*common.UpstreamConfig,
	rlr *RateLimitersRegistry,
	vr *vendors.VendorsRegistry,
	mt *health.Tracker,
	scoreRefreshInterval time.Duration,
) *UpstreamsRegistry {
	return &UpstreamsRegistry{
		prjId:                prjId,
		scoreRefreshInterval: scoreRefreshInterval,
		logger:               logger,
		clientRegistry:       NewClientRegistry(logger),
		rateLimitersRegistry: rlr,
		vendorsRegistry:      vr,
		metricsTracker:       mt,
		upsCfg:               upsCfg,
		sortedUpstreams:      make(map[string]map[string][]*Upstream),
		upstreamScores:       make(map[string]map[string]map[string]float64),
		upstreamsMu:          &sync.RWMutex{},
	}
}

func (u *UpstreamsRegistry) Bootstrap(ctx context.Context) error {
	err := u.registerUpstreams()
	if err != nil {
		return err
	}
	return u.scheduleScoreCalculationTimers(ctx)
}

func (u *UpstreamsRegistry) NewUpstream(
	projectId string,
	cfg *common.UpstreamConfig,
	logger *zerolog.Logger,
	mt *health.Tracker,
) (*Upstream, error) {
	return NewUpstream(projectId, cfg, u.clientRegistry, u.rateLimitersRegistry, u.vendorsRegistry, logger, mt)
}

func (u *UpstreamsRegistry) PrepareUpstreamsForNetwork(networkId string) error {
	u.upstreamsMu.Lock()
	defer u.upstreamsMu.Unlock()

	var upstreams []*Upstream
	for _, ups := range u.allUpstreams {
		if s, e := ups.SupportsNetwork(networkId); e == nil && s {
			upstreams = append(upstreams, ups)
		} else if e != nil {
			u.logger.Warn().Err(e).
				Str("upstreamId", ups.Config().Id).
				Str("networkId", networkId).
				Msgf("failed to check if upstream supports network")
		}
	}
	if len(upstreams) == 0 {
		return common.NewErrNoUpstreamsFound(u.prjId, networkId)
	}

	if _, ok := u.sortedUpstreams[networkId]; !ok {
		u.sortedUpstreams[networkId] = make(map[string][]*Upstream)
	}
	if _, ok := u.sortedUpstreams[networkId]["*"]; !ok {
		cpUps := make([]*Upstream, len(upstreams))
		copy(cpUps, upstreams)
		u.sortedUpstreams[networkId]["*"] = cpUps
	}
	if _, ok := u.sortedUpstreams["*"]; !ok {
		u.sortedUpstreams["*"] = make(map[string][]*Upstream)
	}
	if _, ok := u.sortedUpstreams["*"]["*"]; !ok {
		cpUps := make([]*Upstream, len(upstreams))
		copy(cpUps, upstreams)
		u.sortedUpstreams["*"]["*"] = cpUps
	}

	// Initialize score for this or any network and any method for each upstream
	for _, ups := range upstreams {
		if _, ok := u.upstreamScores[ups.Config().Id]; !ok {
			u.upstreamScores[ups.Config().Id] = make(map[string]map[string]float64)
		}

		if _, ok := u.upstreamScores[ups.Config().Id][networkId]; !ok {
			u.upstreamScores[ups.Config().Id][networkId] = make(map[string]float64)
		}
		if _, ok := u.upstreamScores[ups.Config().Id][networkId]["*"]; !ok {
			u.upstreamScores[ups.Config().Id][networkId]["*"] = 0
		}

		if _, ok := u.upstreamScores[ups.Config().Id]["*"]; !ok {
			u.upstreamScores[ups.Config().Id]["*"] = make(map[string]float64)
		}
		if _, ok := u.upstreamScores[ups.Config().Id]["*"]["*"]; !ok {
			u.upstreamScores[ups.Config().Id]["*"]["*"] = 0
		}
	}

	return nil
}

func (u *UpstreamsRegistry) GetSortedUpstreams(networkId, method string) ([]*Upstream, error) {
	u.upstreamsMu.RLock()
	upsList := u.sortedUpstreams[networkId][method]
	u.upstreamsMu.RUnlock()

	if upsList == nil {
		u.upstreamsMu.Lock()
		defer u.upstreamsMu.Unlock()

		upsList = u.sortedUpstreams[networkId]["*"]
		if upsList == nil {
			upsList = u.sortedUpstreams["*"]["*"]
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

		// Initialize scores for this method on this network and "any" network
		for _, ups := range methodUpsList {
			if _, ok := u.upstreamScores[ups.Config().Id][networkId][method]; !ok {
				u.upstreamScores[ups.Config().Id][networkId][method] = 0
			}
			if _, ok := u.upstreamScores[ups.Config().Id]["*"][method]; !ok {
				u.upstreamScores[ups.Config().Id]["*"][method] = 0
			}
		}

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

func (u *UpstreamsRegistry) sortUpstreams(networkId, method string, upstreams []*Upstream) {
	// Calculate total score
	totalScore := 0.0
	for _, ups := range upstreams {
		score := u.upstreamScores[ups.Config().Id][networkId][method]
		if score < 0 {
			score = 0
		}
		totalScore += score
	}

	// If all scores are 0 or negative, fall back to random shuffle
	if totalScore == 0 {
		rand.Shuffle(len(upstreams), func(i, j int) {
			upstreams[i], upstreams[j] = upstreams[j], upstreams[i]
		})
		return
	}

	sort.Slice(upstreams, func(i, j int) bool {
		scoreI := u.upstreamScores[upstreams[i].Config().Id][networkId][method]
		scoreJ := u.upstreamScores[upstreams[j].Config().Id][networkId][method]

		if scoreI < 0 {
			scoreI = 0
		}
		if scoreJ < 0 {
			scoreJ = 0
		}

		if scoreI != scoreJ {
			return scoreI > scoreJ
		}

		// If random values are equal, sort by upstream ID for consistency
		return upstreams[i].Config().Id < upstreams[j].Config().Id
	})
}

func (u *UpstreamsRegistry) RefreshUpstreamNetworkMethodScores() error {
	u.upstreamsMu.Lock()
	defer u.upstreamsMu.Unlock()

	if len(u.allUpstreams) == 0 {
		u.logger.Debug().Str("projectId", u.prjId).Msgf("no upstreams yet to refresh scores")
		return nil
	}

	ln := len(u.sortedUpstreams)
	u.logger.Trace().Str("projectId", u.prjId).Int("networks", ln).Msgf("refreshing upstreams scores")

	allNetworks := make([]string, 0, ln)
	for networkId := range u.sortedUpstreams {
		allNetworks = append(allNetworks, networkId)
	}

	for _, networkId := range allNetworks {
		for method, upsList := range u.sortedUpstreams[networkId] {
			u.updateScoresAndSort(networkId, method, upsList)
		}
	}

	return nil
}

func (u *UpstreamsRegistry) registerUpstreams() error {
	for _, upsCfg := range u.upsCfg {
		upstream, err := u.NewUpstream(u.prjId, upsCfg, u.logger, u.metricsTracker)
		if err != nil {
			return err
		}
		u.allUpstreams = append(u.allUpstreams, upstream)
	}

	if len(u.allUpstreams) == 0 {
		return common.NewErrNoUpstreamsDefined(u.prjId)
	}

	return nil
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

func (u *UpstreamsRegistry) updateScoresAndSort(networkId, method string, upsList []*Upstream) {
	var p90Latencies, errorRates, totalRequests, throttledRates, blockHeadLags, finalizationLags []float64

	for _, ups := range upsList {
		metrics := u.metricsTracker.GetUpstreamMethodMetrics(ups.Config().Id, networkId, method)
		metrics.Mutex.RLock()
		u.logger.Trace().
			Str("projectId", u.prjId).
			Str("networkId", networkId).
			Str("method", method).
			Str("upstreamId", ups.Config().Id).
			Interface("metrics", metrics).
			Msg("queried upstream metrics")
		p90Latencies = append(p90Latencies, metrics.LatencySecs.P90())
		blockHeadLags = append(blockHeadLags, metrics.BlockHeadLag)
		finalizationLags = append(finalizationLags, metrics.FinalizationLag)
		rateLimitedTotal := metrics.RemoteRateLimitedTotal + metrics.SelfRateLimitedTotal
		if metrics.RequestsTotal > 0 {
			errorRates = append(errorRates, metrics.ErrorsTotal/metrics.RequestsTotal)
			throttledRates = append(throttledRates, rateLimitedTotal/metrics.RequestsTotal)
			totalRequests = append(totalRequests, metrics.RequestsTotal)
		} else {
			errorRates = append(errorRates, 0)
			throttledRates = append(throttledRates, 0)
			totalRequests = append(totalRequests, 0)
		}
		metrics.Mutex.RUnlock()
	}

	normP90Latencies := normalizeValues(p90Latencies)
	normErrorRates := normalizeValues(errorRates)
	normThrottledRates := normalizeValues(throttledRates)
	normTotalRequests := normalizeValues(totalRequests)
	normBlockHeadLags := normalizeValues(blockHeadLags)
	normFinalizationLags := normalizeValues(finalizationLags)
	for i, ups := range upsList {
		score := u.calculateScore(
			normTotalRequests[i],
			normP90Latencies[i],
			normErrorRates[i],
			normThrottledRates[i],
			normBlockHeadLags[i],
			normFinalizationLags[i],
		)
		u.upstreamScores[ups.Config().Id][networkId][method] = score
		u.logger.Trace().Str("projectId", u.prjId).
			Str("upstreamId", ups.Config().Id).
			Str("networkId", networkId).
			Str("method", method).
			Float64("score", score).
			Msgf("refreshed score")
	}

	u.sortUpstreams(networkId, method, upsList)
	u.sortedUpstreams[networkId][method] = upsList

	newSortStr := ""
	for _, ups := range upsList {
		newSortStr += fmt.Sprintf("%s ", ups.Config().Id)
	}

	u.logger.Trace().Str("projectId", u.prjId).Str("networkId", networkId).Str("method", method).Str("newSort", newSortStr).Msgf("sorted upstreams")
}

func (u *UpstreamsRegistry) calculateScore(
	normTotalRequests,
	normP90Latency,
	normErrorRate,
	normThrottledRate,
	normBlockHeadLag,
	normFinalizationLag float64,
) float64 {
	score := 0.0

	// Higher score for lower total requests (to balance the load)
	score += expCurve(1 - normTotalRequests)

	// Higher score for lower p90 latency
	score += expCurve(1-normP90Latency) * 4

	// Higher score for lower error rate
	score += expCurve(1-normErrorRate) * 8

	// Higher score for lower throttled rate
	score += expCurve(1-normThrottledRate) * 3

	// Higher score for lower block head lag
	score += expCurve(1-normBlockHeadLag) * 2

	// Higher score for lower finalization lag
	score += expCurve(1 - normFinalizationLag)

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

func (u *UpstreamsRegistry) GetUpstreamsHealth() (*UpstreamsHealth, error) {
	sortedUpstreams := make(map[string]map[string][]string)
	upstreamScores := make(map[string]map[string]map[string]float64)

	for nw, methods := range u.sortedUpstreams {
		for method, ups := range methods {
			upstreamIds := make([]string, len(ups))
			for i, ups := range ups {
				upstreamIds[i] = ups.Config().Id
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
