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
	networksMu           sync.RWMutex
	networksRegistry     *NetworksRegistry
	consumerAuthRegistry *auth.AuthRegistry
	adminAuthRegistry    *auth.AuthRegistry
	rateLimitersRegistry *upstream.RateLimitersRegistry
	upstreamsRegistry    *upstream.UpstreamsRegistry
	evmJsonRpcCache      *EvmJsonRpcCache
}

func (p *PreparedProject) GetNetwork(networkId string) (network *Network, err error) {
	p.networksMu.RLock()
	network, ok := p.Networks[networkId]
	p.networksMu.RUnlock()
	if !ok {
		p.networksMu.Lock()
		defer p.networksMu.Unlock()
		network, err = p.initializeNetwork(networkId)
		if err != nil {
			return nil, err
		}
		p.Networks[networkId] = network
	}
	return
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

func (p *PreparedProject) AuthenticateAdmin(ctx context.Context, nq *common.NormalizedRequest, ap *auth.AuthPayload) error {
	if p.adminAuthRegistry != nil {
		err := p.adminAuthRegistry.Authenticate(ctx, nq, ap)
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *PreparedProject) Forward(ctx context.Context, networkId string, nq *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	network, err := p.GetNetwork(networkId)
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
	lg := p.Logger.With().Str("method", method).Int64("id", nq.Id()).Str("ptr", fmt.Sprintf("%p", nq)).Logger()
	lg.Debug().Msgf("forwarding request to network")
	resp, err := network.Forward(ctx, nq)

	if err == nil || common.HasErrorCode(err, common.ErrCodeEndpointClientSideException) {
		if err != nil {
			lg.Info().Err(err).Msgf("finished forwarding request for network with some client-side exception")
		} else {
			lg.Info().Msgf("successfully forwarded request for network")
		}
		health.MetricNetworkSuccessfulRequests.WithLabelValues(p.Config.Id, network.NetworkId, method).Inc()
		return resp, err
	} else {
		lg.Warn().Err(err).Msgf("failed to forward request for network")
		health.MetricNetworkFailedRequests.WithLabelValues(network.ProjectId, network.NetworkId, method, common.ErrorSummary(err)).Inc()
	}

	return nil, err
}

func (p *PreparedProject) initializeNetwork(networkId string) (*Network, error) {
	// 1) Find all upstreams that support this network
	err := p.upstreamsRegistry.PrepareUpstreamsForNetwork(networkId)
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
		nwCfg = &common.NetworkConfig{
			Failsafe: common.NetworkDefaultFailsafeConfig,
		}

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

	err = nw.Bootstrap(p.appCtx)
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

	rules := rlb.GetRulesByMethod(method)
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
