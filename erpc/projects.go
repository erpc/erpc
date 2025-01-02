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
	"github.com/erpc/erpc/util"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
)

type PreparedProject struct {
	Config   *common.ProjectConfig
	Networks map[string]*Network
	Logger   *zerolog.Logger

	appCtx               context.Context
	initializer          *util.Initializer
	projectMu            *sync.RWMutex
	networkInitializers  *sync.Map
	networksRegistry     *NetworksRegistry
	consumerAuthRegistry *auth.AuthRegistry
	rateLimitersRegistry *upstream.RateLimitersRegistry
	upstreamsRegistry    *upstream.UpstreamsRegistry
	evmJsonRpcCache      *EvmJsonRpcCache
}

type ProjectHealthInfo struct {
	upstream.UpstreamsHealth
	Initialization *util.InitializerStatus `json:"initialization,omitempty"`
}

func (p *PreparedProject) Bootstrap(ctx context.Context) error {
	p.projectMu.Lock()

	if p.initializer == nil {
		p.initializer = util.NewInitializer(
			p.Logger,
			nil,
		)
		nl := p.Config.Networks
		tasks := []*util.BootstrapTask{}
		for _, nwCfg := range nl {
			tasks = append(tasks, p.createNetworkBootstrapTask(nwCfg.NetworkId()))
		}
		p.projectMu.Unlock()
		return p.initializer.ExecuteTasks(ctx, tasks...)
	}
	p.projectMu.Unlock()

	return p.initializer.WaitForTasks(ctx)
}

func (p *PreparedProject) createNetworkBootstrapTask(networkId string) *util.BootstrapTask {
	return util.NewBootstrapTask(
		fmt.Sprintf("network/%s", networkId),
		func(ctx context.Context) error {
			return p.bootstrapNetwork(ctx, networkId)
		},
	)
}

func (p *PreparedProject) GetNetwork(ctx context.Context, networkId string) (*Network, error) {
	p.projectMu.RLock()
	network, ok := p.Networks[networkId]
	p.projectMu.RUnlock()
	if ok {
		return network, nil
	}

	err := p.initializer.ExecuteTasks(ctx, p.createNetworkBootstrapTask(networkId))
	if err != nil {
		return nil, err
	}

	p.projectMu.RLock()
	network, ok = p.Networks[networkId]
	p.projectMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("network %s is not properly initialized", networkId)
	}

	return network, nil
}

func (p *PreparedProject) GatherHealthInfo() (*ProjectHealthInfo, error) {
	upstreamsHealth, err := p.upstreamsRegistry.GetUpstreamsHealth()
	if err != nil {
		return nil, err
	}
	return &ProjectHealthInfo{
		UpstreamsHealth: *upstreamsHealth,
		Initialization:  p.initializer.Status(),
	}, nil
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
				lg.Info().Object("response", resp).Msgf("successfully forwarded request for network")
			} else {
				lg.Info().Msgf("successfully forwarded request for network")
			}
		}
		health.MetricNetworkSuccessfulRequests.WithLabelValues(p.Config.Id, network.NetworkId, method, strconv.Itoa(resp.Attempts())).Inc()
		return resp, err
	} else {
		lg.Debug().Err(err).Object("request", nq).Msgf("failed to forward request for network")
		health.MetricNetworkFailedRequests.WithLabelValues(network.ProjectId, network.NetworkId, method, strconv.Itoa(resp.Attempts()), common.ErrorSummary(err)).Inc()
	}

	return nil, err
}

func (p *PreparedProject) bootstrapNetwork(ctx context.Context, networkId string) error {
	p.Logger.Info().Str("networkId", networkId).Msgf("initializing network")

	// 1) Find all upstreams that support this network
	err := p.upstreamsRegistry.PrepareUpstreamsForNetwork(ctx, networkId)
	if err != nil {
		return err
	}

	// 2) Find if any network configs defined on project-level
	nwCfg, err := p.resolveNetworkConfig(networkId)
	if err != nil {
		return err
	}

	// 3) Register and bootstrap the network
	nw, err := p.networksRegistry.RegisterNetwork(
		p.Logger,
		p.Config,
		nwCfg,
	)
	if err != nil {
		return err
	}

	err = nw.Bootstrap(ctx)
	if err != nil {
		return err
	}

	p.projectMu.Lock()
	p.Networks[networkId] = nw
	p.projectMu.Unlock()

	return nil
}

func (p *PreparedProject) resolveNetworkConfig(networkId string) (*common.NetworkConfig, error) {
	var nwCfg *common.NetworkConfig
	for _, n := range p.Config.Networks {
		if n.NetworkId() == networkId {
			nwCfg = n
			break
		}
	}

	if nwCfg == nil {
		nwCfg = &common.NetworkConfig{}
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
		nwCfg.SetDefaults(p.Config.Upstreams, p.Config.NetworkDefaults)
		p.projectMu.Lock()
		if p.Config.Networks == nil || len(p.Config.Networks) == 0 {
			p.Config.Networks = []*common.NetworkConfig{}
		}
		p.Config.Networks = append(p.Config.Networks, nwCfg)
		p.projectMu.Unlock()
	}

	return nwCfg, nil
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
