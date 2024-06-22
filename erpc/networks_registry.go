package erpc

import (
	"errors"
	"fmt"
	"sync"

	"github.com/failsafe-go/failsafe-go"
	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/data"
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

	var policies []failsafe.Policy[common.NormalizedResponse]
	if (nwCfg != nil) && (nwCfg.Failsafe != nil) {
		pls, err := upstream.CreateFailSafePolicies(upstream.ScopeNetwork, key, nwCfg.Failsafe)
		if err != nil {
			return nil, err
		}
		policies = pls
	}

	var rateLimiterDal data.RateLimitersDAL

	r.preparedNetworks[key] = &Network{
		ProjectId:        prjCfg.Id,
		NetworkId:        nwCfg.NetworkId(),
		failsafePolicies: policies,
		Config:           nwCfg,
		Logger:           logger,

		upstreamsMutex:       &sync.RWMutex{},
		rateLimiterDal:       rateLimiterDal,
		rateLimitersRegistry: r.rateLimitersRegistry,
		failsafeExecutor:     failsafe.NewExecutor(policies...),
	}

	if nwCfg.Architecture == "" {
		nwCfg.Architecture = common.ArchitectureEvm
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

func (nr *NetworksRegistry) GetNetwork(projectId, networkId string) *Network {
	return nr.preparedNetworks[fmt.Sprintf("%s-%s", projectId, networkId)]
}
