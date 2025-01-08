package erpc

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/failsafe-go/failsafe-go"
	"github.com/rs/zerolog"
)

type NetworksRegistry struct {
	project              *PreparedProject
	appCtx               context.Context
	upstreamsRegistry    *upstream.UpstreamsRegistry
	metricsTracker       *health.Tracker
	evmJsonRpcCache      *EvmJsonRpcCache
	rateLimitersRegistry *upstream.RateLimitersRegistry
	preparedNetworks     sync.Map // map[string]*Network
	initializer          *util.Initializer
	logger               *zerolog.Logger
}

func NewNetworksRegistry(
	project *PreparedProject,
	appCtx context.Context,
	upstreamsRegistry *upstream.UpstreamsRegistry,
	metricsTracker *health.Tracker,
	evmJsonRpcCache *EvmJsonRpcCache,
	rateLimitersRegistry *upstream.RateLimitersRegistry,
	logger *zerolog.Logger,
) *NetworksRegistry {
	r := &NetworksRegistry{
		project:              project,
		appCtx:               appCtx,
		upstreamsRegistry:    upstreamsRegistry,
		metricsTracker:       metricsTracker,
		evmJsonRpcCache:      evmJsonRpcCache,
		rateLimitersRegistry: rateLimitersRegistry,
		preparedNetworks:     sync.Map{},
		initializer:          util.NewInitializer(logger, nil),
		logger:               logger,
	}
	return r
}

func NewNetwork(
	logger *zerolog.Logger,
	project *PreparedProject,
	nwCfg *common.NetworkConfig,
	rateLimitersRegistry *upstream.RateLimitersRegistry,
	upstreamsRegistry *upstream.UpstreamsRegistry,
	metricsTracker *health.Tracker,
) (*Network, error) {
	lg := logger.With().Str("component", "proxy").Str("networkId", nwCfg.NetworkId()).Logger()

	var policyArray []failsafe.Policy[*common.NormalizedResponse]
	key := fmt.Sprintf("%s/%s", project.Config.Id, nwCfg.NetworkId())
	pls, err := upstream.CreateFailSafePolicies(&lg, common.ScopeNetwork, key, nwCfg.Failsafe)
	if err != nil {
		return nil, err
	}
	for _, policy := range pls {
		policyArray = append(policyArray, policy)
	}
	var timeoutDuration *time.Duration
	if nwCfg.Failsafe != nil && nwCfg.Failsafe.Timeout != nil {
		d, err := time.ParseDuration(nwCfg.Failsafe.Timeout.Duration)
		timeoutDuration = &d
		if err != nil {
			return nil, err
		}
	}

	lg.Debug().Interface("config", nwCfg.Failsafe).Msg("creating network")

	network := &Network{
		ProjectId: project.Config.Id,
		NetworkId: nwCfg.NetworkId(),
		Logger:    &lg,

		cfg: nwCfg,

		upstreamsRegistry:    upstreamsRegistry,
		metricsTracker:       metricsTracker,
		rateLimitersRegistry: rateLimitersRegistry,

		bootstrapOnce:    sync.Once{},
		inFlightRequests: &sync.Map{},
		timeoutDuration:  timeoutDuration,
		failsafeExecutor: failsafe.NewExecutor(policyArray...),
	}

	if nwCfg.Architecture == "" {
		nwCfg.Architecture = common.ArchitectureEvm
	}

	return network, nil
}

func (nr *NetworksRegistry) Bootstrap(ctx context.Context) {
	// Auto register statically-defined networks
	nl := nr.project.Config.Networks
	tasks := []*util.BootstrapTask{}
	for _, nwCfg := range nl {
		tasks = append(tasks, nr.buildNetworkBootstrapTask(nwCfg.NetworkId()))
	}
	err := nr.initializer.ExecuteTasks(ctx, tasks...)
	if err != nil {
		nr.logger.Error().Err(err).Msg("failed to bootstrap networks on first attempt (will keep retrying in the background)")
	}
}

