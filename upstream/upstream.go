package upstream

import "fmt"

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
			return nil, fmt.Errorf("client not initialized for evm upstream: %v", u.Id)
		}

		if u.Client.GetType() == "HttpJsonRpcClient" {
			// TODO check supported/unsupported methods for this upstream
			return normalizedReq.JsonRpcRequest()
		} else {
			return nil, fmt.Errorf("unsupported evm client type: %v for upstream: %v", u.Client.GetType(), u.Id)
		}
	default:
		return nil, fmt.Errorf("unsupported architecture: %v for upstream: %v", u.Architecture, u.Id)
	}
}
