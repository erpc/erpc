package upstream

import (
	"fmt"
	"time"

	"github.com/flair-sdk/erpc/config"
	"github.com/rs/zerolog/log"
)

type PreparedUpstream struct {
	Id       string
	Endpoint string
	Metadata map[string]string
	NetworkId string
}

type UpstreamOrchestrator struct {
	config       *config.Config
	upstreamsMap map[string]map[string][]string
}

func NewUpstreamOrchestrator(cfg *config.Config) *UpstreamOrchestrator {
	return &UpstreamOrchestrator{
		config: cfg,
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
	u.upstreamsMap = make(map[string]map[string][]string)
	for _, project := range u.config.Projects {
		log.Info().Msgf("Loading upstreams for project: %v", project)
		if _, ok := u.upstreamsMap[project.Id]; !ok {
			u.upstreamsMap[project.Id] = make(map[string][]string)
		}

		for _, upstream := range project.Upstreams {
			u.upstreamsMap[project.Id][upstream.Id] = []string{upstream.Endpoint}
		}
	}
}

// Function to find the best upstream for a project/network
func (u *UpstreamOrchestrator) FindBestUpstream(project string, network string) (string, error) {
	if _, ok := u.upstreamsMap[project]; !ok {
		return "", fmt.Errorf("project %s not found", project)
	}

	if _, ok := u.upstreamsMap[project][network]; !ok {
		return "", fmt.Errorf("network %s not found for project %s", network, project)
	}

	if len(u.upstreamsMap[project][network]) == 0 {
		return "", fmt.Errorf("no upstreams found for project %s and network %s", project, network)
	}

	// For now let's just return the first upstream
	return u.upstreamsMap[project][network][0], nil
}

// Proactively update the health information of upstreams of a project/network and reorder them so the highest performing upstreams are at the top
func (u *UpstreamOrchestrator) RefreshNetworkUpstreamsHealth(project string, network string) {
	// For now let's randomly reorder the upstreams:
	// Get the upstreams for the project/network
	upstreams := u.upstreamsMap[project][network]

	// Randomly reorder the upstreams
	var reorderedUpstreams []string
	for i := len(upstreams) - 1; i >= 0; i-- {
		reorderedUpstreams = append(reorderedUpstreams, upstreams[i])
	}

	// Update the upstreams for the project/network
	u.upstreamsMap[project][network] = reorderedUpstreams

	// Log the reordering
	log.Info().Msgf("Reordered upstreams for project: %s and network: %s", project, network)
}
