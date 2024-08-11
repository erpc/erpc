package erpc

import (
	"context"
	"fmt"
	"math"
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
	authRegistry         *auth.AuthRegistry
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

func (p *PreparedProject) Authenticate(ctx context.Context, nq common.NormalizedRequest, ap *auth.AuthPayload) error {
	if p.authRegistry != nil {
		err := p.authRegistry.Authenticate(ctx, nq, ap)
		if err != nil {
			return err
		}
	}

	return nil
}

func (p *PreparedProject) Forward(ctx context.Context, networkId string, nq *upstream.NormalizedRequest) (common.NormalizedResponse, error) {
	network, err := p.GetNetwork(networkId)
	if err != nil {
		return nil, err
	}
	method, _ := nq.Method()

	if err := p.acquireRateLimitPermit(nq); err != nil {
		return nil, err
	}

	timer := prometheus.NewTimer(health.MetricNetworkRequestDuration.WithLabelValues(
		p.Config.Id,
		network.NetworkId,
		method,
	))
	defer timer.ObserveDuration()

	health.MetricNetworkRequestsReceived.WithLabelValues(p.Config.Id, network.NetworkId, method).Inc()
	p.Logger.Debug().Str("method", method).Msgf("forwarding request to network")
	resp, err := network.Forward(ctx, nq)

	if err == nil || common.HasErrorCode(err, common.ErrCodeEndpointClientSideException) {
		if err != nil {
			p.Logger.Info().Err(err).Msgf("finished forwarding request for network with some client-side exception")
		} else {
			p.Logger.Info().Msgf("successfully forward request for network")
		}
		health.MetricNetworkSuccessfulRequests.WithLabelValues(p.Config.Id, network.NetworkId, method).Inc()
		return resp, err
	} else {
		p.Logger.Warn().Err(err).Str("method", method).Msgf("failed to forward request for network")
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
		allUps, err := p.upstreamsRegistry.GetSortedUpstreams(networkId, "*")
		if err != nil {
			return nil, err
		}
		nwCfg = &common.NetworkConfig{
			Failsafe: &common.FailsafeConfig{
				Retry: &common.RetryPolicyConfig{
					MaxAttempts:     int(math.Min(float64(len(allUps)), 3)),
					Delay:           "1s",
					Jitter:          "500ms",
					BackoffMaxDelay: "10s",
					BackoffFactor:   2,
				},
				Timeout: &common.TimeoutPolicyConfig{
					Duration: "30s",
				},
				Hedge: &common.HedgePolicyConfig{
					Delay:    "500ms",
					MaxCount: 1,
				},
			},
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

func (p *PreparedProject) acquireRateLimitPermit(req *upstream.NormalizedRequest) error {
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
