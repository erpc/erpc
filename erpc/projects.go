package erpc

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/auth"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/prometheus/client_golang/prometheus"
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

	timer := prometheus.NewTimer(prometheus.ObserverFunc(func(v float64) {
		telemetry.MetricNetworkRequestDuration.WithLabelValues(
			p.Config.Id,
			network.networkId,
			method,
		).Observe(v)
	}))
	defer timer.ObserveDuration()

	telemetry.MetricNetworkRequestsReceived.WithLabelValues(p.Config.Id, network.networkId, method).Inc()
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

	if err == nil {
		telemetry.MetricNetworkSuccessfulRequests.WithLabelValues(
			p.Config.Id,
			network.networkId,
			method,
			strconv.FormatInt(int64(resp.Attempts()), 10),
		).Inc()
		if lg.GetLevel() == zerolog.TraceLevel {
			lg.Info().Object("response", resp).Msgf("successfully forwarded request for network")
		} else {
			lg.Info().Msgf("successfully forwarded request for network")
		}
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
		).Inc()
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

func (p *PreparedProject) executeShadowRequests(ctx context.Context, network *Network, shadowUpstreams []*upstream.Upstream, resp *common.NormalizedResponse) {
	if resp == nil || len(shadowUpstreams) == 0 {
		return
	}

	// Derive the original request from the response
	origReq := resp.Request()
	if origReq == nil {
		return
	}

	method, _ := origReq.Method()

	// Compute the expected hash of the original upstream response once
	expectedHash, err := resp.Hash(ctx)
	if err != nil {
		p.Logger.Error().Err(err).Msg("failed to compute hash for original response while executing shadow requests")
		return
	}

	// Fire shadow requests concurrently
	for _, ups := range shadowUpstreams {
		ups := ups // capture loop variable
		go func() {
			ctx, cancel := context.WithCancel(p.networksRegistry.appCtx)
			defer cancel()

			shadowCtx, span := common.StartDetailSpan(ctx, "Project.executeShadowRequest")
			defer span.End()

			// Build a safe copy of the original request so that shadow requests do not race on shared state
			var shadowReq *common.NormalizedRequest
			if body := origReq.Body(); body != nil {
				// Copy the bytes to avoid accidental mutations
				cpy := append([]byte(nil), body...)
				shadowReq = common.NewNormalizedRequest(cpy)
			} else {
				jrq, errReq := origReq.JsonRpcRequest(shadowCtx)
				if errReq != nil {
					p.Logger.Error().Err(errReq).Msg("failed to clone json-rpc request for shadow upstream")
					return
				}
				bodyBytes, errMarshal := common.SonicCfg.Marshal(jrq)
				if errMarshal != nil {
					p.Logger.Error().Err(errMarshal).Msg("failed to marshal cloned json-rpc request for shadow upstream")
					return
				}
				shadowReq = common.NewNormalizedRequest(bodyBytes)
				// Pre-populate the parsed request so Forward() does not need to unmarshal again
				_, _ = shadowReq.JsonRpcRequest(shadowCtx)
			}

			// Copy directives so behaviour is consistent
			if dirs := origReq.Directives(); dirs != nil {
				shadowReq.SetDirectives(dirs.Clone())
			}

			// Set network reference for completeness (not strictly required for forwarding)
			shadowReq.SetNetwork(origReq.Network())

			// Execute the request against the shadow upstream (bypass exclusion)
			shadowResp, errForward := ups.Forward(shadowCtx, shadowReq, true)
			if errForward != nil {
				telemetry.MetricShadowResponseErrorTotal.WithLabelValues(
					p.Config.Id,
					ups.VendorName(),
					network.networkId,
					ups.Id(),
					method,
					common.ErrorFingerprint(errForward),
				).Inc()
				p.Logger.Debug().Err(errForward).
					Str("component", "shadowTraffic").
					Str("projectId", p.Config.Id).
					Str("networkId", network.networkId).
					Str("upstreamId", ups.Id()).
					Str("method", method).
					Msg("shadow request returned error")
				return
			}

			if shadowResp == nil {
				telemetry.MetricShadowResponseErrorTotal.WithLabelValues(
					p.Config.Id,
					ups.VendorName(),
					network.networkId,
					ups.Id(),
					method,
					"nil_response",
				).Inc()
				p.Logger.Debug().
					Str("component", "shadowTraffic").
					Str("projectId", p.Config.Id).
					Str("networkId", network.networkId).
					Str("upstreamId", ups.Id()).
					Str("method", method).
					Msg("shadow request returned nil response")
				return
			}

			shadowHash, errHash := shadowResp.Hash(shadowCtx)
			if errHash != nil {
				telemetry.MetricShadowResponseErrorTotal.WithLabelValues(
					p.Config.Id,
					ups.VendorName(),
					network.networkId,
					ups.Id(),
					method,
					"hash_error",
				).Inc()
				p.Logger.Debug().Err(errHash).
					Str("component", "shadowTraffic").
					Str("projectId", p.Config.Id).
					Str("networkId", network.networkId).
					Str("upstreamId", ups.Id()).
					Str("method", method).
					Msg("failed to compute hash for shadow response")
				return
			}

			if shadowHash == expectedHash {
				telemetry.MetricShadowResponseIdenticalTotal.WithLabelValues(
					p.Config.Id,
					ups.VendorName(),
					network.networkId,
					ups.Id(),
					method,
				).Inc()
				p.Logger.Trace().
					Str("component", "shadowTraffic").
					Str("projectId", p.Config.Id).
					Str("networkId", network.networkId).
					Str("upstreamId", ups.Id()).
					Str("method", method).
					Msg("shadow response identical to primary response")
			} else {
				isEmpty := shadowResp.IsResultEmptyish(shadowCtx)
				telemetry.MetricShadowResponseMismatchTotal.WithLabelValues(
					p.Config.Id,
					ups.VendorName(),
					network.networkId,
					ups.Id(),
					method,
					strconv.FormatBool(isEmpty),
				).Inc()
				p.Logger.Error().
					Str("component", "shadowTraffic").
					Str("projectId", p.Config.Id).
					Str("networkId", network.networkId).
					Str("upstreamId", ups.Id()).
					Str("method", method).
					Str("expectedHash", expectedHash).
					Str("shadowHash", shadowHash).
					Object("originalResponse", resp).
					Object("shadowResponse", shadowResp).
					Msg("shadow response hash mismatch")
			}

			shadowResp.Release()
		}()
	}
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
