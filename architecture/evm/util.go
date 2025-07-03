package evm

import (
	"strings"
)

func IsWriteMethod(method string) bool {
	return method == "eth_sendRawTransaction" ||
		method == "eth_sendTransaction" ||
		method == "eth_createAccessList" ||
		method == "eth_submitTransaction" ||
		method == "eth_submitWork" ||
		method == "eth_newFilter" ||
		method == "eth_newBlockFilter" ||
		method == "eth_newPendingTransactionFilter"
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
		strings.Contains(txt, "trie does not") ||
		strings.Contains(txt, "greater than latest") ||
		strings.Contains(txt, "not currently canonical") ||
		strings.Contains(txt, "requested data is not available")
}
