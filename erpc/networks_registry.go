package erpc

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/architecture/svm"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/consensus"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/internal/policy"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
)

type NetworksRegistry struct {
	project              *PreparedProject
	appCtx               context.Context
	upstreamsRegistry    *upstream.UpstreamsRegistry
	metricsTracker       *health.Tracker
	evmJsonRpcCache      *evm.EvmJsonRpcCache
	svmJsonRpcCache      *svm.SvmJsonRpcCache
	rateLimitersRegistry *upstream.RateLimitersRegistry
	policyEngine         *policy.Engine
	preparedNetworks     sync.Map // map[string]*Network
	aliasToNetworkId     map[string]aliasEntry
	aliasMu              *sync.RWMutex
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
	svmJsonRpcCache *svm.SvmJsonRpcCache,
	rateLimitersRegistry *upstream.RateLimitersRegistry,
	policyEngine *policy.Engine,
	logger *zerolog.Logger,
) *NetworksRegistry {
	lg := logger.With().Str("component", "networksRegistry").Logger()
	r := &NetworksRegistry{
		project:              project,
		appCtx:               appCtx,
		upstreamsRegistry:    upstreamsRegistry,
		metricsTracker:       metricsTracker,
		evmJsonRpcCache:      evmJsonRpcCache,
		svmJsonRpcCache:      svmJsonRpcCache,
		rateLimitersRegistry: rateLimitersRegistry,
		policyEngine:         policyEngine,
		preparedNetworks:     sync.Map{},
		aliasToNetworkId:     map[string]aliasEntry{},
		aliasMu:              &sync.RWMutex{},
		initializer:          util.NewInitializer(appCtx, &lg, nil),
		logger:               logger,
	}
	// Eagerly register aliases from statically defined networks so aliasing works immediately
	if project != nil && project.Config != nil {
		project.cfgMu.RLock()
		for _, nwCfg := range project.Config.Networks {
			if nwCfg != nil && nwCfg.Alias != "" {
				// SplitN limit 2: three-part SVM IDs (svm:<chain>:<cluster>) keep
				// the chain:cluster tail as the chainID half of the alias entry.
				parts := strings.SplitN(nwCfg.NetworkId(), ":", 2)
				if len(parts) == 2 {
					r.registerAlias(nwCfg.Alias, parts[0], parts[1])
				}
			}
		}
		project.cfgMu.RUnlock()
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
	policyEngine *policy.Engine,
) (*Network, error) {
	lg := logger.With().Str("component", "proxy").Str("networkId", nwCfg.NetworkId()).Logger()

	_ = projectId // network executor scope is per-network; project label comes from the metrics tracker.

	// Build a provider that resolves the dynamic block-unavailable retry delay
	// from the network's EMA-estimated block time. Returns 0 before warmup so
	// the static fallback kicks in.
	var dynamicBlockUnavailableDelay func() time.Duration
	if metricsTracker != nil {
		networkId := nwCfg.NetworkId()
		mult := common.DefaultBlockUnavailableDelayMultiplier
		if nwCfg.Evm != nil && nwCfg.Evm.BlockUnavailableDelayMultiplier != nil && *nwCfg.Evm.BlockUnavailableDelayMultiplier > 0 {
			mult = *nwCfg.Evm.BlockUnavailableDelayMultiplier
		}
		dynamicBlockUnavailableDelay = func() time.Duration {
			if bt := metricsTracker.GetNetworkBlockTime(networkId); bt > 0 {
				return time.Duration(float64(bt) * mult)
			}
			return 0
		}
	}

	// Build one networkExecutor per Failsafe config entry, plus a no-op
	// catch-all so unmatched (method, finality) pairs always resolve.
	var failsafeExecutors []*networkExecutor
	if len(nwCfg.Failsafe) > 0 {
		for _, fsCfg := range nwCfg.Failsafe {
			var cons consensusRunner
			if fsCfg.Consensus != nil {
				c, err := consensus.NewConsensus(fsCfg.Consensus, &lg)
				if err != nil {
					return nil, err
				}
				cons = c
			}
			ex, err := NewNetworkExecutor(fsCfg, &lg, cons, dynamicBlockUnavailableDelay)
			if err != nil {
				return nil, err
			}
			failsafeExecutors = append(failsafeExecutors, ex)
		}
	}

	// Catch-all no-op executor.
	noop, _ := NewNetworkExecutor(nil, &lg, nil, dynamicBlockUnavailableDelay)
	failsafeExecutors = append(failsafeExecutors, noop)

	lg.Debug().Interface("config", nwCfg.Failsafe).Msgf("created %d failsafe executors", len(failsafeExecutors))

	// Pre-compute a stable label for network: prefer alias if set, else use networkId
	netId := nwCfg.NetworkId()
	// Keep label as alias if present, else empty. Empty will render as "n/a" via Network.Label().
	var netLabel string
	if a := nwCfg.Alias; a != "" {
		netLabel = a
	}

	network := &Network{
		cfg:          nwCfg,
		logger:       &lg,
		projectId:    projectId,
		networkId:    netId,
		networkLabel: netLabel,

		appCtx:               appCtx,
		upstreamsRegistry:    upstreamsRegistry,
		metricsTracker:       metricsTracker,
		rateLimitersRegistry: rateLimitersRegistry,
		policyEngine:         policyEngine,

		bootstrapOnce:     sync.Once{},
		inFlightRequests:  &sync.Map{},
		failsafeExecutors: failsafeExecutors,
		initializer:       util.NewInitializer(appCtx, &lg, nil),
	}

	if nwCfg.Architecture == "" {
		nwCfg.Architecture = common.ArchitectureEvm
	}

	// Wire the architecture handler so per-network hooks dispatch correctly.
	// prepareNetwork also sets this; keeping it here means tests that construct
	// networks via NewNetwork directly get the same behavior as production.
	if handler, err := common.GetArchitectureHandler(nwCfg.Architecture); err == nil {
		network.architectureHandler = handler
	}

	return network, nil
}

func (nr *NetworksRegistry) Bootstrap(appCtx context.Context) {
	// Auto register statically-defined networks in background (non-blocking)
	nr.project.cfgMu.RLock()
	nl := nr.project.Config.Networks
	nr.project.cfgMu.RUnlock()

	tasks := []*util.BootstrapTask{}
	for _, nwCfg := range nl {
		tasks = append(tasks, nr.buildNetworkBootstrapTask(nwCfg.NetworkId()))
	}
	go func() {
		if err := nr.initializer.ExecuteTasks(appCtx, tasks...); err != nil {
			nr.logger.Error().Err(err).Interface("status", nr.initializer.Status()).Msg("failed to bootstrap networks in background")
		} else {
			nr.logger.Info().Interface("status", nr.initializer.Status()).Msg("networks bootstrap completed")
		}
	}()
}

func (nr *NetworksRegistry) GetNetwork(ctx context.Context, networkId string) (*Network, error) {
	// If network already prepared, return it
	if pn, ok := nr.preparedNetworks.Load(networkId); ok {
		return pn.(*Network), nil
	}

	if !util.IsValidNetworkId(networkId) {
		return nil, common.NewErrInvalidRequest(fmt.Errorf("invalid network id format: '%s' either use a network alias (/main/arbitrum) or a valid network id (/main/evm/42161)", networkId))
	}

	// Schedule tasks and wait using the caller's context; tasks run on appCtx internally
	if err := nr.initializer.ExecuteTasks(ctx, nr.buildNetworkBootstrapTask(networkId)); err != nil {
		return nil, err
	}

	// If during first attempt of initialization it fails we must return err so user retries
	ntw, ok := nr.preparedNetworks.Load(networkId)
	if !ok {
		return nil, common.NewErrNetworkNotFound(networkId)
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
	nr.aliasMu.RLock()
	entry, ok := nr.aliasToNetworkId[alias]
	nr.aliasMu.RUnlock()
	if ok {
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
				// Network config resolution failed definitively (e.g., invalid format)
				return common.NewTaskFatal(err)
			}
			// passing task ctx here will cancel the task if the request is finished or the initializer is cancelled/times out
			err = nr.upstreamsRegistry.PrepareUpstreamsForNetwork(ctx, networkId)
			ups := nr.upstreamsRegistry.GetNetworkUpstreams(nr.appCtx, networkId)
			if len(ups) == 0 {
				// Warn, not Error: a network resolving to zero upstreams is
				// usually a client requesting an unsupported/lazy network (e.g.
				// an unknown chain id), which is expected and self-resolving —
				// not an operator-actionable error. The initializer's per-task
				// retry backoff (request path) already throttles how often this
				// bootstrap re-runs, so this no longer fires once per request.
				nr.logger.Warn().
					Str("projectId", nr.project.Config.Id).
					Str("networkId", networkId).
					Err(err).
					Msg("network initialization ended with zero upstreams")
			}
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
		nr.policyEngine,
	)
	if err != nil {
		return nil, err
	}

	handler, err := common.GetArchitectureHandler(nwCfg.Architecture)
	if err != nil {
		return nil, fmt.Errorf("unsupported network architecture %q: %w", nwCfg.Architecture, err)
	}
	network.architectureHandler = handler

	// Architecture-specific cache wiring. Each architecture has its own cache
	// implementation because key partitioning differs (EVM uses blockRef, SVM
	// uses commitment+slotRef). A given network gets exactly one cache.
	switch nwCfg.Architecture {
	case common.ArchitectureEvm:
		if nr.evmJsonRpcCache != nil {
			network.cacheDal = nr.evmJsonRpcCache.WithProjectId(nr.project.Config.Id)
		}
	case common.ArchitectureSvm:
		if nr.svmJsonRpcCache != nil {
			network.cacheDal = nr.svmJsonRpcCache.WithProjectId(nr.project.Config.Id)
		}
	}
	// Register alias for lazy-created networks to support alias-based routing
	if nwCfg.Alias != "" {
		// SplitN limit 2: three-part SVM IDs (svm:<chain>:<cluster>) keep the
		// chain:cluster tail as the chainID half of the alias entry.
		parts := strings.SplitN(nwCfg.NetworkId(), ":", 2)
		if len(parts) == 2 {
			nr.registerAlias(nwCfg.Alias, parts[0], parts[1])
		}
	}
	return network, nil
}

// registerAlias safely registers an alias -> (architecture, chainID) mapping.
// If the alias already exists with a different target, it logs and keeps the existing one.
func (nr *NetworksRegistry) registerAlias(alias, arch, chain string) {
	if alias == "" || arch == "" || chain == "" {
		return
	}
	nr.aliasMu.Lock()
	defer nr.aliasMu.Unlock()
	if existing, ok := nr.aliasToNetworkId[alias]; ok {
		if existing.architecture != arch || existing.chainID != chain {
			nr.logger.Warn().Str("alias", alias).Str("existing", existing.architecture+":"+existing.chainID).Str("new", arch+":"+chain).Msg("skipping duplicate alias registration with different target")
		}
		return
	}
	nr.aliasToNetworkId[alias] = aliasEntry{architecture: arch, chainID: chain}
	nr.logger.Debug().Str("alias", alias).Str("networkId", arch+":"+chain).Msg("registered network alias")
}

func (nr *NetworksRegistry) resolveNetworkConfig(networkId string) (*common.NetworkConfig, error) {
	prj := nr.project

	nwCfg := prj.FindNetworkConfig(networkId)
	if nwCfg == nil {
		// Create a new config if none was found
		nwCfg = &common.NetworkConfig{}
		// SplitN limit 2: the tail keeps any further colons so SVM
		// svm:<chain>:<cluster> IDs are parsed below (s[1] = "<chain>:<cluster>").
		s := strings.SplitN(networkId, ":", 2)
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
		case common.ArchitectureSvm:
			// s[1] is "<cluster>" (implicit solana) or "<chain>:<cluster>".
			chain, cluster := "", s[1]
			if i := strings.Index(s[1], ":"); i >= 0 {
				chain, cluster = s[1][:i], s[1][i+1:]
			}
			if cluster == "" {
				return nil, common.NewErrInvalidEvmChainId(networkId)
			}
			nwCfg.Svm = &common.SvmNetworkConfig{Chain: chain, Cluster: cluster}
		default:
			return nil, common.NewErrInvalidEvmChainId(networkId)
		}
		if err := nwCfg.SetDefaults(prj.Config.Upstreams, prj.Config.NetworkDefaults); err != nil {
			return nil, fmt.Errorf("failed to set defaults for network config: %w", err)
		}
		prj.ExposeNetworkConfig(nwCfg)
	}
	return nwCfg, nil
}
