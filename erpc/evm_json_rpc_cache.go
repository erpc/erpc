package erpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/rs/zerolog"
)

type EvmJsonRpcCache struct {
	conn    data.Connector
	network *Network
	logger  *zerolog.Logger
}

const (
	JsonRpcCacheContext common.ContextKey = "jsonRpcCache"
)

func NewEvmJsonRpcCache(ctx context.Context, logger *zerolog.Logger, cfg *common.ConnectorConfig) (*EvmJsonRpcCache, error) {
	logger.Info().Msg("initializing evm json rpc cache...")
	err := populateDefaults(cfg)
	if err != nil {
		return nil, err
	}

	c, err := data.NewConnector(ctx, logger, cfg)
	if err != nil {
		return nil, err
	}

	return &EvmJsonRpcCache{
		conn:   c,
		logger: logger,
	}, nil
}

func (c *EvmJsonRpcCache) WithNetwork(network *Network) *EvmJsonRpcCache {
	network.Logger.Debug().Msgf("creating EvmJsonRpcCache")
	return &EvmJsonRpcCache{
		logger:  c.logger,
		conn:    c.conn,
		network: network,
	}
}

func (c *EvmJsonRpcCache) Get(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	rpcReq, err := req.JsonRpcRequest()
	if err != nil {
		return nil, err
	}

	blockRef, blockNumber, err := common.ExtractEvmBlockReferenceFromRequest(rpcReq)
	if err != nil {
		return nil, err
	}
	if blockRef == "" {
		blockRef = "*"
	}
	if blockNumber != 0 {
		s, err := c.shouldCacheForBlock(blockNumber)
		if err != nil {
			return nil, err
		}
		if !s {
			return nil, nil
		}
	} else {
		return nil, nil
	}

	groupKey, requestKey, err := generateKeysForJsonRpcRequest(req, blockRef)
	if err != nil {
		return nil, err
	}

	var resultString string
	if blockRef != "*" {
		resultString, err = c.conn.Get(ctx, data.ConnectorMainIndex, groupKey, requestKey)
	} else {
		resultString, err = c.conn.Get(ctx, data.ConnectorReverseIndex, groupKey, requestKey)
	}
	if err != nil {
		return nil, err
	}

	jrr := &common.JsonRpcResponse{
		JSONRPC: rpcReq.JSONRPC,
		ID:      rpcReq.ID,
		Error:   nil,
		Result:  json.RawMessage(resultString),
	}

	return common.NewNormalizedResponse().
		WithRequest(req).
		WithJsonRpcResponse(jrr), nil
}

func (c *EvmJsonRpcCache) Set(ctx context.Context, req *common.NormalizedRequest, resp *common.NormalizedResponse) error {
	rpcReq, err := req.JsonRpcRequest()
	if err != nil {
		return err
	}

	rpcResp, err := resp.JsonRpcResponse()
	if err != nil {
		return err
	}

	lg := c.logger.With().Str("network", req.NetworkId()).Str("method", rpcReq.Method).Logger()

	if rpcResp == nil || rpcResp.Result == nil || rpcResp.Error != nil {
		lg.Debug().Msg("not caching response because it has no result or has error")
		return nil
	}

	blockRef, blockNumber, err := common.ExtractEvmBlockReferenceFromRequest(rpcReq)
	if err != nil {
		return err
	}

	if blockRef == "" {
		blockRef, blockNumber, err = common.ExtractEvmBlockReferenceFromResponse(rpcReq, rpcResp)
		if err != nil {
			return err
		}
	}

	if blockRef == "" || blockNumber == 0 {
		// Do not cache if we can't resolve a block reference (e.g. latest block requests)
		lg.Debug().
			Str("blockRef", blockRef).
			Int64("blockNumber", blockNumber).
			Msg("will not cache the response because it has no block reference or block number")
		return nil
	}

	s, e := c.shouldCacheForBlock(blockNumber)
	if !s || e != nil {
		lg.Debug().
			Err(e).
			Str("blockRef", blockRef).
			Int64("blockNumber", blockNumber).
			Interface("result", rpcResp.Result).
			Msg("will not cache the response because block is not finalized")
		return e
	}

	lg.Debug().
		Str("blockRef", blockRef).
		Int64("blockNumber", blockNumber).
		Interface("result", rpcResp.Result).
		Msg("caching the response")

	pk, rk, err := generateKeysForJsonRpcRequest(req, blockRef)
	if err != nil {
		return err
	}

	resultStr, err := sonic.Marshal(rpcResp.Result)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeoutCause(ctx, 10*time.Second, errors.New("cache driver timeout during set"))
	defer cancel()
	return c.conn.Set(ctx, pk, rk, string(resultStr))
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

func (c *EvmJsonRpcCache) shouldCacheForBlock(blockNumber int64) (bool, error) {
	b, e := c.network.EvmIsBlockFinalized(blockNumber)
	return b, e
}

func generateKeysForJsonRpcRequest(req *common.NormalizedRequest, blockRef string) (string, string, error) {
	cacheKey, err := req.CacheHash()
	if err != nil {
		return "", "", err
	}

	if blockRef != "" {
		return fmt.Sprintf("%s:%s", req.NetworkId(), blockRef), cacheKey, nil
	} else {
		return fmt.Sprintf("%s:nil", req.NetworkId()), cacheKey, nil
	}
}

func populateDefaults(cfg *common.ConnectorConfig) error {
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
	case data.RedisDriverName:
		if cfg.Redis.Addr == "" {
			cfg.Redis.Addr = "localhost:6379"
		}
	case data.PostgreSQLDriverName:
		if cfg.PostgreSQL.ConnectionUri == "" {
			cfg.PostgreSQL.ConnectionUri = "postgres://erpc:erpc@localhost:5432/erpc"
		}
		if cfg.PostgreSQL.Table == "" {
			cfg.PostgreSQL.Table = "erpc_json_rpc_cache"
		}
	}

	return nil
}
