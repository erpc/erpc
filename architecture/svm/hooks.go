package svm

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
)

// commitmentInjectableMethods is the whitelist of read methods where appending
// or setting {commitment: ...} on the params options object is semantically safe.
// Write methods (sendTransaction, simulateTransaction, requestAirdrop) are
// intentionally excluded — they either use a different option name
// (preflightCommitment) or apply commitment locally, so rewriting would change
// upstream semantics silently.
//
// Methods that take no options at all (getGenesisHash, getVersion, getHealth,
// getBlockTime, etc.) are also excluded so we don't append a spurious map.
var commitmentInjectableMethods = map[string]bool{
	"getAccountInfo":             true,
	"getBalance":                 true,
	"getBlock":                   true,
	"getBlockHeight":             true,
	"getBlockProduction":         true,
	"getBlocks":                  true,
	"getBlocksWithLimit":         true,
	"getEpochInfo":               true,
	"getInflationGovernor":       true,
	"getInflationRate":           true,
	"getLargestAccounts":         true,
	"getLatestBlockhash":         true,
	"getLeaderSchedule":          true,
	"getMultipleAccounts":        true,
	"getProgramAccounts":         true,
	"getSignaturesForAddress":    true,
	"getSignatureStatuses":       true,
	"getSlot":                    true,
	"getSlotLeader":              true,
	"getStakeActivation":         true,
	"getStakeMinimumDelegation":  true,
	"getSupply":                  true,
	"getTokenAccountBalance":     true,
	"getTokenAccountsByDelegate": true,
	"getTokenAccountsByOwner":    true,
	"getTokenLargestAccounts":    true,
	"getTokenSupply":             true,
	"getTransaction":             true,
	"getTransactionCount":        true,
	"getVoteAccounts":            true,
	"isBlockhashValid":           true,
}

// projectPreForward_getGenesisHash short-circuits getGenesisHash using the
// hardcoded table — cluster genesis hashes are immutable, so we never need an
// upstream round-trip. Mirrors EVM's eth_chainId short-circuit.
func projectPreForward_getGenesisHash(ctx context.Context, n common.Network, r *common.NormalizedRequest) (bool, *common.NormalizedResponse, error) {
	if r.ShouldSkipCacheRead("") {
		return false, nil, nil
	}
	cfg := n.Config()
	if cfg == nil || cfg.Svm == nil || cfg.Svm.Cluster == "" {
		return false, nil, nil
	}
	hash, ok := common.KnownGenesisHash(cfg.Svm.Chain, cfg.Svm.Cluster)
	if !ok || hash == "" {
		// Unknown cluster — let the upstream answer, don't fabricate.
		return false, nil, nil
	}

	id := r.ID()
	if id == nil {
		id = util.RandomID()
	}
	jrr, err := common.NewJsonRpcResponse(id, hash, nil)
	if err != nil {
		return true, nil, fmt.Errorf("failed to build getGenesisHash response: %w", err)
	}
	nr := common.NewNormalizedResponse().WithRequest(r).WithJsonRpcResponse(jrr)
	return true, nr, nil
}

// networkPreForward_injectCommitment stamps the network-level default commitment
// onto outgoing request params so every upstream observes the same commitment
// level, regardless of its own server-side default. Without this, two upstreams
// with different local defaults (e.g. one "processed", one "finalized") would
// return subtly different data for the same user request, poisoning the cache
// and failsafe consensus.
//
// The hook returns (false, nil, nil) unconditionally — it never short-circuits
// the request. It only mutates params so the upstream forward continues with
// the rewritten body. Conservative by design:
//
//   - Only methods in commitmentInjectableMethods are touched. The write/local
//     methods retain their exact original body.
//   - If the caller already specified a commitment anywhere in params, we do
//     nothing — user intent beats network default.
//   - If no options object is present at all, we append one at the end (the
//     documented param position for Solana options across all whitelisted
//     methods).
func networkPreForward_injectCommitment(ctx context.Context, n common.Network, r *common.NormalizedRequest) (bool, *common.NormalizedResponse, error) {
	cfg := n.Config()
	if cfg == nil || cfg.Svm == nil || cfg.Svm.Commitment == "" {
		return false, nil, nil
	}
	defaultCommitment := strings.ToLower(cfg.Svm.Commitment)
	if defaultCommitment != "finalized" && defaultCommitment != "confirmed" && defaultCommitment != "processed" {
		// Reject unknown commitment strings rather than forwarding a malformed options
		// object the upstream won't understand.
		return false, nil, nil
	}

	method, err := r.Method()
	if err != nil || !commitmentInjectableMethods[method] {
		return false, nil, nil
	}

	rpcReq, err := r.JsonRpcRequest(ctx)
	if err != nil {
		return false, nil, nil
	}
	rpcReq.Lock()
	defer rpcReq.Unlock()

	// If a commitment is already set in any map param, honor the caller's choice.
	for _, p := range rpcReq.Params {
		if m, ok := p.(map[string]interface{}); ok {
			if _, has := m["commitment"]; has {
				return false, nil, nil
			}
		}
	}

	// Prefer mutating the last map-valued param (the conventional options slot).
	// If no map exists, append a new one. Either way, invalidate the memoized
	// CacheHash so downstream cache lookups key on the post-mutation params.
	for i := len(rpcReq.Params) - 1; i >= 0; i-- {
		if m, ok := rpcReq.Params[i].(map[string]interface{}); ok {
			m["commitment"] = defaultCommitment
			rpcReq.InvalidateCacheHash()
			return false, nil, nil
		}
	}
	rpcReq.Params = append(rpcReq.Params, map[string]interface{}{"commitment": defaultCommitment})
	rpcReq.InvalidateCacheHash()
	return false, nil, nil
}

