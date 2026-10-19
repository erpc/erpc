package svm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"

	"github.com/erpc/erpc/common"
)

// PoolFirstAvailableSlot returns the pool's ledger floor: the MIN across
// upstreams, because a request below one upstream's floor fails over to
// another. That is the same "never deny data some upstream has" rule that makes
// the upper guard use MAX.
//
// known=false unless EVERY upstream's floor is observed. A MIN over a partial
// set is not a conservative estimate, it is an over-report in the forbidden
// direction: with a known validator at 200000 and an unknown archive,
// MIN-over-known says "history starts at 200000" — unknown reading as
// unavailable. An upstream we cannot inspect counts as unknown for the same
// reason.
//
// Floors are only observed when an upstream declares firstAvailableSlotPlus
// (opt-in poll); without that the pool floor is unknown and callers fail open.
func PoolFirstAvailableSlot(upstreams []common.Upstream) (int64, bool) {
	if len(upstreams) == 0 {
		return 0, false
	}
	minSlot := int64(math.MaxInt64)
	for _, u := range upstreams {
		sup, ok := u.(common.SvmUpstream)
		if !ok {
			return 0, false
		}
		poller := sup.SvmStatePoller()
		if poller == nil || poller.IsObjectNull() {
			return 0, false
		}
		slot, known := poller.FirstAvailableSlot()
		if !known {
			return 0, false
		}
		if slot < minSlot {
			minSlot = slot
		}
	}
	return minSlot, true
}

// solanaDefaultSignaturesLimit is the page size agave applies to
// getSignaturesForAddress when the caller omits `limit`
// (https://solana.com/docs/rpc/http/getsignaturesforaddress: "Default: 1000").
// It is a wire-contract fact, so the short-page check can compare against it
// instead of treating an omitted limit as unknowable.
const solanaDefaultSignaturesLimit = 1000

// upstreamPostForward_historyParity turns two SUCCESSFUL-but-incomplete answers
// into MissingData so the sweep continues to the next upstream — the same
// "this upstream does not have it, another may" verdict a pruned validator's
// -32011 already produces, for the nodes that answer 200 instead of erroring.
//
// An upstream that already holds the pool's deepest known history (floor known
// and <= PoolFirstAvailableSlot) is exempt: its incomplete answer is the pool's
// best (see holdsDeepestHistory). Unknown floors keep the sweep going.
//
// The verdict is whole-request: the original request is re-sent to the next
// upstream and this response is discarded. There is no splice or merge across
// upstreams. The discarded response is still the request's last-valid response
// (stored by the upstream layer before this hook runs), so when every upstream
// is equally incomplete the client receives that answer rather than an error.
//
// Consensus participant slots (common.IsConsensusSlot) skip this hook: parallel
// upstreams vote on the bodies they returned. Converting identical legitimate
// short pages into MissingData would make consensus agree on an error and the
// network layer would not fall back to last-valid-response. Cross-upstream retry
// belongs to the serial sweep path, not consensus fan-out.
//
// Errors pass through untouched: -32011 / -32020 and friends are classified by
// the error normalizer and fail over on their own.
func upstreamPostForward_historyParity(ctx context.Context, u common.Upstream, r *common.NormalizedRequest, rs *common.NormalizedResponse) (*common.NormalizedResponse, error) {
	if common.IsConsensusSlot(ctx) {
		return rs, nil
	}
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
	if nulls == 0 || holdsDeepestHistory(u, r) {
		return nil
	}
	return newHistoryParityMissingData(
		u,
		fmt.Sprintf("getSignatureStatuses with searchTransactionHistory returned %d null status(es) of %d", nulls, len(result.Value)),
		map[string]interface{}{"method": "getSignatureStatuses", "nulls": nulls, "total": len(result.Value)},
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
	if int64(len(page)) >= limit || holdsDeepestHistory(u, r) {
		return nil
	}
	return newHistoryParityMissingData(
		u,
		fmt.Sprintf("getSignaturesForAddress returned a short page (%d of limit %d)", len(page), limit),
		map[string]interface{}{"method": "getSignaturesForAddress", "returned": len(page), "limit": limit},
	)
}

// holdsDeepestHistory reports whether u's ledger floor is known and no higher
// than the pool's floor (MIN across the request's selected upstreams), i.e. no
// upstream in the pool can reach further back than u. Such an upstream's short
// page / null status is the best answer the pool has, not a miss: sweeping on
// would only offer the client a shallower answer, and the last-valid-response
// slot is last-writer-wins, so a shallower upstream answering later would
// REPLACE it.
//
// Fails open on every unknown: u's floor unobserved, or any pool member's floor
// unobserved (PoolFirstAvailableSlot) — an unobserved archive may be deeper, so
// the sweep continues exactly as before.
func holdsDeepestHistory(u common.Upstream, r *common.NormalizedRequest) bool {
	sup, ok := u.(common.SvmUpstream)
	if !ok {
		return false
	}
	poller := sup.SvmStatePoller()
	if poller == nil || poller.IsObjectNull() {
		return false
	}
	floor, known := poller.FirstAvailableSlot()
	if !known {
		return false
	}
	poolFloor, poolKnown := PoolFirstAvailableSlot(r.Upstreams())
	return poolKnown && floor <= poolFloor
}

// newHistoryParityMissingData builds the incomplete-answer verdict: MissingData
// (so the sweep continues to the next upstream) marked PERMANENT. Permanence
// governs only the time-delayed re-sweep after a completed sweep, never
// cross-upstream failover (that is retryableTowardNetwork, unaffected). Waiting
// one block time cannot lengthen an address's history or make an unseen
// signature land, so re-sweeping every upstream after a delay — until
// EmptyResultMaxAttempts — would only multiply load for the same answer. The
// client then receives the request's last-valid response.
func newHistoryParityMissingData(u common.Upstream, msg string, details map[string]interface{}) error {
	err := common.NewErrEndpointMissingData(
		common.NewErrJsonRpcExceptionInternal(0, common.JsonRpcErrorMissingData, msg, nil, details),
		u,
	)
	if me, ok := err.(*common.ErrEndpointMissingData); ok {
		me.WithPermanentMissingData(true)
		me.WithHistoryParityContinuation(true)
	}
	return err
}
