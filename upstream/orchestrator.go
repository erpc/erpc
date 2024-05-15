package upstream

import (
	"fmt"
	"slices"
	"time"

	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/config"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog/log"
)

const (
	ArchitectureEvm    = "evm"
	ArchitectureSolana = "solana"
)

type UpstreamOrchestrator struct {
	config        *config.Config
	clientManager *ClientManager
	upstreamsMap  map[string]map[string][]*PreparedUpstream
}

type NormalizedUpstreamMetrics struct {
	P90Latency    float64
	ErrorsTotal   float64
	RequestsTotal float64
	LastCollect   time.Time

	BlocksLag float64
}

var normalizedUpstreamMetrics = make(map[string]map[string]map[string]*NormalizedUpstreamMetrics)

func NewUpstreamOrchestrator(cfg *config.Config) *UpstreamOrchestrator {
	return &UpstreamOrchestrator{
		config:        cfg,
		clientManager: NewClientManager(),
	}
}

// Bootstrap function that has a timer to periodically reorder upstreams based on their health/performance
func (u *UpstreamOrchestrator) Bootstrap() error {
	// Start a timer to periodically reorder upstreams based on their health/performance
	timer := time.NewTicker(10 * time.Second)
	go func() {
		for range timer.C {
			for project, networks := range u.upstreamsMap {
				for network := range networks {
					u.RefreshNetworkUpstreamsHealth(project, network)
				}
			}
		}
	}()

	// Load initial upstreams from the hard-coded config
	u.upstreamsMap = make(map[string]map[string][]*PreparedUpstream)
	for _, project := range u.config.Projects {
		log.Info().Msgf("loading upstreams for project: %v", project.Id)
		if _, ok := u.upstreamsMap[project.Id]; !ok {
			u.upstreamsMap[project.Id] = make(map[string][]*PreparedUpstream)
		}

		for _, upstream := range project.Upstreams {
			preparedUpstream, err := u.prepareUpstream(project.Id, upstream)

			if err != nil {
				return fmt.Errorf("failed to prepare upstream on initialize: %s", upstream.Id)
			}

			for _, networkId := range preparedUpstream.NetworkIds {
				if _, ok := u.upstreamsMap[project.Id][networkId]; !ok {
					u.upstreamsMap[project.Id][networkId] = []*PreparedUpstream{}
				}

				u.upstreamsMap[project.Id][networkId] = append(u.upstreamsMap[project.Id][networkId], preparedUpstream)
			}
		}
	}

	return nil
}

func (u *UpstreamOrchestrator) GetUpstreamsForNetwork(projectId string, networkId string) ([]*PreparedUpstream, error) {
	if _, ok := u.upstreamsMap[projectId]; !ok {
		return nil, common.NewErrProjectNotFound(projectId)
	}

	if _, ok := u.upstreamsMap[projectId][networkId]; !ok {
		return nil, NewErrNetworkNotFound(networkId)
	}

	if len(u.upstreamsMap[projectId][networkId]) == 0 {
		return nil, NewErrNoUpstreamsDefined(projectId, networkId)
	}

	return u.upstreamsMap[projectId][networkId], nil
}

// Proactively update the health information of upstreams of a project/network and reorder them so the highest performing upstreams are at the top
func (u *UpstreamOrchestrator) RefreshNetworkUpstreamsHealth(projectId string, networkId string) {
	upstreams := u.upstreamsMap[projectId][networkId]
	type UpstreamWithMetrics struct {
		upstream   *PreparedUpstream
		errorRate  float64
		p90Latency float64
	}

	extractMetricsForUpstreams()

	var upstreamsWithMetrics []*UpstreamWithMetrics
	for _, upstream := range upstreams {
		p90Latency, errorRate := getMetricsForUpstream(projectId, networkId, upstream.Id)
		upstreamsWithMetrics = append(upstreamsWithMetrics, &UpstreamWithMetrics{
			upstream:   upstream,
			errorRate:  errorRate,
			p90Latency: p90Latency,
		})
	}

	slices.SortFunc(upstreamsWithMetrics, func(a, b *UpstreamWithMetrics) int {
		if a.errorRate == b.errorRate {
			if a.p90Latency == b.p90Latency {
				return 0
			}
			if a.p90Latency < b.p90Latency {
				return -1
			}
			return 1
		}
		if a.errorRate < b.errorRate {
			return -1
		}
		return 1
	})

	// Extract sorted upstreams
	var reorderedUpstreams []*PreparedUpstream
	for _, upstreamWithMetrics := range upstreamsWithMetrics {
		reorderedUpstreams = append(reorderedUpstreams, upstreamWithMetrics.upstream)
	}

	// Log the reordering
	log.Debug().Msgf("reordered upstreams for project: %s and network: %s", projectId, networkId)

	// Update the upstreams for the project/network
	u.upstreamsMap[projectId][networkId] = reorderedUpstreams
}

