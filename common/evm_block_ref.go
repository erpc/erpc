package common

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

func ExtractEvmBlockReferenceFromRequest(r *JsonRpcRequest) (string, int64, error) {
	if r == nil {
		return "", 0, errors.New("cannot extract block reference when json-rpc request is nil")
	}

	switch r.Method {
	case "eth_getBlockByNumber",
		"eth_getUncleByBlockNumberAndIndex",
		"eth_getTransactionByBlockNumberAndIndex",
		"eth_getUncleCountByBlockNumber",
		"eth_getBlockTransactionCountByNumber",
		"eth_getBlockReceipts":
		if len(r.Params) > 0 {
			if bns, ok := r.Params[0].(string); ok {
				if strings.HasPrefix(bns, "0x") {
					bni, err := HexToInt64(bns)
					if err != nil {
						return "", 0, err
					}
					return strconv.FormatInt(bni, 10), bni, nil
				} else {
					return "", 0, nil
				}
			}
		} else {
			return "", 0, fmt.Errorf("unexpected no parameters for method %s", r.Method)
		}

	case "eth_getLogs":
		if len(r.Params) > 0 {
			if logsFilter, ok := r.Params[0].(map[string]interface{}); ok {
				if from, ok := logsFilter["fromBlock"].(string); ok {
					if to, ok := logsFilter["toBlock"].(string); ok && strings.HasPrefix(to, "0x") {
						toInt, err := HexToInt64(to)
						if err != nil {
							return "", 0, err
						}
						// Block ref is combo of from-to which makes sure cache key is unique for this range.
						// Block number is the highest value to ensure non-finalized ranges are not cached.
						return strings.ToLower(fmt.Sprintf("%s-%s", from, to)), toInt, nil
					}
				}
			}
		}

		return "", 0, nil

	case "eth_getBalance",
		"eth_getCode",
		"eth_getTransactionCount",
		"eth_call",
		"eth_feeHistory",
		"eth_getAccount":
		if len(r.Params) > 1 {
			if bns, ok := r.Params[1].(string); ok {
				if strings.HasPrefix(bns, "0x") {
					bni, err := HexToInt64(bns)
					if err != nil {
						return bns, 0, err
					}
					return strconv.FormatInt(bni, 10), bni, nil
				} else {
					return "", 0, nil
				}
			}
		} else {
			return "", 0, fmt.Errorf("unexpected missing 2nd parameter for method %s: %+v", r.Method, r.Params)
		}

	case "eth_chainId",
		"eth_getTransactionReceipt",
		"eth_getTransactionByHash":
		// For certain data it is safe to keep the data in cache even after reorg,
		// because if client explcitly querying such data (e.g. a specific tx hash receipt)
		// they know it might be reorged from a separate process.
		// For example this is not safe to do for eth_getBlockByNumber because users
		// require this method always give them current accurate data (even if it's reorged).
		// Returning "*" as blockRef means that these data can be cached irrevelant of their block.
		return "*", 0, nil

	case "eth_getBlockByHash",
		"eth_getTransactionByBlockHashAndIndex",
		"eth_getBlockTransactionCountByHash",
		"eth_getUncleCountByBlockHash":
		if len(r.Params) > 0 {
			if blockHash, ok := r.Params[0].(string); ok {
				return blockHash, 0, nil
			}
			return "", 0, fmt.Errorf("first parameter is not a string for method %s it is %+v", r.Method, r.Params)
		}

	case "eth_getProof",
		"eth_getStorageAt":
		if len(r.Params) > 2 {
			if bns, ok := r.Params[2].(string); ok {
				if strings.HasPrefix(bns, "0x") {
					bni, err := HexToInt64(bns)
					if err != nil {
						return bns, 0, err
					}
					return strconv.FormatInt(bni, 10), bni, nil
				} else {
					return "", 0, nil
				}
			}
		} else {
			return "", 0, fmt.Errorf("unexpected missing 3rd parameter for method %s: %+v", r.Method, r.Params)
		}

	default:
		return "", 0, nil
	}

	return "", 0, nil
}

func ExtractEvmBlockReferenceFromResponse(rpcReq *JsonRpcRequest, rpcResp *JsonRpcResponse) (string, int64, error) {
	if rpcReq == nil {
		return "", 0, errors.New("cannot extract block reference when json-rpc request is nil")
	}

	if rpcResp == nil {
		return "", 0, errors.New("cannot extract block reference when json-rpc response is nil")
	}

	switch rpcReq.Method {
	case "eth_getTransactionReceipt",
		"eth_getTransactionByHash":
		if rpcResp.Result != nil {
			result, err := rpcResp.ParsedResult()
			if err != nil {
				return "", 0, err
			}
			if tx, ok := result.(map[string]interface{}); ok {
				var blockHash string
				var blockNumber int64

				if blockHash, ok := tx["blockHash"].(string); !ok || blockHash == "" {
					return "", 0, nil
				}
				if bns, ok := tx["blockNumber"].(string); ok && bns != "" {
					bn, err := HexToInt64(bns)
					if err != nil {
						return "", 0, err
					}
					blockNumber = bn
				}

				return blockHash, blockNumber, nil
			}
		}

	case "arbtrace_replayTransaction",
		"trace_replayTransaction",
		"debug_traceTransaction",
		"trace_transaction":
		// We cannot extract block number from trace responses, but we will cache them
		// because after a reorg the consumer must not even request this transaction hash,
		// it is not part of the final reorged block.
		// "nil" means there's no specific block reference for this cache item
		// "1" is a placeholder to pass the block number check (is there a cleaner nicer way?)
		//
		// TODO is there a way to find block number without a new request? (e.g. adding a flag to such requests that exposes block number)
		return "nil", 1, nil
	case "eth_chainId":
		// This request is supposed to always return the same response.
		// "all" means this applies to all blocks
		// "1" is a placeholder to pass the block number check (is there a cleaner nicer way?)
		return "all", 1, nil

	default:
		return "", 0, nil
	}

	return "", 0, nil
}
