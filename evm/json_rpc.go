package evm

import (
	"fmt"

	"github.com/erpc/erpc/common"
)

func NormalizeHttpJsonRpc(nrq common.NormalizedRequest, r *common.JsonRpcRequest) error {
	switch r.Method {
	case "eth_getBlockByNumber",
		"eth_getUncleByBlockNumberAndIndex",
		"eth_getTransactionByBlockNumberAndIndex",
		"eth_getUncleCountByBlockNumber",
		"eth_getBlockTransactionCountByNumber":
		if len(r.Params) > 0 {
			bks, ok := r.Params[0].(string)
			if !ok {
				return fmt.Errorf("invalid block number, must be 0x hex string, or numer or latest/finalized")
			}
			ntw := nrq.Network()
			if ntw != nil {
				etk := ntw.EvmBlockTracker()
				if etk != nil {
					switch bks {
					case "latest":
						lb := etk.LatestBlock()
						if lb > 0 {
							lbh, err := common.NormalizeHex(lb)
							if err == nil {
								r.Params[0] = lbh
							}
						}
					case "finalized":
						fb := etk.FinalizedBlock()
						if fb > 0 {
							fbh, err := common.NormalizeHex(fb)
							if err == nil {
								r.Params[0] = fbh
							}
						}
					default:
						b, err := common.NormalizeHex(r.Params[0])
						if err == nil {
							r.Params[0] = b
						}
					}
				}
			}

		}
	case "eth_getBalance",
		"eth_getStorageAt",
		"eth_getCode",
		"eth_getTransactionCount",
		"eth_call",
		"eth_estimateGas":
		if len(r.Params) > 1 {
			b, err := common.NormalizeHex(r.Params[1])
			if err != nil {
				return err
			}
			r.Params[1] = b
		}
	case "eth_getLogs":
		if len(r.Params) > 0 {
			if paramsMap, ok := r.Params[0].(map[string]interface{}); ok {
				if fromBlock, ok := paramsMap["fromBlock"]; ok {
					b, err := common.NormalizeHex(fromBlock)
					if err != nil {
						return err
					}
					paramsMap["fromBlock"] = b
				}
				if toBlock, ok := paramsMap["toBlock"]; ok {
					b, err := common.NormalizeHex(toBlock)
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
