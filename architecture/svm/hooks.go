package svm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

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
//   - getSignatureStatuses: older validators ignore commitment. Preserve its
//     processed default instead of injecting a level they cannot honor.
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

// projectPreForward_negativeId answers a negative integer request id the way a
// Solana node does. Agave reads an id as null, a u64, or a string, so a
// negative id fails to parse there and every node answers -32700 "Parse
// error" with id null (HTTP 200), whatever the method. That reply depends on
// the id alone, so it is given here and the request asks no upstream; EVM
// nodes accept any number as an id, so the rule is SVM-only.
func projectPreForward_negativeId(ctx context.Context, r *common.NormalizedRequest) (bool, *common.NormalizedResponse, error) {
	rpcReq, err := r.JsonRpcRequest(ctx)
	if err != nil || rpcReq == nil {
		return false, nil, nil
	}
	if !idUnreadable(rpcReq.IDRawBytes()) {
		return false, nil, nil
	}
	rpcReq.AnswerIDAsNull()
	return true, nil, parseError()
}

// idUnreadable reports whether an agave node fails to read the verbatim id
// bytes: a negative number is neither null, a u64, nor a string.
func idUnreadable(id []byte) bool {
	return bytes.HasPrefix(bytes.TrimSpace(id), []byte("-"))
}

func parseError() error {
	return common.NewErrInvalidRequest(
		common.NewErrJsonRpcExceptionInternal(0, common.JsonRpcErrorParseException, "Parse error", nil, nil),
	)
}

// BatchParseError returns the error an agave node answers for the whole batch
// when one element carries an id it cannot read. agave parses a batch as one
// value, so such an element fails the entire parse: one -32700 with id null,
// nothing run. It returns nil when every id is readable.
func BatchParseError(requests []json.RawMessage) error {
	for _, raw := range requests {
		var envelope struct {
			ID json.RawMessage `json:"id"`
		}
		if common.SonicCfg.Unmarshal(raw, &envelope) == nil && idUnreadable(envelope.ID) {
			return parseError()
		}
	}
	return nil
}

// u64SlotFirstMethods take a u64 slot as params[0].
var u64SlotFirstMethods = map[string]struct{}{
	"getBlock":           {},
	"getBlockTime":       {},
	"getBlockCommitment": {},
	"getBlocks":          {},
	"getBlocksWithLimit": {},
}

// minContextSlotConfigMethods are the read methods whose config object
// declares minContextSlot (agave RpcContextConfig and the configs that embed
// it). Every other method ignores the member, whatever its type, so only
// these reject a malformed one.
var minContextSlotConfigMethods = map[string]struct{}{
	"getAccountInfo":             {},
	"getBalance":                 {},
	"getBlockHeight":             {},
	"getEpochInfo":               {},
	"getFeeForMessage":           {},
	"getInflationReward":         {},
	"getLatestBlockhash":         {},
	"getMultipleAccounts":        {},
	"getProgramAccounts":         {},
	"getSignaturesForAddress":    {},
	"getSlot":                    {},
	"getSlotLeader":              {},
	"getStakeMinimumDelegation":  {},
	"getTokenAccountsByDelegate": {},
	"getTokenAccountsByOwner":    {},
	"getTransactionCount":        {},
	"isBlockhashValid":           {},
	"simulateTransaction":        {},
}

