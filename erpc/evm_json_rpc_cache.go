package erpc

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/config"
	"github.com/flair-sdk/erpc/data"
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
	conn    data.Connector
	network *PreparedNetwork
}

const (
	JsonRpcCacheContext common.ContextKey = "jsonRpcCache"
)

func NewEvmJsonRpcCache(ctx context.Context, cfg *config.ConnectorConfig) (*EvmJsonRpcCache, error) {
	err := populateDefaults(cfg)
	if err != nil {
		return nil, err
	}

	c, err := data.NewConnector(
		ctx,
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

func (c *EvmJsonRpcCache) WithNetwork(network *PreparedNetwork) *EvmJsonRpcCache {
	network.Logger.Debug().Msgf("creating EvmJsonRpcCache")
	return &EvmJsonRpcCache{
		conn:    c.conn,
		network: network,
	}
}

func (c *EvmJsonRpcCache) GetWithReader(ctx context.Context, req *common.NormalizedRequest) (io.Reader, error) {
	rpcReq, err := req.JsonRpcRequest()
	if err != nil {
		return nil, err
	}

	blockRef, _, err := extractBlockReferenceFromRequest(rpcReq)
	if err != nil {
		return nil, err
	}
	if blockRef == "" {
		blockRef = "*"
	}

	groupKey, requestKey, err := generateKeysForJsonRpcRequest(req, blockRef)
	if err != nil {
		return nil, err
	}

	if blockRef != "*" {
		return c.conn.GetWithReader(ctx, data.ConnectorMainIndex, groupKey, requestKey)
	} else {
		return c.conn.GetWithReader(ctx, data.ConnectorReverseIndex, groupKey, requestKey)
	}
}

func (c *EvmJsonRpcCache) SetWithWriter(ctx context.Context, req *common.NormalizedRequest) (io.WriteCloser, error) {
	rpcReq, err := req.JsonRpcRequest()
	if err != nil {
		return nil, err
	}

	blockRef, blockNumber, err := extractBlockReferenceFromRequest(rpcReq)
	if err != nil {
		return nil, err
	}

	if blockRef != "" {
		s, e := c.shouldCacheForBlock(blockNumber)
		if !s || e != nil {
			return nil, e
		}
		pk, rk, err := generateKeysForJsonRpcRequest(req, blockRef)
		if err != nil {
			return nil, err
		}
		return c.conn.SetWithWriter(ctx, pk, rk)
	} else {
		// Empty keys forces the connector to use the keyResolver when response is fully received
		ctx = context.WithValue(ctx, JsonRpcCacheContext, map[string]interface{}{
			"req":   req,
			"cache": c,
		})
		return c.conn.SetWithWriter(ctx, "", "")
	}
}

func (c *EvmJsonRpcCache) DeleteByGroupKey(ctx context.Context, groupKeys ...string) error {
	for _, groupKey := range groupKeys {
		err := c.conn.Delete(ctx, data.ConnectorMainIndex, groupKey, "")
		if err != nil {
			return err
		}
	}

	return nil
}

func (c *EvmJsonRpcCache) shouldCacheForBlock(blockNumber uint64) (bool, error) {
	b, e := c.network.EvmIsBlockFinalized(blockNumber)
	log.Debug().Msgf("shouldCacheForBlock on block: %d, finalized: %t, err: %v", blockNumber, b, e)
	return b, e
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

func resolveEvmJsonRpcCacheKeys(ctx context.Context, dv *data.DataValue) (pk string, rk string, err error) {
	values := ctx.Value(JsonRpcCacheContext).(map[string]interface{})

	req := values["req"].(*common.NormalizedRequest)
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

	blockRef, blockNumber, err := extractBlockReferenceFromResponse(rpcReq, rpcRes)
	if err != nil {
		return "", "", err
	}

	cache := values["cache"].(*EvmJsonRpcCache)
	s, e := cache.shouldCacheForBlock(blockNumber)
	if !s || e != nil {
		return "", "", e
	}

	return generateKeysForJsonRpcRequest(req, blockRef)
}

func generateKeysForJsonRpcRequest(req *common.NormalizedRequest, blockRef string) (string, string, error) {
	rpcReq, err := req.JsonRpcRequest()
	if err != nil {
		return "", "", err
	}

	cacheKey, err := generateCacheKey(rpcReq)
	if err != nil {
		return "", "", err
	}

	if blockRef != "" {
		return fmt.Sprintf("evm:%s:%s", req.NetworkId, blockRef), fmt.Sprintf("%s:%s", req.NetworkId, cacheKey), nil
	} else {
		return fmt.Sprintf("evm:%s:nil", req.NetworkId), fmt.Sprintf("%s:%s", req.NetworkId, cacheKey), nil
	}
}

func resolveOnlySuccessfulResponses(_ context.Context, dv *data.DataValue) (*data.DataValue, error) {
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
	case data.DynamoDBDriverName:
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

func extractBlockReferenceFromRequest(r *common.JsonRpcRequest) (string, uint64, error) {
	if r == nil {
		return "", 0, errors.New("cannot extract block reference when json-rpc request is nil")
	}

	switch r.Method {
	case "eth_getBlockByNumber",
		"eth_getUncleByBlockNumberAndIndex",
		"eth_getTransactionByBlockNumberAndIndex",
		"eth_getUncleCountByBlockNumber",
		"eth_getBlockTransactionCountByNumber":
		log.Debug().Msgf("extractBlockReferenceFromRequest r.Params: %+v", r.Params)
		if len(r.Params) > 0 {
			if bns, ok := r.Params[0].(string); ok {
				if strings.HasPrefix(bns, "0x") {
					bni, err := hexutil.DecodeUint64(bns)
					if err != nil {
						return "", 0, err
					}
					return bns, bni, nil
				} else {
					return "", 0, nil
				}
			}
		} else {
			return "", 0, fmt.Errorf("unexpected no parameters for method %s", r.Method)
		}

	case "eth_getBalance",
		"eth_getStorageAt",
		"eth_getCode",
		"eth_getTransactionCount",
		"eth_call",
		"eth_estimateGas":
		log.Debug().Msgf("extractBlockReferenceFromRequest r.Params: %+v", r.Params)
		if len(r.Params) > 1 {
			if bns, ok := r.Params[1].(string); ok {
				if strings.HasPrefix(bns, "0x") {
					bni, err := hexutil.DecodeUint64(bns)
					if err != nil {
						return "", 0, err
					}
					return bns, bni, nil
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

func extractBlockReferenceFromResponse(rpcReq *common.JsonRpcRequest, rpcResp *common.JsonRpcResponse) (string, uint64, error) {
	if rpcReq == nil {
		return "", 0, errors.New("cannot extract block reference when json-rpc request is nil")
	}

	if rpcResp == nil {
		return "", 0, errors.New("cannot extract block reference when json-rpc response is nil")
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
						return "", bn, err
					}
					return fmt.Sprintf("%d", bn), bn, nil
				}
			}
		}

	default:
		return "", 0, nil
	}

	return "", 0, nil
}
