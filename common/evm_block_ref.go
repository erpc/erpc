package common

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// List of methods that static result is expected for. i.e. result will not change over time
var EvmStaticMethods = map[string]bool{
	"eth_chainId": true,
}

func ExtractEvmBlockReference(rpcReq *JsonRpcRequest, rpcResp *JsonRpcResponse) (string, int64, error) {
	blockRef, blockNumber, err := ExtractEvmBlockReferenceFromRequest(rpcReq)
	if err != nil {
		return "", 0, err
	}

	if blockRef == "" || blockNumber == 0 {
		brf, bnm, err := ExtractEvmBlockReferenceFromResponse(rpcReq, rpcResp)
		if err != nil {
			return "", 0, err
		}
		if blockRef == "" && brf != "" {
			blockRef = brf
		}
		if blockNumber == 0 && bnm != 0 {
			blockNumber = bnm
		}
	}

	return blockRef, blockNumber, nil
}

func ExtractEvmBlockReferenceFromRequest(r *JsonRpcRequest) (string, int64, error) {
	if r == nil {
		return "", 0, errors.New("cannot extract block reference when json-rpc request is nil")
	}

	r.RLock()
	defer r.RUnlock()

	if _, ok := EvmStaticMethods[r.Method]; ok {
		// Static methods are not expected to change over time so we can cache them forever
		// We use block number 1 as a signal to indicate data is finalized on first ever block
		return "*", 1, nil
	}

	blockParamIndex := -1
	switch r.Method {
	case "eth_getProof",
		"eth_getStorageAt",
		"arbtrace_call":
		blockParamIndex = 2
	case "eth_feeHistory",
		"eth_getAccount",
		"eth_getBalance",
		"eth_estimateGas",
		"eth_getTransactionCount",
		"eth_getCode",
		"eth_call",
		"debug_traceCall",
		"eth_simulateV1",
		"erigon_getBlockByTimestamp",
		"arbtrace_callMany":
		blockParamIndex = 1
	case "eth_getBlockByNumber",
		"eth_getUncleByBlockNumberAndIndex",
		"eth_getTransactionByBlockNumberAndIndex",
		"eth_getUncleCountByBlockNumber",
		"eth_getBlockTransactionCountByNumber",
		"eth_getBlockReceipts",
		"trace_block",
		"debug_traceBlockByNumber",
		"trace_replayBlockTransactions",
		"debug_storageRangeAt",
		"eth_getTransactionByBlockHashAndIndex",
		"eth_getBlockByHash",
		"debug_traceBlockByHash",
		"debug_getRawBlock",
		"debug_getRawHeader",
		"debug_getRawReceipts",
		"eth_getBlockTransactionCountByHash",
		"eth_getUncleCountByBlockHash",
		"erigon_getHeaderByNumber",
		"arbtrace_block",
		"arbtrace_replayBlockTransactions":
		blockParamIndex = 0
	case "eth_getLogs":
		// Special handling for eth_getLogs
		if len(r.Params) > 0 {
			if logsFilter, ok := r.Params[0].(map[string]interface{}); ok {
				fromRef, fromBn, err := parseCompositeBlockParam(logsFilter["fromBlock"])
				if err != nil {
					return "", 0, err
				}
				toRef, toBn, err := parseCompositeBlockParam(logsFilter["toBlock"])
				if err != nil {
					return "", 0, err
				}
				blockRef := strings.ToLower(fmt.Sprintf("%s-%s", fromRef, toRef))
				// Use the higher block number for caching purposes
				blockNumber := fromBn
				if toBn > fromBn {
					blockNumber = toBn
				}
				return blockRef, blockNumber, nil
			}
		}
	}

	var err error
	var blockRef string
	var blockNumber int64

	if blockParamIndex >= 0 && len(r.Params)-1 >= blockParamIndex {
		blockRef, blockNumber, err = parseCompositeBlockParam(r.Params[blockParamIndex])
		if err != nil {
			return "", 0, err
		}
		if blockRef != "" && blockNumber > 0 {
			return blockRef, blockNumber, nil
		}
	}

	switch r.Method {
	case "eth_getTransactionReceipt",
		"eth_getTransactionByHash",
		"arbtrace_replayTransaction",
		"trace_replayTransaction",
		"debug_traceTransaction",
		"trace_rawTransaction",
		"trace_transaction",
		"debug_traceBlock":
		// For certain data it is safe to keep the data in cache even after reorg,
		// because if client explcitly querying such data (e.g. a specific tx hash receipt)
		// they know it might be reorged from a separate process.
		// For example this is not safe to do for eth_getBlockByNumber because users
		// require this method always give them current accurate data (even if it's reorged).
		// Returning "*" as blockRef means that these data are safe be cached irrevelant of their block.
		return "*", 0, nil

	case "eth_blockNumber",
		"eth_blobBaseFee",
		"eth_hashrate",
		"net_peerCount",
		"eth_gasPrice":
		// Certain methods are expected to always return unfinalized data.
		// For these methods we can always return "*" as blockRef to indicate data it can be cached
		// if there's an 'unfinalized' cache policy specifically targeting these methods.
		return "*", 0, nil
	}

	// For unknown methods blockRef and/or blockNumber will be empty therefore such data will not be cached despite any caching policies.
	// This strict behavior avoids caching data that we're not aware of its nature.
	return blockRef, blockNumber, nil
}

