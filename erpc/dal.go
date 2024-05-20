package erpc

import (
	"context"
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

func (dal *EvmDAL) Get(ctx context.Context, req *common.NormalizedRequest) (string, error) {
	rpcReq, err := req.JsonRpcRequest()
	if err != nil {
		return "", err
	}

	method := rpcReq.Method

	switch method {
	case "eth_getLogs":
		return dal.getLogs(ctx, rpcReq)
	default:
		return dal.getSimple(ctx, rpcReq)
	}
}

func (dal *EvmDAL) GetWithReader(ctx context.Context, req *common.NormalizedRequest) (io.Reader, error) {
	rpcReq, err := req.JsonRpcRequest()
	if err != nil {
		return nil, err
	}

	key := fmt.Sprintf("%s:%s", rpcReq.Method, rpcReq.Params)
	return dal.store.GetWithReader(ctx, key)
}

func (dal *EvmDAL) getSimple(ctx context.Context, req *common.JsonRpcRequest) (string, error) {
	key := fmt.Sprintf("%s:%s", req.Method, req.Params)
	rawRespStr, err := dal.store.Get(ctx, key)
	if err != nil {
		return "", err
	}
	return rawRespStr, nil
}

func (dal *EvmDAL) getLogs(ctx context.Context, req *common.JsonRpcRequest) (string, error) {
	// TODO implement advanced filtering and scatter-gather
	return dal.getSimple(ctx, req)
}

func (dal *EvmDAL) Set(ctx context.Context, req *common.NormalizedRequest, value string) (int, error) {
	rpcReq, err := req.JsonRpcRequest()
	if err != nil {
		return 0, err
	}

	key := fmt.Sprintf("%s:%s", rpcReq.Method, rpcReq.Params)

	return dal.store.Set(ctx, key, value)
}

func (dal *EvmDAL) SetWithWriter(ctx context.Context, req *common.NormalizedRequest) (io.WriteCloser, error) {
	rpcReq, err := req.JsonRpcRequest()
	if err != nil {
		return nil, err
	}

	key := fmt.Sprintf("%s:%s", rpcReq.Method, rpcReq.Params)

	return dal.store.SetWithWriter(ctx, key)
}

func (dal *EvmDAL) Delete(ctx context.Context, req *common.NormalizedRequest) error {
	rpcReq, err := req.JsonRpcRequest()
	if err != nil {
		return err
	}

	key := fmt.Sprintf("%s:%s", rpcReq.Method, rpcReq.Params)
	return dal.store.Delete(ctx, key)
}
