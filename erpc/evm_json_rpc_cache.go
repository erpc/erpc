package erpc

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
)

type EvmJsonRpcCache struct {
	policies []*data.CachePolicy
	network  *Network
	logger   *zerolog.Logger
}

const (
	JsonRpcCacheContext common.ContextKey = "jsonRpcCache"
)

func NewEvmJsonRpcCache(ctx context.Context, logger *zerolog.Logger, cfg *common.CacheConfig) (*EvmJsonRpcCache, error) {
	logger.Info().Msg("initializing evm json rpc cache...")

	// Create connectors map
	connectors := make(map[string]data.Connector)
	for _, connCfg := range cfg.Connectors {
		c, err := data.NewConnector(ctx, logger, connCfg)
		if err != nil {
			return nil, fmt.Errorf("failed to create connector %s: %w", connCfg.Id, err)
		}
		connectors[connCfg.Id] = c
	}

	// Create policies
	var policies []*data.CachePolicy
	for _, policyCfg := range cfg.Policies {
		connector, exists := connectors[policyCfg.Connector]
		if !exists {
			return nil, fmt.Errorf("connector %s not found for policy", policyCfg.Connector)
		}

		policy, err := data.NewCachePolicy(policyCfg, connector)
		if err != nil {
			return nil, fmt.Errorf("failed to create policy: %w", err)
		}
		policies = append(policies, policy)
	}

	return &EvmJsonRpcCache{
		policies: policies,
		logger:   logger,
	}, nil
}

func (c *EvmJsonRpcCache) WithNetwork(network *Network) *EvmJsonRpcCache {
	network.Logger.Debug().Msgf("creating EvmJsonRpcCache")
	return &EvmJsonRpcCache{
		logger:   c.logger,
		policies: c.policies,
		network:  network,
	}
}

func (c *EvmJsonRpcCache) findSetPolicies(networkId string, method string, finality common.DataFinalityState) []*data.CachePolicy {
	var policies []*data.CachePolicy
	for _, policy := range c.policies {
		if policy.MatchesForSet(networkId, method, finality) {
			policies = append(policies, policy)
		}
	}
	return policies
}

func (c *EvmJsonRpcCache) findGetPolicies(networkId string, method string) []*data.CachePolicy {
	var policies []*data.CachePolicy
	for _, policy := range c.policies {
		if policy.MatchesForGet(networkId, method) {
			policies = append(policies, policy)
		}
	}
	return policies
}

func (c *EvmJsonRpcCache) Get(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	rpcReq, err := req.JsonRpcRequest()
	if err != nil {
		return nil, err
	}

	ntwId := req.NetworkId()

	// Find matching policy
	policies := c.findGetPolicies(ntwId, rpcReq.Method)
	if len(policies) == 0 {
		return nil, nil
	}

	var jrr *common.JsonRpcResponse
	for _, policy := range policies {
		connector := policy.GetConnector()
		jrr, err = c.doGet(ctx, connector, req, rpcReq)
		if jrr != nil {
			break
		}
		if c.logger.GetLevel() == zerolog.TraceLevel {
			c.logger.Trace().Interface("policy", policy).Str("connector", connector.Id()).Err(err).Msg("skipping cache policy because it returned nil or error")
		} else {
			c.logger.Debug().Str("connector", connector.Id()).Err(err).Msg("skipping cache policy because it returned nil or error")
		}
	}

	if jrr == nil {
		return nil, nil
	}

	if c.logger.GetLevel() <= zerolog.DebugLevel {
		c.logger.Trace().Str("method", rpcReq.Method).RawJSON("result", jrr.Result).Msg("returning cached response")
	} else {
		c.logger.Debug().Str("method", rpcReq.Method).Msg("returning cached response")
	}

	return common.NewNormalizedResponse().
		WithRequest(req).
		WithFromCache(true).
		WithJsonRpcResponse(jrr), nil
}

