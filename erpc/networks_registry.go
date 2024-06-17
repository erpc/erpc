package erpc

import (
	"errors"
	"fmt"
	"sync"

	"github.com/failsafe-go/failsafe-go"
	"github.com/flair-sdk/erpc/config"
	"github.com/flair-sdk/erpc/data"
	"github.com/flair-sdk/erpc/upstream"
	"github.com/rs/zerolog"
)

type NetworksRegistry struct {
	rateLimitersRegistry *upstream.RateLimitersRegistry
	preparedNetworks     map[string]*PreparedNetwork
}

func NewNetworksRegistry(rateLimitersRegistry *upstream.RateLimitersRegistry) *NetworksRegistry {
	r := &NetworksRegistry{
		rateLimitersRegistry: rateLimitersRegistry,
		preparedNetworks:     make(map[string]*PreparedNetwork),
	}
	return r
}

func (r *NetworksRegistry) RegisterNetwork(
	logger *zerolog.Logger,
	evmJsonRpcCache *EvmJsonRpcCache,
	prjCfg *config.ProjectConfig,
	nwCfg *config.NetworkConfig,
) (*PreparedNetwork, error) {
	var key = fmt.Sprintf("%s-%s", prjCfg.Id, nwCfg.NetworkId())

	if pn, ok := r.preparedNetworks[key]; ok {
		return pn, nil
	}

	var policies []failsafe.Policy[*upstream.NormalizedResponse]
	if (nwCfg != nil) && (nwCfg.Failsafe != nil) {
		pls, err := upstream.CreateFailSafePolicies(upstream.ScopeNetwork, key, nwCfg.Failsafe)
		if err != nil {
			return nil, err
		}
		policies = pls
	}

	var rateLimiterDal data.RateLimitersDAL

	r.preparedNetworks[key] = &PreparedNetwork{
		ProjectId:        prjCfg.Id,
		NetworkId:        nwCfg.NetworkId(),
		FailsafePolicies: policies,
		Config:           nwCfg,
		Logger:           logger,

		upstreamsMutex:       &sync.RWMutex{},
		rateLimiterDal:       rateLimiterDal,
		rateLimitersRegistry: r.rateLimitersRegistry,
		failsafeExecutor:     failsafe.NewExecutor(policies...),
	}

	if nwCfg.Architecture == "" {
		nwCfg.Architecture = upstream.ArchitectureEvm
	}

	switch nwCfg.Architecture {
	case "evm":
		if evmJsonRpcCache != nil {
			r.preparedNetworks[key].cacheDal = evmJsonRpcCache.WithNetwork(r.preparedNetworks[key])
		}
	default:
		return nil, errors.New("unknown network architecture")
	}

	return r.preparedNetworks[key], nil
}

func (nr *NetworksRegistry) GetNetwork(projectId, networkId string) *PreparedNetwork {
	return nr.preparedNetworks[fmt.Sprintf("%s-%s", projectId, networkId)]
}
