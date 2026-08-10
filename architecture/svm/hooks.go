package svm

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Sentinel commitmentOptionsIndex values for methods whose options object is
// the TRAILING param while the positional arity varies, so the injector has to
// locate the slot dynamically instead of at a fixed index.
const (
	// optionsTrailing: no positional arg is required, so the options object may
	// legally be param 0. getLeaderSchedule is the case — its first arg is an
	// OPTIONAL epoch slot which may be omitted or null, and agave accepts a
	// config object in its place (RpcLeaderScheduleConfigWrapper is an untagged
	// SlotOnly|ConfigOnly enum). Shapes: [] | [{cfg}] | [slot] | [slot, {cfg}].
	optionsTrailing = -1
	// optionsTrailingAfterOne: at least one positional arg must precede the
	// options object. getBlocks is the case ([start] | [start, end]); appending
	// a config object to [] would put it where the required start slot belongs.
	optionsTrailingAfterOne = -2
)

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
	"getAccountInfo":                    1,
	"getBalance":                        1,
	"getMinimumBalanceForRentExemption": 1,
	"getBlock":                          1,
	// Same signature as getBlock — slot first, options second — and routed to
	// the same handler, so it needs the same injection index.
	"getConfirmedBlock":       1,
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
	"getBlocks":         optionsTrailingAfterOne,
	"getLeaderSchedule": optionsTrailing,
}

// atLeastConfirmedMethods are the methods whose config rejects
// commitment=processed outright — agave answers -32602 "Method does not
// support commitment below `confirmed`". The Solana JSON-RPC reference
// documents their commitment field as `confirmed | finalized` only
// (https://solana.com/docs/rpc/http): a processed slot can sit on a minority
// fork that is later abandoned, so block/transaction lookups refuse it.
//
// Deliberately NOT in this set (all three accept processed per the same
// reference, verified field-by-field): getBlockProduction, getLeaderSchedule,
// and every write method in writeCommitmentField.
var atLeastConfirmedMethods = map[string]struct{}{
	"getBlock": {},
	// Deprecated alias of getBlock, and agave applies the same restriction to
	// it. Without this the clamp is skipped and a network defaulting to
	// processed injects an unclamped commitment, which the upstream then
	// rejects -32602 — so the alias is broken outright on such a network,
	// while getBlock next to it works.
	"getConfirmedBlock":       {},
	"getBlocks":               {},
	"getBlocksWithLimit":      {},
	"getSignaturesForAddress": {},
	"getTransaction":          {},
}

