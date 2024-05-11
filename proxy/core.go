package proxy

import (
	"encoding/json"
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
func (p *ProxyCore) Forward(project string, network string, w http.ResponseWriter, r *http.Request) {
	// TODO check if request exists in the hot, warm, or cold cache

	bestUpstream, errGet := p.upstreamOrchestrator.GetBestUpstream(project, network)
	if errGet != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(errGet.Error()))
		return
	}

	clientType := bestUpstream.Client.GetType()
	log.Debug().Msgf("forwarding request to upstream: %s of type: %s", bestUpstream.Id, clientType)

	switch clientType {
	case "HttpJsonRpcClient":
		jsonRpcClient, okClient := bestUpstream.Client.(*upstream.HttpJsonRpcClient)
		if !okClient {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("failed to cast client to HttpJsonRpcClient"))
			return
		}

		preparedReq, errPrepare := p.normalizer.NormalizeJsonRpcRequest(r)
		if errPrepare != nil {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(errPrepare.Error()))
			return
		}
		resp, errCall := jsonRpcClient.SendRequest(preparedReq)

		if errCall != nil {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(errCall.Error()))
			return
		}

		// TODO cache the response in the hot, warm, or cold cache
		respBytes, errMarshal := json.Marshal(resp)
		if errMarshal != nil {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(errMarshal.Error()))
			return
		}

		w.Header().Add("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(respBytes)
	default:
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("unsupported client type: " + bestUpstream.Client.GetType()))
	}
}
