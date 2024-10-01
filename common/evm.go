package common

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
