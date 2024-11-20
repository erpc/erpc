package erpc

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/erpc/erpc/auth"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/upstream"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
)

type PreparedProject struct {
	Config   *common.ProjectConfig
	Networks map[string]*Network
	Logger   *zerolog.Logger

	appCtx               context.Context
	projectMu            *sync.RWMutex
	networkInitializers  *sync.Map
	networksRegistry     *NetworksRegistry
	consumerAuthRegistry *auth.AuthRegistry
	rateLimitersRegistry *upstream.RateLimitersRegistry
	upstreamsRegistry    *upstream.UpstreamsRegistry
	evmJsonRpcCache      *EvmJsonRpcCache
}

type initOnce struct {
	once sync.Once
	err  error
}

func (p *PreparedProject) Bootstrap(ctx context.Context) error {
	p.Logger.Debug().Msgf("initializing all staticly-defined networks")

	go func() {
		for _, nwCfg := range p.Config.Networks {
			_, err := p.initializeNetwork(ctx, nwCfg.NetworkId())
			if err != nil {
				p.Logger.Error().Err(err).Msgf("failed to initialize network %s", nwCfg.NetworkId())
			}
		}
	}()

	return nil
}

func (p *PreparedProject) GetNetwork(ctx context.Context, networkId string) (*Network, error) {
	p.projectMu.RLock()
	network, ok := p.Networks[networkId]
	p.projectMu.RUnlock()
	if ok {
		return network, nil
	}

	value, _ := p.networkInitializers.LoadOrStore(networkId, &initOnce{})
	initializer := value.(*initOnce)

	initializer.once.Do(func() {
		var err error
		network, err := p.initializeNetwork(ctx, networkId)
		if err != nil {
			initializer.err = err
			return
		}

		p.projectMu.Lock()
		p.Networks[networkId] = network
		p.projectMu.Unlock()
	})

	if initializer.err != nil {
		return nil, initializer.err
	}

	p.projectMu.RLock()
	network = p.Networks[networkId]
	p.projectMu.RUnlock()

	return network, nil
}

func (p *PreparedProject) GatherHealthInfo() (*upstream.UpstreamsHealth, error) {
	return p.upstreamsRegistry.GetUpstreamsHealth()
}

func (p *PreparedProject) AuthenticateConsumer(ctx context.Context, nq *common.NormalizedRequest, ap *auth.AuthPayload) error {
	if p.consumerAuthRegistry != nil {
		err := p.consumerAuthRegistry.Authenticate(ctx, nq, ap)
		if err != nil {
			return err
		}
	}

	return nil
}

func (p *PreparedProject) Forward(ctx context.Context, networkId string, nq *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	// We use app context here so that network lazy loading runs within app context not request, as we want it to continue even if current request is cancelled/timed out
	network, err := p.GetNetwork(p.appCtx, networkId)
	if err != nil {
		return nil, err
	}
	if err := p.acquireRateLimitPermit(nq); err != nil {
		return nil, err
	}

	method, _ := nq.Method()

	timer := prometheus.NewTimer(prometheus.ObserverFunc(func(v float64) {
		health.MetricNetworkRequestDuration.WithLabelValues(
			p.Config.Id,
			network.NetworkId,
			method,
		).Observe(v)
	}))
	defer timer.ObserveDuration()

	health.MetricNetworkRequestsReceived.WithLabelValues(p.Config.Id, network.NetworkId, method).Inc()
	lg := p.Logger.With().
		Str("component", "proxy").
		Str("projectId", p.Config.Id).
		Str("networkId", network.NetworkId).
		Str("method", method).
		Interface("id", nq.ID()).
		Str("ptr", fmt.Sprintf("%p", nq)).
		Logger()

	if lg.GetLevel() == zerolog.TraceLevel {
		lg.Debug().Object("request", nq).Msgf("forwarding request for network")
	} else {
		lg.Debug().Msgf("forwarding request for network")
	}
	resp, err := network.Forward(ctx, nq)

	if err == nil || common.HasErrorCode(err, common.ErrCodeEndpointClientSideException) {
		if err != nil {
			lg.Info().Err(err).Msgf("finished forwarding request for network with some client-side exception")
		} else {
			if lg.GetLevel() == zerolog.TraceLevel {
				lg.Info().Err(err).Object("response", resp).Msgf("successfully forwarded request for network")
			} else {
				lg.Info().Msgf("successfully forwarded request for network")
			}
		}
		health.MetricNetworkSuccessfulRequests.WithLabelValues(p.Config.Id, network.NetworkId, method).Inc()
		return resp, err
	} else {
		health.MetricNetworkFailedRequests.WithLabelValues(network.ProjectId, network.NetworkId, method, common.ErrorSummary(err)).Inc()
	}

	return nil, err
}

