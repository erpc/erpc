package data

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/config"
	"github.com/rs/zerolog/log"
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

type EvmJsonRpcCache struct {
	conn Connector
}

const (
	JsonRpcRequestKey  common.ContextKey = "jsonRpcReq"
	JsonRpcResponseKey common.ContextKey = "jsonRpcRes"
)

func NewEvmJsonRpcCache(cfg *config.ConnectorConfig) (*EvmJsonRpcCache, error) {
	err := populateDefaults(cfg)
	if err != nil {
		return nil, err
	}

	c, err := NewConnector(
		cfg,
		resolveEvmJsonRpcCacheKeys,
		resolveOnlySuccessfulResponses,
	)
	if err != nil {
		return nil, err
	}

	return &EvmJsonRpcCache{
		conn: c,
	}, nil
}

func (c *EvmJsonRpcCache) GetWithReader(ctx context.Context, req *common.NormalizedRequest) (io.Reader, error) {
	rpcReq, err := req.JsonRpcRequest()
	if err != nil {
		return nil, err
	}

	cacheKey, err := generateCacheKey(rpcReq)
	if err != nil {
		return nil, err
	}
	requestKey := fmt.Sprintf("%s:%s", req.NetworkId, cacheKey)

	blockNumber, _ := extractBlockReferenceFromRequest(rpcReq)
	if blockNumber != "" {
		groupKey := fmt.Sprintf("evm:%s:%s", req.NetworkId, blockNumber)
		return c.conn.GetWithReader(ctx, ConnectorMainIndex, groupKey, requestKey)
	} else {
		groupKey := fmt.Sprintf("evm:%s:*", req.NetworkId)
		return c.conn.GetWithReader(ctx, ConnectorReverseIndex, groupKey, requestKey)
	}
}

func (c *EvmJsonRpcCache) SetWithWriter(ctx context.Context, req *common.NormalizedRequest) (io.WriteCloser, error) {
	rpcReq, err := req.JsonRpcRequest()
	if err != nil {
		return nil, err
	}

	blockNumber, err := extractBlockReferenceFromRequest(rpcReq)
	if err != nil {
		return nil, err
	}

	if blockNumber != "" {
		groupKey := fmt.Sprintf("evm:%s:%s", req.NetworkId, blockNumber)
		cacheKey, err := generateCacheKey(rpcReq)
		if err != nil {
			return nil, err
		}
		requestKey := fmt.Sprintf("%s:%s", req.NetworkId, cacheKey)
		return c.conn.SetWithWriter(ctx, groupKey, requestKey)
	} else {
		// Empty keys forces the connector to use the keyResolver when response is fully received
		ctx = context.WithValue(ctx, JsonRpcRequestKey, req)
		return c.conn.SetWithWriter(ctx, "", "")
	}
}

func (c *EvmJsonRpcCache) DeleteByGroupKey(ctx context.Context, groupKeys ...string) error {
	for _, groupKey := range groupKeys {
		err := c.conn.Delete(ctx, ConnectorMainIndex, groupKey, "")
		if err != nil {
			return err
		}
	}

	return nil
}

func generateCacheKey(r *common.JsonRpcRequest) (string, error) {
	hasher := sha256.New()

	for _, p := range r.Params {
		err := hashValue(hasher, p)
		if err != nil {
			return "", err
		}
	}

	b := sha256.Sum256(hasher.Sum(nil))
	return fmt.Sprintf("%s:%x", r.Method, b), nil
}

