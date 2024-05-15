package upstream

import (
	"fmt"
)

type PreparedUpstream struct {
	Id               string
	Architecture     string // ArchitectureEvm, ArchitectureSolana, ...
	Endpoint         string
	RateLimitBucket  string
	HealthCheckGroup string
	Metadata         map[string]string
	NetworkIds       []string

	Client ClientInterface
}

func (u *PreparedUpstream) PrepareRequest(normalizedReq *NormalizedRequest) (interface{}, error) {
	switch u.Architecture {
	case ArchitectureEvm:
		if u.Client == nil {
			return nil, NewErrJsonRpcRequestPreparation(fmt.Errorf("client not initialized for evm upstream"), map[string]interface{}{
				"upstreamId": u.Id,
			})
		}

		if u.Client.GetType() == "HttpJsonRpcClient" {
			// TODO check supported/unsupported methods for this upstream
			return normalizedReq.JsonRpcRequest()
		} else {
			return nil, NewErrJsonRpcRequestPreparation(fmt.Errorf("unsupported evm client type for upstream"), map[string]interface{}{
				"upstreamId": u.Id,
				"clientType": u.Client.GetType(),
			})
		}
	default:
		return nil, NewErrJsonRpcRequestPreparation(fmt.Errorf("unsupported architecture for upstream"), map[string]interface{}{
			"upstreamId":   u.Id,
			"architecture": u.Architecture,
		})
	}
}