func ExtractEvmBlockReferenceFromResponse(rpcReq *JsonRpcRequest, rpcResp *JsonRpcResponse) (string, int64, error) {
	if rpcReq == nil {
		return "", 0, errors.New("cannot extract block reference when json-rpc request is nil")
	}

	if rpcResp == nil {
		return "", 0, errors.New("cannot extract block reference when json-rpc response is nil")
	}

	switch rpcReq.Method {
	case "eth_getBlockReceipts":
		if len(rpcResp.Result) > 0 {
			blockRef, _ := rpcResp.PeekStringByPath(0, "blockHash")
			blockNumberStr, _ := rpcResp.PeekStringByPath(0, "blockNumber")

			var blockNumber int64
			if blockNumberStr != "" {
				bn, err := HexToInt64(blockNumberStr)
				if err != nil {
					return "", 0, err
				}
				blockNumber = bn
			}

			if blockRef == "" && blockNumber > 0 {
				blockRef = strconv.FormatInt(blockNumber, 10)
			}

			return blockRef, blockNumber, nil
		}
	case "eth_getTransactionReceipt",
		"eth_getTransactionByHash",
		"eth_getTransactionByBlockHashAndIndex",
		"erigon_getBlockReceiptsByBlockHash":
		if len(rpcResp.Result) > 0 {
			blockRef, _ := rpcResp.PeekStringByPath("blockHash")
			blockNumberStr, _ := rpcResp.PeekStringByPath("blockNumber")

			var blockNumber int64
			if blockNumberStr != "" {
				bn, err := HexToInt64(blockNumberStr)
				if err != nil {
					return "", 0, err
				}
				blockNumber = bn
			}

			if blockRef == "" && blockNumber > 0 {
				blockRef = strconv.FormatInt(blockNumber, 10)
			}

			return blockRef, blockNumber, nil
		}
	case "eth_getBlockByNumber",
		"eth_getBlockByHash",
		"erigon_getHeaderByHash",
		"erigon_getHeaderByNumber",
		"eth_getUncleByBlockHashAndIndex":
		if len(rpcResp.Result) > 0 {
			blockRef, _ := rpcResp.PeekStringByPath("hash")
			blockNumberStr, _ := rpcResp.PeekStringByPath("number")

			var blockNumber int64
			if blockNumberStr != "" {
				bn, err := HexToInt64(blockNumberStr)
				if err != nil {
					return "", 0, err
				}
				blockNumber = bn
			}

			if blockRef == "" && blockNumber > 0 {
				blockRef = strconv.FormatInt(blockNumber, 10)
			}

			return blockRef, blockNumber, nil
		}
	}

	return "", 0, nil
}

func parseCompositeBlockParam(param interface{}) (string, int64, error) {
	var blockRef string
	var blockNumber int64

	switch v := param.(type) {
	case string:
		if strings.HasPrefix(v, "0x") {
			if len(v) == 66 { // Block hash
				blockRef = v
			} else {
				// Could be block number in hex
				bni, err := HexToInt64(v)
				if err != nil {
					return blockRef, blockNumber, err
				}
				blockNumber = bni
			}
		} else {
			// Block tag ('latest', 'earliest', 'pending')
			blockRef = v
		}
	case map[string]interface{}:
		// Extract blockHash if present
		if blockHashValue, exists := v["blockHash"]; exists {
			if bh, ok := blockHashValue.(string); ok {
				blockRef = bh
			}
		}
		// Extract blockNumber if present
		if blockNumberValue, exists := v["blockNumber"]; exists {
			if bns, ok := blockNumberValue.(string); ok {
				bni, err := HexToInt64(bns)
				if err != nil {
					return blockRef, blockNumber, err
				}
				blockNumber = bni
			}
		}
		// Extract blockTag if present
		if blockTagValue, exists := v["blockTag"]; exists {
			if bt, ok := blockTagValue.(string); ok {
				blockRef = bt
			}
		}
	}

	if blockRef == "" && blockNumber > 0 {
		blockRef = strconv.FormatInt(blockNumber, 10)
	}

	return blockRef, blockNumber, nil
}
