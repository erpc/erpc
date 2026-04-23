package evm

import (
	"context"

	"github.com/erpc/erpc/common"
)

// upstreamPostForward_markUnexpectedEmpty converts empty results for point-lookups
// (blocks, transactions, receipts, traces, etc.) to missing-data so network retry can rotate.
func upstreamPostForward_markUnexpectedEmpty(
	ctx context.Context,
	u common.Upstream,
	rq *common.NormalizedRequest,
	rs *common.NormalizedResponse,
	re error,
) (*common.NormalizedResponse, error) {
	if re != nil || rs == nil || rs.IsObjectNull() || !rs.IsResultEmptyish() {
		return rs, re
	}

	if rq != nil {
		if rd := rq.Directives(); rd != nil && !rd.RetryEmpty {
			return rs, re
		}
	}

	// Build a simple message and include raw result in details for diagnostics.
	method, _ := rq.Method()
	details := map[string]interface{}{"method": method}
	if jrr, jerr := rs.JsonRpcResponse(ctx); jerr == nil && jrr != nil {
		details["rawResult"] = jrr.GetResultString()
	}

	return rs, common.NewErrEndpointMissingData(
		common.NewErrJsonRpcExceptionInternal(
			0,
			common.JsonRpcErrorMissingData,
			"upstream returned unexpected empty data",
			nil,
			details,
		),
		u,
	)
}

// normalizeEmptyArrayResponse returns a new NormalizedResponse with result `[]`,
// inheriting metadata from rs. Takes ownership of rs (calls Release()).
func normalizeEmptyArrayResponse(
	ctx context.Context,
	u common.Upstream,
	rq *common.NormalizedRequest,
	rs *common.NormalizedResponse,
) (*common.NormalizedResponse, error) {
	jrr, err := common.NewJsonRpcResponse(rq.ID(), []interface{}{}, nil)
	if err != nil {
		return nil, err
	}
	nnr := common.NewNormalizedResponse().WithRequest(rq).WithJsonRpcResponse(jrr)
	nnr.SetFromCache(rs.FromCache())
	nnr.SetEvmBlockRef(rs.EvmBlockRef())
	nnr.SetEvmBlockNumber(rs.EvmBlockNumber())
	nnr.SetDuration(rs.Duration())
	nnr.SetAttempts(rs.Attempts())
	nnr.SetRetries(rs.Retries())
	nnr.SetHedges(rs.Hedges())
	nnr.SetUpstream(u)
	rq.SetLastValidResponse(ctx, nnr)
	rs.Release()
	return nnr, nil
}
