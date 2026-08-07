package svm

import (
	"context"

	"github.com/erpc/erpc/common"
)

// neverCacheMethods returns realtime finality — forcing the cache layer to skip
// these entirely. Three categories, cross-checked against the Solana JSON-RPC
// reference at https://solana.com/docs/rpc/http:
//
//   - Mutating or effectful: sendTransaction, sendRawTransaction,
//     simulateTransaction, requestAirdrop. Caching these would break
//     at-least-once semantics callers expect.
//   - Transient realtime snapshots: getLatestBlockhash, getRecentBlockhash
//     (deprecated but still live on older validators), getFeeForMessage,
//     getSignatureStatuses, getVoteAccounts, getLeaderSchedule, getEpochInfo,
//     getSlotLeaders, getRecentPerformanceSamples, getRecentPrioritizationFees.
//     These all reflect "now" and go stale in under one slot (~400ms); caching
//     them would surface stale state to the caller without detection.
//   - Direct balance reads: getBalance, getTokenAccountBalance. These already
//     fall through to Realtime via step 4, but hardcoding them here makes the
//     no-cache intent explicit and guards against a configmap policy with a
//     non-zero realtime TTL inadvertently caching them. Financial callers
//     (e.g. deposit-address sweeps) must never receive a stale balance.
//
// Note: getEpochSchedule is intentionally excluded — epoch schedule constants
// (slotsPerEpoch, leaderScheduleSlotOffset, etc.) only change at epoch
// boundaries (~2 days / 432,000 slots). It falls through to the moving-head
// bucket below and is cached under the realtime policy's TTL.
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
	"getSlotLeaders":              true,
	"getRecentPerformanceSamples": true,
	"getRecentPrioritizationFees": true,
	"requestAirdrop":              true,
	// Direct balance reads — never cache; these feed financial decisions (e.g.
	// deposit-address sweeps) and must never return a stale value.
	"getBalance":             true,
	"getTokenAccountBalance": true,
}

// alwaysFinalizedMethods return finalized data by their nature — regardless of
// the request's commitment param, the response is safe to treat as final.
//
// IMPORTANT: only methods finalized by *construction* belong here — the answer
// must be uniquely determined by the request itself and unable to change once
// it exists. getBlock, getTransaction and friends do NOT qualify unconditionally
// (they accept a commitment parameter and can return *confirmed*, not-yet-rooted
// data that a minority-fork switch can still drop); they live in
// slotPinnedMethods and are promoted only at commitment == finalized.
//
// What remains is finalized irrespective of commitment:
//   - getInflationReward: defined only over finalized epochs.
//   - getBlockTime: the production timestamp of a slot; takes no commitment
//     parameter and is stable once the slot exists.
var alwaysFinalizedMethods = map[string]bool{
	"getInflationReward": true,
	"getBlockTime":       true,
}

// slotPinnedMethods are reads whose answer is uniquely determined by an
// explicit positional identifier in the request — a slot number or a
// transaction signature — rather than by wherever the chain's head happens to
// be. Once that identifier's slot is rooted the answer can never change, so at
// commitment == finalized these (and only these) are genuinely immutable and
// safe to cache indefinitely. Below finalized they are Unfinalized: pinned, but
// still droppable by a fork switch.
//
// Everything NOT listed here is a moving-head read. That is the whole point of
// this table: `commitment: finalized` on Solana means "the state at the latest
// ROOTED slot", and the rooted slot advances roughly every 400ms — it does not
// mean immutable. getBalance/getAccountInfo/getProgramAccounts/... at finalized
// therefore answer a different question every slot, exactly like EVM's `latest`
// tag, and must never be classified Finalized (a Finalized classification plus
// a policy without an explicit TTL is a permanent cache entry, so a later
// transfer would never invalidate the balance).
//
// Deliberately excluded, with reasons:
//   - getSignaturesForAddress: the signature list for an address GROWS as new
//     transactions land, so it tracks the head even though it names an address.
//   - getBlocks / getBlocksWithLimit: range queries. getBlocks(start) with no
//     end slot runs to the current head, and either form can name an upper
//     bound the chain has not reached yet, returning a partial list that grows.
//     Ceiling accepted knowingly: a fully-in-the-past getBlocks(start, end)
//     range IS immutable and gets only realtime-TTL caching here. Promoting it
//     would require comparing end against the poller's finalized slot, which
//     would make finality depend on mutable poller state and therefore vary
//     over time for an identical request. Not worth it — getBlocks is cheap
//     next to getBlock.
//   - minContextSlot is NOT a promotion signal either: per the Solana RPC
//     reference it is the minimum bank slot at which the request may be
//     EVALUATED (a node-freshness floor), not a lower bound on returned
//     history. getBalance(pubkey, {minContextSlot: 1}) still answers at the
//     current head.
var slotPinnedMethods = map[string]bool{
	"getBlock": true,
	// Deprecated alias of getBlock, and routed as one: handler.go dispatches
	// both to networkPreForward_getBlock. Without it here the alias falls to
	// step 4 and classifies Realtime, so a finalized historical read through
	// the old name can never match an immutable cache policy — it gets the
	// availability guard but none of the caching.
	"getConfirmedBlock": true,
	"getTransaction":    true,
}

