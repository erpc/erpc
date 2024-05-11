package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/flair-sdk/erpc/upstream"
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

	bestUpstream, errGet := p.upstreamOrchestrator.GetBestUpstream(project, network)
	if errGet != nil {
		return fmt.Errorf("failed to get best upstream: %w", errGet)
	}

	clientType := bestUpstream.Client.GetType()
	log.Debug().Msgf("forwarding request to upstream: %s of type: %s", bestUpstream.Id, clientType)

	switch clientType {
	case "HttpJsonRpcClient":
		jsonRpcClient, okClient := bestUpstream.Client.(*upstream.HttpJsonRpcClient)
		if !okClient {
			return fmt.Errorf("failed to cast client to HttpJsonRpcClient")
		}

		preparedReq, errPrepare := p.normalizer.NormalizeJsonRpcRequest(r)
		if errPrepare != nil {
			return fmt.Errorf("failed to normalize request: %w", errPrepare)
		}
		resp, errCall := jsonRpcClient.SendRequest(preparedReq)

		if errCall != nil {
			return fmt.Errorf("client failed to send request to upstream: %w", errCall)
		}

		// TODO cache the response in the hot, warm, or cold cache
		respBytes, errMarshal := json.Marshal(resp)
		if errMarshal != nil {
			return fmt.Errorf("failed to parse upstream response: %w", errMarshal)
		}

		w.Header().Add("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(respBytes)
		return nil
	default:
		return fmt.Errorf("unsupported client type: %s", bestUpstream.Client.GetType())
	}
}
