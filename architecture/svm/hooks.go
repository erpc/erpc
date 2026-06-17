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
// params per the plan from resolveCommitment, then invalidates the memoized
// CacheHash so the cache keys on the rewritten body.
func networkPreForward_injectCommitment(ctx context.Context, n common.Network, r *common.NormalizedRequest) (bool, *common.NormalizedResponse, error) {
	commitment, action, idx := resolveCommitment(ctx, n, r)
	if action != commitmentSet && action != commitmentAppend {
		// explicit (already set), or skip (non-injectable / legacy non-object
		// slot / missing args / no valid default) → leave the request untouched.
		return false, nil, nil
	}

	rpcReq, err := r.JsonRpcRequest(ctx)
	if err != nil {
		return false, nil, nil
	}
	rpcReq.Lock()
	defer rpcReq.Unlock()

	switch action {
	case commitmentSet:
		if idx >= 0 && idx < len(rpcReq.Params) {
			if m, ok := rpcReq.Params[idx].(map[string]interface{}); ok {
				m["commitment"] = commitment
				rpcReq.InvalidateCacheHash()
			}
		}
	case commitmentAppend:
		rpcReq.Params = append(rpcReq.Params, map[string]interface{}{"commitment": commitment})
		rpcReq.InvalidateCacheHash()
	}
	return false, nil, nil
}

// commitmentAction is the mutation resolveCommitment prescribes for the
// injection hook.
type commitmentAction int

const (
	commitmentSkip     commitmentAction = iota // do nothing; upstream's own default governs
	commitmentExplicit                         // caller already supplied a commitment; honor it
	commitmentSet                              // set commitment on the existing options object at idx
	commitmentAppend                           // append a fresh {commitment} options object
)

// resolveCommitment is the single source of truth for "what commitment will
// actually reach the upstream for this request" — shared by the injection hook
// (which mutates) and GetFinality (which classifies). Keeping one predicate
// guarantees the forwarded/cached commitment and the finality classification
// can never diverge.
//
// Crucially it decides from the request SHAPE + network config, never from
// whether injection has already mutated params, so it returns the same answer
// whether called before injection (e.g. the memoized finality computation in
// erpc/projects.go) or after. It does not mutate.
//
// Returns the effective commitment ("" when unknown — the upstream applies its
// own server-side default), the action injection should take, and the options
// index for commitmentSet.
//
// Shape rules (per commitmentOptionsIndex):
//   - explicit commitment already present                  → (value, commitmentExplicit)
//   - method not injectable / no valid network default     → ("", commitmentSkip)
//   - options slot is an object                            → (default, commitmentSet, idx)
//   - options slot is the next free position               → (default, commitmentAppend)
//   - slot occupied by a non-object (legacy encoding form) → ("", commitmentSkip)
//   - required positional args missing (incl. getBlocks
//     with no start slot)                                  → ("", commitmentSkip)
func resolveCommitment(ctx context.Context, n common.Network, r *common.NormalizedRequest) (string, commitmentAction, int) {
	method, err := r.Method()
	if err != nil {
		return "", commitmentSkip, -1
	}
	rpcReq, err := r.JsonRpcRequest(ctx)
	if err != nil {
		return "", commitmentSkip, -1
	}
	rpcReq.RLock()
	defer rpcReq.RUnlock()

	// 1. A caller-supplied commitment anywhere in the params wins.
	for _, p := range rpcReq.Params {
		if m, ok := p.(map[string]interface{}); ok {
			if v, ok := m["commitment"].(string); ok && v != "" {
				return strings.ToLower(v), commitmentExplicit, -1
			}
		}
	}

	idx, injectable := commitmentOptionsIndex[method]
	if !injectable {
		return "", commitmentSkip, -1
	}

	// 2. Otherwise the network default applies — if it's valid.
	cfg := n.Config()
	if cfg == nil || cfg.Svm == nil || cfg.Svm.Commitment == "" {
		return "", commitmentSkip, -1
	}
	def := strings.ToLower(cfg.Svm.Commitment)
	if def != "finalized" && def != "confirmed" && def != "processed" {
		return "", commitmentSkip, -1
	}

	// 3. Shape decision at the method's options index.
	if idx == optionsAppend {
		// getBlocks: variable arity ([start] or [start,end]); options is the
		// trailing object. Requires at least the start slot.
		if len(rpcReq.Params) == 0 {
			return "", commitmentSkip, -1
		}
		if _, ok := rpcReq.Params[len(rpcReq.Params)-1].(map[string]interface{}); ok {
			return def, commitmentSet, len(rpcReq.Params) - 1
		}
		return def, commitmentAppend, -1
	}
	switch {
	case idx < len(rpcReq.Params):
		if _, ok := rpcReq.Params[idx].(map[string]interface{}); ok {
			return def, commitmentSet, idx
		}
		// Non-object in the options slot (legacy encoding-string form) — leave it.
		return "", commitmentSkip, -1
	case idx == len(rpcReq.Params):
		return def, commitmentAppend, -1
	default:
		// Required positional args missing — don't fabricate them.
		return "", commitmentSkip, -1
	}
}

