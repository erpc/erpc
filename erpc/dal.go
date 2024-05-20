package erpc

import (
	"fmt"
	"io"

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

func (dal *EvmDAL) Get(req *common.NormalizedRequest) (string, error) {
	rpcReq, err := req.JsonRpcRequest()
	if err != nil {
		return "", err
	}

	method := rpcReq.Method

	switch method {
	case "eth_getLogs":
		return dal.getLogs(rpcReq)
	default:
		return dal.getSimple(rpcReq)
	}
}

func (dal *EvmDAL) GetWithReader(req *common.NormalizedRequest) (io.Reader, error) {
	rpcReq, err := req.JsonRpcRequest()
	if err != nil {
		return nil, err
	}

	key := fmt.Sprintf("%s:%s", rpcReq.Method, rpcReq.Params)
	return dal.store.GetWithReader(key)
}

func (dal *EvmDAL) getSimple(req *common.JsonRpcRequest) (string, error) {
	key := fmt.Sprintf("%s:%s", req.Method, req.Params)
	rawRespStr, err := dal.store.Get(key)
	if err != nil {
		return "", err
	}
	return rawRespStr, nil
}

func (dal *EvmDAL) getLogs(req *common.JsonRpcRequest) (string, error) {
	// TODO implement advanced filtering and scatter-gather
	return dal.getSimple(req)
}

func (dal *EvmDAL) Set(req *common.NormalizedRequest, value string) (int, error) {
	rpcReq, err := req.JsonRpcRequest()
	if err != nil {
		return 0, err
	}

	key := fmt.Sprintf("%s:%s", rpcReq.Method, rpcReq.Params)

	return dal.store.Set(key, value)
}

func (dal *EvmDAL) SetWithWriter(req *common.NormalizedRequest) (io.WriteCloser, error) {
	rpcReq, err := req.JsonRpcRequest()
	if err != nil {
		return nil, err
	}

	key := fmt.Sprintf("%s:%s", rpcReq.Method, rpcReq.Params)

	return dal.store.SetWithWriter(key)
}

func (dal *EvmDAL) Delete(req *common.NormalizedRequest) error {
	rpcReq, err := req.JsonRpcRequest()
	if err != nil {
		return err
	}

	key := fmt.Sprintf("%s:%s", rpcReq.Method, rpcReq.Params)
	return dal.store.Delete(key)
}
