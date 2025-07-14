package erpc

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/auth"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
)

type PreparedProject struct {
	Config               *common.ProjectConfig
	Logger               *zerolog.Logger
	networksRegistry     *NetworksRegistry
	consumerAuthRegistry *auth.AuthRegistry
	rateLimitersRegistry *upstream.RateLimitersRegistry
	upstreamsRegistry    *upstream.UpstreamsRegistry
	cfgMu                sync.RWMutex
}

type ProjectHealthInfo struct {
	upstream.UpstreamsHealth
	Initialization *util.InitializerStatus `json:"initialization,omitempty"`
}

func (p *PreparedProject) Bootstrap(appCtx context.Context) error {
	wg := sync.WaitGroup{}
	wg.Add(2)
	var errs []error
	ermu := &sync.Mutex{}
	go func() {
		defer wg.Done()
		err := p.upstreamsRegistry.Bootstrap(appCtx)
		if err != nil {
			ermu.Lock()
			errs = append(errs, err)
			ermu.Unlock()
		}
	}()
	go func() {
		defer wg.Done()
		err := p.networksRegistry.Bootstrap(appCtx)
		if err != nil {
			ermu.Lock()
			errs = append(errs, err)
			ermu.Unlock()
		}
	}()
	wg.Wait()
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

func (p *PreparedProject) GetNetwork(networkId string) (*Network, error) {
	return p.networksRegistry.GetNetwork(networkId)
}

// ExposeNetworkConfig is used to add lazy-loaded network configs to the project
// so that other components can use them, also is returned via erpc_project admin API.
func (p *PreparedProject) ExposeNetworkConfig(nwCfg *common.NetworkConfig) {
	p.cfgMu.Lock()
	defer p.cfgMu.Unlock()

	if p.Config.Networks == nil {
		p.Config.Networks = []*common.NetworkConfig{}
	}
	var existing *common.NetworkConfig
	for _, nw := range p.Config.Networks {
		if nw.NetworkId() == nwCfg.NetworkId() {
			existing = nw
			break
		}
	}
	if existing == nil {
		p.Config.Networks = append(p.Config.Networks, nwCfg)
	}
}

func (p *PreparedProject) GetNetworks() []*Network {
	return p.networksRegistry.GetNetworks()
}

func (p *PreparedProject) GatherHealthInfo() (*ProjectHealthInfo, error) {
	upstreamsHealth, err := p.upstreamsRegistry.GetUpstreamsHealth()
	if err != nil {
		return nil, err
	}
	return &ProjectHealthInfo{
		UpstreamsHealth: *upstreamsHealth,
		Initialization:  p.networksRegistry.initializer.Status(),
	}, nil
}

func (p *PreparedProject) AuthenticateConsumer(ctx context.Context, method string, ap *auth.AuthPayload) error {
	if p.consumerAuthRegistry != nil {
		err := p.consumerAuthRegistry.Authenticate(ctx, method, ap)
		if err != nil {
			return err
		}
	}

	return nil
}

func (p *PreparedProject) Forward(ctx context.Context, networkId string, nq *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	start := time.Now()
	ctx, span := common.StartDetailSpan(ctx, "Project.Forward")
	defer span.End()

	network, err := p.networksRegistry.GetNetwork(networkId)
	if err != nil {
		common.SetTraceSpanError(span, err)
		return nil, err
	}
	if err := p.acquireRateLimitPermit(nq); err != nil {
		common.SetTraceSpanError(span, err)
		return nil, err
	}

	method, _ := nq.Method()

	// Get initial finality from request
	reqFinality := nq.Finality(ctx)

	telemetry.MetricNetworkRequestsReceived.WithLabelValues(p.Config.Id, network.networkId, method, reqFinality.String()).Inc()
	lg := p.Logger.With().
		Str("component", "proxy").
		Str("projectId", p.Config.Id).
		Str("networkId", network.networkId).
		Str("method", method).
		Interface("id", nq.ID()).
		Str("ptr", fmt.Sprintf("%p", nq)).
		Logger()

	resp, err := p.doForward(ctx, network, nq)

	shadowUpstreams := network.ShadowUpstreams()
	if len(shadowUpstreams) > 0 {
		cloneResp, err := common.CopyResponseForRequest(ctx, resp, nq)
		if err != nil {
			lg.Error().Err(err).Msgf("failed to copy response for shadow requests")
		} else {
			go p.executeShadowRequests(ctx, network, shadowUpstreams, cloneResp)
		}
	}

	if err != nil {
		common.SetTraceSpanError(span, err)
	}

	// Get finality from response if available, otherwise use request finality
	finality := reqFinality
	if resp != nil {
		finality = resp.Finality(ctx)
	}

	if err == nil && resp != nil {
		upstream := resp.Upstream()
		vendor := "n/a"
		upstreamId := "n/a"
		if resp.FromCache() {
			vendor = "<cache>"
			upstreamId = "<cache>"
		} else if upstream != nil {
			vendor = upstream.VendorName()
			upstreamId = upstream.Id()
		}
		telemetry.MetricNetworkSuccessfulRequests.WithLabelValues(
			p.Config.Id,
			network.networkId,
			vendor,
			upstreamId,
			method,
			strconv.FormatInt(int64(resp.Attempts()), 10),
			finality.String(),
			strconv.FormatBool(resp.IsResultEmptyish(ctx)),
		).Inc()
		if lg.GetLevel() == zerolog.TraceLevel {
			lg.Info().Object("response", resp).Msgf("successfully forwarded request for network")
		} else {
			lg.Info().Msgf("successfully forwarded request for network")
		}
		telemetry.MetricNetworkRequestDuration.WithLabelValues(
			p.Config.Id,
			network.networkId,
			vendor,
			upstreamId,
			method,
			finality.String(),
		).Observe(time.Since(start).Seconds())
		return resp, err
	} else {
		if common.IsClientError(err) || common.HasErrorCode(err, common.ErrCodeEndpointExecutionException) {
			lg.Info().Err(err).Msgf("finished forwarding request for network with some client-side exception")
		} else {
			if lg.GetLevel() <= zerolog.DebugLevel {
				lg.Info().Err(err).Object("request", nq).Msgf("failed to forward request for network")
			} else {
				lg.Info().Err(err).Msgf("failed to forward request for network")
			}
		}
		telemetry.MetricNetworkFailedRequests.WithLabelValues(
			network.projectId,
			network.networkId,
			method,
			strconv.FormatInt(int64(resp.Attempts()), 10),
			common.ErrorFingerprint(err),
			string(common.ClassifySeverity(err)),
			finality.String(),
		).Inc()
		telemetry.MetricNetworkRequestDuration.WithLabelValues(
			p.Config.Id,
			network.networkId,
			"<error>",
			"<error>",
			method,
			finality.String(),
		).Observe(time.Since(start).Seconds())
	}

	return nil, err
}

func (p *PreparedProject) doForward(ctx context.Context, network *Network, nq *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	switch network.cfg.Architecture {
	case common.ArchitectureEvm:
		if handled, resp, err := evm.HandleNetworkPreForward(ctx, network, nq); handled {
			return evm.HandleNetworkPostForward(ctx, network, nq, resp, err)
		}
	}

	// If not handled, then fallback to the normal forward
	resp, err := network.Forward(ctx, nq)
	return evm.HandleNetworkPostForward(ctx, network, nq, resp, err)
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
				telemetry.MetricProjectRequestSelfRateLimited.WithLabelValues(
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