// projectPreForward_u64Params answers, with agave's own -32602, a request whose
// slot or minContextSlot is not a JSON u64: a float (5.0 and 1e12 included), a
// negative or out-of-range integer, a string, a bool, null, a map or an
// array. agave's serde decode rejects such params before the method runs, so
// every upstream would answer the same; eRPC would instead read 5.0 as slot 5,
// clamp a big number, or parse a string. A null minContextSlot is agave's
// "absent" and passes.
func projectPreForward_u64Params(ctx context.Context, r *common.NormalizedRequest, method string) (bool, *common.NormalizedResponse, error) {
	_, slotFirst := u64SlotFirstMethods[method]
	_, mcsConfig := minContextSlotConfigMethods[method]
	if !slotFirst && !mcsConfig {
		return false, nil, nil
	}
	rpcReq, err := r.JsonRpcRequest(ctx)
	if err != nil || rpcReq == nil {
		return false, nil, nil
	}
	rpcReq.RLock()
	params := exactParams(rpcReq)
	invalid := ""
	if slotFirst && len(params) > 0 {
		_, invalid, _ = agaveU64(params[0])
	}
	if mcsConfig && invalid == "" {
		for _, p := range params {
			m, ok := p.(map[string]interface{})
			if !ok {
				continue
			}
			if v, present := m["minContextSlot"]; present && v != nil {
				_, invalid, _ = agaveU64(v)
				break
			}
		}
	}
	rpcReq.RUnlock()
	if invalid == "" {
		return false, nil, nil
	}
	return true, nil, common.NewErrInvalidRequest(
		common.NewErrJsonRpcExceptionInternal(0, common.JsonRpcErrorInvalidArgument, "Invalid params: "+invalid+".", nil, nil),
	)
}

// exactParams returns the params with numbers as json.Number, decoded from
// the client's bytes, when Params lost a number's literal form (see
// JsonRpcRequest.ParamsLiteral); otherwise Params itself. The caller holds
// the request's read lock.
func exactParams(rpcReq *common.JsonRpcRequest) []interface{} {
	lit := rpcReq.ParamsLiteral()
	if lit == nil {
		return rpcReq.Params
	}
	dec := json.NewDecoder(bytes.NewReader(lit))
	dec.UseNumber()
	var params []interface{}
	if dec.Decode(&params) != nil {
		return rpcReq.Params
	}
	return params
}

// agaveU64 decodes v the way agave's serde_json decodes a u64. ok reports a
// valid u64; otherwise invalid is serde's phrase for what v is, or empty when
// v has no agave equivalent (a float literal outside the f64 range).
func agaveU64(v interface{}) (slot uint64, invalid string, ok bool) {
	const want = ", expected u64"
	switch n := v.(type) {
	case json.Number:
		s := n.String()
		if !strings.ContainsAny(s, ".eE") {
			// serde_json reads an integer literal as u64, else i64, else f64;
			// "-0" is the f64 -0.0.
			if u, err := strconv.ParseUint(s, 10, 64); err == nil {
				return u, "", true
			}
			if i, err := strconv.ParseInt(s, 10, 64); err == nil && i != 0 {
				return 0, fmt.Sprintf("invalid value: integer `%d`%s", i, want), false
			}
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0, "", false
		}
		return 0, "invalid type: floating point `" + agaveFloat(f) + "`" + want, false
	case float64:
		// Params keeps a float64 only for a non-negative integer literal of at
		// most 15 digits; anything else arrives as json.Number above.
		if n >= 0 && n == math.Trunc(n) && n < math.MaxUint64 {
			return uint64(n), "", true
		}
		return 0, "invalid type: floating point `" + agaveFloat(n) + "`" + want, false
	case int64:
		if n < 0 {
			return 0, fmt.Sprintf("invalid value: integer `%d`%s", n, want), false
		}
		return uint64(n), "", true
	case int:
		if n < 0 {
			return 0, fmt.Sprintf("invalid value: integer `%d`%s", n, want), false
		}
		return uint64(n), "", true
	case uint64:
		return n, "", true
	case string:
		return 0, "invalid type: string " + rustDebugString(n) + want, false
	case bool:
		return 0, fmt.Sprintf("invalid type: boolean `%t`%s", n, want), false
	case nil:
		return 0, "invalid type: null" + want, false
	case map[string]interface{}:
		return 0, "invalid type: map" + want, false
	case []interface{}:
		return 0, "invalid type: sequence" + want, false
	}
	return 0, "", false
}

// agaveFloat prints f as serde's error messages do: shortest round-trip
// digits, positional with a ".0" for decimal exponents -5..15, else
// scientific with an unpadded signed exponent (1e+16, 1.5e-6).
func agaveFloat(f float64) string {
	sci := strconv.FormatFloat(f, 'e', -1, 64)
	mant, expText, _ := strings.Cut(sci, "e")
	exp, _ := strconv.Atoi(expText)
	if exp < -5 || exp > 15 {
		if exp < 0 {
			return mant + "e-" + strconv.Itoa(-exp)
		}
		return mant + "e+" + strconv.Itoa(exp)
	}
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}

