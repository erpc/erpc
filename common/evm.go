package common

import "strings"

type EvmNodeType string

const (
	EvmNodeTypeFull      EvmNodeType = "full"
	EvmNodeTypeArchive   EvmNodeType = "archive"
	EvmNodeTypeSequencer EvmNodeType = "sequencer"
	EvmNodeTypeExecution EvmNodeType = "execution"
)

func IsEvmWriteMethod(method string) bool {
	return method == "eth_sendRawTransaction" ||
		method == "eth_sendTransaction" ||
		method == "eth_createAccessList" ||
		method == "eth_submitTransaction" ||
		method == "eth_submitWork" ||
		method == "eth_newFilter" ||
		method == "eth_newBlockFilter" ||
		method == "eth_newPendingTransactionFilter"
}

type EvmStatePoller interface {
	LatestBlock() int64
	FinalizedBlock() int64
	IsBlockFinalized(blockNumber int64) (bool, error)
	SuggestFinalizedBlock(blockNumber int64)
	SuggestLatestBlock(blockNumber int64)
}

func EvmIsMissingDataError(err error) bool {
	return strings.Contains(err.Error(), "missing trie node") ||
		strings.Contains(err.Error(), "header not found") ||
		strings.Contains(err.Error(), "could not find block") ||
		strings.Contains(err.Error(), "unknown block") ||
		strings.Contains(err.Error(), "height must be less than or equal") ||
		strings.Contains(err.Error(), "invalid blockhash finalized") ||
		strings.Contains(err.Error(), "Expect block number from id") ||
		strings.Contains(err.Error(), "block not found") ||
		strings.Contains(err.Error(), "block height passed is invalid") ||
		// Usually happens on Avalanche when querying a pretty recent block:
		strings.Contains(err.Error(), "cannot query unfinalized") ||
		strings.Contains(err.Error(), "height is not available") ||
		// This usually happens when sending a trace_* request to a newly created block:
		strings.Contains(err.Error(), "genesis is not traceable") ||
		strings.Contains(err.Error(), "could not find FinalizeBlock") ||
		strings.Contains(err.Error(), "no historical rpc") ||
		(strings.Contains(err.Error(), "blocks specified") && strings.Contains(err.Error(), "cannot be found")) ||
		strings.Contains(err.Error(), "transaction not found") ||
		strings.Contains(err.Error(), "cannot find transaction") ||
		strings.Contains(err.Error(), "after last accepted block") ||
		strings.Contains(err.Error(), "is greater than latest") ||
		strings.Contains(err.Error(), "No state available")
}

var MethodsWithBlockParam = map[string]int{
	"eth_getBalance":          1,
	"eth_getCode":             1,
	"eth_getTransactionCount": 1,
	"eth_call":                1,
}
