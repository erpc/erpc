package upstream

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/health"
	"github.com/flair-sdk/erpc/vendors"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

type UpstreamsRegistry struct {
	prjId                string
	logger               *zerolog.Logger
	metricsTracker       *health.Tracker
	clientRegistry       *ClientRegistry
	vendorsRegistry      *vendors.VendorsRegistry
	rateLimitersRegistry *RateLimitersRegistry
	mapMu                sync.RWMutex
	upsCfg               []*common.UpstreamConfig

	allUpstreams []*Upstream
	// map of network -> method (or *) => upstreams
	sortedUpstreams map[string]map[string][]*Upstream
	// map of upstream -> network (or *) -> method (or *) => score
	upstreamScores map[string]map[string]map[string]int
}

func NewUpstreamsRegistry(
	logger *zerolog.Logger,
	prjId string,
	upsCfg []*common.UpstreamConfig,
	rlr *RateLimitersRegistry,
	vr *vendors.VendorsRegistry,
	mt *health.Tracker,
) *UpstreamsRegistry {
	return &UpstreamsRegistry{
		prjId:                prjId,
		logger:               logger,
		clientRegistry:       NewClientRegistry(),
		rateLimitersRegistry: rlr,
		vendorsRegistry:      vr,
		metricsTracker:       mt,
		upsCfg:               upsCfg,
		sortedUpstreams:      make(map[string]map[string][]*Upstream),
		upstreamScores:       make(map[string]map[string]map[string]int),
		mapMu:                sync.RWMutex{},
	}
}

func (u *UpstreamsRegistry) Bootstrap(ctx context.Context) error {
	err := u.registerUpstreams()
	if err != nil {
		return err
	}
	return u.scheduleHealthCheckTimers(ctx)
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
	u.mapMu.Lock()
	defer u.mapMu.Unlock()

	var upstreams []*Upstream
	for _, ups := range u.allUpstreams {
		if s, e := ups.SupportsNetwork(networkId); e == nil && s {
			upstreams = append(upstreams, ups)
		} else if e != nil {
			u.logger.Warn().Err(e).
				Str("upstream", ups.Config().Id).
				Str("network", networkId).
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
		u.sortedUpstreams[networkId]["*"] = upstreams
	}

	// Initialize score for this or any network and any method for each upstream
	for _, ups := range upstreams {
		if _, ok := u.upstreamScores[ups.Config().Id]; !ok {
			u.upstreamScores[ups.Config().Id] = make(map[string]map[string]int)
		}

		if _, ok := u.upstreamScores[ups.Config().Id][networkId]; !ok {
			u.upstreamScores[ups.Config().Id][networkId] = make(map[string]int)
		}
		if _, ok := u.upstreamScores[ups.Config().Id][networkId]["*"]; !ok {
			u.upstreamScores[ups.Config().Id][networkId]["*"] = 0
		}

		if _, ok := u.upstreamScores[ups.Config().Id]["*"]; !ok {
			u.upstreamScores[ups.Config().Id]["*"] = make(map[string]int)
		}
		if _, ok := u.upstreamScores[ups.Config().Id]["*"]["*"]; !ok {
			u.upstreamScores[ups.Config().Id]["*"]["*"] = 0
		}
	}

	return nil
}

func (u *UpstreamsRegistry) GetSortedUpstreams(networkId, method string) ([]*Upstream, error) {
	u.mapMu.RLock()
	upsList := u.sortedUpstreams[networkId][method]
	if upsList == nil {
		upsList = u.sortedUpstreams[networkId]["*"]
		if upsList == nil {
			upsList = u.sortedUpstreams["*"]["*"]
			if upsList == nil {
				u.mapMu.RUnlock()
				return nil, common.NewErrNoUpstreamsFound(u.prjId, networkId)
			}
		}
		u.mapMu.RUnlock()

		// Create a copy of the default upstreams list for this method
		methodUpsList := make([]*Upstream, len(upsList))
		copy(methodUpsList, upsList)

		u.mapMu.Lock()
		u.sortedUpstreams[networkId][method] = methodUpsList
		u.mapMu.Unlock()

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
	} else {
		u.mapMu.RUnlock()
	}

	return upsList, nil
}

func (u *UpstreamsRegistry) sortUpstreams(networkId, method string, upstreams []*Upstream) {
	sort.Slice(upstreams, func(i, j int) bool {
		scoreI := u.upstreamScores[upstreams[i].Config().Id][networkId][method]
		scoreJ := u.upstreamScores[upstreams[j].Config().Id][networkId][method]

		if scoreI != scoreJ {
			return scoreI > scoreJ
		}

		// If scores are equal, sort by upstream ID for consistency
		return upstreams[i].Config().Id < upstreams[j].Config().Id
	})
}

func (u *UpstreamsRegistry) refreshUpstreamNetworkMethodScores() error {
	u.mapMu.Lock()
	defer u.mapMu.Unlock()

	if len(u.allUpstreams) == 0 {
		log.Debug().Str("projectId", u.prjId).Msgf("no upstreams yet to refresh scores")
		return nil
	}

	log.Debug().Str("projectId", u.prjId).Msgf("refreshing upstreams scores")

	for networkId, upsMap := range u.sortedUpstreams {
		for method, upsList := range upsMap {
			u.updateScoresAndSort(networkId, method, upsList)
		}
	}

	return nil
}

func (u *UpstreamsRegistry) registerUpstreams() error {
	u.mapMu.Lock()
	defer u.mapMu.Unlock()

	for _, upsCfg := range u.upsCfg {
		upstream, err := u.NewUpstream(u.prjId, upsCfg, u.logger, u.metricsTracker)
		if err != nil {
			return err
		}
		u.metricsTracker.RegisterUpstream(upstream)
		u.allUpstreams = append(u.allUpstreams, upstream)
	}

	if len(u.allUpstreams) == 0 {
		return common.NewErrNoUpstreamsDefined(u.prjId)
	}

	return nil
}

func (u *UpstreamsRegistry) scheduleHealthCheckTimers(ctx context.Context) error {
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				u.refreshUpstreamNetworkMethodScores()
			}
		}
	}()

	return nil
}

