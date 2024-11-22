package common

import (
	"errors"
	"strconv"
	"strings"
)

func ExtractEvmBlockReferenceFromRequest(cacheDal CacheDAL, r *JsonRpcRequest) (string, int64, error) {
	if r == nil {
		return "", 0, errors.New("cannot extract block reference when json-rpc request is nil")
	}
	if cacheDal == nil || cacheDal.IsObjectNull() {
		return "", 0, nil
	}

	r.RLock()
	defer r.RUnlock()

	if cfg := cacheDal.MethodConfig(r.Method); cfg != nil {
		if cfg.Finalized {
			// Static methods are not expected to change over time so we can cache them forever
			// We use block number 1 as a signal to indicate data is finalized on first ever block
			return "*", 1, nil
		} else if cfg.Realtime {
			// Certain methods are expected to always return new data on every new block.
			// For these methods we can always return "*" as blockRef to indicate data it can be cached
			// if there's a 'realtime' cache policy specifically targeting these methods.
			return "*", 0, nil
		}

		var blockRef string
		var blockNumber int64

		if len(cfg.ReqRefs) > 0 {
			if len(cfg.ReqRefs) == 1 && len(cfg.ReqRefs[0]) == 1 && cfg.ReqRefs[0][0] == "*" {
				// This special case is for methods that can be cached regardless of their block number or hash
				// e.g. eth_getTransactionReceipt, eth_getTransactionByHash, etc.
				blockRef = "*"
			} else {
				for _, ref := range cfg.ReqRefs {
					if val, err := r.PeekByPath(ref...); err == nil {
						bref, bn, err := parseCompositeBlockParam(val)
						if err != nil {
							return "", 0, err
						}
						if bref != "" {
							if blockRef == "" {
								blockRef = bref
							} else {
								// This special case is when a method has multiple block parameters (eth_getLogs)
								// We can't use a specific block reference because later if reorg invalidation is added
								// it is not easy to check if this cache entry includes that reorged block.
								// Therefore users must ONLY cache finalized data for these methods (or short TTLs for unfinalized data).
								// Later we'll either need bespoke caching logic for these methods, or find a nice way
								// to invalidate these cache entries when ANY of their blocks are reorged.
								blockRef = "*"
							}
						}
						if bn > blockNumber {
							blockNumber = bn
						}
					} else {
						return "", 0, err
					}
				}
			}
		}

		return blockRef, blockNumber, nil
	}

	// For unknown methods blockRef and/or blockNumber will be empty therefore such data will not be cached despite any caching policies.
	// This strict behavior avoids caching data that we're not aware of its nature.
	return "", 0, nil
}

func ExtractEvmBlockReferenceFromResponse(cacheDal CacheDAL, rpcReq *JsonRpcRequest, rpcResp *JsonRpcResponse) (string, int64, error) {
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
