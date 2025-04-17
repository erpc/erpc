package evm

import (
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
)

func BuildEthChainIdRequest() (*common.JsonRpcRequest, error) {
	jrq := common.NewJsonRpcRequest("eth_chainId", []interface{}{})
	err := jrq.SetID(util.RandomID())
	if err != nil {
		return nil, err
	}

	return jrq, nil
}
