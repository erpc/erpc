package svm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
)

// solanaDefaultSignaturesLimit is the page size agave applies to
// getSignaturesForAddress when the caller omits `limit`
// (https://solana.com/docs/rpc/http/getsignaturesforaddress: "Default: 1000").
// It is a wire-contract fact, so the short-page check can compare against it
// instead of treating an omitted limit as unknowable.
const solanaDefaultSignaturesLimit = 1000

// networkPreForward_getFirstAvailableBlock answers a client's
// getFirstAvailableBlock from the pool: the MIN of the upstreams' observed
// ledger floors, because a request below one upstream's floor fails over to
// another. That is "the oldest slot this pool can serve", which is what a
// client migrating off a single full-history backend actually wants to know —
// forwarding would instead report whichever one upstream happened to be picked.
//
// It fails open (forwards, behaving as without this hook) whenever the answer
// would not be the pool's truth:
//   - fewer than two upstreams: the live answer of the single upstream is
//     strictly fresher than its polled floor, so there is nothing to merge;
//   - ANY selected upstream's floor is unknown (PoolFirstAvailableSlot):
//     advertising MIN over a partial set would read an unobserved archive as
//     "history starts at the pruned validator's floor" — unknown must not read
//     as unavailable.
//
// Internal requests (probes) are never answered locally.
func networkPreForward_getFirstAvailableBlock(ctx context.Context, upstreams []common.Upstream, r *common.NormalizedRequest) (bool, *common.NormalizedResponse, error) {
	if len(upstreams) < 2 {
		return false, nil, nil
	}
	if dirs := r.Directives(); dirs != nil && dirs.IsInternal {
		return false, nil, nil
	}
	floor, known := PoolFirstAvailableSlot(upstreams)
	if !known {
		return false, nil, nil
	}
	id := r.ID()
	if id == nil {
		id = util.RandomID()
	}
	jrr, err := common.NewJsonRpcResponse(id, floor, nil)
	if err != nil {
		return true, nil, fmt.Errorf("failed to build getFirstAvailableBlock response: %w", err)
	}
	return true, common.NewNormalizedResponse().WithRequest(r).WithJsonRpcResponse(jrr), nil
}

// upstreamPostForward_historyParity turns two SUCCESSFUL-but-incomplete answers
// into MissingData so the sweep continues to the next upstream — the same
// "this upstream does not have it, another may" verdict a pruned validator's
// -32011 already produces, for the nodes that answer 200 instead of erroring.
//
// The verdict is whole-request: the original request is re-sent to the next
// upstream and this response is discarded. There is no splice or merge across
// upstreams. The discarded response is still the request's last-valid response
// (stored by the upstream layer before this hook runs), so when every upstream
// is equally incomplete the client receives that answer rather than an error.
//
// Errors pass through untouched: -32011 / -32020 and friends are classified by
// the error normalizer and fail over on their own.
func upstreamPostForward_historyParity(ctx context.Context, u common.Upstream, r *common.NormalizedRequest, rs *common.NormalizedResponse) (*common.NormalizedResponse, error) {
	if rs == nil || r == nil {
		return rs, nil
	}
	method, err := r.Method()
	if err != nil {
		return rs, nil
	}
	switch method {
	case "getSignatureStatuses":
		return rs, signatureStatusesNullToMissingData(ctx, u, r, rs)
	case "getSignaturesForAddress":
		return rs, signaturesForAddressShortPageToMissingData(ctx, u, r, rs)
	}
	return rs, nil
}

// optionsObjectAt returns the params[idx] options object, or nil when absent or
// not an object (legacy/positional encodings are left alone).
func optionsObjectAt(ctx context.Context, r *common.NormalizedRequest, idx int) map[string]interface{} {
	rpcReq, err := r.JsonRpcRequest(ctx)
	if err != nil || rpcReq == nil {
		return nil
	}
	rpcReq.RLock()
	defer rpcReq.RUnlock()
	if idx >= len(rpcReq.Params) {
		return nil
	}
	m, _ := rpcReq.Params[idx].(map[string]interface{})
	if m == nil {
		return nil
	}
	// Shallow copy: callers read it after the lock is released.
	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// signatureStatusesNullToMissingData: with `searchTransactionHistory: true` the
// caller asked every signature to be looked up in the full ledger, so a null
// entry means "this upstream did not find it", not "not recently seen". Another
// upstream with deeper history may have it. Without the flag a null is the
// valid recent-status-cache answer and must be served as is.
//
// Any null in the page triggers the sweep (whole-request semantics): the
// request is re-sent unchanged and the next upstream answers all signatures.
func signatureStatusesNullToMissingData(ctx context.Context, u common.Upstream, r *common.NormalizedRequest, rs *common.NormalizedResponse) error {
	// Only a literal JSON true enables history search; absent/false/non-bool
	// leaves the request on the recent-status path.
	if search, _ := optionsObjectAt(ctx, r, 1)["searchTransactionHistory"].(bool); !search {
		return nil
	}
	jrr, err := rs.JsonRpcResponse(ctx)
	if err != nil || jrr == nil || jrr.Error != nil {
		return nil
	}
	var result struct {
		Value []json.RawMessage `json:"value"`
	}
	if err := common.SonicCfg.Unmarshal(jrr.GetResultBytes(), &result); err != nil {
		return nil
	}
	nulls := 0
	for _, v := range result.Value {
		if bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			nulls++
		}
	}
	if nulls == 0 {
		return nil
	}
	return common.NewErrEndpointMissingData(
		common.NewErrJsonRpcExceptionInternal(
			0,
			common.JsonRpcErrorMissingData,
			fmt.Sprintf("getSignatureStatuses with searchTransactionHistory returned %d null status(es) of %d", nulls, len(result.Value)),
			nil,
			map[string]interface{}{"method": "getSignatureStatuses", "nulls": nulls, "total": len(result.Value)},
		),
		u,
	)
}

// signaturesForAddressShortPageToMissingData: a page shorter than the page size
// the caller asked for means this upstream's history for the address ends
// early — typically a pruned validator that stops where its ledger does. A full
// page proves nothing is missing. The effective page size is `limit`, else
// agave's default of 1000.
//
// A `limit` that is present but not a positive integer is not ours to
// interpret: fail open and let the upstream's own answer stand.
func signaturesForAddressShortPageToMissingData(ctx context.Context, u common.Upstream, r *common.NormalizedRequest, rs *common.NormalizedResponse) error {
	limit := int64(solanaDefaultSignaturesLimit)
	if raw, present := optionsObjectAt(ctx, r, 1)["limit"]; present {
		l, ok := toInt64Exact(raw)
		if !ok || l <= 0 {
			return nil
		}
		limit = l
	}
	jrr, err := rs.JsonRpcResponse(ctx)
	if err != nil || jrr == nil || jrr.Error != nil {
		return nil
	}
	raw := bytes.TrimSpace(jrr.GetResultBytes())
	if len(raw) == 0 || raw[0] != '[' {
		return nil
	}
	var page []json.RawMessage
	if err := common.SonicCfg.Unmarshal(raw, &page); err != nil {
		return nil
	}
	if int64(len(page)) >= limit {
		return nil
	}
	return common.NewErrEndpointMissingData(
		common.NewErrJsonRpcExceptionInternal(
			0,
			common.JsonRpcErrorMissingData,
			fmt.Sprintf("getSignaturesForAddress returned a short page (%d of limit %d)", len(page), limit),
			nil,
			map[string]interface{}{"method": "getSignaturesForAddress", "returned": len(page), "limit": limit},
		),
		u,
	)
}
