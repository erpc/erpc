package evm

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
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

	logger := network.Logger().With().Str("method", "eth_getBlockByNumber").Logger()

	// If response is from cache, skip enforcement otherwise there's no point in caching.
	// As we'll definetely have higher latest block number vs what we have in cache.
	// The correct way to deal with this situation is to set proper TTL for "realtime" cache policy.
	if nr.FromCache() {
		logger.Trace().
			Object("request", nq).
			Object("response", nr).
			Msg("skipping enforcement of highest block number as response is from cache")
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
			logger.Debug().
				Str("blockTag", bnp).
				Object("request", nq).
				Object("response", nr).
				Interface("highestBlockNumber", highestBlockNumber).
				Interface("respBlockNumber", respBlockNumber).
				Interface("err", err).
				Msg("enforcing highest latest block")
			if respBlockNumber > 0 {
				// When extracted block number is 0, it mostly means response is actually a json-rpc error
				// therefore we better fetch the highest block number again.
				ups := nr.Upstream()
				telemetry.MetricUpstreamStaleLatestBlock.WithLabelValues(
					network.ProjectId(),
					ups.VendorName(),
					network.Id(),
					ups.Id(),
					"eth_getBlockByNumber",
				).Inc()
			}
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
			dr.SkipCacheRead = true
			// In case a block number is extracted, it means the node actually has an older latest block.
			// Therefore we exclude the current upstream from the request (as high likely it doesn't have this block).
			// Otherwise we still allow the current upstream to be used in case json-rpc error was an intermittent issue.
			if respBlockNumber > 0 {
				dr.UseUpstream = fmt.Sprintf("!%s", nr.UpstreamId())
			}
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
			logger.Debug().
				Str("blockTag", bnp).
				Interface("highestBlockNumber", highestBlockNumber).
				Interface("respBlockNumber", respBlockNumber).
				Interface("err", err).
				Msg("enforcing highest finalized block")
			if respBlockNumber > 0 {
				// When extracted block number is 0, it mostly means response is actually a json-rpc error
				// therefore we better fetch the highest block number again.
				ups := nr.Upstream()
				telemetry.MetricUpstreamStaleFinalizedBlock.WithLabelValues(
					network.ProjectId(),
					ups.VendorName(),
					network.Id(),
					ups.Id(),
				).Inc()
			}
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
			dr.SkipCacheRead = true
			if respBlockNumber > 0 {
				// In case a block number is extracted, it means the node actually has an older latest block.
				// Therefore we exclude the current upstream from the request (as high likely it doesn't have this block).
				// Otherwise we still allow the current upstream to be used in case json-rpc error was an intermittent issue.
				// Also, if response from cache we don't need to exclude the current upstream.
				dr.UseUpstream = fmt.Sprintf("!%s", nr.UpstreamId())
			}
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
			nr.Upstream(),
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
