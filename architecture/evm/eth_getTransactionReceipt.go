package evm

import (
	"context"
	"strings"

	"github.com/erpc/erpc/common"
)

// upstreamPostForward_eth_getTransactionReceipt decides whether a null receipt
// is the answer or missing data.
//
// A null receipt is the right answer for a pending or unknown transaction. With
// evm.probeNullReceipts on, it becomes missing data when this upstream's own
// eth_getTransactionByHash carries a blockNumber: the receipt exists and this
// upstream just doesn't hold it (pruned, or not indexed yet), so the request
// moves on as for any other missing data, up to the fallback tier. The null is
// still the last valid response, so it is served when no upstream has the
// receipt. Otherwise the method's markEmptyAsErrorMethods entry, if any,
// applies as for every other method.
//
// The probe runs only where it can change the outcome. Without failover every
// routed upstream is already tried on a null, so the side call would buy
// nothing. In a consensus slot a missing-data verdict would make the
// participants agree on an error instead of null (the rule SVM's history sweep
// follows too). Any doubt (no hash, the side call fails, the transaction is
// pending or unknown) keeps the null.
func upstreamPostForward_eth_getTransactionReceipt(
	ctx context.Context,
	n common.Network,
	u common.Upstream,
	rq *common.NormalizedRequest,
	rs *common.NormalizedResponse,
	re error,
) (*common.NormalizedResponse, error) {
	if re != nil || rs == nil || rs.IsObjectNull() || !rs.IsResultEmptyish() {
		return rs, re
	}
	if hash, ok := shouldProbeNullReceipt(ctx, n, rq); ok && txMinedOn(ctx, u, hash) {
		return rs, common.NewErrEndpointMissingData(
			common.NewErrJsonRpcExceptionInternal(
				0,
				common.JsonRpcErrorMissingData,
				"upstream returned a null receipt for a mined transaction",
				nil,
				map[string]interface{}{"method": "eth_getTransactionReceipt", "transactionHash": hash},
			),
			u,
		)
	}
	if isMethodInMarkEmptyList(n, "eth_gettransactionreceipt") {
		return upstreamPostForward_markUnexpectedEmpty(ctx, u, rq, rs, re)
	}
	return rs, re
}

// shouldProbeNullReceipt returns the transaction hash to probe when
// evm.probeNullReceipts is on, the network has failover, the request is not a
// consensus participant, and its first param is a transaction hash.
func shouldProbeNullReceipt(ctx context.Context, n common.Network, rq *common.NormalizedRequest) (string, bool) {
	if n == nil || n.Config() == nil || !n.Config().Evm.ProbeNullReceiptsEnabled() {
		return "", false
	}
	if !n.Config().Failover.Enabled() || common.IsConsensusSlot(ctx) {
		return "", false
	}
	hash := txHashParam(ctx, rq)
	return hash, hash != ""
}

// txMinedOn reports whether u has the transaction mined: its
// eth_getTransactionByHash answers for this hash with a blockNumber.
func txMinedOn(ctx context.Context, u common.Upstream, hash string) bool {
	tr, err := u.Forward(ctx, buildGetTransactionByHashRequest(hash), true, false)
	if tr != nil {
		defer tr.Release()
	}
	if err != nil || tr == nil {
		return false
	}
	jrr, err := tr.JsonRpcResponse(ctx)
	if err != nil || jrr == nil || jrr.Error != nil || jrr.IsResultEmptyish(ctx) {
		return false
	}
	if h, err := jrr.PeekStringByPath(ctx, "hash"); err != nil || !strings.EqualFold(h, hash) {
		return false
	}
	bn, err := jrr.PeekStringByPath(ctx, "blockNumber")
	return err == nil && bn != ""
}

// txHashParam returns the request's first param when it is a 32-byte hex
// hash (0x + 64 hex digits), else "".
func txHashParam(ctx context.Context, rq *common.NormalizedRequest) string {
	if rq == nil {
		return ""
	}
	jrq, err := rq.JsonRpcRequest(ctx)
	if err != nil || jrq == nil {
		return ""
	}
	jrq.RLock()
	defer jrq.RUnlock()
	if len(jrq.Params) == 0 {
		return ""
	}
	h, _ := jrq.Params[0].(string)
	if len(h) != 66 || h[0] != '0' || (h[1] != 'x' && h[1] != 'X') {
		return ""
	}
	for _, c := range h[2:] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return ""
		}
	}
	return h
}
