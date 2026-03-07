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
}

// alwaysFinalizedMethods are immutable once finalized — cache indefinitely.
var alwaysFinalizedMethods = map[string]bool{
	"getBlock":               true,
	"getTransaction":         true,
	"getConfirmedBlock":      true, // deprecated alias
	"getConfirmedTransaction": true, // deprecated alias
	"getInflationReward":     true,
	"getBlocks":              true,
	"getBlockTime":           true,
	"getSignaturesForAddress": true, // once finalized, historical sigs don't change
}

// GetFinality maps a Solana request + response to a DataFinalityState for cache decisions.
//
//   - "finalized" commitment (or absent) → DataFinalityStateFinalized (immutable)
//   - "confirmed" commitment             → DataFinalityStateUnfinalized (short TTL)
//   - "processed" commitment             → DataFinalityStateRealtime (no cache)
//   - neverCacheMethods                  → DataFinalityStateRealtime
//   - alwaysFinalizedMethods             → DataFinalityStateFinalized
func GetFinality(ctx context.Context, _ common.Network, req *common.NormalizedRequest, _ *common.NormalizedResponse) common.DataFinalityState {
	_, span := common.StartDetailSpan(ctx, "solana.GetFinality")
	defer span.End()

	method, err := req.Method()
	if err != nil {
		return common.DataFinalityStateUnknown
	}

	// Never-cache methods take highest priority
	if neverCacheMethods[method] {
		return common.DataFinalityStateRealtime
	}

	// Always-finalized methods
	if alwaysFinalizedMethods[method] {
		return common.DataFinalityStateFinalized
	}

	// Extract commitment from params
	commitment := extractCommitment(req)

	// Normalise once so clients sending "CONFIRMED" still match.
	switch common.SolanaCommitment(strings.ToLower(commitment)) {
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
