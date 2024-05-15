package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/config"
	"github.com/flair-sdk/erpc/health"
	"github.com/flair-sdk/erpc/upstream"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog/log"
)

type ProxyCore struct {
	config               *config.Config
	upstreamOrchestrator *upstream.UpstreamOrchestrator
	rateLimitersHub      *RateLimitersHub
}

func NewProxyCore(
	cfg *config.Config,
	upstreamOrchestrator *upstream.UpstreamOrchestrator,
	rateLimitersHub *RateLimitersHub,
) *ProxyCore {
	return &ProxyCore{
		config:               cfg,
		upstreamOrchestrator: upstreamOrchestrator,
		rateLimitersHub:      rateLimitersHub,
	}
}

// Forward a request for a project/network to an upstream selected by the orchestrator.
func (p *ProxyCore) Forward(project string, network string, req *upstream.NormalizedRequest, w http.ResponseWriter) error {
	// TODO check if request exists in the hot, warm, or cold cache

	currentUpstreams, errUps := p.upstreamOrchestrator.GetUpstreamsForNetwork(project, network)
	if errUps != nil {
		return errUps
	}

	var errorsByUpstream = make(map[string]error)

	log.Debug().Msgf("trying upstreams for project: %s, network: %s upstreams: %v", project, network, currentUpstreams)

	projectConfig := p.config.GetProjectConfig(project)
	if projectConfig == nil {
		return &common.ErrProjectNotFound{
			ProjectId: project,
		}
	}
	if projectConfig.RateLimitBucket != "" {
		networkLimiter, errNetLimit := p.rateLimitersHub.GetBucket(projectConfig.RateLimitBucket)
		if errNetLimit != nil {
			return errNetLimit
		}
		if networkLimiter != nil {
			method, errMethod := req.Method()
			if errMethod != nil {
				return fmt.Errorf("failed to get method from request: %w", errMethod)
			}
			rules := networkLimiter.GetRulesByMethod(method)
			log.Debug().Msgf("found %d network-level rate limiters for network: %s method: %s", len(rules), network, method)
			if len(rules) > 0 {
				for _, rule := range rules {
					if !(*rule.Limiter).TryAcquirePermit() {
						log.Warn().Msgf("network-level rate limit '%v' exceeded for network: %s", rule.Config, network)
						health.MetricUpstreamRequestLocalRateLimited.WithLabelValues(
							project,
							network,
							"",
						).Inc()
						return fmt.Errorf("network-level rate limit '%v' exceeded for network: %s", rule.Config, network)
					} else {
						log.Debug().Msgf("network-level rate limit '%v' passed for network: %s", rule.Config, network)
					}
				}
			}
		}
	}

	for _, thisUpstream := range currentUpstreams {
		preparedReq, errPrep := thisUpstream.PrepareRequest(req)

		log.Debug().Msgf("prepared request for upstream: %s prepared: %v error: %v", thisUpstream.Id, preparedReq, errPrep)

		if preparedReq == nil && errPrep == nil {
			continue
		}

		if errPrep != nil {
			errorsByUpstream[thisUpstream.Id] = errPrep
			continue
		}

		err := p.tryForwardToUpstream(project, network, thisUpstream, preparedReq, w)
		if err == nil {
			log.Info().Msgf("successfully forward request to upstream: %s for project: %s, network: %s", thisUpstream.Id, project, network)
			return nil
		} else {
			log.Debug().Msgf("failed to forward request to upstream: %s for project: %s, network: %s error: %v", thisUpstream.Id, project, network, err)
		}

		errorsByUpstream[thisUpstream.Id] = err
	}

	return NewErrUpstreamsExhausted(errorsByUpstream)
}

func (p *ProxyCore) tryForwardToUpstream(project string, network string, thisUpstream *upstream.PreparedUpstream, preparedRequest interface{}, w http.ResponseWriter) error {
	health.MetricUpstreamRequestTotal.WithLabelValues(
		project,
		network,
		thisUpstream.Id,
	).Inc()
	timer := prometheus.NewTimer(health.MetricUpstreamRequestDuration.WithLabelValues(
		project,
		network,
		thisUpstream.Id,
	))
	defer timer.ObserveDuration()

	clientType := thisUpstream.Client.GetType()
	log.Debug().Msgf("forwarding request to upstream: %s of type: %s", thisUpstream.Id, clientType)

	switch clientType {
	case "HttpJsonRpcClient":
		jsonRpcClient, okClient := thisUpstream.Client.(*upstream.HttpJsonRpcClient)
		if !okClient {
			return NewErrUpstreamClientInitialization(
				fmt.Errorf("failed to cast client to HttpJsonRpcClient"),
				thisUpstream.Id,
			)
		}

		limitersBucket, errLimiters := p.rateLimitersHub.GetBucket(thisUpstream.RateLimitBucket)
		if errLimiters != nil {
			return errLimiters
		}

		log.Debug().Msgf("checking upstream-level rate limiters bucket: %s result: %v", thisUpstream.RateLimitBucket, limitersBucket)

		if limitersBucket != nil {
			rules := limitersBucket.GetRulesByMethod(preparedRequest.(*upstream.JsonRpcRequest).Method)
			log.Debug().Msgf("checking upstream-level rate limiters rules: %v for method: %s", rules, preparedRequest.(*upstream.JsonRpcRequest).Method)
			if len(rules) > 0 {
				for _, rule := range rules {
					log.Debug().Msgf("checking upstream-level rate limiter rule: %v", rule)
					if !(*rule.Limiter).TryAcquirePermit() {
						log.Warn().Msgf("upstream-level rate limit '%v' exceeded for upstream: %s", rule.Config, thisUpstream.Id)
						health.MetricUpstreamRequestLocalRateLimited.WithLabelValues(
							project,
							network,
							thisUpstream.Id,
						).Inc()
						return NewErrUpstreamRateLimitRuleExceeded(
							rule.Config,
							thisUpstream.Id,
						)
					} else {
						log.Debug().Msgf("upstream-level rate limit '%v' passed for upstream: %s", rule.Config, thisUpstream.Id)
					}
				}
			}
		}

		resp, errCall := jsonRpcClient.SendRequest(preparedRequest.(*upstream.JsonRpcRequest))

		if errCall != nil {
			health.MetricUpstreamRequestErrors.WithLabelValues(
				project,
				network,
				thisUpstream.Id,
			).Inc()

			return NewErrUpstreamRequest(
				errCall,
				thisUpstream.Id,
			)
		}

		respBytes, errMarshal := json.Marshal(resp)
		if errMarshal != nil {
			health.MetricUpstreamRequestErrors.WithLabelValues(
				project,
				network,
				thisUpstream.Id,
			).Inc()
			return NewErrUpstreamMalformedResponse(
				errMarshal,
				thisUpstream.Id,
			)
		}

		w.Header().Add("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(respBytes)
		return nil
	default:
		return NewErrUpstreamClientInitialization(
			fmt.Errorf("unsupported client type: %s", clientType),
			thisUpstream.Id,
		)
	}
}
