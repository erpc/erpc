package common

import (
	"fmt"
	"strings"
)

func NormalizeEvmHttpJsonRpc(nrq *NormalizedRequest, jrq *JsonRpcRequest) {
	jrq.Lock()
	defer jrq.Unlock()

	// Intentionally ignore the error to be more robust against params we don't understand but upstreams would be fine
	switch jrq.Method {
	case "eth_getBlockByNumber",
		"eth_getUncleByBlockNumberAndIndex",
		"eth_getTransactionByBlockNumberAndIndex",
		"eth_getUncleCountByBlockNumber",
		"eth_getBlockTransactionCountByNumber":
		if len(jrq.Params) > 0 {
			bks, ok := jrq.Params[0].(string)
			if !ok {
				bkf, ok := jrq.Params[0].(float64)
				if ok {
					bks = fmt.Sprintf("%f", bkf)
					b, err := NormalizeHex(bks)
					if err == nil {
						jrq.Params[0] = b
					}
				}
			}
			if strings.HasPrefix(bks, "0x") {
				b, err := NormalizeHex(bks)
				if err == nil {
					jrq.Params[0] = b
				}
			}
		}

	case "eth_getBalance",
		"eth_getCode",
		"eth_getTransactionCount",
		"eth_call",
		"eth_estimateGas":
		if len(jrq.Params) > 1 {
			if strValue, ok := jrq.Params[1].(string); ok {
				if strings.HasPrefix(strValue, "0x") {
					b, err := NormalizeHex(strValue)
					if err == nil {
						jrq.Params[1] = b
					}
				}
			} else if mapValue, ok := jrq.Params[1].(map[string]interface{}); ok {
				if blockNumber, ok := mapValue["blockNumber"]; ok {
					b, err := NormalizeHex(blockNumber)
					if err == nil {
						mapValue["blockNumber"] = b
					}
				}
			}
		}
	case "eth_getStorageAt":
		if len(jrq.Params) > 2 {
			if strValue, ok := jrq.Params[2].(string); ok {
				if strings.HasPrefix(strValue, "0x") {
					b, err := NormalizeHex(strValue)
					if err == nil {
						jrq.Params[2] = b
					}
				}
			} else if mapValue, ok := jrq.Params[2].(map[string]interface{}); ok {
				if blockNumber, ok := mapValue["blockNumber"]; ok {
					b, err := NormalizeHex(blockNumber)
					if err == nil {
						mapValue["blockNumber"] = b
					}
				}
			}
		}
	case "eth_getLogs":
		if len(jrq.Params) > 0 {
			if paramsMap, ok := jrq.Params[0].(map[string]interface{}); ok {
				if fromBlock, ok := paramsMap["fromBlock"]; ok {
					b, err := NormalizeHex(fromBlock)
					if err == nil {
						paramsMap["fromBlock"] = b
					}
				}
				if toBlock, ok := paramsMap["toBlock"]; ok {
					b, err := NormalizeHex(toBlock)
					if err == nil {
						paramsMap["toBlock"] = b
					}
				}
			}
		}
	}
}