// networkPreForward_validateSignaturesForAddress rejects requests whose slot
// window exceeds the configured MaxSlotsPerSignaturesQuery. Solana's
// getSignaturesForAddress has unbounded server cost when the (before, until)
// signature pair spans a large slot range, and vendor RPCs will either time
// out or return truncated results silently. Bounding the query client-side
// gives predictable 400-class failures instead of flaky partial data.
//
// This is a whole-request rejection (handled=true, err set) — no upstream call
// is made. The user is expected to paginate their own signature range.
func networkPreForward_validateSignaturesForAddress(ctx context.Context, n common.Network, r *common.NormalizedRequest) (bool, *common.NormalizedResponse, error) {
	cfg := n.Config()
	if cfg == nil || cfg.Svm == nil || cfg.Svm.MaxSlotsPerSignaturesQuery <= 0 {
		return false, nil, nil
	}
	rpcReq, err := r.JsonRpcRequest(ctx)
	if err != nil {
		return false, nil, nil
	}
	rpcReq.RLock()
	defer rpcReq.RUnlock()

	// Params shape: [address, {before?, until?, minContextSlot?, limit?, commitment?}]
	// Slot-range check requires BOTH minContextSlot and some upper slot hint. In
	// practice only minContextSlot is slot-indexed; before/until are signatures,
	// not slots. We only have a meaningful bound when the caller specified a
	// minContextSlot far below the current latest — then the implicit range is
	// (minContextSlot, latestSlot]. Reject if that exceeds the cap.
	if len(rpcReq.Params) < 2 {
		return false, nil, nil
	}
	opts, ok := rpcReq.Params[1].(map[string]interface{})
	if !ok {
		return false, nil, nil
	}
	minSlotRaw, has := opts["minContextSlot"]
	if !has {
		return false, nil, nil
	}
	minSlot, ok := toInt64(minSlotRaw)
	if !ok || minSlot <= 0 {
		return false, nil, nil
	}

	// Use the network's highest reported latest slot as the implicit upper
	// bound. When no upstream has reported a slot yet (bootstrap, test), we
	// skip the check — better to let the request through than to reject on
	// missing metadata. The type-assertion is defensive: dispatch guarantees
	// this handler only fires for SVM networks, so the assertion succeeds in
	// production; a failure here means a wiring bug, not a hot-path case.
	svmNet, ok := n.(common.SvmNetwork)
	if !ok {
		return false, nil, nil
	}
	latest := svmNet.SvmHighestLatestSlot(ctx)
	if latest <= 0 {
		return false, nil, nil
	}
	window := latest - minSlot
	if window > cfg.Svm.MaxSlotsPerSignaturesQuery {
		return true, nil, fmt.Errorf(
			"getSignaturesForAddress slot window %d exceeds maxSlotsPerSignaturesQuery=%d (latest=%d, minContextSlot=%d); paginate the query",
			window, cfg.Svm.MaxSlotsPerSignaturesQuery, latest, minSlot,
		)
	}
	return false, nil, nil
}

func toInt64(v interface{}) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	}
	return 0, false
}

// upstreamPostForward_trackContextSlot peeks at response.result.context.slot
// and feeds it into the upstream's SvmStatePoller. Solana RPC responses
// commonly carry a `context.slot` metadata field that tells us the slot the
// node was at when it answered — using it updates our latest-slot view
// without waiting for the next poll tick, which tightens the freshness
// window for failover decisions.
//
// Quietly no-ops on:
//   - nil response / error response
//   - upstreams without an SvmStatePoller (EVM or early bootstrap)
//   - responses where the slot field is missing or unparseable
//
// The assumption is that any slot reported by a successful upstream is
// usable — we don't try to guard against regressions here; that's the
// state poller's rollback-tolerance job.
func upstreamPostForward_trackContextSlot(ctx context.Context, u common.Upstream, rs *common.NormalizedResponse) {
	if rs == nil || u == nil {
		return
	}
	sup, ok := u.(common.SvmUpstream)
	if !ok {
		return
	}
	poller := sup.SvmStatePoller()
	if poller == nil || poller.IsObjectNull() {
		return
	}
	jrr, err := rs.JsonRpcResponse(ctx)
	if err != nil || jrr == nil || jrr.Error != nil {
		return
	}
	slotStr, err := jrr.PeekStringByPath(ctx, "context", "slot")
	if err != nil || slotStr == "" {
		return
	}
	slot, err := strconv.ParseInt(slotStr, 10, 64)
	if err != nil || slot <= 0 {
		return
	}
	poller.SuggestLatestSlot(slot)
}

// upstreamPostForward_sendTransaction marks sendTransaction errors as
// non-retryable across upstreams. Retrying a failed transaction send against a
// different upstream can produce a double-spend once the original tx propagates
// through the cluster. EVM has an analogous guard for eth_sendRawTransaction.
//
// We always wrap as ClientSideException (not just flip retryableTowardNetwork on
// the original error) because the network-level upstream loop bails out on
// common.IsClientError — that check looks specifically for
// ErrCodeEndpointClientSideException rather than walking retryableTowardNetwork
// details. Without the wrap, a ServerSideException from the primary would be
// silently failed over to the secondary.
func upstreamPostForward_sendTransaction(rs *common.NormalizedResponse, re error) (*common.NormalizedResponse, error) {
	if re == nil {
		return rs, nil
	}
	wrapped := common.NewErrEndpointClientSideException(re).WithRetryableTowardNetwork(false)
	return rs, wrapped
}
