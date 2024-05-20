package erpc

import (
	"encoding/json"
	"fmt"

	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/data"
)

type EvmLog struct {
	LogIndex         string
	TransactionIndex string
	TransactionHash  string
	BlockHash        string
	BlockNumber      string
	Address          string
	Topics           []string
	Data             string
}

type EvmLogFilter struct {
	FromBlock string
	ToBlock   string
	Address   string
	Topics    []string
}

type EvmDAL struct {
	store data.Store
}

func NewEvmDAL(store data.Store) *EvmDAL {
	return &EvmDAL{
		store: store,
	}
}

func (dal *EvmDAL) Get(req *common.NormalizedRequest) ([]byte, error) {
	rpcReq, err := req.JsonRpcRequest()
	if err != nil {
		return nil, err
	}

	method := rpcReq.Method

	switch method {
	case "eth_getLogs":
		return dal.getLogs(rpcReq)
	default:
		return dal.getSimple(rpcReq)
	}
}

func (dal *EvmDAL) getSimple(req *common.JsonRpcRequest) ([]byte, error) {
	key := fmt.Sprintf("%s:%s", req.Method, req.Params)
	rawRespStr, err := dal.store.Get(key)
	if err != nil {
		return nil, err
	}
	return rawRespStr, nil
}

func (dal *EvmDAL) getLogs(req *common.JsonRpcRequest) ([]byte, error) {
	return dal.getSimple(req)
}

func (dal *EvmDAL) Set(req *common.NormalizedRequest, value interface{}) (int, error) {
	rpcReq, err := req.JsonRpcRequest()
	if err != nil {
		return 0, err
	}

	key := fmt.Sprintf("%s:%s", rpcReq.Method, rpcReq.Params)
	valueStr, err := json.Marshal(value)
	if err != nil {
		return 0, fmt.Errorf("failed to marshal value in EvmDAL: %w", err)
	}

	return dal.store.Set(key, valueStr)
}

func (dal *EvmDAL) Delete(req *common.NormalizedRequest) error {
	rpcReq, err := req.JsonRpcRequest()
	if err != nil {
		return err
	}

	key := fmt.Sprintf("%s:%s", rpcReq.Method, rpcReq.Params)
	return dal.store.Delete(key)
}
