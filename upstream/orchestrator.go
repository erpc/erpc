package upstream

import (
	"fmt"
	"time"

	"github.com/flair-sdk/erpc/config"
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
	Metadata     map[string]string
	NetworkId    string
	Client       *Client
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
func (u *UpstreamOrchestrator) Bootstrap() {
	// Start a timer to periodically reorder upstreams

	// For now let's just reorder the upstreams every 10 seconds
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
		log.Info().Msgf("Loading upstreams for project: %v", project.Id)
		if _, ok := u.upstreamsMap[project.Id]; !ok {
			u.upstreamsMap[project.Id] = make(map[string][]*PreparedUpstream)
		}

		for _, upstream := range project.Upstreams {
			preparedUpstream, err := u.prepareUpstream(project.Id, upstream)

			if err != nil {
				log.Fatal().Err(err).Msgf("failed to prepare upstream on initialize: %s", upstream.Id)
			}

			if _, ok := u.upstreamsMap[project.Id][preparedUpstream.NetworkId]; !ok {
				u.upstreamsMap[project.Id][preparedUpstream.NetworkId] = []*PreparedUpstream{}
			}

			u.upstreamsMap[project.Id][preparedUpstream.NetworkId] = append(u.upstreamsMap[project.Id][preparedUpstream.NetworkId], preparedUpstream)
		}
	}
}

// Function to find the best upstream for a project/network
func (u *UpstreamOrchestrator) FindBestUpstream(projectId string, networkId string) (*PreparedUpstream, error) {
	if _, ok := u.upstreamsMap[projectId]; !ok {
		return nil, fmt.Errorf("project %s not found", projectId)
	}

	if _, ok := u.upstreamsMap[projectId][networkId]; !ok {
		return nil, fmt.Errorf("network %s not found for project %s", networkId, projectId)
	}

	if len(u.upstreamsMap[projectId][networkId]) == 0 {
		return nil, fmt.Errorf("no upstreams found for project %s and network %s", projectId, networkId)
	}

	return u.upstreamsMap[projectId][networkId][0], nil
}

// Proactively update the health information of upstreams of a project/network and reorder them so the highest performing upstreams are at the top
func (u *UpstreamOrchestrator) RefreshNetworkUpstreamsHealth(projectId string, networkId string) {
	// For now let's randomly reorder the upstreams:
	// Get the upstreams for the project/network
	upstreams := u.upstreamsMap[projectId][networkId]

	// Randomly reorder the upstreams
	var reorderedUpstreams []*PreparedUpstream
	for i := len(upstreams) - 1; i >= 0; i-- {
		reorderedUpstreams = append(reorderedUpstreams, upstreams[i])
	}

	// Update the upstreams for the project/network
	u.upstreamsMap[projectId][networkId] = reorderedUpstreams

	// Log the reordering
	log.Info().Msgf("Reordered upstreams for project: %s and network: %s", projectId, networkId)
	for i, upstream := range reorderedUpstreams {
		log.Info().Msgf("Upstream %d: %s", i, upstream.Id)
	}
}

func (u *UpstreamOrchestrator) prepareUpstream(projectId string, upstream config.Upstream) (*PreparedUpstream, error) {
	var networkId string

	if upstream.Metadata != nil {
		if val, ok := upstream.Metadata["evmChainId"]; ok {
			log.Debug().Msgf("network ID set via evmChainId for project: %s and upstream: %s", projectId, upstream.Id)
			networkId = val
		} else {
			log.Debug().Msgf("network ID not set via evmChainId (%v) for project: %s and upstream: %s", upstream.Metadata["evmChainId"], projectId, upstream.Id)
		}
	}

	if upstream.Architecture == "" {
		upstream.Architecture = ArchitectureEvm
	}

	// TODO create a Client for upstream and try to "detect" the network ID
	// if networkId == "" {
	// }

	if networkId == "" {
		return nil, fmt.Errorf("could not detect network ID for project: %s and upstream: %s either set it manually (e.g. metadata.evmChainId) to make sure endpoint can provide such info (e.g. support eth_chainId method)", projectId, upstream.Id)
	}

	preparedUpstream := &PreparedUpstream{
		Id:           upstream.Id,
		Architecture: upstream.Architecture,
		Endpoint:     upstream.Endpoint,
		Metadata:     upstream.Metadata,
		NetworkId:    networkId,
	}

	client := u.clientManager.GetOrCreateClient(preparedUpstream)
	preparedUpstream.Client = client

	return preparedUpstream, nil
}
