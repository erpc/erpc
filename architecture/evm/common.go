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

	// Future-block guard: do not retry an empty result for a concrete block beyond
	// the network's served latest (by more than maxFutureBlockRetryDistance, which
	// defaults to 0) — it is not yet produced, so every upstream legitimately
	// returns empty. Return the truthful empty instead of churning retries until
	// the request times out.
	if emptyResultIsFutureBlock(ctx, rq) {
		return rs, re
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

// emptyResultIsFutureBlock reports whether `rq` targets a concrete block number
// beyond the network's served latest by more than MaxFutureBlockRetryDistance —
// a not-yet-produced block for which every upstream legitimately returns empty.
// The distance defaults to 0 (only the head itself is retried); a negative
// distance disables the guard. Returns false (fail-open) when the head is
// unknown or the request does not target a concrete numeric block (tags and
// block-hash lookups are never future).
func emptyResultIsFutureBlock(ctx context.Context, rq *common.NormalizedRequest) bool {
	if rq == nil {
		return false
	}
	bn, ok := rq.EvmBlockNumber().(int64)
	if !ok || bn <= 0 {
		return false
	}
	nw := rq.Network()
	if nw == nil {
		return false
	}
	cfg := nw.Config()
	if cfg == nil || cfg.Evm == nil {
		return false
	}
	distance := int64(0)
	if cfg.Evm.MaxFutureBlockRetryDistance != nil {
		distance = *cfg.Evm.MaxFutureBlockRetryDistance
		if distance < 0 {
			// Negative disables the bound: retry all empties (legacy behavior).
			return false
		}
	}
	head := nw.EvmHighestLatestBlockNumber(ctx)
	if head <= 0 {
		// Fail open: without a known head we cannot tell future from behind.
		return false
	}
	return bn > head+distance
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
