package erpc

import (
	"errors"
	"fmt"
	"sync"

	"github.com/failsafe-go/failsafe-go"
	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/health"
	"github.com/flair-sdk/erpc/upstream"
	"github.com/rs/zerolog"
)

type NetworksRegistry struct {
	upstreamsRegistry    *upstream.UpstreamsRegistry
	metricsTracker       *health.Tracker
	evmJsonRpcCache      *EvmJsonRpcCache
	rateLimitersRegistry *upstream.RateLimitersRegistry
	preparedNetworks     map[string]*Network
}

func NewNetworksRegistry(
	upstreamsRegistry *upstream.UpstreamsRegistry,
	metricsTracker *health.Tracker,
	evmJsonRpcCache *EvmJsonRpcCache,
	rateLimitersRegistry *upstream.RateLimitersRegistry,
) *NetworksRegistry {
	r := &NetworksRegistry{
		upstreamsRegistry:    upstreamsRegistry,
		metricsTracker:       metricsTracker,
		evmJsonRpcCache:      evmJsonRpcCache,
		rateLimitersRegistry: rateLimitersRegistry,
		preparedNetworks:     make(map[string]*Network),
	}
	return r
}

func NewNetwork(
	logger *zerolog.Logger,
	prjId string,
	nwCfg *common.NetworkConfig,
	rateLimitersRegistry *upstream.RateLimitersRegistry,
	upstreamsRegistry *upstream.UpstreamsRegistry,
	metricsTracker *health.Tracker,
) (*Network, error) {
	var policies []failsafe.Policy[common.NormalizedResponse]
	if (nwCfg != nil) && (nwCfg.Failsafe != nil) {
		key := fmt.Sprintf("%s-%s", prjId, nwCfg.NetworkId())
		pls, err := upstream.CreateFailSafePolicies(upstream.ScopeNetwork, key, nwCfg.Failsafe)
		if err != nil {
			return nil, err
		}
		policies = pls
	}

	lg := logger.With().Str("network", nwCfg.NetworkId()).Logger()
	network := &Network{
		ProjectId: prjId,
		NetworkId: nwCfg.NetworkId(),
		Config:    nwCfg,
		Logger:    &lg,

		upstreamsRegistry:    upstreamsRegistry,
		metricsTracker:       metricsTracker,
		rateLimitersRegistry: rateLimitersRegistry,

		inFlightMutex:    &sync.Mutex{},
		inFlightRequests: make(map[string]*Multiplexer),
		failsafePolicies: policies,
		failsafeExecutor: failsafe.NewExecutor(policies...),
	}

	if nwCfg.Architecture == "" {
		nwCfg.Architecture = common.ArchitectureEvm
	}

	return network, nil
}

func (r *NetworksRegistry) RegisterNetwork(
	logger *zerolog.Logger,
	prjCfg *common.ProjectConfig,
	nwCfg *common.NetworkConfig,
) (*Network, error) {
	var key = fmt.Sprintf("%s-%s", prjCfg.Id, nwCfg.NetworkId())

	if pn, ok := r.preparedNetworks[key]; ok {
		return pn, nil
	}

	network, err := NewNetwork(logger, prjCfg.Id, nwCfg, r.rateLimitersRegistry, r.upstreamsRegistry, r.metricsTracker)
	if err != nil {
		return nil, err
	}

	switch nwCfg.Architecture {
	case "evm":
		if r.evmJsonRpcCache != nil {
			network.cacheDal = r.evmJsonRpcCache.WithNetwork(network)
		}
	default:
		return nil, errors.New("unknown network architecture")
	}

	r.preparedNetworks[key] = network
	return network, nil
}

func (nr *NetworksRegistry) GetNetwork(projectId, networkId string) *Network {
	return nr.preparedNetworks[fmt.Sprintf("%s-%s", projectId, networkId)]
}
