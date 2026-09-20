package svm

// Commitment-aware consensus matching for SVM networks.
//
// erpc's consensus fan-out sends the same logical request to several
// upstreams and groups the responses by canonical hash — the assumption
// being that identical requests yield identical bytes. On SVM networks
// that assumption has a hole: the commitment level decides WHICH view of
// the ledger a response was produced against, and participants can
// legitimately be evaluated at different levels for the same request:
//
//   - the caller pins nothing and the network's svm.commitment is unset,
//     so each upstream's own server-side default governs (Solana's is
//     finalized, but forks and vendor deployments may differ);
//   - the network pins one level but operator policy is mid-migration and
//     some upstream groups still answer at another.
//
// Two upstreams returning different bytes because they were queried at
// different commitments is NOT disagreement — it is the protocol working
// as specified. Without separating these responses into per-commitment
// groups, consensus either fabricates disputes or crowns a winner from
// incompatible views.
//
// CommitmentMatchKey implements the match-key contract for the consensus
// executor: it derives a stable, architecture-specific bucket per response
// from the request that produced it. The classification below mirrors the
// wire contract (https://solana.com/docs/rpc/http, and agave's RPC
// implementation, which rejects processed for the slot-pinned reads):
//
//   - CommitmentClassStandard: all three levels accepted; match on the
//     exact level (unpinned requests land in their own "unpinned" bucket).
//   - CommitmentClassAtLeastConfirmed: agave rejects processed with
//     -32602 for these (see atLeastConfirmedMethods in hooks.go); the key
//     clamps processed to confirmed, exactly like the injection hook.
//   - CommitmentClassNone: the method has no commitment parameter at all;
//     the key is empty so responses group exactly as before.
//
// Deprecated commitment aliases (recent, single, singleGossip, root, max)
// are normalized to their modern level — agave has treated them as aliases
// since v1.5.5 and the Helius commitment reference documents the mapping.

import (
	"context"
	"strings"

	"github.com/erpc/erpc/common"
)

// CommitmentClass partitions the SVM JSON-RPC surface by how a method
// relates to the commitment parameter.
type CommitmentClass int

const (
	// CommitmentClassStandard: methods that accept processed, confirmed and
	// finalized. Two responses are comparable only at the same level.
	CommitmentClassStandard CommitmentClass = iota

	// CommitmentClassAtLeastConfirmed: slot-pinned reads where agave rejects
	// processed outright (-32602). The effective floor is confirmed, so the
	// match key clamps processed to confirmed instead of splitting a bucket
	// the upstream would never serve.
	CommitmentClassAtLeastConfirmed

	// CommitmentClassNone: methods with no commitment parameter. The
	// response carries no commitment dimension, so the match key is empty
	// and grouping is unchanged.
	CommitmentClassNone
)

// noCommitmentMethods is the set of SVM JSON-RPC methods whose parameters
// contain no commitment dimension — per the HTTP API reference
// (https://solana.com/docs/rpc/http). For these, any "commitment" key a
// caller smuggles into the params object is ignored by the node, so it
// must not split consensus groups either.
//
// sendTransaction/sendRawTransaction live here deliberately: their config
// object carries preflightCommitment (which only steers simulation), the
// response is the transaction signature regardless, and broadcasts
// short-circuit consensus anyway.
var noCommitmentMethods = map[string]struct{}{
	"getBlockCommitment":          {},
	"getBlockTime":                {},
	"getClusterNodes":             {},
	"getEpochSchedule":            {},
	"getFeeRateGovernor":          {},
	"getFirstAvailableBlock":      {},
	"getGenesisHash":              {},
	"getHealth":                   {},
	"getIdentity":                 {},
	"getInflationRate":            {},
	"getMaxRetransmitSlot":        {},
	"getMaxShredInsertSlot":       {},
	"getMinimumLedgerSlot":        {},
	"getRecentPerformanceSamples": {},
	"getSignatureStatuses":        {},
	"getSnapshotSlot":             {},
	"getVersion":                  {},
	"sendRawTransaction":          {},
	"sendTransaction":             {},
}

// CommitmentClassForMethod classifies a method. Unknown methods default to
// CommitmentClassStandard — the conservative choice: they are matched on
// their exact commitment level rather than silently grouped.
func CommitmentClassForMethod(method string) CommitmentClass {
	if _, ok := noCommitmentMethods[method]; ok {
		return CommitmentClassNone
	}
	if _, ok := atLeastConfirmedMethods[method]; ok {
		return CommitmentClassAtLeastConfirmed
	}
	return CommitmentClassStandard
}

// CommitmentMatchKey derives the consensus match bucket for one consensus
// participant's response. It is the SVM implementation of the generic
// match-key hook installed by erpc/networks_registry.go on SVM networks.
//
// Returns "" when no key applies (nil inputs, unresolvable method, or a
// CommitmentClassNone method) — the response then groups by its plain
// canonical hash, preserving today's behavior for methods without a
// commitment dimension.
func CommitmentMatchKey(ctx context.Context, resp *common.NormalizedResponse) string {
	if resp == nil {
		return ""
	}
	req := resp.Request()
	if req == nil {
		return ""
	}
	method, err := req.Method()
	if err != nil || method == "" {
		return ""
	}
	switch CommitmentClassForMethod(method) {
	case CommitmentClassNone:
		return ""
	case CommitmentClassAtLeastConfirmed:
		if c := callerCommitment(ctx, req); c != "" && c != "processed" {
			return c
		}
		// processed (or an alias of it) on a slot-pinned read: the node
		// rejects it, but if anything downstream still evaluates the
		// request, the floor every participant can answer at is confirmed.
		return "confirmed"
	default: // CommitmentClassStandard
		if c := callerCommitment(ctx, req); c != "" {
			return c
		}
		// No caller-supplied commitment and no injection: each upstream's
		// server-side default governs. Keep those responses in their own
		// bucket so differing vendor defaults surface as separate groups
		// instead of a fabricated dispute inside one.
		return "unpinned"
	}
}

// callerCommitment extracts the caller-supplied commitment from a request's
// params, normalized through normalizeCommitmentLevel. It scans the params
// exactly like resolveCommitment's step 1 — but deliberately without the
// network-default fallback: the match key must reflect what THIS request
// pinned, not what the network would inject (injection stamps params before
// fan-out, so injected levels are already visible here as caller-supplied).
func callerCommitment(ctx context.Context, r *common.NormalizedRequest) string {
	rpcReq, err := r.JsonRpcRequest(ctx)
	if err != nil || rpcReq == nil {
		return ""
	}
	rpcReq.RLock()
	defer rpcReq.RUnlock()
	for _, p := range rpcReq.Params {
		if m, ok := p.(map[string]interface{}); ok {
			if v, ok := m["commitment"].(string); ok && v != "" {
				return normalizeCommitmentLevel(v)
			}
		}
	}
	return ""
}

// normalizeCommitmentLevel lowercases a commitment value and maps the
// deprecated aliases agave still accepts (since v1.5.5) onto the modern
// three levels, per the Helius commitment reference:
//
//	recent         -> processed
//	singleGossip   -> confirmed
//	single         -> confirmed
//	root           -> finalized
//	max            -> finalized
//
// Unknown values pass through lowercased: the match key groups by whatever
// the caller sent, and the upstream's -32602 validation is the backstop.
func normalizeCommitmentLevel(v string) string {
	switch strings.ToLower(v) {
	case "processed", "recent":
		return "processed"
	case "confirmed", "single", "singlegossip":
		return "confirmed"
	case "finalized", "root", "max":
		return "finalized"
	default:
		return strings.ToLower(v)
	}
}
