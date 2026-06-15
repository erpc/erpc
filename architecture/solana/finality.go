package solana

import (
	"context"
	"strings"

	"github.com/erpc/erpc/common"
)

// neverCacheMethods are always ephemeral regardless of commitment.
// These are methods whose results change every slot/epoch and must never be cached.
var neverCacheMethods = map[string]bool{
	// Blockhash methods — change every ~400ms; caching causes tx rejections
	"getLatestBlockhash":          true,
	"getRecentBlockhash":          true, // deprecated alias
	"getFeeForMessage":            true,
	// Transaction submission — side-effecting; MUST NOT be cached or deduplicated
	"sendTransaction":             true,
	"sendRawTransaction":          true, // some providers expose this alias
	"simulateTransaction":         true,
	// Real-time statuses — meaningful only for in-flight transactions
	"getSignatureStatuses":        true,
	"getSignatureStatus":          true, // singular deprecated alias
	// Cluster / validator state — changes every epoch/slot
	"getVoteAccounts":             true,
	"getLeaderSchedule":           true,
	"getEpochInfo":                true,
	"getEpochSchedule":            true,
	"getSlotLeaders":              true,
	"getRecentPerformanceSamples": true,
	"getRecentPrioritizationFees": true,
	// Airdrops — side-effecting
	"requestAirdrop":              true,
	// Real-time cluster / node state — changes every slot; never safe to cache.
	// (The state poller polls some of these via upstream.Forward directly, which
	// never runs GetFinality / the network cache path, so polling is unaffected;
	// this list only governs client-facing caching.)
	"getSlot":                true,
	"getBlockHeight":         true,
	"getHealth":              true,
	"getFirstAvailableBlock": true, // advances as the node prunes ledger
	"getHighestSnapshotSlot": true,
	"getMaxRetransmitSlot":   true,
	"getMaxShredInsertSlot":  true,
}

// GetFinality maps a Solana request to a DataFinalityState for cache decisions.
//
//   - neverCacheMethods                  → DataFinalityStateRealtime (ephemeral)
//   - "processed" commitment             → DataFinalityStateRealtime (no cache)
//   - "confirmed" commitment             → DataFinalityStateUnfinalized (short TTL)
//   - "finalized" commitment (or absent) → DataFinalityStateFinalized
//
// Commitment governs finality for every non-ephemeral method — including
// historical-data methods like getBlock/getTransaction. Those are immutable
// only once the queried slot is finalized, so a request that explicitly asks
// for the weaker "confirmed"/"processed" commitment (whose result can still be
// rolled back) must NOT be cached as finalized. When no commitment is given,
// Solana itself defaults to "finalized", so absent → Finalized.
func GetFinality(ctx context.Context, _ common.Network, req *common.NormalizedRequest, _ *common.NormalizedResponse) common.DataFinalityState {
	_, span := common.StartDetailSpan(ctx, "solana.GetFinality")
	defer span.End()

	method, err := req.Method()
	if err != nil {
		return common.DataFinalityStateUnknown
	}

	// Never-cache methods take highest priority — ephemeral regardless of commitment.
	if neverCacheMethods[method] {
		return common.DataFinalityStateRealtime
	}

	// Commitment governs finality. Normalise once so "CONFIRMED" still matches.
	switch common.SolanaCommitment(strings.ToLower(extractCommitment(req))) {
	case common.SolanaCommitmentConfirmed:
		return common.DataFinalityStateUnfinalized
	case common.SolanaCommitmentProcessed:
		return common.DataFinalityStateRealtime
	default: // "finalized", "", or any unrecognised value → treat as finalized
		return common.DataFinalityStateFinalized
	}
}

// extractCommitment pulls the "commitment" field from the last params argument,
// which is typically an object like {"commitment":"finalized",...}.
// The params are already decoded by the JSON-RPC parser into []interface{}, so
// we type-assert directly — no marshal/unmarshal round-trip needed.
func extractCommitment(req *common.NormalizedRequest) string {
	jrr, err := req.JsonRpcRequest()
	if err != nil || jrr == nil || len(jrr.Params) == 0 {
		return ""
	}

	// Solana always puts the config object as the last param.
	last := jrr.Params[len(jrr.Params)-1]
	if cfg, ok := last.(map[string]interface{}); ok {
		if c, ok := cfg["commitment"].(string); ok {
			return c
		}
	}
	return ""
}
