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
