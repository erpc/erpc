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
	normalizer           *Normalizer
}

func NewProxyCore(upstreamOrchestrator *upstream.UpstreamOrchestrator) *ProxyCore {
	return &ProxyCore{
		upstreamOrchestrator: upstreamOrchestrator,
		normalizer:           &Normalizer{},
	}
}

// Forward a request for a project/network to an upstream selected by the orchestrator.
func (p *ProxyCore) Forward(project string, network string, r *http.Request, w http.ResponseWriter) error {
	// TODO check if request exists in the hot, warm, or cold cache

	currentUpstreams, errGet := p.upstreamOrchestrator.GetUpstreams(project, network)
	if errGet != nil {
		return fmt.Errorf("failed to get current upstreams: %w", errGet)
	}

	preparedReq, errPrepare := p.normalizer.NormalizeJsonRpcRequest(r)
	if errPrepare != nil {
		return fmt.Errorf("failed to normalize request: %w", errPrepare)
	}

	var errorsByUpstream = make(map[string]error)

	log.Debug().Msgf("trying upstreams for project: %s, network: %s upstreams: %v", project, network, currentUpstreams)

	for _, thisUpstream := range currentUpstreams {
		log.Debug().Msgf("trying to forward request to upstream: %s", thisUpstream.Id)
		// TODO cache the response in the hot, warm, or cold cache
		err := p.tryForwardToUpstream(project, network, thisUpstream, preparedReq, w)

		log.Debug().Msgf("forwarding request to upstream: %s returned: %v", thisUpstream.Id, err)

		if err == nil {
			return nil
		}

		errorsByUpstream[thisUpstream.Id] = err
	}

	return fmt.Errorf("failed to forward request to any upstream: %v", errorsByUpstream)
}

func (p *ProxyCore) tryForwardToUpstream(project string, network string, thisUpstream *upstream.PreparedUpstream, preparedRequest *upstream.JsonRpcRequest, w http.ResponseWriter) error {
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

		resp, errCall := jsonRpcClient.SendRequest(preparedRequest)

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
