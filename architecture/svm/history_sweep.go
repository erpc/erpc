package svm

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/erpc/erpc/common"
)

// solanaDefaultSignaturesLimit is the page size agave applies to
// getSignaturesForAddress when the caller omits `limit`
// (https://solana.com/docs/rpc/http/getsignaturesforaddress: "Default: 1000").
// It is a wire-contract fact, so the short-page check can compare against it
// instead of treating an omitted limit as unknowable.
const solanaDefaultSignaturesLimit = 1000

// upstreamPostForward_historySweep turns two SUCCESSFUL-but-incomplete answers
// into MissingData so the sweep continues to the next upstream — the same
// "this upstream does not have it, another may" verdict a pruned validator's
// -32011 already produces, for the nodes that answer 200 instead of erroring.
//
// A short page or a null status is also the normal end of history (a young
// address, an `until` cursor, a signature that never landed), so the shape alone
// proves nothing. It continues only when another selected upstream has a known
// floor strictly below this one's (see deeperFloorSelected): only then can the
// pool hold history this upstream cannot see. An unknown floor on either side
// serves the answer as is, the behaviour without this hook.
//
// The verdict is whole-request: the original request is re-sent to the next
// upstream and this response is discarded. There is no splice or merge across
// upstreams. The discarded response stays the request's last-valid response, so
// when the deeper upstream fails the client still receives this answer.
//
// Consensus participant slots (common.IsConsensusSlot) skip this hook: parallel
// upstreams vote on the bodies they returned, and turning identical legitimate
// short pages into MissingData would make consensus agree on an error.
//
// Errors pass through untouched: -32011 / -32020 and friends are classified by
// the error normalizer and fail over on their own.
func upstreamPostForward_historySweep(ctx context.Context, u common.Upstream, r *common.NormalizedRequest, rs *common.NormalizedResponse) (*common.NormalizedResponse, error) {
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
	if nulls == 0 || !deeperFloorSelected(u, r) {
		return nil
	}
	return newHistorySweepMissingData(
		u,
		fmt.Sprintf("getSignatureStatuses with searchTransactionHistory returned %d null status(es) of %d", nulls, len(result.Value)),
		map[string]interface{}{"method": "getSignatureStatuses", "nulls": nulls, "total": len(result.Value)},
	)
}

// signaturesForAddressShortPageToMissingData: a page shorter than the page size
// the caller asked for may mean this upstream's history for the address ends at
// its floor — a pruned validator stops where its ledger does. A full page
// proves nothing is missing. The effective page size is `limit`, else
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
	if int64(len(page)) >= limit || !deeperFloorSelected(u, r) {
		return nil
	}
	return newHistorySweepMissingData(
		u,
		fmt.Sprintf("getSignaturesForAddress returned a short page (%d of limit %d)", len(page), limit),
		map[string]interface{}{"method": "getSignaturesForAddress", "returned": len(page), "limit": limit},
	)
}

// IsHistoryMethod reports whether method reads arbitrarily old ledger history,
// so the upstream with the deepest floor gives the most complete answer.
func IsHistoryMethod(method string) bool {
	switch method {
	case "getSignatureStatuses", "getSignaturesForAddress":
		return true
	}
	return false
}

// OrderByHistoryDepth stably reorders ups so that, inside each run of upstreams
// sharing a failover tier (sameTier), known floors come first, deepest first, and
// unknown floors keep their score order after them. A history read then lands on
// the most complete upstream first and the sweep in upstreamPostForward_historySweep
// rarely needs a second call. Ordering only: every upstream stays eligible.
func OrderByHistoryDepth(ups []common.Upstream, sameTier func(a, b common.Upstream) bool) []common.Upstream {
	if len(ups) < 2 {
		return ups
	}
	out := slices.Clone(ups)
	for start := 0; start < len(out); {
		end := start + 1
		for end < len(out) && sameTier(out[start], out[end]) {
			end++
		}
		slices.SortStableFunc(out[start:end], func(a, b common.Upstream) int {
			fa, ka := upstreamFloor(a)
			fb, kb := upstreamFloor(b)
			switch {
			case ka && kb:
				return cmp.Compare(fa, fb)
			case ka:
				return -1
			case kb:
				return 1
			}
			return 0
		})
		start = end
	}
	return out
}

// deeperFloorSelected reports whether another upstream selected for r has a
// known floor strictly below u's known floor: the only evidence that the pool
// can reach history u cannot. Ties and unknowns are not evidence, so an
// upstream at the pool's deepest floor, or one whose floor is unobserved, has
// the final word.
func deeperFloorSelected(u common.Upstream, r *common.NormalizedRequest) bool {
	floor, known := upstreamFloor(u)
	if !known {
		return false
	}
	for _, other := range r.Upstreams() {
		if other == u {
			continue
		}
		if f, ok := upstreamFloor(other); ok && f < floor {
			return true
		}
	}
	return false
}

func upstreamFloor(u common.Upstream) (int64, bool) {
	sup, ok := u.(common.SvmUpstream)
	if !ok {
		return 0, false
	}
	poller := sup.SvmStatePoller()
	if poller == nil || poller.IsObjectNull() {
		return 0, false
	}
	return poller.FirstAvailableSlot()
}

// newHistorySweepMissingData builds the incomplete-answer verdict: MissingData
// (so the sweep continues to the next upstream) marked PERMANENT. Permanence
// governs only the time-delayed re-sweep after a completed sweep, never
// cross-upstream failover (that is retryableTowardNetwork, unaffected). Waiting
// one block time cannot lengthen an address's history or make an unseen
// signature land, so re-sweeping every upstream after a delay — until
// EmptyResultMaxAttempts — would only multiply load for the same answer. The
// client then receives the request's last-valid response.
func newHistorySweepMissingData(u common.Upstream, msg string, details map[string]interface{}) error {
	err := common.NewErrEndpointMissingData(
		common.NewErrJsonRpcExceptionInternal(0, common.JsonRpcErrorMissingData, msg, nil, details),
		u,
	)
	if me, ok := err.(*common.ErrEndpointMissingData); ok {
		me.WithPermanentMissingData(true)
		me.WithHistorySweepContinuation(true)
	}
	return err
}