// rustDebugString quotes s as Rust's `{:?}` does, which is how serde names an
// unexpected string.
func rustDebugString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, c := range s {
		switch c {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case 0:
			b.WriteString(`\0`)
		default:
			if unicode.IsGraphic(c) {
				b.WriteRune(c)
			} else {
				fmt.Fprintf(&b, `\u{%x}`, c)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
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

// resolveCommitment is the single source of truth for "what commitment does
// erpc PIN on this request" — shared by the injection hook (which mutates) and
// GetFinality (which classifies cacheability). Keeping one predicate guarantees
// the injected commitment and the finality classification can never diverge.
//
// Crucially it decides from the request SHAPE + network config, never from
// whether injection has already mutated params, so it returns the same answer
// whether called before injection (e.g. the memoized finality computation in
// erpc/projects.go) or after. It does not mutate.
//
// Returns the pinned commitment ("" when NOTHING is pinned — no caller value
// and no injectable network default, so the node's own default governs), the
// action injection should take, and the options index for commitmentSet. The
// returned default is CLAMPED to a level the method accepts (see
// clampCommitmentForMethod), so the value reported here is always the one that
// will really reach the upstream — never one the upstream would answer -32602
// to.
//
// "" is deliberately NOT "the level the node will apply": that is
// effectiveCommitment, which folds in Solana's documented default. Serving and
// routing decisions want effectiveCommitment; only cacheability wants this
// stricter "did we pin it ourselves" reading.
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

// solanaDefaultCommitment is the commitment an agave node applies when a
// request carries none: `CommitmentConfig::default()` is Finalized, and the
// JSON-RPC reference documents "Default: finalized" on every commitment-bearing
// method (https://solana.com/docs/rpc/http/getblock). It is a wire-contract
// fact, not an assumption, so the readers below encode it instead of treating
// an absent commitment as unknowable.
const solanaDefaultCommitment = "finalized"

// effectiveCommitment answers "which commitment will the node actually evaluate
// this request at" — the question every SERVING decision needs: the getBlock
// availability guard, the getSlot tip we advertise, which slot counter routing
// filters compare, and which view a harvested context.slot belongs to.
//
// It differs from resolveCommitment in exactly one case, and that case is the
// common one: when neither the caller nor svm.commitment pins a level,
// resolveCommitment returns "" (nothing to INJECT — its job is the mutation
// plan) while the node still evaluates at finalized. Treating that "" as
// unknown is what left implicit-commitment getBlock traffic bounded by the
// indexed frontier alone — i.e. forwarded into the ~60-slot band above the
// finalized root that every upstream answers -32004 for, which is the band the
// guard exists to eliminate. Same defect on the advertising side: an
// implicit-commitment getSlot was floored at the PROCESSED tip, handing clients
// a slot ~60 above finalized and provoking the very -32004 the guard then
// short-circuits.
//
// Returns "" for methods with no commitment dimension and unpinned
// getSignatureStatuses, whose default is processed rather than finalized.
func effectiveCommitment(ctx context.Context, n common.Network, r *common.NormalizedRequest) string {
	if r == nil {
		return ""
	}
	if commitment, _, _ := resolveCommitment(ctx, n, r); commitment != "" {
		return commitment
	}
	method, err := r.Method()
	if err != nil {
		return ""
	}
	if _, hasCommitment := commitmentOptionsIndex[method]; !hasCommitment {
		return ""
	}
	// The node's own default is never a level the method rejects, so no clamp is
	// needed here (clampCommitmentForMethod only narrows "processed").
	return solanaDefaultCommitment
}

// FailsafeCommitment is the level a network failsafe `matchCommitment` rule
// compares: the caller's commitment, else the svm.commitment default that
// injection writes (clamped per method), else "none". It reads the same pinned
// value the injected params carry, so it agrees with svm.RequestKey — the
// multiplexer and cache key — and gives the same answer before and after
// injection. A value the node does not know (e.g. "recent") is returned as is
// and matches no rule token. Write methods are "none": their commitment field
// governs a preflight or simulation, not the read, and an explicit value would
// otherwise match while the injected default would not.
func FailsafeCommitment(ctx context.Context, n common.Network, r *common.NormalizedRequest) string {
	method, err := r.Method()
	if err != nil {
		return "none"
	}
	if _, readPath := commitmentOptionsIndex[method]; !readPath && method != "getSignatureStatuses" {
		return "none"
	}
	if commitment, _, _ := resolveCommitment(ctx, n, r); commitment != "" {
		return commitment
	}
	return "none"
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
// requested slot is ahead of what the provider pool can actually serve. Hitting
// all upstreams only to collect N identical -32004s wastes quota and delays the
// retry.
//
// The bound is COMMITMENT-DEPENDENT, because "can serve" is:
//
//   - finalized     → min(finalizedMax, indexedTip). An upstream returns -32004
//     for any slot above its own finalized root, whatever it has
//     indexed.
//   - anything else → indexedTip alone. Slots between the finalized and indexed
//     frontiers ARE held at processed/confirmed, so bounding
//     them by the finalized tip would reject a wide band every
//     upstream can answer.
//
// The commitment is the EFFECTIVE one (effectiveCommitment), not merely a
// pinned one: a getBlock that names no commitment is evaluated at finalized by
// the node, so it gets the finalized bound. Gating that case on a configured
// svm.commitment would leave every client that omits the field — the default
// shape of every SDK call — forwarded into the dead band below.
//
// finalizedMax is the MAX finalized root across upstreams
// (SvmHighestFinalizedSlotMax), never the majority tip. This is a REJECTION
// gate: its only job is to avoid short-circuiting a slot that SOME upstream can
// serve, so the most-advanced upstream sets the bound. The majority tip
// (SvmHighestFinalizedSlot) exists for the opposite purpose — advertising a
// head to clients, where a lone rogue upstream must not inflate what we
// promise. Using it here would null out blocks the leading upstream has already
// rooted, exactly the failure SvmHighestIndexedSlot's MAX was chosen to avoid.
// The two are consistent in the safe direction: we advertise no more than
// min(finalizedMajority, indexedTip) and serve up to min(finalizedMax,
// indexedTip), so every advertised slot is inside the served bound.
//
// Collapsing both cases to indexedTip (the historical behaviour) left a ~60-slot
// band — finalized tip up to maxShredInsertSlot — in which finalized-commitment
// requests were forwarded and then rejected by every upstream. Measured on
// solana-devnet that band carried ~71% of all getBlock traffic at a 1.76x
// upstream fan-out.
//
// When the guard fires it returns ErrEndpointMissingData — the same error class
// that shouldRetryWithReason maps to "missing_data" — so the 500ms indexing-lag
// retry fires immediately without any upstream calls.
//
// Stale-tracker false-reject: the pool's indexedTip is a snapshot refreshed at
// most once per state-poller debounce, while the live confirmed head advances
// ~1 slot per 400ms — so a caller that just learned the head from
// getSlot(confirmed) is routinely 1..N slots above the snapshot. Measured on
// staging (debounce 2s): ~30 false rejects/min at the head, always 1-2 slots
// ahead of the tip. The guard therefore allows a staleness margin above the
// snapshot (see tipStalenessMargin) — within it the request forwards
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
	slot := leadingSlotParam(ctx, r)
	if _, beyond := beyondPoolTip(ctx, n, slot, effectiveCommitment(ctx, n, r), tipStalenessMargin(n)); !beyond {
		return false, nil, nil
	}

	// agave's own answer for a slot above what it can serve, so a client cannot
	// tell the local answer from a node's: -32004 (RpcCustomError::
	// BlockNotAvailable), which Solana clients read as wait-and-retry.
	return true, nil, common.NewErrEndpointMissingData(
		common.NewErrJsonRpcExceptionInternal(
			svmCodeBlockNotAvailable,
			common.JsonRpcErrorNumber(svmCodeBlockNotAvailable),
			fmt.Sprintf("Block not available for slot %d", slot),
			nil, nil,
		),
		nil,
	)
}

// networkPreForward_getBlockTime answers getBlockTime locally, with agave's own
// error, when the slot is far beyond what the pool can serve. agave's
// get_block_time reads rooted slots from the blockstore and anything above the
// root from a frozen bank, so the bound is the indexed frontier at every
// commitment (the method takes none) — the non-finalized branch of the getBlock
// guard. An upstream answers `-32004 Block not available for slot N`
// (RpcCustomError::BlockNotAvailable) for such a slot, and so does this guard,
// so a client cannot tell the local answer from a node's.
//
// The margin is maxRetryableSlotDistance, not getBlock's tipStalenessMargin.
// The answer is final (no retry follows a pre-forward answer), while a slot a
// few slots past the last snapshot is often reached within the network's retry
// waits — measured against live upstreams, slots 2-10 past the freshest
// processed slot succeeded after ~0.5-2 s on every run. Within 128 slots the
// read forwards; beyond it no retry round reaches it.
func networkPreForward_getBlockTime(ctx context.Context, n common.Network, r *common.NormalizedRequest) (bool, *common.NormalizedResponse, error) {
	slot := leadingSlotParam(ctx, r)
	if _, beyond := beyondPoolTip(ctx, n, slot, "", maxRetryableSlotDistance); !beyond {
		return false, nil, nil
	}
	return true, nil, common.NewErrEndpointMissingData(
		common.NewErrJsonRpcExceptionInternal(
			svmCodeBlockNotAvailable,
			common.JsonRpcErrorNumber(svmCodeBlockNotAvailable),
			fmt.Sprintf("Block not available for slot %d", slot),
			nil, nil,
		),
		nil,
	)
}

// networkPreForward_minContextSlot answers locally, for any method, a request
// whose minContextSlot option is far beyond what the pool can serve at the
// request's effective commitment. agave's get_bank_with_config rejects such a
// request with `-32016 Minimum context slot has not been reached` and
// `data: {"contextSlot": <its bank slot>}` before reading anything, so every
// upstream, failover, retry and hedge would return that same error. The margin
// is maxRetryableSlotDistance, for the reason given on the getBlockTime guard.
//
// The local -32016 carries agave's meaning only — "no node of this pool has
// reached that slot" — with the pool's tip as contextSlot, so clients back off
// and retry exactly as they do for a node's answer. It is not eRPC's
// JsonRpcErrorUnauthorized, which shares the number; eRPC still never emits
// -32016 for its own verdicts on SVM. The outer class matches what the error
// normalizer builds for an upstream -32016.
func networkPreForward_minContextSlot(ctx context.Context, n common.Network, r *common.NormalizedRequest) (bool, *common.NormalizedResponse, error) {
	mcs := MinContextSlotOf(ctx, r)
	if mcs <= 0 {
		return false, nil, nil
	}
	tip, beyond := beyondPoolTip(ctx, n, uint64(mcs), effectiveCommitment(ctx, n, r), maxRetryableSlotDistance)
	if !beyond {
		return false, nil, nil
	}
	details := map[string]interface{}{
		"data": map[string]interface{}{"contextSlot": tip},
	}
	return true, nil, common.NewErrEndpointServerSideException(
		common.NewErrJsonRpcExceptionInternal(
			svmCodeMinContextSlotNotReached,
			common.JsonRpcErrorNumber(svmCodeMinContextSlotNotReached),
			"Minimum context slot has not been reached",
			nil, details,
		),
		details, http.StatusOK,
	)
}

// leadingSlotParam returns the slot in params[0] exactly as the client sent
// it, u64 range included, or 0 when absent or not a u64.
func leadingSlotParam(ctx context.Context, r *common.NormalizedRequest) uint64 {
	rpcReq, err := r.JsonRpcRequest(ctx)
	if err != nil || rpcReq == nil {
		return 0
	}
	rpcReq.RLock()
	defer rpcReq.RUnlock()
	params := exactParams(rpcReq)
	if len(params) == 0 {
		return 0
	}
	slot, _, ok := agaveU64(params[0])
	if !ok {
		return 0
	}
	return slot
}

// beyondPoolTip reports whether slot is more than margin slots beyond what the
// provider pool can serve at commitment, and returns the tip it was compared
// against. It is the rule shared by the getBlock, getBlockTime and
// minContextSlot guards (see networkPreForward_getBlock for the reasoning); each
// guard picks its margin. It fails open — reports false — when
// svm.enforceBlockAvailability is off, the slot is not positive, or the indexed
// frontier is unknown.
func beyondPoolTip(ctx context.Context, n common.Network, slot uint64, commitment string, margin int64) (int64, bool) {
	if slot == 0 {
		return 0, false
	}
	svmNet, ok := n.(common.SvmNetwork)
	if !ok || !svmNet.SvmEnforceBlockAvailability() {
		return 0, false
	}

	// The indexed frontier bounds every commitment level, and only when we
	// actually have it. Unknown frontier → forward: absence of shred-insert
	// tracking is not evidence of unavailability.
	indexedTip := svmNet.SvmHighestIndexedSlot(ctx)
	if indexedTip <= 0 {
		return 0, false
	}

	// At finalized commitment the indexed frontier is far too loose. It sits at
	// or above the processed slot, which is itself ~60 slots above the finalized
	// slot on Solana, and an upstream answers -32004 for anything above its own
	// finalized root regardless of what it has indexed. Measured on devnet, that
	// band is where ~71% of getBlock requests died: forwarded by this guard,
	// then rejected by every upstream.
	//
	// MAX, not the majority tip: see the rejection-gate rationale on the
	// getBlock guard. A zero value means UNKNOWN, never a literal bound —
	// treating it as one would collapse the bound to 0 and gate every request.
	tip := indexedTip
	if commitment == "finalized" {
		if finalizedTip := svmNet.SvmHighestFinalizedSlotMax(ctx); finalizedTip > 0 && finalizedTip < tip {
			tip = finalizedTip
		}
	}

	// Overflow-safe form of `slot > tip + margin`: a hostile or bogus upstream
	// tip near math.MaxInt64 must not wrap the addition. tip is positive here.
	return tip, slot > uint64(tip) && slot-uint64(tip) > uint64(margin)
}

// tipStalenessMargin returns the slot tolerance the getBlock guard adds on top
// of whichever tip bounds the request — the indexed frontier, or the finalized
// root at finalized commitment. Both are snapshots refreshed at most once per
// StatePollerDebounce while the chain advances one slot per ~400ms, so the
// maximum legitimate gap between a live head and either snapshot is roughly
// debounce/400ms slots; +2 covers tick scheduling and the cross-source skew
// between getSlot (served by the most-ahead upstream) and the MAX-over-snapshots
// tips. Default debounce (400ms) → margin 3; staging's 2s debounce → margin 7.
func tipStalenessMargin(n common.Network) int64 {
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
// The observation is routed by the request's EFFECTIVE commitment
// (effectiveCommitment — what the NODE applied, including its documented
// finalized default when the request pins nothing): context.slot on a
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
		if effectiveCommitment(ctx, n, r) == "finalized" {
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
	//
	// finalizedTip here is the MAJORITY tip (SvmHighestFinalizedSlot), not the
	// pool max the getBlock guard bounds by: advertising a head is where a lone
	// rogue upstream must not inflate what we promise. The asymmetry is safe in
	// one direction only, and this is that direction — every slot we advertise
	// is at or below the guard's served bound.
	const finalizedIndexingLagFallback = 32

	commitment := effectiveCommitment(ctx, network, nq)
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
		// "processed" — LatestSlot IS the processed tip, so the floor is exact.
		//
		// "" cannot reach here: getSlot carries a commitment dimension, so
		// effectiveCommitment always resolves it (to the node's finalized
		// default when nothing pins it). That matters — the previous
		// resolveCommitment reading landed an unpinned getSlot HERE and floored
		// it at the processed tip, ~60 slots above the finalized root, which is
		// precisely the slot band the getBlock guard rejects. We advertised a
		// head we then refused to serve.
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
