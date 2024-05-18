package upstream

import (
	"fmt"
	"slices"
	"time"

	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/config"
	"github.com/flair-sdk/erpc/resiliency"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

const (
	ArchitectureEvm    = "evm"
	ArchitectureSolana = "solana"
)

type UpstreamsRegistry struct {
	OnUpstreamsPriorityChange func(projectId string, networkId string) error

	config                    *config.Config
	clientRegistry            *ClientRegistry
	upstreamsMapByNetwork     map[string]map[string]map[string]*PreparedUpstream
	upstreamsMapByHealthGroup map[string]map[string]*PreparedUpstream
	rateLimitersRegistry      *resiliency.RateLimitersRegistry
}

func NewUpstreamsRegistry(cfg *config.Config, rlr *resiliency.RateLimitersRegistry) (*UpstreamsRegistry, error) {
	r := &UpstreamsRegistry{
		config:               cfg,
		clientRegistry:       NewClientRegistry(),
		rateLimitersRegistry: rlr,
	}
	err := r.bootstrap()
	return r, err
}

// Bootstrap function that has a timer to periodically reorder upstreams based on their health/performance
func (u *UpstreamsRegistry) bootstrap() error {
	// Load initial upstreams from the hard-coded config
	u.upstreamsMapByNetwork = make(map[string]map[string]map[string]*PreparedUpstream)
	u.upstreamsMapByHealthGroup = make(map[string]map[string]*PreparedUpstream)
	for _, project := range u.config.Projects {
		lg := log.With().Str("project", project.Id).Logger()
		lg.Info().Msgf("loading upstreams for static project")
		if _, ok := u.upstreamsMapByNetwork[project.Id]; !ok {
			u.upstreamsMapByNetwork[project.Id] = make(map[string]map[string]*PreparedUpstream)
		}
		for _, ups := range project.Upstreams {
			preparedUpstream, err := u.NewUpstream(project.Id, &lg, ups)
			if err != nil {
				return common.NewErrUpstreamInitialization(err, ups.Id)
			}
			for _, networkId := range preparedUpstream.NetworkIds {
				if _, ok := u.upstreamsMapByNetwork[project.Id][networkId]; !ok {
					u.upstreamsMapByNetwork[project.Id][networkId] = make(map[string]*PreparedUpstream)
				}
				u.upstreamsMapByNetwork[project.Id][networkId][ups.Id] = preparedUpstream
			}
			if ups.HealthCheckGroup != "" {
				if _, ok := u.upstreamsMapByHealthGroup[ups.HealthCheckGroup]; !ok {
					u.upstreamsMapByHealthGroup[ups.HealthCheckGroup] = make(map[string]*PreparedUpstream)
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
			time.Sleep(5 * time.Second)
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

		go func(healthCheckGroup *config.HealthCheckGroupConfig, checkIntervalDuration time.Duration) {
			for {
				u.refreshUpstreamGroupScores(healthCheckGroup, u.upstreamsMapByHealthGroup[healthGroupId])
				time.Sleep(time.Duration(checkIntervalDuration))
			}
		}(healthCheckGroup, checkIntervalDuration)
	}

	return nil
}

func (u *UpstreamsRegistry) GetUpstreamsByProject(projectId string) ([]*PreparedUpstream, error) {
	if _, ok := u.upstreamsMapByNetwork[projectId]; !ok {
		return nil, common.NewErrProjectNotFound(projectId)
	}

	if len(u.upstreamsMapByNetwork[projectId]) == 0 {
		return nil, common.NewErrNoUpstreamsDefined(projectId)
	}

	var upstreams []*PreparedUpstream
	for _, upstreamsForProject := range u.upstreamsMapByNetwork[projectId] {
		for _, upstream := range upstreamsForProject {
			upstreams = append(upstreams, upstream)
		}
	}

	slices.SortFunc(upstreams, func(a, b *PreparedUpstream) int {
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
func (u *UpstreamsRegistry) refreshUpstreamGroupScores(healthGroupCfg *config.HealthCheckGroupConfig, upstreams map[string]*PreparedUpstream) error {
	log.Debug().Str("healthCheckGroup", healthGroupCfg.Id).Msgf("refreshing upstreams scores")

	var p90Latencies, errorRates, totalRequests, throttledRates, blockLags []float64
	var changedProjectAndNetworks map[string]map[string]bool = make(map[string]map[string]bool)
	for _, upstream := range upstreams {
		if upstream.Metrics != nil {
			p90Latencies = append(p90Latencies, upstream.Metrics.P90Latency)
			if upstream.Metrics.RequestsTotal > 0 {
				errorRates = append(errorRates, upstream.Metrics.ErrorsTotal/upstream.Metrics.RequestsTotal)
				throttledRates = append(throttledRates, upstream.Metrics.ThrottledTotal/upstream.Metrics.RequestsTotal)
				totalRequests = append(totalRequests, upstream.Metrics.RequestsTotal)
			} else {
				errorRates = append(errorRates, 0)
				throttledRates = append(throttledRates, 0)
				totalRequests = append(totalRequests, 0)
			}
			blockLags = append(blockLags, upstream.Metrics.BlocksLag)

			if changedProjectAndNetworks[upstream.ProjectId] == nil {
				changedProjectAndNetworks[upstream.ProjectId] = make(map[string]bool)
			}

			for _, networkId := range upstream.NetworkIds {
				changedProjectAndNetworks[upstream.ProjectId][networkId] = true
			}
		}
	}

	normP90Latencies := normalizeFloatValues(p90Latencies)
	normErrorRates := normalizeFloatValues(errorRates)
	normThrottledRates := normalizeFloatValues(throttledRates)
	normTotalRequests := normalizeFloatValues(totalRequests)
	normBlockLags := normalizeFloatValues(blockLags)

	i := 0
	for _, upstream := range upstreams {
		if upstream.Metrics != nil {
			upstream.Score = 0

			// Higher score for lower total requests (to balance the load)
			upstream.Score += 1 - normTotalRequests[i]

			// Higher score for lower p90 latency
			upstream.Score += 1 - normP90Latencies[i]

			// Higher score for lower error rate
			upstream.Score += (1 - normErrorRates[i]) * 4

			// Higher score for lower throttled rate
			upstream.Score += (1 - normThrottledRates[i]) * 3

			// Higher score for lower block lag
			upstream.Score += (1 - normBlockLags[i]) * 2

			log.Debug().Str("healthCheckGroup", healthGroupCfg.Id).Str("upstream", upstream.Id).Float64("score", upstream.Score).Msgf("refreshed score")
			i++
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
		log.Debug().Msgf("no upstreams to collect metrics for")
		return
	}

	log.Debug().Msgf("collecting upstreams metrics from prometheus")

	// Get and parse current prometheus metrics data
	mfs, err := prometheus.DefaultGatherer.Gather()
	if mfs == nil {
		log.Error().Msgf("failed to gather prometheus metrics: %v", err)
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

				log.Trace().
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

func (u *UpstreamsRegistry) NewUpstream(projectId string, logger *zerolog.Logger, upstream *config.UpstreamConfig) (*PreparedUpstream, error) {
	var networkIds []string = []string{}

	lg := logger.With().Str("upstream", upstream.Id).Logger()

	if upstream.Metadata != nil {
		if val, ok := upstream.Metadata["evmChainId"]; ok {
			lg.Debug().Str("network", val).Msgf("network ID set to %s via evmChainId", val)
			networkIds = append(networkIds, val)
		} else {
			lg.Debug().Msgf("network ID not set via metadata.evmChainId: %v", upstream.Metadata["evmChainId"])
		}
	}

	if upstream.Architecture == "" {
		upstream.Architecture = ArchitectureEvm
	}

	// TODO create a Client for upstream and try to "detect" the network ID(s)
	// if networkIds == nil || len(networkIds) == 0 {
	// }

	if len(networkIds) == 0 {
		return nil, common.NewErrUpstreamNetworkNotDetected(projectId, upstream.Id)
	}

	policies, err := resiliency.CreateFailSafePolicies(upstream.Id, upstream.Failsafe)
	if err != nil {
		return nil, err
	}

	preparedUpstream := &PreparedUpstream{
		Id:               upstream.Id,
		Architecture:     upstream.Architecture,
		Endpoint:         upstream.Endpoint,
		Metadata:         upstream.Metadata,
		RateLimitBucket:  upstream.RateLimitBucket,
		HealthCheckGroup: upstream.HealthCheckGroup,
		FailsafePolicies: policies,

		ProjectId:  projectId,
		NetworkIds: networkIds,
		Logger:     lg,

		rateLimitersRegistry: u.rateLimitersRegistry,
	}

	lg.Debug().Msgf("prepared upstream")

	if client, err := u.clientRegistry.GetOrCreateClient(preparedUpstream); err != nil {
		return nil, err
	} else {
		preparedUpstream.Client = client
	}

	return preparedUpstream, nil
}

func normalizeFloatValues(values []float64) []float64 {
	if len(values) == 0 {
		return values
	}
	var min float64 = 0
	// min, max := values[0], values[0]
	max := values[0]
	for _, value := range values {
		// if value < min {
		// 	min = value
		// }
		if value > max {
			max = value
		}
	}
	// if min == max {
	// 	normalized := make([]float64, len(values))
	// 	for i := range normalized {
	// 		normalized[i] = 1
	// 	}
	// 	return normalized
	// }
	normalized := make([]float64, len(values))
	for i, value := range values {
		if (max - min) > 0 {
			normalized[i] = (value - min) / (max - min)
		} else {
			normalized[i] = 0
		}
	}
	return normalized
}
