package erpc

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
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

func (c *EvmJsonRpcCache) findSetPolicies(networkId, method string, params []interface{}, finality common.DataFinalityState) []*data.CachePolicy {
	var policies []*data.CachePolicy
	for _, policy := range c.policies {
		// Add debug logging for complex param matching
		if c.logger.GetLevel() <= zerolog.TraceLevel {
			c.logger.Trace().
				Str("networkId", networkId).
				Str("method", method).
				Interface("params", params).
				Interface("policy", policy).
				Str("finality", finality.String()).
				Msg("checking policy match for set")
		}

		if policy.MatchesForSet(networkId, method, params, finality) {
			policies = append(policies, policy)
		}
	}
	return policies
}

func (c *EvmJsonRpcCache) findGetPolicies(networkId, method string, params []interface{}) []*data.CachePolicy {
	var policies []*data.CachePolicy
	for _, policy := range c.policies {
		// Add debug logging for complex param matching
		if c.logger.GetLevel() <= zerolog.TraceLevel {
			c.logger.Trace().
				Str("networkId", networkId).
				Str("method", method).
				Interface("params", params).
				Interface("policy", policy).
				Msg("checking policy match for get")
		}

		if policy.MatchesForGet(networkId, method, params) {
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
	policies := c.findGetPolicies(ntwId, rpcReq.Method, rpcReq.Params)
	if len(policies) == 0 {
		return nil, nil
	}

	var jrr *common.JsonRpcResponse
	for _, policy := range policies {
		connector := policy.GetConnector()
		jrr, err = c.doGet(ctx, connector, req, rpcReq)
		if jrr != nil {
			health.MetricCacheGetSuccessHitTotal.WithLabelValues(
				c.network.ProjectId,
				req.NetworkId(),
				rpcReq.Method,
				connector.Id(),
				policy.String(),
				policy.GetTTL().String(),
			).Inc()
			break
		} else if err == nil {
			health.MetricCacheGetSuccessMissTotal.WithLabelValues(
				c.network.ProjectId,
				req.NetworkId(),
				rpcReq.Method,
				connector.Id(),
				policy.String(),
				policy.GetTTL().String(),
			).Inc()
		} else {
			health.MetricCacheGetErrorTotal.WithLabelValues(
				c.network.ProjectId,
				req.NetworkId(),
				rpcReq.Method,
				connector.Id(),
				policy.String(),
				policy.GetTTL().String(),
				common.ErrorSummary(err),
			).Inc()
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

	blockRef, blockNumber, err := common.ExtractEvmBlockReference(rpcReq, rpcResp)
	if err != nil {
		return err
	}

	finState := resp.FinalityState()
	policies := c.findSetPolicies(ntwId, rpcReq.Method, rpcReq.Params, finState)
	if len(policies) == 0 {
		return nil
	}

	// if blockRef == "" && blockNumber == 0 {
	// 	// Do not cache if we can't resolve a block reference (e.g. latest block requests)
	// 	lg.Debug().
	// 		Str("blockRef", blockRef).
	// 		Int64("blockNumber", blockNumber).
	// 		Msg("will not cache the response because it has no block reference or block number")
	// 	return nil
	// }

	// s, e := c.shouldCacheForBlock(blockNumber)
	// if !s || e != nil {
	// 	if lg.GetLevel() <= zerolog.DebugLevel {
	// 		lg.Debug().
	// 			Err(e).
	// 			Str("blockRef", blockRef).
	// 			Int64("blockNumber", blockNumber).
	// 			Str("result", util.Mem2Str(rpcResp.Result)).
	// 			Msg("will not cache the response based on policy config")
	// 	}
	// 	return e
	// }

	pk, rk, err := generateKeysForJsonRpcRequest(req, blockRef)
	if err != nil {
		return err
	}

	if lg.GetLevel() <= zerolog.TraceLevel {
		lg.Trace().
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

			shouldCache, err := shouldCacheResponse(lg, resp, rpcResp, policy)
			if !shouldCache {
				if err != nil {
					health.MetricCacheSetErrorTotal.WithLabelValues(
						c.network.ProjectId,
						req.NetworkId(),
						rpcReq.Method,
						connector.Id(),
						policy.String(),
						ttl.String(),
						common.ErrorSummary(err),
					).Inc()
				}
				return
			}

			ctx, cancel := context.WithTimeoutCause(ctx, 5*time.Second, errors.New("evm json-rpc cache driver timeout during set"))
			defer cancel()
			err = connector.Set(ctx, pk, rk, util.Mem2Str(rpcResp.Result), ttl)
			if err != nil {
				errsMu.Lock()
				errs = append(errs, err)
				errsMu.Unlock()
				health.MetricCacheSetErrorTotal.WithLabelValues(
					c.network.ProjectId,
					req.NetworkId(),
					rpcReq.Method,
					connector.Id(),
					policy.String(),
					ttl.String(),
					common.ErrorSummary(err),
				).Inc()
			} else {
				health.MetricCacheSetSuccessTotal.WithLabelValues(
					c.network.ProjectId,
					req.NetworkId(),
					rpcReq.Method,
					connector.Id(),
					policy.String(),
					ttl.String(),
				).Inc()
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
	resp *common.NormalizedResponse,
	rpcResp *common.JsonRpcResponse,
	policy *data.CachePolicy,
) (bool, error) {
	// Never cache responses with errors
	if rpcResp != nil && rpcResp.Error != nil {
		lg.Debug().Msg("skip caching because response contains an error")
		return false, nil
	}

	// Only check for emptyish results if allowEmptyish is false
	if !policy.AllowEmptyish() {
		if resp == nil || rpcResp == nil || rpcResp.Result == nil {
			lg.Debug().Msg("skip caching because response is nil and allowEmptyish=false")
			return false, nil
		}
		if resp.IsObjectNull() || resp.IsResultEmptyish() {
			lg.Debug().Msg("skip caching because result is empty/null and allowEmptyish=false")
			return false, nil
		}
	}

	return true, nil
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