func (u *UpstreamsRegistry) updateScoresAndSort(networkId, method string, upsList []*Upstream) {
	var p90Latencies, errorRates, totalRequests, throttledRates []float64

	for _, ups := range upsList {
		metrics := u.metricsTracker.GetUpstreamMethodMetrics(networkId, ups.Config().Id, method)
		p90Latencies = append(p90Latencies, metrics.P90LatencySecs)

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
	}

	normP90Latencies := normalizeIntValues(p90Latencies, 100)
	normErrorRates := normalizeIntValues(errorRates, 100)
	normThrottledRates := normalizeIntValues(throttledRates, 100)
	normTotalRequests := normalizeIntValues(totalRequests, 100)

	for i, ups := range upsList {
		score := u.calculateScore(normTotalRequests[i], normP90Latencies[i], normErrorRates[i], normThrottledRates[i])
		u.upstreamScores[ups.Config().Id][networkId][method] = score

		log.Debug().Str("projectId", u.prjId).
			Str("upstream", ups.Config().Id).
			Str("method", method).
			Int("score", score).
			Msgf("refreshed score")
	}

	u.sortUpstreams(networkId, method, upsList)
	u.sortedUpstreams[networkId][method] = upsList

	newSortStr := ""
	for _, ups := range upsList {
		newSortStr += fmt.Sprintf("%s ", ups.Config().Id)
	}

	log.Debug().Str("projectId", u.prjId).Str("networkId", networkId).Str("method", method).Str("newSort", newSortStr).Msgf("sorted upstreams")
}

func (u *UpstreamsRegistry) calculateScore(normTotalRequests, normP90Latency, normErrorRate, normThrottledRate int) int {
	score := 0

	// Higher score for lower total requests (to balance the load)
	score += 100 - normTotalRequests

	// Higher score for lower p90 latency
	score += 100 - normP90Latency

	// Higher score for lower error rate
	score += (100 - normErrorRate) * 4

	// Higher score for lower throttled rate
	score += (100 - normThrottledRate) * 3

	return score
}

func normalizeIntValues(values []float64, scale int) []int {
	if len(values) == 0 {
		return []int{}
	}
	var min float64 = 0
	max := values[0]
	for _, value := range values {
		if value > max {
			max = value
		}
	}
	normalized := make([]int, len(values))
	for i, value := range values {
		if (max - min) > 0 {
			normalized[i] = int((value - min) / (max - min) * float64(scale))
		} else {
			normalized[i] = 0
		}
	}
	return normalized
}
