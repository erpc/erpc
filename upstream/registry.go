package upstream

import (
	"fmt"
	"slices"
	"time"

	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/vendors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

type UpstreamsRegistry struct {
	OnUpstreamsPriorityChange func(projectId string, networkId string) error

	logger                    *zerolog.Logger
	config                    *common.Config
	clientRegistry            *ClientRegistry
	vendorsRegistry           *vendors.VendorsRegistry
	rateLimitersRegistry      *RateLimitersRegistry
	upstreamsMapByNetwork     map[string]map[string]map[string]*Upstream
	upstreamsMapByHealthGroup map[string]map[string]*Upstream
}

func NewUpstreamsRegistry(
	logger *zerolog.Logger,
	cfg *common.Config,
	rlr *RateLimitersRegistry,
	vr *vendors.VendorsRegistry,
) (*UpstreamsRegistry, error) {
	r := &UpstreamsRegistry{
		logger:               logger,
		config:               cfg,
		clientRegistry:       NewClientRegistry(),
		rateLimitersRegistry: rlr,
		vendorsRegistry:      vr,
	}
	err := r.bootstrap()
	return r, err
}

// Bootstrap function that has a timer to periodically reorder upstreams based on their health/performance
func (u *UpstreamsRegistry) bootstrap() error {
	// Load initial upstreams from the hard-coded config
	u.upstreamsMapByNetwork = make(map[string]map[string]map[string]*Upstream)
	u.upstreamsMapByHealthGroup = make(map[string]map[string]*Upstream)
	for _, project := range u.config.Projects {
		lg := log.With().Str("project", project.Id).Logger()
		lg.Info().Msgf("loading upstreams for static project: %+v", project)
		if _, ok := u.upstreamsMapByNetwork[project.Id]; !ok {
			u.upstreamsMapByNetwork[project.Id] = make(map[string]map[string]*Upstream)
		}
		for _, ups := range project.Upstreams {
			preparedUpstream, err := u.NewUpstream(project.Id, ups, &lg)
			if err != nil {
				return common.NewErrUpstreamInitialization(err, ups.Id)
			}
			for _, networkId := range preparedUpstream.NetworkIds {
				if _, ok := u.upstreamsMapByNetwork[project.Id][networkId]; !ok {
					u.upstreamsMapByNetwork[project.Id][networkId] = make(map[string]*Upstream)
				}
				u.upstreamsMapByNetwork[project.Id][networkId][ups.Id] = preparedUpstream
			}
			if ups.HealthCheckGroup != "" {
				if _, ok := u.upstreamsMapByHealthGroup[ups.HealthCheckGroup]; !ok {
					u.upstreamsMapByHealthGroup[ups.HealthCheckGroup] = make(map[string]*Upstream)
				}
				u.upstreamsMapByHealthGroup[ups.HealthCheckGroup][ups.Id] = preparedUpstream
			}
		}
	}

	err := u.scheduleHealthCheckTimers()
	if err != nil {
		return err
	}

	return nil
}

func (u *UpstreamsRegistry) scheduleHealthCheckTimers() error {
	// A global timer to collect metrics for all upstreams
	// TODO make this work per-group and more accurate metrics collection vs prometheus
	go func() {
		for {
			u.collectMetricsForAllUpstreams()
			time.Sleep(60 * time.Second)
		}
	}()

	// A timer for each health check group to refresh the scores of the upstreams
	for healthGroupId := range u.upstreamsMapByHealthGroup {
		healthCheckGroup := u.config.HealthChecks.GetGroupConfig(healthGroupId)

		if healthCheckGroup == nil {
			return common.NewErrHealthCheckGroupNotFound(healthGroupId)
		}

		checkIntervalDuration, err := time.ParseDuration(healthCheckGroup.CheckInterval)
		if err != nil {
			return common.NewErrInvalidHealthCheckConfig(fmt.Errorf("could not pase checkInterval: %w", err), healthGroupId)
		}
		log.Debug().Str("healthCheckGroup", healthGroupId).Dur("interval", checkIntervalDuration).Msgf("scheduling health check timer")

		go func(healthCheckGroup *common.HealthCheckGroupConfig, checkIntervalDuration time.Duration) {
			for {
				u.refreshUpstreamGroupScores(healthCheckGroup, u.upstreamsMapByHealthGroup[healthCheckGroup.Id])
				time.Sleep(time.Duration(checkIntervalDuration))
			}
		}(healthCheckGroup, checkIntervalDuration)
	}

	return nil
}

func (u *UpstreamsRegistry) GetUpstreamsByProject(projectId string) ([]*Upstream, error) {
	if _, ok := u.upstreamsMapByNetwork[projectId]; !ok {
		return nil, common.NewErrProjectNotFound(projectId)
	}

	if len(u.upstreamsMapByNetwork[projectId]) == 0 {
		return nil, common.NewErrNoUpstreamsDefined(projectId)
	}

	var upstreams []*Upstream
	for _, upstreamsForProject := range u.upstreamsMapByNetwork[projectId] {
		for _, upstream := range upstreamsForProject {
			upstreams = append(upstreams, upstream)
		}
	}

	slices.SortFunc(upstreams, func(a, b *Upstream) int {
		if a.Score == b.Score {
			return 1
		}
		if a.Score < b.Score {
			return -1
		}
		return 1
	})

	return upstreams, nil
}

// Proactively update the health information of upstreams of a project/network and reorder them so the highest performing upstreams are at the top
func (u *UpstreamsRegistry) refreshUpstreamGroupScores(healthGroupCfg *common.HealthCheckGroupConfig, upstreams map[string]*Upstream) error {
	log.Debug().Str("healthCheckGroup", healthGroupCfg.Id).Msgf("refreshing upstreams scores")

	var p90Latencies, errorRates, totalRequests, throttledRates, blockLags []float64
	var comparingUpstreams []*Upstream
	var changedProjectAndNetworks map[string]map[string]bool = make(map[string]map[string]bool)
	for _, ups := range upstreams {
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

			if changedProjectAndNetworks[ups.ProjectId] == nil {
				changedProjectAndNetworks[ups.ProjectId] = make(map[string]bool)
			}

			for _, networkId := range ups.NetworkIds {
				changedProjectAndNetworks[ups.ProjectId][networkId] = true
			}

			comparingUpstreams = append(comparingUpstreams, ups)
		}
	}

	normP90Latencies := normalizeIntValues(p90Latencies, 100)
	normErrorRates := normalizeIntValues(errorRates, 100)
	normThrottledRates := normalizeIntValues(throttledRates, 100)
	normTotalRequests := normalizeIntValues(totalRequests, 100)
	normBlockLags := normalizeIntValues(blockLags, 100)

	for i, ups := range comparingUpstreams {
		if ups.Metrics != nil {
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
	}

	if u.OnUpstreamsPriorityChange != nil {
		for projectId, networks := range changedProjectAndNetworks {
			for networkId := range networks {
				u.OnUpstreamsPriorityChange(projectId, networkId)
			}
		}
	}

	return nil
}

func (u *UpstreamsRegistry) collectMetricsForAllUpstreams() {
	if len(u.upstreamsMapByNetwork) == 0 {
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

			if project != "" && network != "" && upstream != "" && category != "" {
				var metrics = u.upstreamsMapByNetwork[project][network][upstream].Metrics
				if metrics == nil {
					metrics = &UpstreamMetrics{}
					u.upstreamsMapByNetwork[project][network][upstream].Metrics = metrics
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
