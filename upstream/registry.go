package upstream

import (
	"fmt"
	"sync"
	"time"

	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/vendors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

type UpstreamsRegistry struct {
	OnUpstreamsPriorityChange func(projectId string) error

	logger                      *zerolog.Logger
	config                      *common.Config
	clientRegistry              *ClientRegistry
	vendorsRegistry             *vendors.VendorsRegistry
	rateLimitersRegistry        *RateLimitersRegistry
	mapMu                       sync.RWMutex
	upstreamsMapByProject       map[string][]*Upstream
	upstreamsMapByProjectWithId map[string]map[string]*Upstream
	upstreamsMapByHealthGroup   map[string]map[string]*Upstream
	shutdownChan                chan struct{}
}

func NewUpstreamsRegistry(
	logger *zerolog.Logger,
	cfg *common.Config,
	rlr *RateLimitersRegistry,
	vr *vendors.VendorsRegistry,
) *UpstreamsRegistry {
	r := &UpstreamsRegistry{
		logger:                      logger,
		config:                      cfg,
		clientRegistry:              NewClientRegistry(),
		rateLimitersRegistry:        rlr,
		vendorsRegistry:             vr,
		upstreamsMapByProject:       make(map[string][]*Upstream),
		upstreamsMapByProjectWithId: make(map[string]map[string]*Upstream),
		upstreamsMapByHealthGroup:   make(map[string]map[string]*Upstream),
		shutdownChan:                make(chan struct{}),
	}
	return r
}

func (u *UpstreamsRegistry) Bootstrap() error {
	return u.scheduleHealthCheckTimers()
}

func (u *UpstreamsRegistry) Shutdown() error {
	close(u.shutdownChan)
	return nil
}

func (u *UpstreamsRegistry) scheduleHealthCheckTimers() error {
	// A global timer to collect metrics for all upstreams
	// TODO make this work per-group and more accurate metrics collection vs prometheus
	go func() {
		for {
			select {
			case <-u.shutdownChan:
				return
			default:
				u.collectMetricsForAllUpstreams()
				time.Sleep(5 * time.Second)
			}
		}
	}()

	// A timer for each health check group to refresh the scores of the upstreams
	if u.config == nil || u.config.HealthChecks == nil || len(u.config.HealthChecks.Groups) == 0 {
		log.Debug().Msgf("no health check groups defined, skipping health check timers")
		return nil
	}

	u.mapMu.Lock()
	for _, healthCheckGroup := range u.config.HealthChecks.Groups {
		healthGroupId := healthCheckGroup.Id
		checkIntervalDuration, err := time.ParseDuration(healthCheckGroup.CheckInterval)
		if err != nil {
			return common.NewErrInvalidHealthCheckConfig(fmt.Errorf("could not pase checkInterval: %w", err), healthGroupId)
		}
		log.Debug().Str("healthCheckGroup", healthGroupId).Dur("interval", checkIntervalDuration).Msgf("scheduling health check timer")
		go func(healthCheckGroup *common.HealthCheckGroupConfig, checkIntervalDuration time.Duration) {
			for {
				select {
				case <-u.shutdownChan:
					return
				default:
					u.refreshUpstreamGroupScores(healthCheckGroup, u.upstreamsMapByHealthGroup[healthCheckGroup.Id])
					time.Sleep(time.Duration(checkIntervalDuration))
				}
			}
		}(healthCheckGroup, checkIntervalDuration)
	}
	u.mapMu.Unlock()

	return nil
}

func (u *UpstreamsRegistry) GetUpstreamsByProject(prjCfg *common.ProjectConfig) ([]*Upstream, error) {
	u.mapMu.RLock()
	if upstreams, ok := u.upstreamsMapByProject[prjCfg.Id]; ok {
		u.mapMu.RUnlock()
		return upstreams, nil
	}
	u.mapMu.RUnlock()

	var upstreams []*Upstream
	u.mapMu.Lock()
	for _, upsCfg := range prjCfg.Upstreams {
		upstream, err := u.NewUpstream(prjCfg.Id, upsCfg, u.logger)
		if err != nil {
			return nil, err
		}
		if upsCfg.HealthCheckGroup != "" {
			if _, ok := u.upstreamsMapByHealthGroup[upsCfg.HealthCheckGroup]; !ok {
				u.upstreamsMapByHealthGroup[upsCfg.HealthCheckGroup] = make(map[string]*Upstream)
			}
			u.upstreamsMapByHealthGroup[upsCfg.HealthCheckGroup][upsCfg.Id] = upstream
		}
		if _, ok := u.upstreamsMapByProjectWithId[prjCfg.Id]; !ok {
			u.upstreamsMapByProjectWithId[prjCfg.Id] = make(map[string]*Upstream)
		}
		u.upstreamsMapByProjectWithId[prjCfg.Id][upsCfg.Id] = upstream
		upstreams = append(upstreams, upstream)
	}
	u.mapMu.Unlock()

	if len(upstreams) == 0 {
		return nil, common.NewErrNoUpstreamsDefined(prjCfg.Id)
	}

	u.upstreamsMapByProject[prjCfg.Id] = upstreams
	return upstreams, nil
}

// Proactively update the health information of upstreams of a project/network and reorder them so the highest performing upstreams are at the top
func (u *UpstreamsRegistry) refreshUpstreamGroupScores(healthGroupCfg *common.HealthCheckGroupConfig, upstreams map[string]*Upstream) error {
	u.mapMu.RLock()
	defer u.mapMu.RUnlock()

	if len(upstreams) == 0 {
		log.Debug().Str("healthCheckGroup", healthGroupCfg.Id).Msgf("no upstreams yet to refresh scores for health check group")
		return nil
	}

	log.Debug().Str("healthCheckGroup", healthGroupCfg.Id).Msgf("refreshing upstreams scores")

	var p90Latencies, errorRates, totalRequests, throttledRates, blockLags []float64
	var comparingUpstreams []*Upstream
	var changedProjects map[string]bool = make(map[string]bool)
	for _, ups := range upstreams {
		ups.metricsMu.RLock()
		if ups.Metrics != nil {
			p90Latencies = append(p90Latencies, ups.Metrics.P90Latency)
			if ups.Metrics.RequestsTotal > 0 {
				errorRates = append(errorRates, ups.Metrics.ErrorsTotal/ups.Metrics.RequestsTotal)
				throttledRates = append(throttledRates, ups.Metrics.ThrottledTotal/ups.Metrics.RequestsTotal)
				totalRequests = append(totalRequests, ups.Metrics.RequestsTotal)
			} else {
				errorRates = append(errorRates, 0)
				throttledRates = append(throttledRates, 0)
				totalRequests = append(totalRequests, 0)
			}
			blockLags = append(blockLags, ups.Metrics.BlocksLag)

			changedProjects[ups.ProjectId] = true
			comparingUpstreams = append(comparingUpstreams, ups)
		}
		ups.metricsMu.RUnlock()
	}

	normP90Latencies := normalizeIntValues(p90Latencies, 100)
	normErrorRates := normalizeIntValues(errorRates, 100)
	normThrottledRates := normalizeIntValues(throttledRates, 100)
	normTotalRequests := normalizeIntValues(totalRequests, 100)
	normBlockLags := normalizeIntValues(blockLags, 100)

	for i, ups := range comparingUpstreams {
		ups.Score = 0

		// Higher score for lower total requests (to balance the load)
		ups.Score += 100 - normTotalRequests[i]

		// Higher score for lower p90 latency
		ups.Score += 100 - normP90Latencies[i]

		// Higher score for lower error rate
		ups.Score += (100 - normErrorRates[i]) * 4

		// Higher score for lower throttled rate
		ups.Score += (100 - normThrottledRates[i]) * 3

		// Higher score for lower block lag
		ups.Score += (100 - normBlockLags[i]) * 2

		log.Debug().Str("healthCheckGroup", healthGroupCfg.Id).
			Str("upstream", ups.Config().Id).
			Int("score", ups.Score).
			Msgf("refreshed score")
	}

	if u.OnUpstreamsPriorityChange != nil {
		for projectId := range changedProjects {
			u.OnUpstreamsPriorityChange(projectId)
		}
	}

	return nil
}

func (u *UpstreamsRegistry) collectMetricsForAllUpstreams() {
	if len(u.upstreamsMapByProject) == 0 {
		u.logger.Debug().Msgf("no upstreams to collect metrics for")
		return
	}

	u.logger.Debug().Msgf("collecting upstreams metrics from prometheus")

	// Get and parse current prometheus metrics data
	mfs, err := prometheus.DefaultGatherer.Gather()
	if mfs == nil {
		u.logger.Error().Msgf("failed to gather prometheus metrics: %v", err)
		return
	}

	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			labels := m.GetLabel()
			var project, network, upstream, category string
			for _, label := range labels {
				if label.GetName() == "project" {
					project = label.GetValue()
				}
				if label.GetName() == "network" {
					network = label.GetValue()
				}
				if label.GetName() == "upstream" {
					upstream = label.GetValue()
				}
				if label.GetName() == "category" {
					category = label.GetValue()
				}
			}

			if project != "" && upstream != "" && category != "" {
				// Skip collection if project not registered yet
				// TODO improve this scenario
				if _, ok := u.upstreamsMapByProjectWithId[project]; !ok {
					continue
				}
				ups := u.upstreamsMapByProjectWithId[project][upstream]
				if ups == nil {
					continue
				}

				ups.metricsMu.RLock()
				var metrics = ups.Metrics
				if metrics == nil {
					metrics = &UpstreamMetrics{}
					ups.metricsMu.RUnlock()
					ups.metricsMu.Lock()
					ups.Metrics = metrics
					ups.metricsMu.Unlock()
				} else {
					ups.metricsMu.RUnlock()
				}

				if mf.GetName() == "erpc_upstream_request_duration_seconds" {
					percentiles := m.GetSummary().GetQuantile()
					for _, p := range percentiles {
						switch p.GetQuantile() {
						case 0.9:
							metrics.P90Latency = p.GetValue()
						}
					}
				} else if mf.GetName() == "erpc_upstream_request_errors_total" {
					metrics.ErrorsTotal = m.GetCounter().GetValue()
				} else if mf.GetName() == "erpc_upstream_request_total" {
					metrics.RequestsTotal = m.GetCounter().GetValue()
				} else if mf.GetName() == "erpc_upstream_request_local_rate_limited_total" {
					metrics.ThrottledTotal = m.GetCounter().GetValue()
				}

				metrics.LastCollect = time.Now()

				u.logger.Trace().
					Str("project", project).
					Str("network", network).
					Str("upstream", upstream).
					Str("category", category).
					Object("metrics", metrics).
					Msgf("collected metrics")
			}
		}
	}
}

func (u *UpstreamsRegistry) NewUpstream(projectId string, cfg *common.UpstreamConfig, logger *zerolog.Logger) (*Upstream, error) {
	return NewUpstream(projectId, cfg, u.clientRegistry, u.rateLimitersRegistry, u.vendorsRegistry, logger)
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
