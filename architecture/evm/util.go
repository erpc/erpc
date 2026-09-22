package evm

import (
	"strings"
)

// IsNonRetryableWriteMethod returns true for write methods that should NOT be retried/hedged.
// Note: eth_sendRawTransaction is intentionally excluded because it supports idempotency handling.
// Matching is case-insensitive like hook dispatch: a guard against re-sending a
// write must not be escapable by the casing the client chose.
func IsNonRetryableWriteMethod(method string) bool {
	switch strings.ToLower(method) {
	case "eth_sendtransaction",
		"eth_createaccesslist",
		"eth_submittransaction",
		"eth_submitwork",
		"eth_newfilter",
		"eth_newblockfilter",
		"eth_newpendingtransactionfilter":
		return true
	default:
		return false
	}
}

func IsMissingDataError(err error) bool {
	txt := err.Error()
	return strings.Contains(txt, "missing trie node") ||
		strings.Contains(txt, "header not found") ||
		strings.Contains(txt, "could not find block") ||
		strings.Contains(txt, "unknown block") ||
		strings.Contains(txt, "Unknown block") ||
		strings.Contains(txt, "height must be less than or equal") ||
		strings.Contains(txt, "invalid blockhash finalized") ||
		strings.Contains(txt, "Expect block number from id") ||
		strings.Contains(txt, "block not found") ||
		strings.Contains(txt, "Block not found") ||
		strings.Contains(txt, "block height passed is invalid") ||
		// Usually happens on Avalanche when querying a pretty recent block:
		strings.Contains(txt, "cannot query unfinalized") ||
		strings.Contains(txt, "height is not available") ||
		// This usually happens when sending a trace_* request to a newly created block:
		strings.Contains(txt, "genesis is not traceable") ||
		strings.Contains(txt, "could not find FinalizeBlock") ||
		strings.Contains(txt, "no historical rpc") ||
		(strings.Contains(txt, "blocks specified") && strings.Contains(txt, "cannot be found")) ||
		strings.Contains(txt, "transaction not found") ||
		strings.Contains(txt, "cannot find transaction") ||
		strings.Contains(txt, "after last accepted block") ||
		strings.Contains(txt, "No state available") ||
		(strings.Contains(txt, "historical state") && (strings.Contains(txt, "is not available") || strings.Contains(txt, "unavailable"))) ||
		strings.Contains(txt, "trie does not") ||
		strings.Contains(txt, "greater than latest") ||
		strings.Contains(txt, "not currently canonical") ||
		strings.Contains(txt, "requested data is not available")
}
