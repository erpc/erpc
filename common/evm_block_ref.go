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
	methodConfig := getMethodConfig(cacheDal, r.Method)
	if methodConfig == nil {
		// For unknown methods blockRef and/or blockNumber will be empty therefore such data will not be cached despite any caching policies.
		// This strict behavior avoids caching data that we're not aware of its nature.
		return "", 0, nil
	}

	r.RLock()
	defer r.RUnlock()

	if methodConfig.Finalized {
		// Static methods are not expected to change over time so we can cache them forever
		// We use block number 1 as a signal to indicate data is finalized on first ever block
		return "*", 1, nil
	} else if methodConfig.Realtime {
		// Certain methods are expected to always return new data on every new block.
		// For these methods we can always return "*" as blockRef to indicate data it can be cached
		// if there's a 'realtime' cache policy specifically targeting these methods.
		return "*", 0, nil
	}

	var blockRef string
	var blockNumber int64

	if len(methodConfig.ReqRefs) > 0 {
		if len(methodConfig.ReqRefs) == 1 && len(methodConfig.ReqRefs[0]) == 1 && methodConfig.ReqRefs[0][0] == "*" {
			// This special case is for methods that can be cached regardless of their block number or hash
			// e.g. eth_getTransactionReceipt, eth_getTransactionByHash, etc.
			blockRef = "*"
		} else {
			for _, ref := range methodConfig.ReqRefs {
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

func ExtractEvmBlockReferenceFromResponse(cacheDal CacheDAL, rpcReq *JsonRpcRequest, rpcResp *JsonRpcResponse) (string, int64, error) {
	if rpcReq == nil {
		return "", 0, errors.New("cannot extract block reference when json-rpc request is nil")
	}
	if rpcResp == nil {
		return "", 0, errors.New("cannot extract block reference when json-rpc response is nil")
	}

	methodConfig := getMethodConfig(cacheDal, rpcReq.Method)
	if methodConfig == nil || len(rpcResp.Result) == 0 {
		return "", 0, nil
	}

	// Use response refs from method config to extract block reference
	if len(methodConfig.RespRefs) > 0 {
		blockRef := ""
		blockNumber := int64(0)

		for _, ref := range methodConfig.RespRefs {
			if val, err := rpcResp.PeekStringByPath(ref...); err == nil {
				bref, bn, err := parseCompositeBlockParam(val)
				if err != nil {
					return "", 0, err
				}
				if bref != "" && (blockRef == "" || blockRef == "*") {
					blockRef = bref
				}
				if bn > blockNumber {
					blockNumber = bn
				}
			}
		}

		if blockNumber > 0 {
			blockRef = strconv.FormatInt(blockNumber, 10)
		}

		return blockRef, blockNumber, nil
	}

	return "", 0, nil
}

func getMethodConfig(cacheDal CacheDAL, method string) (cfg *CacheMethodConfig) {
	if cacheDal != nil && !cacheDal.IsObjectNull() {
		// First lookup the method in configured cache methods
		cfg = cacheDal.MethodConfig(method)
	}

	if cfg == nil {
		// If cacheDal is nil or empty or missing the method, we should get the method config from the default set of known methods.
		// This is necessary so that usual blockNumber detection used in various flows still resolves correctly.
		cfg = DefaultWithBlockCacheMethods[method]
		if cfg == nil {
			cfg = DefaultSpecialCacheMethods[method]
		}
	}

	return
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