// clampCommitmentForMethod narrows the commitment we are about to INJECT to a
// level the target method actually accepts.
//
// Policy: CLAMP, not skip. Skipping injection for these methods would leave
// each upstream on its own server-side default — precisely the cross-upstream
// divergence injection exists to eliminate — and would make resolveCommitment
// report "" so the finality classification and cache key lose the commitment
// as well. Clamping to the nearest legal level (processed → confirmed) keeps
// every upstream in lockstep and stays as close as legally possible to the
// operator's "freshest data" intent.
//
// This applies ONLY to the injected network default. A caller-supplied
// commitment is classified commitmentExplicit and never rewritten: if a client
// explicitly asks getBlock for processed, the upstream's -32602 is the honest
// answer and silently upgrading it would hand back data the client did not ask
// for.
func clampCommitmentForMethod(method, commitment string) string {
	if commitment != "processed" {
		return commitment
	}
	if _, narrow := atLeastConfirmedMethods[method]; narrow {
		return "confirmed"
	}
	return commitment
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
//
// The value injected is never blindly the configured default: methods whose
// commitment field is narrower than the configured level get the nearest legal
// level instead (see clampCommitmentForMethod — processed → confirmed for
// getBlock and friends). Injecting an unaccepted level would turn a valid
// request into an upstream -32602. Clamping, rather than skipping injection,
// is the deliberate choice; the rationale is on clampCommitmentForMethod.
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
// index for commitmentSet. The returned default is CLAMPED to a level the
// method accepts (see clampCommitmentForMethod), so the value reported here is
// always the one that will really reach the upstream — never one the upstream
// would answer -32602 to.
//
// Shape rules (per commitmentOptionsIndex):
//   - explicit commitment already present                  → (value, commitmentExplicit)
//   - method not injectable / no valid network default     → ("", commitmentSkip)
//   - options slot is an object                            → (default, commitmentSet, idx)
//   - options slot is the next free position               → (default, commitmentAppend)
//   - slot occupied by a non-object (legacy encoding form) → ("", commitmentSkip)
//   - required positional args missing (incl. getBlocks
//     with no start slot)                                  → ("", commitmentSkip)
//
// Trailing-object methods (negative index) resolve the slot from the current
// arity instead: last param already an object → commitmentSet on it, otherwise
// commitmentAppend — except getBlocks with empty params, whose required start
// slot we refuse to displace.
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
	def = clampCommitmentForMethod(method, def)

	// 3. Shape decision at the method's options index.
	if idx < 0 {
		// Trailing-object shape: the options object is always the last param,
		// but the positional arity varies.
		if len(rpcReq.Params) == 0 {
			if idx == optionsTrailingAfterOne {
				// Required leading positional arg missing — don't fabricate it.
				return "", commitmentSkip, -1
			}
			return def, commitmentAppend, -1
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
// clamps to a level the method accepts, and never corrupts a legacy non-object
// slot or fabricates missing positional args. Non-short-circuiting.
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
	// Same clamp as the read path so one predicate governs every injection. No
	// write method is narrower than processed today, so this is currently a
	// no-op — it is here so a future entry in writeCommitmentField cannot
	// silently reintroduce the "-32602 from an injected level" bug.
	def = clampCommitmentForMethod(method, def)

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

// networkPreForward_getBlock short-circuits getBlock/getConfirmedBlock when the
// requested slot is ahead of what the provider pool has indexed. Solana RPC nodes
// return -32004 for any slot above their maxShredInsertSlot; hitting all upstreams
// only to collect N identical -32004s wastes quota and delays the retry.
//
// When the guard fires it returns ErrEndpointMissingData — the same error class
// that shouldRetryWithReason maps to "missing_data" — so the 500ms indexing-lag
// retry fires immediately without any upstream calls.
//
// Stale-tracker false-reject: the pool's indexedTip is a snapshot refreshed at
// most once per state-poller debounce, while the live confirmed head advances
// ~1 slot per 400ms — so a caller that just learned the head from
// getSlot(confirmed) is routinely 1..N slots above the snapshot. Measured on
// staging (debounce 2s): ~30 false -32014/min at the head, always 1-2 slots
// ahead of the tip. The guard therefore allows a staleness margin above the
// snapshot (see indexedTipStalenessMargin) — within it the request forwards
// (the pool almost certainly indexed the slot since the last poll; the serving
// upstream is often the very one that answered the getSlot). Beyond the margin
// the guard still short-circuits genuinely-future slots, keeping the
// all-upstreams-fail → exhaust → retry cycle eliminated.
//
// Fails OPEN whenever the indexed frontier is unknown. Absence of shred-insert
// tracking is not evidence of unavailability, so a cold poller (or a vendor
// that doesn't expose getMaxShredInsertSlot) must not cost us requests every
// upstream could serve.
func networkPreForward_getBlock(ctx context.Context, n common.Network, r *common.NormalizedRequest) (bool, *common.NormalizedResponse, error) {
	svmNet, ok := n.(common.SvmNetwork)
	if !ok {
		return false, nil, nil
	}
	if !svmNet.SvmEnforceBlockAvailability() {
		return false, nil, nil
	}

	rpcReq, err := r.JsonRpcRequest(ctx)
	if err != nil {
		return false, nil, nil
	}
	rpcReq.RLock()
	defer rpcReq.RUnlock()

	if len(rpcReq.Params) == 0 {
		return false, nil, nil
	}
	slot, ok := toInt64(rpcReq.Params[0])
	if !ok || slot <= 0 {
		return false, nil, nil
	}

	// The indexed frontier is the ONLY sound upper bound here, and only when we
	// actually have it. The finalized tip is not a substitute: maxShredInsertSlot
	// is structurally at or above the processed slot, which is itself far above
	// the finalized slot, so falling back to the finalized tip would reject a
	// wide band of slots that every upstream already holds — the guard would
	// fail CLOSED exactly when it knows least. Unknown frontier → forward.
	indexedTip := svmNet.SvmHighestIndexedSlot(ctx)
	if indexedTip <= 0 {
		return false, nil, nil
	}
	// Overflow-safe form of `slot <= indexedTip + margin`: a hostile or bogus
	// upstream tip near math.MaxInt64 would wrap that addition negative and turn
	// the guard into a reject-everything gate. Subtracting from slot instead
	// cannot wrap — slot is > 0 and the margin is a small positive derived from
	// the poll debounce.
	if slot-indexedTipStalenessMargin(n) <= indexedTip {
		return false, nil, nil
	}

	// Wrap with ErrJsonRpcExceptionInternal(-32014) so TranslateToJsonRpcException
	// preserves the wire code. Without this, the guard returns -32603 to clients;
	// sol-client only maps -32004/-32014 to BlockNotAvailableException (wait-retry).
	return true, nil, common.NewErrEndpointMissingData(
		common.NewErrJsonRpcExceptionInternal(
			svmCodeBlockStatusNotAvail,
			common.JsonRpcErrorMissingData,
			fmt.Sprintf("slot %d not yet indexed by provider pool (tip: %d)", slot, indexedTip),
			nil, nil,
		),
		nil,
	)
}

// indexedTipStalenessMargin returns the slot tolerance the getBlock guard adds
// on top of the pool's indexed frontier. The frontier snapshot is refreshed at
// most once per StatePollerDebounce while the chain advances one slot per
// ~400ms, so the maximum legitimate gap between a live confirmed head and the
// snapshot is roughly debounce/400ms slots; +2 covers tick scheduling and the
// cross-source skew between getSlot (served by the most-ahead upstream) and
// the MAX-over-snapshots frontier. Default debounce (400ms) → margin 3;
// staging's 2s debounce → margin 7.
func indexedTipStalenessMargin(n common.Network) int64 {
	margin := int64(2)
	if cfg := n.Config(); cfg != nil && cfg.Svm != nil {
		if d := time.Duration(cfg.Svm.StatePollerDebounce); d > 0 {
			margin += int64(d / (400 * time.Millisecond))
		}
	}
	return margin
}

// contextSlotMethods are the methods whose result is Solana's `RpcResponse<T>`
// envelope — `{"context":{"slot":…},"value":…}` — which is the ONLY result
// shape that carries a context slot. Enumerated from the Solana JSON-RPC
// reference (https://solana.com/docs/rpc/http).
//
// Every other method returns a bare value (getSlot → integer, getBlock →
// block object, getTransaction → transaction object, ...), so peeking for
// context.slot there is a guaranteed miss. It is not a free miss: the peek
// walks the whole result, and full getBlock responses reach multiple megabytes
// in staging, so the scan was burning CPU proportional to the largest payloads
// we serve in exchange for nothing. Gating on the envelope methods keeps the
// opportunistic harvest and drops the pointless walks.
//
// getProgramAccounts is included even though it only returns the envelope when
// the caller passes withContext:true — its non-envelope form is a small array,
// so the miss is cheap and the hit is worth having.
var contextSlotMethods = map[string]struct{}{
	"getAccountInfo":             {},
	"getBalance":                 {},
	"getBlockProduction":         {},
	"getFeeForMessage":           {},
	"getLargestAccounts":         {},
	"getLatestBlockhash":         {},
	"getMultipleAccounts":        {},
	"getProgramAccounts":         {},
	"getSignatureStatuses":       {},
	"getStakeMinimumDelegation":  {},
	"getSupply":                  {},
	"getTokenAccountBalance":     {},
	"getTokenAccountsByDelegate": {},
	"getTokenAccountsByOwner":    {},
	"getTokenLargestAccounts":    {},
	"getTokenSupply":             {},
	"isBlockhashValid":           {},
	"simulateTransaction":        {},
}

// upstreamPostForward_trackContextSlot peeks at response.result.context.slot
// and feeds it into the upstream's SvmStatePoller. Solana RPC responses
// commonly carry a `context.slot` metadata field that tells us the slot the
// node was at when it answered — using it updates our slot view without
// waiting for the next poll tick, which tightens the freshness window for
// failover decisions AND lets the poller's traffic gate skip redundant
// getSlot calls (see SvmStatePoller.Poll).
//
// The observation is routed by the request's EFFECTIVE commitment (the same
// resolveCommitment predicate injection and finality use): context.slot on a
// finalized-commitment response is a finalized slot, so it feeds the
// finalized view too. A finalized slot is always a valid lower bound for the
// latest view, so it feeds both; weaker commitments feed only latest.
//
// Quietly no-ops on:
//   - nil request / nil response / error response
//   - methods whose result shape cannot carry context.slot (see
//     contextSlotMethods) — checked FIRST, before the response is touched
//   - upstreams without an SvmStatePoller (EVM or early bootstrap)
//   - responses where the slot field is missing or unparseable
//
// The assumption is that any slot reported by a successful upstream is
// usable — we don't try to guard against regressions here; that's the
// state poller's rollback-tolerance job.
func upstreamPostForward_trackContextSlot(ctx context.Context, n common.Network, u common.Upstream, r *common.NormalizedRequest, rs *common.NormalizedResponse) {
	if rs == nil || u == nil || r == nil {
		return
	}
	// Gate on the method before any response inspection: this is the hot path
	// for every SVM response, including multi-megabyte getBlock results.
	method, err := r.Method()
	if err != nil {
		return
	}
	if _, envelope := contextSlotMethods[method]; !envelope {
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
	if n != nil {
		if commitment, _, _ := resolveCommitment(ctx, n, r); commitment == "finalized" {
			poller.SuggestFinalizedSlot(slot)
		}
	}
	poller.SuggestLatestSlot(slot)
}

// upstreamPostForward_nonRetryableWrite marks errors from SVM write methods as
// non-retryable across upstreams. The method set is IsNonRetryableWriteMethod:
// sendTransaction / sendRawTransaction, where a retry against a second upstream
// can double-broadcast once the original tx propagates through the cluster; and
// requestAirdrop, which is genuinely non-idempotent — it MINTS per call, so a
// failover after an effective first attempt mints twice.
// EVM has an analogous guard for eth_sendRawTransaction.
//
// We always wrap as ClientSideException (not just flip retryableTowardNetwork on
// the original error) because the network-level upstream loop bails out on
// common.IsClientError — that check looks specifically for
// ErrCodeEndpointClientSideException rather than walking retryableTowardNetwork
// details. Without the wrap, a ServerSideException from the primary would be
// silently failed over to the secondary.
//
// The wrap applies ONLY to errors from an attempt that may have reached the
// wire. See preDispatchErrorCodes.
func upstreamPostForward_nonRetryableWrite(rs *common.NormalizedResponse, re error) (*common.NormalizedResponse, error) {
	if re == nil {
		return rs, nil
	}
	// A write that was never transmitted cannot have taken effect, so there is
	// nothing to protect against a second dispatch — suppressing failover here
	// turns a routine "this upstream is unavailable" into a hard client error
	// while healthy upstreams sit unused.
	if common.HasErrorCode(re, preDispatchErrorCodes...) {
		return rs, re
	}
	wrapped := common.NewErrEndpointClientSideException(re).WithRetryableTowardNetwork(false)
	return rs, wrapped
}

// preDispatchErrorCodes are failures that PROVE the request never reached the
// upstream's wire: selection/policy rejections, local rate-limit rejections and
// an open circuit breaker are all produced before any bytes are sent. They are
// properties of the ROUTE, not of the transaction, so the single-dispatch
// guarantee is not at stake and the sweep must be free to try the next upstream.
//
// Deliberately excluded: transport errors, timeouts and upstream 5xx. Those
// mean the request may well have been received and executed before the failure
// surfaced — "the server failed" is not permission to run a mint again.
var preDispatchErrorCodes = []common.ErrorCode{
	common.ErrCodeUpstreamRequestSkipped,
	common.ErrCodeUpstreamMethodIgnored,
	common.ErrCodeUpstreamShadowing,
	common.ErrCodeUpstreamNotAllowed,
	common.ErrCodeUpstreamExcludedByPolicy,
	common.ErrCodeUpstreamRateLimitRuleExceeded,
	common.ErrCodeFailsafeCircuitBreakerOpen,
}

// networkPostForward_getSlot enforces the highest-known slot on a getSlot
// response, including cache hits. When the upstream (or cache) returns a slot
// below the tip already observed by this instance, the response is replaced
// with the tip value — ensuring clients never observe the slot number moving
// backwards through a cache window.
//
// It handles getSlot ONLY. getBlockHeight is a different counter (block height
// trails the slot number by every skipped slot) and must never be rewritten to
// a slot value — see HandleNetworkPostForward.
//
// The correction floor is chosen per COMMITMENT, and never exceeds the tip for
// the commitment the caller actually asked for. Raising a confirmed result to
// the processed tip would hand back a slot that is not confirmed and may never
// be (its fork can be abandoned), which is a commitment-contract violation, not
// a freshness improvement.
//
// Unlike EVM eth_blockNumber, SVM getSlot returns a bare integer (not hex) and
// enforcement is unconditional (no EnforceHighestBlock directive gate needed).
func networkPostForward_getSlot(ctx context.Context, network common.Network, nq *common.NormalizedRequest, nr *common.NormalizedResponse, re error) (*common.NormalizedResponse, error) {
	if re != nil || nr == nil {
		return nr, re
	}

	ctx, span := common.StartDetailSpan(ctx, "Network.PostForward.getSlot", trace.WithAttributes(
		attribute.String("request.id", fmt.Sprintf("%v", nq.ID())),
		attribute.String("network.id", network.Id()),
	))
	defer span.End()

	jrr, err := nr.JsonRpcResponse(ctx)
	if err != nil || jrr == nil || jrr.Error != nil {
		common.SetTraceSpanError(span, err)
		return nr, re
	}

	var slotNumber int64
	if err := json.Unmarshal(jrr.GetResultBytes(), &slotNumber); err != nil || slotNumber <= 0 {
		return nr, re
	}

	svmNet, ok := network.(common.SvmNetwork)
	if !ok {
		return nr, re
	}
	reqCtx := context.WithValue(ctx, common.RequestContextKey, nq)

	// For finalized commitment, cap the response to the highest slot the provider
	// pool has actually indexed. getSlot(finalized) reflects the consensus layer;
	// getBlock on a just-finalized slot returns -32004 until the provider's indexer
	// writes it. We use min(finalizedTip, indexedTip) so that neither a slow
	// indexer nor a fast one can push callers into the un-indexed window.
	// When indexedTip is unavailable (poller cold or provider doesn't support
	// getMaxShredInsertSlot) we fall back to finalizedTip - 32, the fixed
	// one-epoch lag that has always been safe for mainnet providers.
	const finalizedIndexingLagFallback = 32

	commitment, _, _ := resolveCommitment(ctx, network, nq)
	var highestSlot int64
	switch commitment {
	case "finalized":
		finalizedTip := svmNet.SvmHighestFinalizedSlot(reqCtx)
		indexedTip := svmNet.SvmHighestIndexedSlot(reqCtx)
		if finalizedTip > 0 && indexedTip > 0 {
			if indexedTip < finalizedTip {
				highestSlot = indexedTip
			} else {
				highestSlot = finalizedTip
			}
		} else if finalizedTip > 0 {
			highestSlot = finalizedTip - finalizedIndexingLagFallback
		}
	case "confirmed":
		// No confirmed tip exists to floor against. The poller tracks the
		// PROCESSED slot in LatestSlot, and processed runs ahead of confirmed —
		// flooring a confirmed answer with it would return an unconfirmed slot
		// to a caller who explicitly asked for confirmed. Pass through
		// uncorrected rather than fabricate a confirmed tip.
		//
		// ponytail: no confirmed tip tracked. Upgrade path — have SvmStatePoller
		// poll getSlot{commitment:confirmed} into its own shared counter and add
		// a branch here that floors against it.
		return nr, re
	default:
		// "processed", or "" when no network default is configured and the
		// upstream's own server-side default governs. LatestSlot IS the
		// processed tip, so the floor is exact for "processed".
		//
		// ponytail: for "" the effective level is whatever the upstream defaults
		// to, which we cannot observe, so we keep the historical processed-tip
		// floor. Configure svm.commitment (the normal deployment) and the
		// effective level is always known here.
		highestSlot = svmNet.SvmHighestLatestSlot(reqCtx)
	}
	// If the state poller hasn't populated the tip yet (e.g. devnet upstreams
	// rate-limiting the poller), fall back to using the response itself as the
	// tip estimate and apply the lag directly. This prevents passing the raw
	// consensus tip to callers when the poller is unavailable — getBlock on an
	// unindexed tip returns -32004. For non-finalized commitments there is no
	// indexing-lag concern, so pass through unchanged.
	if highestSlot <= 0 {
		if commitment != "finalized" {
			return nr, re
		}
		highestSlot = slotNumber - finalizedIndexingLagFallback
		if highestSlot <= 0 {
			return nr, re
		}
	}
	// For finalized: clamp from both directions. Stale cached values are upgraded
	// to highestSlot; fresh values above highestSlot are capped down. Both prevent
	// callers from receiving a slot whose block is not yet indexed by RPC nodes.
	// For non-finalized: only upgrade stale values (no indexing lag concern).
	if slotNumber == highestSlot || (commitment != "finalized" && slotNumber > highestSlot) {
		return nr, re
	}

	method, _ := nq.Method()
	if ups := nr.Upstream(); ups != nil {
		telemetry.MetricUpstreamStaleLatestBlock.WithLabelValues(
			network.ProjectId(), ups.VendorName(), network.Label(), ups.Id(), method,
		).Inc()
		network.Logger().Debug().
			Str("method", method).
			Int64("knownHighestSlot", highestSlot).
			Int64("responseSlot", slotNumber).
			Str("upstreamId", ups.Id()).
			Msg("upstream returned older slot than we know, falling back to highest known slot")
	} else {
		network.Logger().Debug().
			Str("method", method).
			Int64("knownHighestSlot", highestSlot).
			Int64("responseSlot", slotNumber).
			Bool("fromCache", nr.FromCache()).
			Msg("response contains older slot than we know, falling back to highest known slot")
	}

	newJrr, err := common.NewJsonRpcResponse(nq.ID(), highestSlot, nil)
	if err != nil {
		common.SetTraceSpanError(span, err)
		return nil, err
	}
	corrected := common.NewNormalizedResponse().WithRequest(nq).WithJsonRpcResponse(newJrr)
	if nr.FromCache() {
		corrected.WithFromCache(true)
	}
	nr.Release()
	return corrected, nil
}
