package solana

import (
	"context"
	"strings"

	"github.com/erpc/erpc/common"
)

// neverCacheMethods are always ephemeral regardless of commitment.
// These are methods whose results change every slot/epoch and must never be cached.
var neverCacheMethods = map[string]bool{
	"getLatestBlockhash":          true,
	"getRecentBlockhash":          true, // deprecated but still present
	"getRecentPerformanceSamples": true,
	"getVoteAccounts":             true,
	"getLeaderSchedule":           true,
	"getEpochInfo":                true,
	"getFeeForMessage":            true,
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

	switch strings.ToLower(commitment) {
	case "finalized", "":
		return common.DataFinalityStateFinalized
	case "confirmed":
		return common.DataFinalityStateUnfinalized
	case "processed":
		return common.DataFinalityStateRealtime
	default:
		return common.DataFinalityStateFinalized
	}
}

// extractCommitment pulls the "commitment" field from the last params argument,
// which is typically an object like {"commitment":"finalized",...}.
func extractCommitment(req *common.NormalizedRequest) string {
	jrr, err := req.JsonRpcRequest()
	if err != nil || jrr == nil || len(jrr.Params) == 0 {
		return ""
	}

	// Solana always puts config object as the last param
	last := jrr.Params[len(jrr.Params)-1]

	// Marshal back to bytes so we can unmarshal as a config object
	lastBytes, err2 := common.SonicCfg.Marshal(last)
	if err2 != nil {
		return ""
	}
	var cfg map[string]interface{}
	if err := common.SonicCfg.Unmarshal(lastBytes, &cfg); err != nil {
		return ""
	}
	if c, ok := cfg["commitment"].(string); ok {
		return c
	}
	return ""
}
