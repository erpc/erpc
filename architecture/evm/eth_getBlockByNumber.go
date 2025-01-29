package evm

import (
	"context"
	"fmt"
	"strings"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
)

func BuildGetBlockByNumberRequest(blockNumberOrTag interface{}, includeTransactions bool) (*common.JsonRpcRequest, error) {
	var bkt string
	var err error

	switch v := blockNumberOrTag.(type) {
	case string:
		bkt = v
		if !strings.HasPrefix(bkt, "0x") {
			switch bkt {
			case "latest", "finalized", "safe", "pending", "earliest":
				// Acceptable tags
			default:
				return nil, fmt.Errorf("invalid block number or tag for eth_getBlockByNumber: %v", v)
			}
		}
	case int, int64, float64:
		bkt, err = common.NormalizeHex(v)
		if err != nil {
			return nil, fmt.Errorf("invalid block number or tag for eth_getBlockByNumber: %v", v)
		}
	default:
		return nil, fmt.Errorf("invalid block number or tag for eth_getBlockByNumber: %v", v)
	}

	return common.NewJsonRpcRequest("eth_getBlockByNumber", []interface{}{bkt, includeTransactions}), nil
}

// networkPostForward_eth_getBlockByNumber is a customized logic for the eth_getBlockByNumber method where it checks with upstream EvmStatePoller
// and returns the highest value (including making the call)
// - Should this actually prefer calling highest blockhead upstream?
func networkPostForward_eth_getBlockByNumber(ctx context.Context, network common.Network, nq *common.NormalizedRequest, nr *common.NormalizedResponse, re error) (*common.NormalizedResponse, error) {
	if re != nil {
		return nr, re
	}

	rqj, err := nq.JsonRpcRequest()
	if err != nil {
		return nil, err
	}
	if len(rqj.Params) < 1 {
		return nr, re
	}
	bnp, ok := rqj.Params[0].(string)
	if !ok {
		return nr, re
	}
	if bnp != "latest" && bnp != "finalized" {
		return nr, re
	}

	switch bnp {
	case "latest":
		highestBlockNumber := network.EvmHighestLatestBlockNumber()
		_, respBlockNumber, err := ExtractBlockReferenceFromResponse(nr)
		if err != nil {
			return nil, err
		}
		if highestBlockNumber > respBlockNumber {
			health.MetricUpstreamStaleLatestBlock.WithLabelValues(
				network.ProjectId(),
				network.Id(),
				nr.UpstreamId(),
			).Inc()
			var itx bool
			if len(rqj.Params) > 1 {
				itx, _ = rqj.Params[1].(bool)
			}
			request, err := BuildGetBlockByNumberRequest(highestBlockNumber, itx)
			if err != nil {
				return nil, err
			}
			err = request.SetID(nq.ID())
			if err != nil {
				return nil, err
			}
			nq := common.NewNormalizedRequestFromJsonRpcRequest(request)
			nq.SetDirectives(nq.Directives())
			nq.SetNetwork(network)
			return network.Forward(ctx, nq)
		} else {
			return nr, re
		}
	case "finalized":
		highestBlockNumber := network.EvmHighestFinalizedBlockNumber()
		_, respBlockNumber, err := ExtractBlockReferenceFromResponse(nr)
		if err != nil {
			return nil, err
		}
		if highestBlockNumber > respBlockNumber {
			health.MetricUpstreamStaleFinalizedBlock.WithLabelValues(
				network.ProjectId(),
				network.Id(),
				nr.UpstreamId(),
			).Inc()
			request, err := BuildGetBlockByNumberRequest(highestBlockNumber, true)
			if err != nil {
				return nil, err
			}
			err = request.SetID(nq.ID())
			if err != nil {
				return nil, err
			}
			nq := common.NewNormalizedRequestFromJsonRpcRequest(request)
			nq.SetDirectives(nq.Directives())
			nq.SetNetwork(network)
			return network.Forward(ctx, nq)
		} else {
			return nr, re
		}
	default:
		return nr, re
	}
}