func hashValue(h io.Writer, v interface{}) error {
	switch t := v.(type) {
	case bool:
		_, err := h.Write([]byte(fmt.Sprintf("%t", t)))
		return err
	case int:
		_, err := h.Write([]byte(fmt.Sprintf("%d", t)))
		return err
	case float64:
		_, err := h.Write([]byte(fmt.Sprintf("%f", t)))
		return err
	case string:
		_, err := h.Write([]byte(t))
		return err
	case []interface{}:
		for _, i := range t {
			err := hashValue(h, i)
			if err != nil {
				return err
			}
		}
		return nil
	case map[string]interface{}:
		for k, i := range t {
			if _, err := h.Write([]byte(k)); err != nil {
				return err
			}
			err := hashValue(h, i)
			if err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("unsupported type for value: %+v", v)
	}
}

func resolveEvmJsonRpcCacheKeys(ctx context.Context, dv *DataValue) (pk string, rk string, err error) {
	req := ctx.Value(JsonRpcRequestKey).(*common.NormalizedRequest)
	rpcReq, err := req.JsonRpcRequest()
	if err != nil {
		return "", "", err
	}

	rpcRes, err := dv.AsJsonRpcResponse()
	if err != nil {
		return "", "", err
	}
	if rpcRes == nil {
		return "", "", errors.New("json-rpc response is nil")
	}

	blockNumber, err := extractBlockReferenceFromResponse(rpcReq, rpcRes)
	if err != nil {
		return "", "", err
	}

	if blockNumber != "" {
		return fmt.Sprintf("evm:%s:%s", req.NetworkId, blockNumber), fmt.Sprintf("%s:%s:%s", req.NetworkId, rpcReq.Method, rpcReq.Params), nil
	} else {
		return fmt.Sprintf("evm:%s:nil", req.NetworkId), fmt.Sprintf("%s:%s:%s", req.NetworkId, rpcReq.Method, rpcReq.Params), nil
	}
}

func resolveOnlySuccessfulResponses(_ context.Context, dv *DataValue) (*DataValue, error) {
	e, err := dv.AsJsonRpcResponse()
	if err != nil {
		return nil, err
	}

	if e.Error != nil || e.Result == nil {
		return nil, nil
	}

	return dv, nil
}

func populateDefaults(cfg *config.ConnectorConfig) error {
	switch cfg.Driver {
	case DynamoDBDriverName:
		if cfg.DynamoDB.Table == "" {
			cfg.DynamoDB.Table = "erpc_json_rpc_cache"
		}
		if cfg.DynamoDB.PartitionKeyName == "" {
			cfg.DynamoDB.PartitionKeyName = "groupKey"
		}
		if cfg.DynamoDB.RangeKeyName == "" {
			cfg.DynamoDB.RangeKeyName = "requestKey"
		}
		if cfg.DynamoDB.ReverseIndexName == "" {
			cfg.DynamoDB.ReverseIndexName = "idx_groupKey_requestKey"
		}

	default:
		return common.NewErrInvalidConnectorDriver(cfg.Driver)
	}

	return nil
}

func extractBlockReferenceFromRequest(r *common.JsonRpcRequest) (string, error) {
	if r == nil {
		return "", errors.New("cannot extract block reference when json-rpc request is nil")
	}

	switch r.Method {
	case "eth_getBlockByNumber",
		"eth_getUncleByBlockNumberAndIndex",
		"eth_getTransactionByBlockNumberAndIndex",
		"eth_getUncleCountByBlockNumber",
		"eth_getBlockTransactionCountByNumber":
		if len(r.Params) > 0 {
			return fmt.Sprintf("%s", r.Params[0]), nil
		} else {
			return "", fmt.Errorf("unexpected no parameters for method %s", r.Method)
		}

	case "eth_getBalance",
		"eth_getStorageAt",
		"eth_getCode",
		"eth_getTransactionCount",
		"eth_call",
		"eth_estimateGas":
		if len(r.Params) > 1 {
			return fmt.Sprintf("%s", r.Params[1]), nil
		} else {
			return "", fmt.Errorf("unexpected missing 2nd parameter for method %s: %+v", r.Method, r.Params)
		}

	case "eth_getBlockByHash":
		if len(r.Params) > 0 {
			if blockHash, ok := r.Params[0].(string); ok {
				return blockHash, nil
			}
			return "", fmt.Errorf("first parameter is not a string for method %s it is %+v", r.Method, r.Params)
		}

	default:
		return "", nil
	}

	return "", nil
}

func extractBlockReferenceFromResponse(rpcReq *common.JsonRpcRequest, rpcResp *common.JsonRpcResponse) (string, error) {
	if rpcReq == nil {
		return "", errors.New("cannot extract block reference when json-rpc request is nil")
	}

	if rpcResp == nil {
		return "", errors.New("cannot extract block reference when json-rpc response is nil")
	}

	switch rpcReq.Method {
	case "eth_getTransactionReceipt":
		if rpcResp.Result != nil {
			if receipt, ok := rpcResp.Result.(map[string]interface{}); ok {
				log.Debug().Msgf("extractBlockReferenceFromResponse receipt: %+v", receipt)
				if blockNumber, ok := receipt["blockNumber"].(string); ok {
					log.Debug().Msgf("extractBlockReferenceFromResponse blockNumber: %+v", blockNumber)
					bn, err := hexutil.DecodeUint64(blockNumber)
					if err != nil {
						return "", err
					}
					return fmt.Sprintf("%d", bn), nil
				}
			}
		}

	default:
		return "", nil
	}

	return "", nil
}