// GetFinality resolves the finality of an SVM request/response pair. It is
// intentionally a free function (not a method on SvmArchitectureHandler) so
// erpc/networks.go can call it without taking a dependency on the handler's
// concrete type. Priority:
//
//  1. neverCacheMethods      → realtime (the cache layer additionally hard-skips these)
//  2. alwaysFinalizedMethods → finalized (immutable by construction)
//  3. slotPinnedMethods      → finalized at commitment == finalized,
//     otherwise unfinalized (pinned to a slot, but fork-droppable below rooted)
//  4. everything else        → realtime (moving-head read)
//
// Step 4 is the load-bearing rule and the reason step 3 needs a table at all:
// `commitment: finalized` on Solana is the state at the latest ROOTED slot, a
// head that advances roughly every 400ms — NOT an immutability guarantee. Only
// a response pinned to a specific past slot or signature is truly immutable.
// A moving-head read (getBalance, getAccountInfo, getProgramAccounts,
// getTokenAccountBalance, …) is therefore Realtime at every commitment level,
// which is exactly what EVM does with the `latest`/`finalized` block TAGS
// (erpc/networks.go maps any non-numeric blockRef to Realtime). Classifying
// them Finalized would be a permanent-cache bug, since DataFinalityStateFinalized
// is the zero value that a policy with no explicit `finality` matches and an
// unset TTL means "no expiry" in the connectors.
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
	if !slotPinnedMethods[method] {
		// Moving head: the answer changes as the rooted slot advances.
		return common.DataFinalityStateRealtime
	}

	if IsFinalizedCommitment(ctx, network, req) {
		return common.DataFinalityStateFinalized
	}
	// confirmed / processed / unknown: pinned to a slot but not yet rooted, so
	// a fork switch can still replace it.
	return common.DataFinalityStateUnfinalized
}

// IsFinalizedCommitment reports whether the commitment that will actually
// reach the upstream for this request is "finalized".
//
// This is deliberately NOT the same question as GetFinality — since the
// moving-head fix the two concepts have diverged and conflating them is the
// trap to avoid. GetFinality answers "is this RESPONSE immutable enough to
// cache", so getBalance at commitment:finalized is Realtime (the rooted head
// moves every ~400ms). IsFinalizedCommitment answers "which slot does the node
// evaluate this at", which for that same getBalance is finalized — and that is
// the question upstream ROUTING needs when deciding whether an upstream's
// FinalizedSlot (rather than its processed tip) is the right thing to compare
// against. Use this for routing/selection; use GetFinality for cacheability.
//
// Thin wrapper over resolveCommitment so there stays exactly one
// commitment-resolution path shared by injection, finality and routing.
func IsFinalizedCommitment(ctx context.Context, network common.Network, req *common.NormalizedRequest) bool {
	if req == nil {
		return false
	}
	commitment, _, _ := resolveCommitment(ctx, network, req)
	return commitment == "finalized"
}
