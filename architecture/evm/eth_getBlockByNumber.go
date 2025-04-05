package evm

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
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

func networkPostForward_eth_getBlockByNumber(ctx context.Context, network common.Network, nq *common.NormalizedRequest, nr *common.NormalizedResponse, re error) (*common.NormalizedResponse, error) {
	ctx, span := common.StartDetailSpan(ctx, "Network.PostForward.eth_getBlockByNumber", trace.WithAttributes(
		attribute.String("request.id", fmt.Sprintf("%v", nq.ID())),
		attribute.String("network.id", network.Id()),
	))
	defer span.End()

	nr, err := enforceHighestBlock(ctx, network, nq, nr, re)
	if err != nil {
		common.SetTraceSpanError(span, err)
		return nr, err
	}
	return enforceNonNullBlock(nr)
}

func enforceHighestBlock(ctx context.Context, network common.Network, nq *common.NormalizedRequest, nr *common.NormalizedResponse, re error) (*common.NormalizedResponse, error) {
	if re != nil {
		return nr, re
	}

	ncfg := network.Config()
	if ncfg == nil ||
		ncfg.Evm == nil ||
		ncfg.Evm.Integrity == nil ||
		ncfg.Evm.Integrity.EnforceHighestBlock == nil ||
		!*ncfg.Evm.Integrity.EnforceHighestBlock {
		// If integrity check for highest block is disabled, skip this hook.
		return nr, re
	}

	rqj, err := nq.JsonRpcRequest(ctx)
	if err != nil {
		return nil, err
	}
	rqj.RLock()
	defer rqj.RUnlock()

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
		highestBlockNumber := network.EvmHighestLatestBlockNumber(ctx)
		_, respBlockNumber, err := ExtractBlockReferenceFromResponse(ctx, nr)
		if err != nil {
			return nil, err
		}
		if highestBlockNumber > respBlockNumber {
			upsId := nr.UpstreamId()
			if upsId == "" {
				upsId = "n/a"
			}
			health.MetricUpstreamStaleLatestBlock.WithLabelValues(
				network.ProjectId(),
				network.Id(),
				upsId,
				"eth_getBlockByNumber",
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
			dr := nq.Directives().Clone()
			// Exclude the current upstream from the request (as high likely it doesn't have this block)
			dr.UseUpstream = fmt.Sprintf("!%s", nr.UpstreamId())
			nq.SetDirectives(dr)
			nq.SetNetwork(network)
			nnr, err := network.Forward(ctx, nq)
			// This is needed in case highest block number is corrupted somehow and for example
			// it is requesting a very high non-existent block number.
			return pickHighestBlock(ctx, nnr, nr, err)
		} else {
			return nr, re
		}
	case "finalized":
		highestBlockNumber := network.EvmHighestFinalizedBlockNumber(ctx)
		_, respBlockNumber, err := ExtractBlockReferenceFromResponse(ctx, nr)
		if err != nil {
			return nil, err
		}
		if highestBlockNumber > respBlockNumber {
			upsId := nr.UpstreamId()
			if upsId == "" {
				upsId = "n/a"
			}
			health.MetricUpstreamStaleFinalizedBlock.WithLabelValues(
				network.ProjectId(),
				network.Id(),
				upsId,
				"eth_getBlockByNumber",
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
			dr := nq.Directives().Clone()
			// Exclude the current upstream from the request (as high likely it doesn't have this block)
			dr.UseUpstream = fmt.Sprintf("!%s", nr.UpstreamId())
			nq.SetDirectives(dr)
			nq.SetNetwork(network)
			nnr, err := network.Forward(ctx, nq)
			// This is needed in case highest block number is corrupted somehow and for example
			// it is requesting a very high non-existent block number.
			return pickHighestBlock(ctx, nnr, nr, err)
		} else {
			return nr, re
		}
	default:
		return nr, re
	}
}

// enforceNonNullBlock checks if the block result is null/empty and returns an appropriate error
func enforceNonNullBlock(nr *common.NormalizedResponse) (*common.NormalizedResponse, error) {
	if nr == nil || nr.IsObjectNull() || nr.IsResultEmptyish() {
		rq := nr.Request()
		details := make(map[string]interface{})
		var bnp string
		if rq != nil {
			rqj, _ := rq.JsonRpcRequest()
			if rqj != nil && len(rqj.Params) > 0 {
				bnp, _ = rqj.Params[0].(string)
				details["blockNumber"] = bnp
			}
		}
		return nil, common.NewErrEndpointMissingData(
			common.NewErrJsonRpcExceptionInternal(
				0,
				common.JsonRpcErrorMissingData,
				"block not found with number "+bnp,
				nil,
				details,
			),
		)
	}
	return nr, nil
}

func pickHighestBlock(ctx context.Context, x *common.NormalizedResponse, y *common.NormalizedResponse, err error) (*common.NormalizedResponse, error) {
	ctx, span := common.StartDetailSpan(ctx, "Evm.PickHighestBlock")
	defer span.End()

	xnull := x == nil || x.IsObjectNull() || x.IsResultEmptyish()
	ynull := y == nil || y.IsObjectNull() || y.IsResultEmptyish()
	if xnull && ynull && err != nil {
		return nil, err
	} else if xnull && !ynull {
		return y, nil
	} else if !xnull && ynull {
		return x, nil
	}
	xjrr, err := x.JsonRpcResponse(ctx)
	if err != nil || xjrr == nil {
		return y, nil
	}
	yjrr, err := y.JsonRpcResponse(ctx)
	if err != nil || yjrr == nil {
		return x, nil
	}
	xbn, err := xjrr.PeekStringByPath(ctx, "number")
	if err != nil {
		return y, nil
	}
	span.SetAttributes(attribute.String("block_number_1", xbn))
	ybn, err := yjrr.PeekStringByPath(ctx, "number")
	if err != nil {
		return x, nil
	}
	span.SetAttributes(attribute.String("block_number_2", ybn))
	xbnInt, err := strconv.ParseInt(xbn, 0, 64)
	if err != nil {
		return y, nil
	}
	ybnInt, err := strconv.ParseInt(ybn, 0, 64)
	if err != nil {
		return x, nil
	}
	if xbnInt > ybnInt {
		return x, nil
	}
	return y, nil
}