func (p *PreparedProject) initializeNetwork(ctx context.Context, networkId string) (*Network, error) {
	p.Logger.Info().Str("networkId", networkId).Msgf("initializing network")

	// 1) Find all upstreams that support this network
	err := p.upstreamsRegistry.PrepareUpstreamsForNetwork(ctx, networkId)
	if err != nil {
		return nil, err
	}

	// 2) Find if any network configs defined on project-level
	var nwCfg *common.NetworkConfig
	for _, n := range p.Config.Networks {
		if n.NetworkId() == networkId {
			nwCfg = n
			break
		}
	}

	if nwCfg == nil {
		nwCfg = common.NewDefaultNetworkConfig(p.Config.Upstreams)

		s := strings.Split(networkId, ":")
		if len(s) != 2 {
			// TODO use more appropriate error for non-evm
			return nil, common.NewErrInvalidEvmChainId(networkId)
		}
		nwCfg.Architecture = common.NetworkArchitecture(s[0])
		switch nwCfg.Architecture {
		case common.ArchitectureEvm:
			c, e := strconv.Atoi(s[1])
			if e != nil {
				return nil, e
			}
			nwCfg.Evm = &common.EvmNetworkConfig{
				ChainId: int64(c),
			}
		}
		p.projectMu.Lock()
		if p.Config.Networks == nil || len(p.Config.Networks) == 0 {
			p.Config.Networks = []*common.NetworkConfig{}
		}
		p.Config.Networks = append(p.Config.Networks, nwCfg)
		p.projectMu.Unlock()
	}

	// 3) Register and prepare the network in registry
	nw, err := p.networksRegistry.RegisterNetwork(
		p.Logger,
		p.Config,
		nwCfg,
	)
	if err != nil {
		return nil, err
	}

	err = nw.Bootstrap(ctx)
	if err != nil {
		return nil, err
	}

	return nw, nil
}

func (p *PreparedProject) acquireRateLimitPermit(req *common.NormalizedRequest) error {
	if p.Config.RateLimitBudget == "" {
		return nil
	}

	rlb, errNetLimit := p.rateLimitersRegistry.GetBudget(p.Config.RateLimitBudget)
	if errNetLimit != nil {
		return errNetLimit
	}
	if rlb == nil {
		return nil
	}

	method, errMethod := req.Method()
	if errMethod != nil {
		return errMethod
	}
	lg := p.Logger.With().Str("method", method).Logger()

	rules, errRules := rlb.GetRulesByMethod(method)
	if errRules != nil {
		return errRules
	}
	lg.Debug().Msgf("found %d network-level rate limiters", len(rules))

	if len(rules) > 0 {
		for _, rule := range rules {
			permit := rule.Limiter.TryAcquirePermit()
			if !permit {
				health.MetricProjectRequestSelfRateLimited.WithLabelValues(
					p.Config.Id,
					method,
				).Inc()
				return common.NewErrProjectRateLimitRuleExceeded(
					p.Config.Id,
					p.Config.RateLimitBudget,
					fmt.Sprintf("%+v", rule.Config),
				)
			} else {
				lg.Debug().Object("rateLimitRule", rule.Config).Msgf("project-level rate limit passed")
			}
		}
	}

	return nil
}