func extractMetricsForUpstreams() {
	// Get and parse current prometheus metrics data
	mfs, err := prometheus.DefaultGatherer.Gather()
	if mfs == nil {
		log.Error().Msgf("failed to gather prometheus metrics: %v", err)
		return
	}

	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			labels := m.GetLabel()
			var project, network, upstream string
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
			}

			if project != "" && network != "" && upstream != "" {
				if _, ok := normalizedUpstreamMetrics[project]; !ok {
					normalizedUpstreamMetrics[project] = make(map[string]map[string]*NormalizedUpstreamMetrics)
				}

				if _, ok := normalizedUpstreamMetrics[project][network]; !ok {
					normalizedUpstreamMetrics[project][network] = make(map[string]*NormalizedUpstreamMetrics)
				}

				if mf.GetName() == "erpc_upstream_request_duration_seconds" {
					percentiles := m.GetSummary().GetQuantile()
					for _, p := range percentiles {
						switch p.GetQuantile() {
						case 0.9:
							normalizedUpstreamMetrics[project][network][upstream] = &NormalizedUpstreamMetrics{
								P90Latency: p.GetValue(),
							}
						}
					}
				} else if mf.GetName() == "erpc_upstream_request_errors_total" {
					normalizedUpstreamMetrics[project][network][upstream].ErrorsTotal = m.GetCounter().GetValue()
				} else if mf.GetName() == "erpc_upstream_request_total" {
					normalizedUpstreamMetrics[project][network][upstream].RequestsTotal = m.GetCounter().GetValue()
				}

				normalizedUpstreamMetrics[project][network][upstream].LastCollect = time.Now()
			}
		}

	}
}

func (u *UpstreamOrchestrator) prepareUpstream(projectId string, upstream *config.UpstreamConfig) (*PreparedUpstream, error) {
	var networkIds []string = []string{}

	if upstream.Metadata != nil {
		if val, ok := upstream.Metadata["evmChainId"]; ok {
			log.Debug().Msgf("network ID set to %s via evmChainId for project: %s and upstream: %s", val, projectId, upstream.Id)
			networkIds = append(networkIds, val)
		} else {
			log.Debug().Msgf("network ID not set via evmChainId (%v) for project: %s and upstream: %s", upstream.Metadata["evmChainId"], projectId, upstream.Id)
		}
	}

	if upstream.Architecture == "" {
		upstream.Architecture = ArchitectureEvm
	}

	// TODO create a Client for upstream and try to "detect" the network ID(s)
	// if networkIds == nil || len(networkIds) == 0 {
	//
	// }

	if len(networkIds) == 0 {
		return nil, fmt.Errorf("could not detect any network ID for project: %s and upstream: %s either set it manually (e.g. metadata.evmChainId) to make sure endpoint can provide such info (e.g. support eth_chainId method)", projectId, upstream.Id)
	}

	preparedUpstream := &PreparedUpstream{
		Id:               upstream.Id,
		Architecture:     upstream.Architecture,
		Endpoint:         upstream.Endpoint,
		Metadata:         upstream.Metadata,
		NetworkIds:       networkIds,
		RateLimitBucket:  upstream.RateLimitBucket,
		HealthCheckGroup: upstream.HealthCheckGroup,
	}

	if client, err := u.clientManager.GetOrCreateClient(preparedUpstream); err != nil {
		return nil, err
	} else {
		preparedUpstream.Client = client
	}

	return preparedUpstream, nil
}

func getMetricsForUpstream(projectId, networkId, upstreamId string) (float64, float64) {
	if _, ok := normalizedUpstreamMetrics[projectId]; !ok {
		return 0, 0
	}

	if _, ok := normalizedUpstreamMetrics[projectId][networkId]; !ok {
		return 0, 0
	}

	if _, ok := normalizedUpstreamMetrics[projectId][networkId][upstreamId]; !ok {
		return 0, 0
	}

	mt := normalizedUpstreamMetrics[projectId][networkId][upstreamId]

	var errorRate float64 = 0
	p90Latency := mt.P90Latency
	if mt.RequestsTotal > 0 {
		errorRate = mt.ErrorsTotal / mt.RequestsTotal
	}

	return p90Latency, errorRate
}
