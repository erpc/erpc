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
	aliasToNetworkId     map[string]aliasEntry
	initializer          *util.Initializer
	logger               *zerolog.Logger
}

type aliasEntry struct {
	architecture string
	chainID      string
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
		aliasToNetworkId:     map[string]aliasEntry{},
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

	// Create failsafe executors from configs
	var failsafeExecutors []*FailsafeExecutor
	if len(nwCfg.Failsafe) > 0 {
		for _, fsCfg := range nwCfg.Failsafe {
			pls, err := upstream.CreateFailSafePolicies(&lg, common.ScopeNetwork, key, fsCfg)
			if err != nil {
				return nil, err
			}
			policyArray := upstream.ToPolicyArray(pls, "timeout", "consensus", "hedge", "retry")

			var timeoutDuration *time.Duration
			if fsCfg.Timeout != nil {
				timeoutDuration = fsCfg.Timeout.Duration.DurationPtr()
			}

			method := fsCfg.MatchMethod
			if method == "" {
				method = "*"
			}
			failsafeExecutors = append(failsafeExecutors, &FailsafeExecutor{
				method:     method,
				finalities: fsCfg.MatchFinality,
				executor:   failsafe.NewExecutor(policyArray...),
				timeout:    timeoutDuration,
			})
		}
	}

	// Create a default executor if no failsafe config is provided or matched
	failsafeExecutors = append(failsafeExecutors, &FailsafeExecutor{
		method:     "*", // "*" means match any method
		finalities: nil, // nil means match any finality
		executor:   failsafe.NewExecutor[*common.NormalizedResponse](),
		timeout:    nil,
	})

	lg.Debug().Interface("config", nwCfg.Failsafe).Msgf("created %d failsafe executors", len(failsafeExecutors))

	network := &Network{
		cfg:       nwCfg,
		logger:    &lg,
		projectId: projectId,
		networkId: nwCfg.NetworkId(),

		appCtx:               appCtx,
		upstreamsRegistry:    upstreamsRegistry,
		metricsTracker:       metricsTracker,
		rateLimitersRegistry: rateLimitersRegistry,

		bootstrapOnce:     sync.Once{},
		inFlightRequests:  &sync.Map{},
		failsafeExecutors: failsafeExecutors,
		initializer:       util.NewInitializer(appCtx, &lg, nil),
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

	// Populate alias map for statically defined networks
	for _, nwCfg := range nr.project.Config.Networks {
		if nwCfg.Alias != "" {
			parts := strings.Split(nwCfg.NetworkId(), ":")
			if len(parts) == 2 {
				if _, ok := nr.aliasToNetworkId[nwCfg.Alias]; ok {
					return fmt.Errorf("alias %s already registered for network %s", nwCfg.Alias, nwCfg.NetworkId())
				}
				nr.aliasToNetworkId[nwCfg.Alias] = aliasEntry{
					architecture: parts[0],
					chainID:      parts[1],
				}
				nr.logger.Debug().Str("alias", nwCfg.Alias).Str("networkId", nwCfg.NetworkId()).Msg("registered network alias")
			}
		}
	}
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

	if !util.IsValidNetworkId(networkId) {
		return nil, fmt.Errorf("invalid network id format: '%s' either use a network alias (/main/arbitrum) or a valid network id (/main/evm/42161)", networkId)
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

func (nr *NetworksRegistry) ResolveAlias(alias string) (string, string) {
	if entry, ok := nr.aliasToNetworkId[alias]; ok {
		return entry.architecture, entry.chainID
	}
	return "", ""
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
			// passing task ctx here will cancel the task if the request is finished or the initializer is cancelled/times out
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
