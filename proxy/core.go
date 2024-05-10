package proxy

import (
	"encoding/json"
	"net/http"

	"github.com/flair-sdk/erpc/upstream"
	"github.com/rs/zerolog/log"
)

type ProxyCore struct {
	upstreamOrchestrator *upstream.UpstreamOrchestrator
}

type PreparedJsonRpcRequest struct {
	method string
	params []interface{}
}

func NewProxyCore(upstreamOrchestrator *upstream.UpstreamOrchestrator) *ProxyCore {
	return &ProxyCore{
		upstreamOrchestrator: upstreamOrchestrator,
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
		preparedJsonRpcRequest := p.ExtractAndPrepareJsonRpcRequest(r)
		resp, errCall := jsonRpcClient.Call(preparedJsonRpcRequest.method, preparedJsonRpcRequest.params)

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
		w.Write([]byte("Unsupported client type: " + bestUpstream.Client.GetType()))
	}
}

// Extract and prepare the request for forwarding.
func (p *ProxyCore) ExtractAndPrepareJsonRpcRequest(r *http.Request) *PreparedJsonRpcRequest {
	decoder := json.NewDecoder(r.Body)
	var requestJson map[string]interface{}
	err := decoder.Decode(&requestJson)
	if err != nil {
		return nil
	}

	method := requestJson["method"].(string)
	params := requestJson["params"].([]interface{})

	return &PreparedJsonRpcRequest{
		method: method,
		params: params,
	}
}