func (c *EvmJsonRpcCache) doGet(ctx context.Context, connector data.Connector, req *common.NormalizedRequest, rpcReq *common.JsonRpcRequest) (*common.JsonRpcResponse, error) {
	rpcReq.RLock()
	defer rpcReq.RUnlock()

	blockRef, blockNumber, err := common.ExtractEvmBlockReferenceFromRequest(rpcReq)
	if err != nil {
		return nil, err
	}
	if blockRef == "" && blockNumber == 0 {
		return nil, nil
	}
	if blockNumber != 0 {
		s, err := c.shouldCacheForBlock(blockNumber)
		if err == nil && !s {
			return nil, nil
		}
	}

	groupKey, requestKey, err := generateKeysForJsonRpcRequest(req, blockRef)
	if err != nil {
		return nil, err
	}

	var resultString string
	if blockRef != "*" {
		resultString, err = connector.Get(ctx, data.ConnectorMainIndex, groupKey, requestKey)
	} else {
		resultString, err = connector.Get(ctx, data.ConnectorReverseIndex, groupKey, requestKey)
	}
	if err != nil {
		return nil, err
	}

	if resultString == `` || resultString == `""` || resultString == "null" || resultString == "[]" || resultString == "{}" {
		return nil, nil
	}

	jrr := &common.JsonRpcResponse{
		Result: util.Str2Mem(resultString),
	}
	err = jrr.SetID(rpcReq.ID)
	if err != nil {
		return nil, err
	}

	return jrr, nil
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

	ntwId := req.NetworkId()
	lg := c.logger.With().Str("networkId", ntwId).Str("method", rpcReq.Method).Logger()

	shouldCache, err := shouldCacheResponse(lg, req, resp, rpcReq, rpcResp)
	if !shouldCache || err != nil {
		return err
	}

	blockRef, blockNumber, err := common.ExtractEvmBlockReference(rpcReq, rpcResp)
	if err != nil {
		return err
	}

	finState := resp.FinalityState()
	policies := c.findSetPolicies(ntwId, rpcReq.Method, finState)
	if len(policies) == 0 {
		return nil
	}

	if blockRef == "" && blockNumber == 0 {
		// Do not cache if we can't resolve a block reference (e.g. latest block requests)
		lg.Debug().
			Str("blockRef", blockRef).
			Int64("blockNumber", blockNumber).
			Msg("will not cache the response because it has no block reference or block number")
		return nil
	}

	if blockNumber > 0 {
		s, e := c.shouldCacheForBlock(blockNumber)
		if !s || e != nil {
			if lg.GetLevel() <= zerolog.DebugLevel {
				lg.Debug().
					Err(e).
					Str("blockRef", blockRef).
					Int64("blockNumber", blockNumber).
					Str("result", util.Mem2Str(rpcResp.Result)).
					Msg("will not cache the response because block is not finalized")
			}
			return e
		}
	}

	pk, rk, err := generateKeysForJsonRpcRequest(req, blockRef)
	if err != nil {
		return err
	}

	if lg.GetLevel() <= zerolog.TraceLevel {
		lg.Debug().
			Str("blockRef", blockRef).
			Str("primaryKey", pk).
			Str("rangeKey", rk).
			Int64("blockNumber", blockNumber).
			Interface("policies", policies).
			RawJSON("result", rpcResp.Result).
			Str("finalityState", finState.String()).
			Msg("caching the response")
	} else {
		lg.Debug().
			Str("blockRef", blockRef).
			Str("primaryKey", pk).
			Str("rangeKey", rk).
			Int("policies", len(policies)).
			Int64("blockNumber", blockNumber).
			Str("finalityState", finState.String()).
			Msg("caching the response")
	}

	wg := sync.WaitGroup{}
	errs := []error{}
	errsMu := sync.Mutex{}
	for _, policy := range policies {
		wg.Add(1)
		go func(policy *data.CachePolicy) {
			defer wg.Done()
			connector := policy.GetConnector()
			ttl := policy.GetTTL()

			ctx, cancel := context.WithTimeoutCause(ctx, 5*time.Second, errors.New("evm json-rpc cache driver timeout during set"))
			defer cancel()
			err := connector.Set(ctx, pk, rk, util.Mem2Str(rpcResp.Result), ttl)
			if err != nil {
				errsMu.Lock()
				errs = append(errs, err)
				errsMu.Unlock()
			}
		}(policy)
	}
	wg.Wait()

	if len(errs) > 0 {
		if len(errs) == 1 {
			return errs[0]
		}
		return fmt.Errorf("failed to set cache for %d policies: %v", len(errs), errs)
	}

	return nil
}

func shouldCacheResponse(
	lg zerolog.Logger,
	req *common.NormalizedRequest,
	resp *common.NormalizedResponse,
	rpcReq *common.JsonRpcRequest,
	rpcResp *common.JsonRpcResponse,
) (bool, error) {
	if resp == nil ||
		resp.IsObjectNull() ||
		resp.IsResultEmptyish() ||
		rpcResp == nil ||
		rpcResp.Result == nil ||
		rpcResp.Error != nil {
		ups := resp.Upstream()
		if ups != nil {
			upsCfg := ups.Config()
			if upsCfg.Evm != nil {
				if ups.EvmSyncingState() == common.EvmSyncingStateNotSyncing {
					blkNum, err := req.EvmBlockNumber()
					if err == nil && blkNum > 0 {
						ntw := req.Network()
						if ntw != nil {
							// We explicitly check for finality on the same upstream that provided the response
							// to make sure on that specific node the block is actually finalized (vs any other node).
							if stp := ntw.EvmStatePollerOf(ups.Config().Id); stp != nil {
								if fin, err := stp.IsBlockFinalized(blkNum); err == nil && fin {
									return fin, nil
								}
							}
						}
					}
				}
			}
		}

		lg.Debug().Msg("skip caching because it has no result or has error and we cannot determine finality and sync-state")
		return false, nil
	}

	switch rpcReq.Method {
	case "eth_getTransactionByHash",
		"eth_getTransactionReceipt",
		"eth_getTransactionByBlockHashAndIndex",
		"eth_getTransactionByBlockNumberAndIndex":

		// When transactions are not yet included in a block blockNumber/blockHash is still unknown
		// For these transaction for now we will not cache the response, but still must be returned
		// to the client because they might be intentionally looking for pending txs.
		// Is there a reliable way to cache these and bust in-case of a reorg?
		blkRef, blkNum, err := common.ExtractEvmBlockReferenceFromResponse(rpcReq, rpcResp)
		if err != nil {
			lg.Error().Err(err).Msg("skip caching because error extracting block reference from response")
			return false, err
		}
		if blkRef == "" && blkNum == 0 {
			lg.Debug().Msg("skip caching because block number/hash is not yet available (unconfirmed tx?)")
			return false, nil
		}

		return true, nil
	}

	return true, nil
}

func (c *EvmJsonRpcCache) shouldCacheForBlock(blockNumber int64) (bool, error) {
	// This check returns true if the block is considered finalized by any upstream
	return c.network.EvmIsBlockFinalized(blockNumber)
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
