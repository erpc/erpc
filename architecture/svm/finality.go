package svm

import (
	"context"

	"github.com/erpc/erpc/common"
)

// neverCacheMethods returns realtime finality — forcing the cache layer to skip
// these entirely. Two categories, all cross-checked against the Solana JSON-RPC
// reference at https://solana.com/docs/rpc/http:
//
//   - Mutating or effectful: sendTransaction, sendRawTransaction,
//     simulateTransaction, requestAirdrop. Caching these would break
//     at-least-once semantics callers expect.
//   - Transient realtime snapshots: getLatestBlockhash, getRecentBlockhash
//     (deprecated but still live on older validators), getFeeForMessage,
//     getSignatureStatuses, getVoteAccounts, getLeaderSchedule, getEpochInfo,
//     getEpochSchedule, getSlotLeaders, getRecentPerformanceSamples,
//     getRecentPrioritizationFees. These all reflect "now" and go stale in
//     under one slot (~400ms); caching them would surface stale state to the
//     caller without detection.
var neverCacheMethods = map[string]bool{
	"getLatestBlockhash":          true,
	"getRecentBlockhash":          true,
	"getFeeForMessage":            true,
	"sendTransaction":             true,
	"sendRawTransaction":          true,
	"simulateTransaction":         true,
	"getSignatureStatuses":        true,
	"getVoteAccounts":             true,
	"getLeaderSchedule":           true,
	"getEpochInfo":                true,
	"getEpochSchedule":            true,
	"getSlotLeaders":              true,
	"getRecentPerformanceSamples": true,
	"getRecentPrioritizationFees": true,
	"requestAirdrop":              true,
}

// alwaysFinalizedMethods return finalized data by their nature — regardless of
// the request's commitment param, the response is safe to treat as final.
//
// IMPORTANT: only methods finalized by *construction* belong here. getBlock,
// getBlocks, getTransaction, and getSignaturesForAddress do NOT — per the
// Solana JSON-RPC reference they each accept a commitment parameter and will
// return *confirmed* (not yet rooted) data, which can still be dropped on a
// minority-fork switch. Classifying a confirmed response as finalized would let
// it be permanently cached and routed as final. Those methods are instead
// classified by their effective commitment in GetFinality below (finalized only
// when commitment == finalized).
//
// What remains is finalized irrespective of commitment:
//   - getInflationReward: defined only over finalized epochs.
//   - getBlockTime: the production timestamp of a slot; takes no commitment
//     parameter and is stable once the slot exists.
var alwaysFinalizedMethods = map[string]bool{
	"getInflationReward": true,
	"getBlockTime":       true,
}

// GetFinality resolves the finality of an SVM request/response pair. It is
// intentionally a free function (not a method on SvmArchitectureHandler) so
// erpc/networks.go can call it without taking a dependency on the handler's
// concrete type. Priority:
//
//  1. neverCacheMethods       → realtime (no cache)
//  2. alwaysFinalizedMethods  → finalized
//  3. Effective commitment    → finalized / unfinalized
//  4. Unknown (no effective commitment) → unfinalized (safe; TTL bounds staleness)
//
// Step 3 uses resolveCommitment — the SAME predicate the injection hook uses —
// so finality reflects the commitment that actually reaches the upstream, not
// merely whether a network default exists. When injection legitimately skips a
// request (legacy encoding-string form, missing args, non-injectable method),
// no default reaches the upstream and the response is classified Unfinalized
// rather than wrongly trusting the network default. Because resolveCommitment
// reads request shape + config (not mutation state), this is correct whether
// GetFinality runs before or after injection (finality is memoized on the first
// call, which happens pre-injection in erpc/projects.go).
func GetFinality(ctx context.Context, network common.Network, req *common.NormalizedRequest, _ *common.NormalizedResponse) common.DataFinalityState {
	if req == nil {
		return common.DataFinalityStateUnknown
	}

	method, _ := req.Method()
	if method == "" {
		return common.DataFinalityStateUnknown
	}

	if neverCacheMethods[method] {
		return common.DataFinalityStateRealtime
	}
	if alwaysFinalizedMethods[method] {
		return common.DataFinalityStateFinalized
	}

	commitment, _, _ := resolveCommitment(ctx, network, req)
	switch commitment {
	case "finalized":
		return common.DataFinalityStateFinalized
	case "confirmed", "processed":
		return common.DataFinalityStateUnfinalized
	}

	return common.DataFinalityStateUnfinalized
}
