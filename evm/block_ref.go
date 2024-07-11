package evm

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/flair-sdk/erpc/common"
)

func ExtractBlockReference(r *common.JsonRpcRequest) (string, uint64, error) {
	if r == nil {
		return "", 0, errors.New("cannot extract block reference when json-rpc request is nil")
	}

	switch r.Method {
	case "eth_getBlockByNumber",
		"eth_getUncleByBlockNumberAndIndex",
		"eth_getTransactionByBlockNumberAndIndex",
		"eth_getUncleCountByBlockNumber",
		"eth_getBlockTransactionCountByNumber":
		if len(r.Params) > 0 {
			if bns, ok := r.Params[0].(string); ok {
				if strings.HasPrefix(bns, "0x") {
					bni, err := hexutil.DecodeUint64(bns)
					if err != nil {
						return "", 0, err
					}
					return strconv.FormatUint(bni, 10), bni, nil
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
						toUint, err := hexutil.DecodeUint64(to)
						if err != nil {
							return "", 0, err
						}
						// Block ref is combo of from-to which makes sure cache key is unique for this range.
						// Block number is the highest value to ensure non-finalized ranges are not cached.
						return strings.ToLower(fmt.Sprintf("%s-%s", from, to)), toUint, nil
					}
				}
			}
		}

		return "", 0, nil

	case "eth_getBalance",
		"eth_getStorageAt",
		"eth_getCode",
		"eth_getTransactionCount",
		"eth_call",
		"eth_estimateGas":
		if len(r.Params) > 1 {
			if bns, ok := r.Params[1].(string); ok {
				if strings.HasPrefix(bns, "0x") {
					bni, err := hexutil.DecodeUint64(bns)
					if err != nil {
						return bns, 0, err
					}
					return strconv.FormatUint(bni, 10), bni, nil
				} else {
					return "", 0, nil
				}
			}
		} else {
			return "", 0, fmt.Errorf("unexpected missing 2nd parameter for method %s: %+v", r.Method, r.Params)
		}

	case "eth_getBlockByHash":
		if len(r.Params) > 0 {
			if blockHash, ok := r.Params[0].(string); ok {
				return blockHash, 0, nil
			}
			return "", 0, fmt.Errorf("first parameter is not a string for method %s it is %+v", r.Method, r.Params)
		}

	default:
		return "", 0, nil
	}

	return "", 0, nil
}