// writeCommitmentTarget locates the commitment field on a write method's config
// object: which param index the object lives at, and the field name that
// carries the commitment level.
type writeCommitmentTarget struct {
	idx   int
	field string
}

// writeCommitmentField maps the write/effectful methods (excluded from the
// read-path commitmentOptionsIndex) to where their commitment is expressed. Per
// the Solana JSON-RPC reference (https://solana.com/docs/rpc/http) the field name
// differs by method, so this is NOT a blanket "preflightCommitment" — that would
// be wrong for simulateTransaction/requestAirdrop:
//   - sendTransaction:     config at index 1, field "preflightCommitment"
//     (governs the preflight simulation; ignored when skipPreflight is true).
//   - simulateTransaction: config at index 1, field "commitment".
//   - requestAirdrop:      config at index 2, field "commitment".
//
// sendRawTransaction is intentionally absent: it is a non-spec alias carrying a
// raw transaction string with no config object to normalize.
var writeCommitmentField = map[string]writeCommitmentTarget{
	"sendTransaction":     {1, "preflightCommitment"},
	"simulateTransaction": {1, "commitment"},
	"requestAirdrop":      {2, "commitment"},
}

// networkPreForward_injectWriteCommitment stamps the network default commitment
// onto write/effectful methods via their method-specific config field, mirroring
// the read-path injection so every upstream preflights / simulates / airdrops at
// the same commitment regardless of its own server-side default.
//
// These methods are never cached (see neverCacheMethods), so the driver here is
// cross-upstream consistency, not cache-key stability. Like the read path it
// honors a caller-supplied value, skips when no valid network default is set,
// and never corrupts a legacy non-object slot or fabricates missing positional
// args. Non-short-circuiting.
func networkPreForward_injectWriteCommitment(ctx context.Context, n common.Network, r *common.NormalizedRequest) (bool, *common.NormalizedResponse, error) {
	method, err := r.Method()
	if err != nil {
		return false, nil, nil
	}
	target, ok := writeCommitmentField[method]
	if !ok {
		return false, nil, nil
	}

	cfg := n.Config()
	if cfg == nil || cfg.Svm == nil || cfg.Svm.Commitment == "" {
		return false, nil, nil
	}
	def := strings.ToLower(cfg.Svm.Commitment)
	if def != "finalized" && def != "confirmed" && def != "processed" {
		return false, nil, nil
	}

	rpcReq, err := r.JsonRpcRequest(ctx)
	if err != nil {
		return false, nil, nil
	}
	rpcReq.Lock()
	defer rpcReq.Unlock()

	switch {
	case target.idx < len(rpcReq.Params):
		m, ok := rpcReq.Params[target.idx].(map[string]interface{})
		if !ok {
			// Non-object in the config slot (legacy/unexpected shape) — leave it.
			return false, nil, nil
		}
		if _, exists := m[target.field]; exists {
			// Caller already specified the commitment for this method — honor it.
			return false, nil, nil
		}
		m[target.field] = def
		rpcReq.InvalidateCacheHash()
	case target.idx == len(rpcReq.Params):
		rpcReq.Params = append(rpcReq.Params, map[string]interface{}{target.field: def})
		rpcReq.InvalidateCacheHash()
	default:
		// Required positional args missing (e.g. requestAirdrop without lamports)
		// — don't fabricate them; let the upstream report the error.
		return false, nil, nil
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
