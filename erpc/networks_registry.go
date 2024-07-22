package erpc

import (
	"errors"
	"fmt"
	"sync"

	"github.com/failsafe-go/failsafe-go"
	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/upstream"
	"github.com/rs/zerolog"
)

type NetworksRegistry struct {
	rateLimitersRegistry *upstream.RateLimitersRegistry
	preparedNetworks     map[string]*Network
}

func NewNetworksRegistry(rateLimitersRegistry *upstream.RateLimitersRegistry) *NetworksRegistry {
	r := &NetworksRegistry{
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

		inFlightMutex:        &sync.Mutex{},
		inFlightRequests:     make(map[string]*multiplexedInFlightRequest),
		upstreamsMutex:       &sync.RWMutex{},
		rateLimitersRegistry: rateLimitersRegistry,
		failsafePolicies:     policies,
		failsafeExecutor:     failsafe.NewExecutor(policies...),
		shutdownChan:         make(chan struct{}),
	}

	if nwCfg.Architecture == "" {
		nwCfg.Architecture = common.ArchitectureEvm
	}

	return network, nil
}

func (r *NetworksRegistry) RegisterNetwork(
	logger *zerolog.Logger,
	evmJsonRpcCache *EvmJsonRpcCache,
	prjCfg *common.ProjectConfig,
	nwCfg *common.NetworkConfig,
) (*Network, error) {
	var key = fmt.Sprintf("%s-%s", prjCfg.Id, nwCfg.NetworkId())

	if pn, ok := r.preparedNetworks[key]; ok {
		return pn, nil
	}

	network, err := NewNetwork(logger, prjCfg.Id, nwCfg, r.rateLimitersRegistry)
	if err != nil {
		return nil, err
	}

	switch nwCfg.Architecture {
	case "evm":
		if evmJsonRpcCache != nil {
			network.cacheDal = evmJsonRpcCache.WithNetwork(network)
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

func (nr *NetworksRegistry) Shutdown() error {
	for _, network := range nr.preparedNetworks {
		if err := network.Shutdown(); err != nil {
			return err
		}
	}
	return nil
}