func (nr *NetworksRegistry) GetNetwork(networkId string) (*Network, error) {
	// If network already prepared, return it
	if pn, ok := nr.preparedNetworks.Load(networkId); ok {
		return pn.(*Network), nil
	}

	// Use appCtx because even if current request times out we still want to keep bootstrapping the network
	err := nr.initializer.ExecuteTasks(nr.appCtx, nr.buildNetworkBootstrapTask(networkId))
	if err != nil {
		return nil, err
	}

	// If during first attempt of initialization it fails we must return err so user retries
	ntw, ok := nr.preparedNetworks.Load(networkId)
	if !ok {
		return nil, fmt.Errorf("network %s is not properly initialized yet", networkId)
	}

	return ntw.(*Network), nil
}

func (nr *NetworksRegistry) GetNetworks() []*Network {
	networks := []*Network{}
	nr.preparedNetworks.Range(func(key, value any) bool {
		networks = append(networks, value.(*Network))
		return true
	})
	return networks
}

func (nr *NetworksRegistry) buildNetworkBootstrapTask(networkId string) *util.BootstrapTask {
	return util.NewBootstrapTask(
		fmt.Sprintf("network/%s", networkId),
		func(ctx context.Context) error {
			nr.logger.Debug().Str("networkId", networkId).Msg("attempt to bootstrap network")
			nwCfg, err := nr.resolveNetworkConfig(networkId)
			if err != nil {
				return err
			}
			err = nr.upstreamsRegistry.PrepareUpstreamsForNetwork(ctx, networkId)
			if err != nil {
				return err
			}
			network, err := nr.prepareNetwork(nwCfg)
			if err != nil {
				return err
			}
			nr.preparedNetworks.Store(networkId, network)
			err = network.Bootstrap(ctx)
			if err != nil {
				return err
			}
			nr.logger.Debug().Str("networkId", networkId).Msg("network bootstrap completed")
			return nil
		},
	)
}

func (nr *NetworksRegistry) prepareNetwork(nwCfg *common.NetworkConfig) (*Network, error) {
	if pn, ok := nr.preparedNetworks.Load(nwCfg.NetworkId()); ok {
		return pn.(*Network), nil
	}

	network, err := NewNetwork(
		nr.project.Logger,
		nr.project,
		nwCfg,
		nr.rateLimitersRegistry,
		nr.upstreamsRegistry,
		nr.metricsTracker,
	)
	if err != nil {
		return nil, err
	}

	switch nwCfg.Architecture {
	case "evm":
		if nr.evmJsonRpcCache != nil {
			network.cacheDal = nr.evmJsonRpcCache.WithNetwork(network)
		}
	default:
		return nil, errors.New("unknown network architecture")
	}

	return network, nil
}

func (nr *NetworksRegistry) resolveNetworkConfig(networkId string) (*common.NetworkConfig, error) {
	prj := nr.project

	// Try to find config
	var nwCfg *common.NetworkConfig
	for _, cfg := range prj.Config.Networks {
		if cfg.NetworkId() == networkId {
			nwCfg = cfg
			break
		}
	}
	if nwCfg == nil {
		// Create a new config if none was found
		nwCfg = &common.NetworkConfig{}
		s := strings.Split(networkId, ":")
		if len(s) != 2 {
			return nil, common.NewErrInvalidEvmChainId(networkId)
		}
		nwCfg.Architecture = common.NetworkArchitecture(s[0])
		switch nwCfg.Architecture {
		case common.ArchitectureEvm:
			c, e := strconv.Atoi(s[1])
			if e != nil {
				return nil, e
			}
			nwCfg.Evm = &common.EvmNetworkConfig{ChainId: int64(c)}
		}
		nwCfg.SetDefaults(prj.Config.Upstreams, prj.Config.NetworkDefaults)
		prj.ExposeNetworkConfig(nwCfg)
	}
	return nwCfg, nil
}
