package upstream

import (
	"fmt"
	"sort"
	"time"

	"github.com/flair-sdk/erpc/config"
	"github.com/prometheus/client_golang/prometheus"
	promgo "github.com/prometheus/client_model/go"
	"github.com/rs/zerolog/log"
)

const (
	ArchitectureEvm    = "evm"
	ArchitectureSolana = "solana"
)

type PreparedUpstream struct {
	Id           string
	Architecture string // ArchitectureEvm, ArchitectureSolana, ...
	Endpoint     string

	Metadata   map[string]string
	NetworkIds []string
	Client     ClientInterface
}

type UpstreamOrchestrator struct {
	config        *config.Config
	clientManager *ClientManager
	upstreamsMap  map[string]map[string][]*PreparedUpstream
}

func NewUpstreamOrchestrator(cfg *config.Config) *UpstreamOrchestrator {
	return &UpstreamOrchestrator{
		config:        cfg,
		clientManager: NewClientManager(),
	}
}

// Bootstrap function that has a timer to periodically reorder upstreams based on their health/performance
func (u *UpstreamOrchestrator) Bootstrap() error {
	// Start a timer to periodically reorder upstreams

	// TODO For now let's just reorder the upstreams every 10 seconds
	// TODO In reality we would want to reorder the upstreams based on their health/performance/ciruit breaker status
	timer := time.NewTicker(10 * time.Second)
	go func() {
		for range timer.C {
			// For each project/network, reorder the upstreams based on their health/performance
			for project, networks := range u.upstreamsMap {
				for network := range networks {
					// Reorder the upstreams based on their health/performance
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

// Function to find the best upstream for a project/network
func (u *UpstreamOrchestrator) GetUpstreams(projectId string, networkId string) ([]*PreparedUpstream, error) {
	if _, ok := u.upstreamsMap[projectId]; !ok {
		return nil, fmt.Errorf("project %s not found", projectId)
	}

	if _, ok := u.upstreamsMap[projectId][networkId]; !ok {
		return nil, fmt.Errorf("network %s not found for project %s", networkId, projectId)
	}

	if len(u.upstreamsMap[projectId][networkId]) == 0 {
		return nil, fmt.Errorf("no upstreams found for project %s and network %s", projectId, networkId)
	}

	return u.upstreamsMap[projectId][networkId], nil
}

// Proactively update the health information of upstreams of a project/network and reorder them so the highest performing upstreams are at the top
func (u *UpstreamOrchestrator) RefreshNetworkUpstreamsHealth(projectId string, networkId string) {
	// For now let's randomly reorder the upstreams:
	// Get the upstreams for the project/network
	upstreams := u.upstreamsMap[projectId][networkId]

	// Create a slice of upstreams with metrics
	type UpstreamWithMetrics struct {
		upstream  *PreparedUpstream
		errorRate float64
		latency   float64
	}

	var upstreamsWithMetrics []UpstreamWithMetrics
	for _, upstream := range upstreams {
		latency, errorRate := getMetricsDataForUpstream(projectId, networkId, upstream.Id)
		upstreamsWithMetrics = append(upstreamsWithMetrics, UpstreamWithMetrics{
			upstream:  upstream,
			errorRate: errorRate,
			latency:   latency,
		})
	}

	// Sort by lowest error rate and latency
	sort.Slice(upstreamsWithMetrics, func(i, j int) bool {
		log.Debug().Msgf("comparing upstreams %s (errorRate=%f latency=%f) and %s (errorRate=%f latency=%f)", upstreamsWithMetrics[i].upstream.Id, upstreamsWithMetrics[i].errorRate, upstreamsWithMetrics[i].latency, upstreamsWithMetrics[j].upstream.Id, upstreamsWithMetrics[j].errorRate, upstreamsWithMetrics[j].latency)
		if upstreamsWithMetrics[i].errorRate == upstreamsWithMetrics[j].errorRate {
			return upstreamsWithMetrics[i].latency < upstreamsWithMetrics[j].latency
		}
		return upstreamsWithMetrics[i].errorRate < upstreamsWithMetrics[j].errorRate
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

func (u *UpstreamOrchestrator) prepareUpstream(projectId string, upstream config.UpstreamConfig) (*PreparedUpstream, error) {
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
		Id:           upstream.Id,
		Architecture: upstream.Architecture,
		Endpoint:     upstream.Endpoint,
		Metadata:     upstream.Metadata,
		NetworkIds:   networkIds,
	}

	if client, err := u.clientManager.GetOrCreateClient(preparedUpstream); err != nil {
		return nil, err
	} else {
		preparedUpstream.Client = client
	}

	return preparedUpstream, nil
}

func getMetricsDataForUpstream(projectId, networkId, upstreamId string) (float64, float64) {
	// Get and parse current prometheus metrics data
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		log.Error().Msgf("failed to gather prometheus metrics: %v", err)
		// TODO think of a proper/sane default
		return 1, 1
	}

	var latencyNinetyPercentile float64 = 0
	var requestsTotal float64 = 0
	var errorsTotal float64 = 0

	// Find the metrics for the upstream
	for _, mf := range mfs {
		if mf.GetName() == "erpc_upstream_request_duration_seconds" {
			for _, m := range mf.GetMetric() {
				labels := m.GetLabel()
				if doLabelsMatch(labels, projectId, networkId, upstreamId) {
					percentiles := m.GetSummary().GetQuantile()
					for _, p := range percentiles {
						switch p.GetQuantile() {
						case 0.9:
							latencyNinetyPercentile = p.GetValue()
						}
					}
				}
			}
		}

		if mf.GetName() == "erpc_upstream_request_errors_total" {
			for _, m := range mf.GetMetric() {
				labels := m.GetLabel()
				if doLabelsMatch(labels, projectId, networkId, upstreamId) {
					errorsTotal = m.GetCounter().GetValue()
				}
			}
		}

		if mf.GetName() == "erpc_upstream_request_total" {
			for _, m := range mf.GetMetric() {
				labels := m.GetLabel()
				// log.Debug().Msgf("ERT prometheus labels: %d %v", len((labels)), labels)
				if doLabelsMatch(labels, projectId, networkId, upstreamId) {
					// if labels[0].GetValue() == projectId && labels[1].GetValue() == networkId && labels[2].GetValue() == upstreamId {
					// log.Debug().Msgf("ERT prometheus value: %f", m.GetCounter().GetValue())
					requestsTotal = m.GetCounter().GetValue()
				}
			}
		}
	}

	log.Debug().Msgf("found metrics for project: %s network: %s upstream: %s latency: %f errors: %f requests: %f", projectId, networkId, upstreamId, latencyNinetyPercentile, errorsTotal, requestsTotal)

	var errorRate float64 = 0

	if requestsTotal > 0 {
		errorRate = errorsTotal / requestsTotal
	}

	return latencyNinetyPercentile, errorRate
}

func doLabelsMatch(labels []*promgo.LabelPair, projectId, networkId, upstreamId string) bool {
	var projectLabel *promgo.LabelPair
	var networkLabel *promgo.LabelPair
	var upstreamLabel *promgo.LabelPair

	for _, label := range labels {
		if label.GetName() == "project" {
			projectLabel = label
		} else if label.GetName() == "network" {
			networkLabel = label
		} else if label.GetName() == "upstream" {
			upstreamLabel = label
		}
	}

	if projectLabel == nil || networkLabel == nil || upstreamLabel == nil {
		return false
	}

	return projectLabel.GetValue() == projectId && networkLabel.GetValue() == networkId && upstreamLabel.GetValue() == upstreamId
}
