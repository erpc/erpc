package erpc

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/erpc/erpc/architecture/evm"
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
	evmJsonRpcCache      *evm.EvmJsonRpcCache
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
	evmJsonRpcCache *evm.EvmJsonRpcCache,
	rateLimitersRegistry *upstream.RateLimitersRegistry,
	logger *zerolog.Logger,
) *NetworksRegistry {
	lg := logger.With().Str("component", "networksRegistry").Logger()
	r := &NetworksRegistry{
		project:              project,
		appCtx:               appCtx,
		upstreamsRegistry:    upstreamsRegistry,
		metricsTracker:       metricsTracker,
		evmJsonRpcCache:      evmJsonRpcCache,
		rateLimitersRegistry: rateLimitersRegistry,
		preparedNetworks:     sync.Map{},
		initializer:          util.NewInitializer(appCtx, &lg, nil),
		logger:               logger,
	}
	return r
}

func NewNetwork(
	appCtx context.Context,
	logger *zerolog.Logger,
	projectId string,
	nwCfg *common.NetworkConfig,
	rateLimitersRegistry *upstream.RateLimitersRegistry,
	upstreamsRegistry *upstream.UpstreamsRegistry,
	metricsTracker *health.Tracker,
) (*Network, error) {
	lg := logger.With().Str("component", "proxy").Str("networkId", nwCfg.NetworkId()).Logger()

	key := fmt.Sprintf("%s/%s", projectId, nwCfg.NetworkId())
	pls, err := upstream.CreateFailSafePolicies(&lg, common.ScopeNetwork, key, nwCfg.Failsafe)
	if err != nil {
		return nil, err
	}
	policyArray := upstream.ToPolicyArray(pls, "timeout", "retry", "hedge", "consensus")
	var timeoutDuration *time.Duration
	if nwCfg.Failsafe != nil && nwCfg.Failsafe.Timeout != nil {
		timeoutDuration = nwCfg.Failsafe.Timeout.Duration.DurationPtr()
	}
	lg.Debug().Interface("config", nwCfg.Failsafe).Msg("creating network")

	network := &Network{
		cfg:       nwCfg,
		logger:    &lg,
		projectId: projectId,
		networkId: nwCfg.NetworkId(),

		appCtx:               appCtx,
		upstreamsRegistry:    upstreamsRegistry,
		metricsTracker:       metricsTracker,
		rateLimitersRegistry: rateLimitersRegistry,

		bootstrapOnce:    sync.Once{},
		inFlightRequests: &sync.Map{},
		timeoutDuration:  timeoutDuration,
		failsafeExecutor: failsafe.NewExecutor(policyArray...),
		initializer:      util.NewInitializer(appCtx, &lg, nil),
	}

	if nwCfg.Architecture == "" {
		nwCfg.Architecture = common.ArchitectureEvm
	}

	return network, nil
}

func (nr *NetworksRegistry) Bootstrap(appCtx context.Context) error {
	// Auto register statically-defined networks
	nr.project.cfgMu.RLock()
	defer nr.project.cfgMu.RUnlock()

	nl := nr.project.Config.Networks
	tasks := []*util.BootstrapTask{}
	for _, nwCfg := range nl {
		tasks = append(tasks, nr.buildNetworkBootstrapTask(nwCfg.NetworkId()))
	}
	err := nr.initializer.ExecuteTasks(appCtx, tasks...)
	if err != nil {
		return err
	}
	return nil
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

	// Check if upstreams are available for this network
	network := ntw.(*Network)
	upstreams := nr.upstreamsRegistry.GetNetworkUpstreams(networkId)
	if len(upstreams) == 0 {
		// Instead of returning an error, wait a short time for upstreams to initialize
		// This helps with race conditions where the network is created but upstreams aren't ready yet
		ctx, cancel := context.WithTimeout(context.Background(), 1000*time.Millisecond)
		defer cancel()

		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				// If we timeout waiting, return the network anyway - subsequent requests will likely succeed
				nr.logger.Warn().Str("networkId", networkId).Msg("returning network with no upstreams yet, subsequent requests may succeed")
				return network, nil
			case <-ticker.C:
				upstreams = nr.upstreamsRegistry.GetNetworkUpstreams(networkId)
				if len(upstreams) > 0 {
					return network, nil
				}
			}
		}
	}

	return network, nil
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
			// The reason we pass appCtx here is because we want to bootstrap the network even if the current request times out.
			// Another edge-case is when network is partially initialized (i.e. 20% of upstreams are initialized) we want
			// the request to continue without waiting too much, still keep upstreams initializing in the background,
			// if we pass the task's "ctx" it will be cancelled when the request is finished.
			err = nr.upstreamsRegistry.PrepareUpstreamsForNetwork(nr.appCtx, networkId)
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
		nr.appCtx,
		nr.logger,
		nr.project.Config.Id,
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
			network.cacheDal = nr.evmJsonRpcCache.WithProjectId(nr.project.Config.Id)
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
		if err := nwCfg.SetDefaults(prj.Config.Upstreams, prj.Config.NetworkDefaults); err != nil {
			return nil, fmt.Errorf("failed to set defaults for network config: %w", err)
		}
		prj.ExposeNetworkConfig(nwCfg)
	}
	return nwCfg, nil
}
