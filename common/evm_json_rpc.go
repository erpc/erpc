package common

import (
	"fmt"
)

func NormalizeEvmHttpJsonRpc(nrq *NormalizedRequest, r *JsonRpcRequest) error {
	switch r.Method {
	case "eth_getBlockByNumber",
		"eth_getUncleByBlockNumberAndIndex",
		"eth_getTransactionByBlockNumberAndIndex",
		"eth_getUncleCountByBlockNumber",
		"eth_getBlockTransactionCountByNumber":
		if len(r.Params) > 0 {
			bks, ok := r.Params[0].(string)
			if !ok {
				return fmt.Errorf("invalid block number, must be 0x hex string, or number or latest/finalized")
			}
			ntw := nrq.Network()
			if ntw != nil {
				etk := ntw.EvmBlockTracker()
				if etk != nil {
					switch bks {
					case "latest":
						lb := etk.LatestBlock()
						if lb > 0 {
							lbh, err := NormalizeHex(lb)
							if err == nil {
								r.Params[0] = lbh
							}
						}
					case "finalized":
						fb := etk.FinalizedBlock()
						if fb > 0 {
							fbh, err := NormalizeHex(fb)
							if err == nil {
								r.Params[0] = fbh
							}
						}
					default:
						b, err := NormalizeHex(r.Params[0])
						if err == nil {
							r.Params[0] = b
						}
					}
				}
			}

		}
	case "eth_getBalance",
		"eth_getCode",
		"eth_getTransactionCount",
		"eth_call",
		"eth_estimateGas":
		if len(r.Params) > 1 {
			b, err := NormalizeHex(r.Params[1])
			if err != nil {
				return err
			}
			r.Params[1] = b
		}
	case "eth_getStorageAt":
		if len(r.Params) > 2 {
			b, err := NormalizeHex(r.Params[2])
			if err != nil {
				return err
			}
			r.Params[2] = b
		}
	case "eth_getLogs":
		if len(r.Params) > 0 {
			if paramsMap, ok := r.Params[0].(map[string]interface{}); ok {
				if fromBlock, ok := paramsMap["fromBlock"]; ok {
					b, err := NormalizeHex(fromBlock)
					if err != nil {
						return err
					}
					paramsMap["fromBlock"] = b
				}
				if toBlock, ok := paramsMap["toBlock"]; ok {
					b, err := NormalizeHex(toBlock)
					if err != nil {
						return err
					}
					paramsMap["toBlock"] = b
				}
			}
		}
	}

	return nil
}
