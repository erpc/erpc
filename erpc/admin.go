package erpc

import (
	"context"
	"fmt"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/upstream"
)

func (p *PreparedProject) HandleAdminRequest(ctx context.Context, nq *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	method, err := nq.Method()
	if err != nil {
		return nil, err
	}

	switch method {
	case "erpc_config":
		jrr, err := nq.JsonRpcRequest()
		if err != nil {
			return nil, err
		}
		type configResult struct {
			Project          *common.ProjectConfig           `json:"project"`
			RateLimitBudgets []*common.RateLimitBudgetConfig `json:"rateLimitBudgets"`
		}
		result := configResult{
			Project: p.Config,
			// TODO should we just return 'relevant' budgets to avoid irrelevant data?
			RateLimitBudgets: p.rateLimitersRegistry.GetBudgets(),
		}
		return common.NewNormalizedResponse().WithJsonRpcResponse(
			&common.JsonRpcResponse{
				JSONRPC: jrr.JSONRPC,
				ID:      jrr.ID,
				Error:   nil,
				Result:  result,
			},
		), nil
	case "erpc_health":
		jrr, err := nq.JsonRpcRequest()
		if err != nil {
			return nil, err
		}
		health, err := p.gatherHealthInfo()
		if err != nil {
			return nil, err
		}
		return common.NewNormalizedResponse().WithJsonRpcResponse(
			&common.JsonRpcResponse{
				JSONRPC: jrr.JSONRPC,
				ID:      jrr.ID,
				Error:   nil,
				Result:  health,
			},
		), nil
	default:
		return nil, common.NewErrEndpointUnsupported(
			fmt.Errorf("admin method %s is not supported", method),
		)
	}
}

func (p *PreparedProject) gatherHealthInfo() (*upstream.UpstreamsHealth, error) {
	return p.upstreamsRegistry.GetUpstreamsHealth()
}
