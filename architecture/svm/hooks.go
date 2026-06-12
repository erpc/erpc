package svm

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
)

// optionsAppend marks a method whose options object is always the trailing
// param but whose positional arity varies (only getBlocks: [start] or
// [start, end]), so the injector locates the slot dynamically.
const optionsAppend = -1

// commitmentOptionsIndex maps each commitment-injectable read method to the
// param index where its options/config object lives, per the Solana JSON-RPC
// reference (https://solana.com/docs/rpc/http). The injector sets `commitment`
// on the object at that index (creating it when the slot is exactly the next
// position) and SKIPS injection when the slot is occupied by a non-object —
// e.g. the legacy getBlock(slot, "base64") / getTransaction(sig, "json")
// encoding-string form — so a valid request shape is never corrupted.
//
// Excluded on purpose:
//   - Write/effectful methods (sendTransaction, simulateTransaction,
//     requestAirdrop): use preflightCommitment or apply commitment locally.
//   - No-parameter methods (getGenesisHash, getVersion, getHealth, getIdentity,
//     getInflationRate, getBlockTime, ...): appending an options object yields
//     an invalid shape (-32602 "No parameters were expected").
//   - Methods whose config carries no commitment field (getSignatureStatuses,
//     whose only option is searchTransactionHistory).
var commitmentOptionsIndex = map[string]int{
	// options object is the first/only param
	"getBlockHeight":            0,
	"getBlockProduction":        0,
	"getEpochInfo":              0,
	"getInflationGovernor":      0,
	"getLargestAccounts":        0,
	"getLatestBlockhash":        0,
	"getSlot":                   0,
	"getSlotLeader":             0,
	"getStakeMinimumDelegation": 0,
	"getSupply":                 0,
	"getTransactionCount":       0,
	"getVoteAccounts":           0,
	// one positional arg precedes the options object
	"getAccountInfo":          1,
	"getBalance":              1,
	"getBlock":                1,
	"getLeaderSchedule":       1,
	"getMultipleAccounts":     1,
	"getProgramAccounts":      1,
	"getSignaturesForAddress": 1,
	"getStakeActivation":      1,
	"getTokenAccountBalance":  1,
	"getTokenLargestAccounts": 1,
	"getTokenSupply":          1,
	"getTransaction":          1,
	"isBlockhashValid":        1,
	// two positional args precede the options object
	"getBlocksWithLimit":         2,
	"getTokenAccountsByDelegate": 2,
	"getTokenAccountsByOwner":    2,
	// variable arity; options is the trailing object
	"getBlocks": optionsAppend,
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
// Despite the name it is invoked from HandleProjectPreForward (before the
// network-layer cache read) — see that method for why. It returns
// (false, nil, nil) unconditionally; it never short-circuits, only mutates
// params, then invalidates the memoized CacheHash so the cache keys on the
// rewritten body. Shape rules (per commitmentOptionsIndex):
//
//   - Method not in the table → untouched (write/no-param/no-commitment methods).
//   - Options slot already an object with a commitment → honor the caller.
//   - Options slot is the next free position → append a fresh {commitment} object.
//   - Options slot occupied by a non-object (legacy encoding-string form) →
//     skip, so we never produce an invalid param shape.
//   - Required positional args missing → skip; let the upstream report it.
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
	if err != nil {
		return false, nil, nil
	}
	idx, ok := commitmentOptionsIndex[method]
	if !ok {
		return false, nil, nil
	}

	rpcReq, err := r.JsonRpcRequest(ctx)
	if err != nil {
		return false, nil, nil
	}
	rpcReq.Lock()
	defer rpcReq.Unlock()

	setOnMap := func(m map[string]interface{}) bool {
		if _, has := m["commitment"]; has {
			return false // honor caller-supplied commitment
		}
		m["commitment"] = defaultCommitment
		rpcReq.InvalidateCacheHash()
		return true
	}

	// getBlocks has variable positional arity ([start] or [start,end]); its
	// options object, when present, is always the trailing param and every
	// positional arg is a number — so locate it dynamically.
	if idx == optionsAppend {
		if len(rpcReq.Params) > 0 {
			if m, ok := rpcReq.Params[len(rpcReq.Params)-1].(map[string]interface{}); ok {
				setOnMap(m)
				return false, nil, nil
			}
		}
		rpcReq.Params = append(rpcReq.Params, map[string]interface{}{"commitment": defaultCommitment})
		rpcReq.InvalidateCacheHash()
		return false, nil, nil
	}

	switch {
	case idx < len(rpcReq.Params):
		// Options slot already exists. Only mutate it if it's an object; a
		// non-object there is a legacy encoding-string (or a positional arg),
		// and rewriting it would corrupt the request — leave it untouched.
		if m, ok := rpcReq.Params[idx].(map[string]interface{}); ok {
			setOnMap(m)
		}
	case idx == len(rpcReq.Params):
		// Options slot is exactly the next position — append a fresh object.
		rpcReq.Params = append(rpcReq.Params, map[string]interface{}{"commitment": defaultCommitment})
		rpcReq.InvalidateCacheHash()
	default:
		// idx > len(params): required positional args are missing. Don't
		// fabricate them; let the upstream return a descriptive error.
	}
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
