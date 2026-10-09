package evm

import (
	"bytes"
	"context"
	"errors"

	"github.com/erpc/erpc/common"
)

// A null eth_call result violates the JSON-RPC method contract (hex DATA is
// required, including for empty output). Use a server-side endpoint exception
// rather than MissingData: the latter is gated by RetryEmpty=false at both
// failsafe scopes, while this malformed response must penalize the upstream
// and retry toward another one regardless of that directive.
func upstreamPostForward_eth_call(ctx context.Context, rq *common.NormalizedRequest, rs *common.NormalizedResponse, re error) (*common.NormalizedResponse, error) {
	if re != nil || rs == nil {
		return rs, re
	}
	jrr, err := rs.JsonRpcResponse(ctx)
	if err != nil || jrr == nil || jrr.Error != nil || !bytes.Equal(bytes.TrimSpace(jrr.GetResultBytes()), []byte("null")) {
		return rs, re
	}
	// Do not retain the malformed response: the network retry loop can otherwise
	// promote a non-nil response from a failed attempt to its best response.
	// Upstream.Forward stored it as last-valid before this post-forward check.
	rq.ClearLastValidResponseIf(rs)
	return nil, common.NewErrEndpointServerSideException(errors.New("upstream returned null for eth_call instead of hex DATA"), nil, 0)
}

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

	// Confidence guard: beyond the head plus the configured safety margin, an
	// empty is likely truthful. Near-tip empties may be from a lagging upstream.
	if emptyResultBeyondConfidence(ctx, rq) {
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

// emptyResultBeyondConfidence reports whether `rq` targets a concrete block number
// beyond the network's required confidence head plus its safety margin. The
// head is the latest head for
// EmptyResultConfidence=blockHead (the default) or the finalized head for
// finalizedBlock. Returns false (fail-open) when the head is unknown or the request
// does not target a concrete numeric block (tags and block-hash lookups never qualify).
func emptyResultBeyondConfidence(ctx context.Context, rq *common.NormalizedRequest) bool {
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
	var head int64
	if cfg.Evm.EmptyResultConfidence == common.AvailbilityConfidenceFinalized {
		head = common.EvmHighestFinalizedBlockNumber(nw, ctx)
	} else {
		head = common.EvmHighestLatestBlockNumber(nw, ctx)
	}
	if head <= 0 {
		// Fail open: without a known head we cannot tell beyond-confidence from behind.
		return false
	}
	margin := cfg.Evm.FutureBlockMargin()
	return margin >= 0 && bn > head && bn-head > margin
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
