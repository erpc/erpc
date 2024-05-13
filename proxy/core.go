package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/flair-sdk/erpc/health"
	"github.com/flair-sdk/erpc/upstream"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog/log"
)

type ProxyCore struct {
	upstreamOrchestrator *upstream.UpstreamOrchestrator
	rateLimitersHub      *RateLimitersHub
}

func NewProxyCore(
	upstreamOrchestrator *upstream.UpstreamOrchestrator,
	rateLimitersHub *RateLimitersHub,
) *ProxyCore {
	return &ProxyCore{
		upstreamOrchestrator: upstreamOrchestrator,
		rateLimitersHub:      rateLimitersHub,
	}
}

// Forward a request for a project/network to an upstream selected by the orchestrator.
func (p *ProxyCore) Forward(project string, network string, r *http.Request, w http.ResponseWriter) error {
	// TODO check if request exists in the hot, warm, or cold cache

	currentUpstreams, errUps := p.upstreamOrchestrator.GetUpstreamsForNetwork(project, network)
	if errUps != nil {
		return fmt.Errorf("failed to get current upstreams: %w", errUps)
	}

	var errorsByUpstream = make(map[string]error)

	normalizedReq := upstream.NewNormalizedRequest(r)
	log.Debug().Msgf("trying upstreams for project: %s, network: %s upstreams: %v", project, network, currentUpstreams)

	for _, thisUpstream := range currentUpstreams {
		preparedReq, errPrep := thisUpstream.PrepareRequest(normalizedReq)

		if preparedReq == nil && errPrep == nil {
			continue
		}

		if errPrep != nil {
			errorsByUpstream[thisUpstream.Id] = fmt.Errorf("failed to prepare request for upstream: %w", errPrep)
			continue
		}

		err := p.tryForwardToUpstream(project, network, thisUpstream, preparedReq, w)
		if err == nil {
			log.Info().Msgf("successfully forward request to upstream: %s for project: %s, network: %s", thisUpstream.Id, project, network)

			return nil
		}

		errorsByUpstream[thisUpstream.Id] = err
	}

	return fmt.Errorf("failed to forward request to any upstream: %v", errorsByUpstream)
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
			return fmt.Errorf("failed to cast client to HttpJsonRpcClient")
		}

		limitersBucket, errLimiters := p.rateLimitersHub.GetBucket(thisUpstream.RateLimitBucket)
		if errLimiters != nil {
			return fmt.Errorf("failed to get rate limiters bucket '%s' error: %w", thisUpstream.RateLimitBucket, errLimiters)
		}

		log.Debug().Msgf("checking rate limiters bucket: %s result: %v", thisUpstream.RateLimitBucket, limitersBucket)

		if limitersBucket != nil {
			rules := limitersBucket.GetRulesByMethod(preparedRequest.(*upstream.JsonRpcRequest).Method)
			log.Debug().Msgf("checking rate limiters rules: %v for method: %s", rules, preparedRequest.(*upstream.JsonRpcRequest).Method)
			if len(rules) > 0 {
				for _, rule := range rules {
					log.Debug().Msgf("checking rate limiter rule: %v", rule)
					if !(*rule.Limiter).TryAcquirePermit() {
						log.Warn().Msgf("rate limit '%v' exceeded for upstream: %s", rule.Config, thisUpstream.Id)
						health.MetricUpstreamRequestLocalRateLimited.WithLabelValues(
							project,
							network,
							thisUpstream.Id,
						).Inc()
						return fmt.Errorf("rate limit '%v' exceeded for upstream: %s", rule.Config, thisUpstream.Id)
					} else {
						log.Debug().Msgf("rate limit '%v' passed for upstream: %s remaining capacity: %v", rule.Config, thisUpstream.Id, (*rule.Limiter))
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
			return fmt.Errorf("client failed to send request to upstream: %w", errCall)
		}

		respBytes, errMarshal := json.Marshal(resp)
		if errMarshal != nil {
			health.MetricUpstreamRequestErrors.WithLabelValues(
				project,
				network,
				thisUpstream.Id,
			).Inc()
			return fmt.Errorf("failed to parse upstream response: %w", errMarshal)
		}

		w.Header().Add("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(respBytes)
		return nil
	default:
		return fmt.Errorf("unsupported client type: %s", thisUpstream.Client.GetType())
	}
}
