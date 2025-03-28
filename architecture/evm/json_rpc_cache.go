package evm

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
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

type EvmJsonRpcCache struct {
	projectId string
	policies  []*data.CachePolicy
	methods   map[string]*common.CacheMethodConfig
	logger    *zerolog.Logger
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
		methods:  cfg.Methods,
		logger:   logger,
	}, nil
}

func (c *EvmJsonRpcCache) WithProjectId(projectId string) *EvmJsonRpcCache {
	lg := c.logger.With().Str("projectId", projectId).Logger()
	lg.Debug().Msgf("cloning EvmJsonRpcCache for project")
	return &EvmJsonRpcCache{
		logger:    &lg,
		policies:  c.policies,
		methods:   c.methods,
		projectId: projectId,
	}
}

func (c *EvmJsonRpcCache) SetPolicies(policies []*data.CachePolicy) {
	c.policies = policies
}

func (c *EvmJsonRpcCache) Get(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	ctx, span := common.StartSpan(ctx, "Cache.Get",
		trace.WithAttributes(
			attribute.String("network.id", req.NetworkId()),
		),
	)
	defer span.End()

	if common.IsTracingDetailed {
		span.SetAttributes(
			attribute.String("request.id", fmt.Sprintf("%v", req.ID())),
		)
	}

	start := time.Now()
	rpcReq, err := req.JsonRpcRequest(ctx)
	if err != nil {
		common.SetTraceSpanError(span, err)
		return nil, err
	}

	_, policySpan := common.StartDetailSpan(ctx, "Cache.FindGetPolicies")

	ntwId := req.NetworkId()
	finState := c.getFinalityState(ctx, req, nil)
	policies, err := c.findGetPolicies(ntwId, rpcReq.Method, rpcReq.Params, finState)
	span.SetAttributes(
		attribute.String("request.method", rpcReq.Method),
		attribute.String("request.finality", finState.String()),
		attribute.Int("cache.policies_matched", len(policies)),
	)
	if err != nil {
		common.SetTraceSpanError(policySpan, err)
		policySpan.End()
		return nil, err
	}
	if len(policies) == 0 {
		health.MetricCacheGetSkippedTotal.WithLabelValues(
			c.projectId,
			req.NetworkId(),
			rpcReq.Method,
		).Inc()
		span.SetAttributes(attribute.Bool("cache.hit", false))
		policySpan.End()
		return nil, nil
	}

	policySpan.End()

	var jrr *common.JsonRpcResponse
	var connector data.Connector
	var policy *data.CachePolicy
	for _, policy = range policies {
		connector = policy.GetConnector()
		policyCtx, policySpan := common.StartDetailSpan(ctx, "Cache.GetForPolicy", trace.WithAttributes(
			attribute.String("cache.policy_summary", policy.String()),
			attribute.String("cache.connector_id", connector.Id()),
		))
		jrr, err = c.doGet(policyCtx, connector, req, rpcReq)
		if err != nil {
			common.SetTraceSpanError(policySpan, err)
			health.MetricCacheGetErrorTotal.WithLabelValues(
				c.projectId,
				req.NetworkId(),
				rpcReq.Method,
				connector.Id(),
				policy.String(),
				policy.GetTTL().String(),
				common.ErrorSummary(err),
			).Inc()
			health.MetricCacheGetErrorDuration.WithLabelValues(
				c.projectId,
				req.NetworkId(),
				rpcReq.Method,
				connector.Id(),
				policy.String(),
				policy.GetTTL().String(),
				common.ErrorSummary(err),
			).Observe(time.Since(start).Seconds())
		}
		if c.logger.GetLevel() == zerolog.TraceLevel {
			c.logger.Trace().Interface("policy", policy).Str("connector", connector.Id()).Interface("id", req.ID()).Err(err).Msg("skipping cache policy during GET because it returned nil or error")
		} else {
			c.logger.Debug().Str("connector", connector.Id()).Interface("id", req.ID()).Err(err).Msg("skipping cache policy during GET because it returned nil or error")
		}
		policySpan.End()
		if jrr != nil {
			break
		}
	}

	if jrr == nil {
		health.MetricCacheGetSuccessMissTotal.WithLabelValues(
			c.projectId,
			req.NetworkId(),
			rpcReq.Method,
			connector.Id(),
			policy.String(),
			policy.GetTTL().String(),
		).Inc()
		health.MetricCacheGetSuccessMissDuration.WithLabelValues(
			c.projectId,
			req.NetworkId(),
			rpcReq.Method,
			connector.Id(),
			policy.String(),
			policy.GetTTL().String(),
		).Observe(time.Since(start).Seconds())
		span.SetAttributes(attribute.Bool("cache.hit", false))
		return nil, nil
	}

	health.MetricCacheGetSuccessHitTotal.WithLabelValues(
		c.projectId,
		req.NetworkId(),
		rpcReq.Method,
		connector.Id(),
		policy.String(),
		policy.GetTTL().String(),
	).Inc()
	health.MetricCacheGetSuccessHitDuration.WithLabelValues(
		c.projectId,
		req.NetworkId(),
		rpcReq.Method,
		connector.Id(),
		policy.String(),
		policy.GetTTL().String(),
	).Observe(time.Since(start).Seconds())
	span.SetAttributes(attribute.Bool("cache.hit", true))
	if c.logger.GetLevel() <= zerolog.DebugLevel {
		c.logger.Trace().Str("method", rpcReq.Method).Interface("id", req.ID()).RawJSON("result", jrr.Result).Msg("returning cached response")
	} else {
		c.logger.Debug().Str("method", rpcReq.Method).Interface("id", req.ID()).Msg("returning cached response")
	}

	return common.NewNormalizedResponse().
		WithRequest(req).
		WithFromCache(true).
		WithJsonRpcResponse(jrr), nil
}

func (c *EvmJsonRpcCache) Set(ctx context.Context, req *common.NormalizedRequest, resp *common.NormalizedResponse) error {
	ctx, span := common.StartSpan(ctx, "Cache.Set", trace.WithAttributes(
		attribute.String("upstream.id", resp.Upstream().Config().Id),
	))
	defer span.End()

	if common.IsTracingDetailed {
		span.SetAttributes(
			attribute.String("request.id", fmt.Sprintf("%v", req.ID())),
		)
	}

	// TODO after subscription epic this method can be called for every new block data to pre-populate the cache,
	// based on the evmJsonRpcCache.hyrdation.filters which is only the data (logs, txs) that user cares about.
	start := time.Now()
	rpcReq, err := req.JsonRpcRequest(ctx)
	if err != nil {
		common.SetTraceSpanError(span, err)
		return err
	}

	rpcResp, err := resp.JsonRpcResponse(ctx)
	if err != nil {
		common.SetTraceSpanError(span, err)
		return err
	}

	ntwId := req.NetworkId()
	lg := c.logger.With().Str("networkId", ntwId).Str("method", rpcReq.Method).Interface("id", req.ID()).Logger()

	span.SetAttributes(
		attribute.String("request.method", rpcReq.Method),
		attribute.String("network.id", ntwId),
	)

	blockRef, blockNumber, err := ExtractBlockReferenceFromRequest(ctx, req)
	if err != nil {
		common.SetTraceSpanError(span, err)
		return err
	}

	finState := c.getFinalityState(ctx, req, resp)
	policies, err := c.findSetPolicies(ntwId, rpcReq.Method, rpcReq.Params, finState)
	span.SetAttributes(
		attribute.String("block.finality", finState.String()),
		attribute.Int("cache.policies_matched", len(policies)),
	)
	if common.IsTracingDetailed {
		span.SetAttributes(
			attribute.String("block.ref", blockRef),
			attribute.Int64("block.number", blockNumber),
		)
	}

	lg.Trace().Err(err).Interface("policies", policies).Str("finality", finState.String()).Msg("result of finding cache policy during SET")
	if err != nil {
		common.SetTraceSpanError(span, err)
		return err
	}
	if len(policies) == 0 {
		return nil
	}

	if blockRef == "" {
		// Do not cache if we can't resolve a block reference (e.g. unknown methods)
		if lg.GetLevel() <= zerolog.TraceLevel {
			lg.Trace().
				Object("request", req).
				Str("blockRef", blockRef).
				Int64("blockNumber", blockNumber).
				Msg("will not cache the response because we cannot resolve a block reference")
		} else {
			lg.Debug().
				Str("method", rpcReq.Method).
				Str("blockRef", blockRef).
				Int64("blockNumber", blockNumber).
				Msg("will not cache the response because we cannot resolve a block reference")
		}
		return nil
	}

	pk, rk, err := generateKeysForJsonRpcRequest(req, blockRef, ctx)
	if err != nil {
		common.SetTraceSpanError(span, err)
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
						c.projectId,
						req.NetworkId(),
						rpcReq.Method,
						connector.Id(),
						policy.String(),
						ttl.String(),
						common.ErrorSummary(err),
					).Inc()
					health.MetricCacheSetErrorDuration.WithLabelValues(
						c.projectId,
						req.NetworkId(),
						rpcReq.Method,
						connector.Id(),
						policy.String(),
						ttl.String(),
						common.ErrorSummary(err),
					).Observe(time.Since(start).Seconds())
				} else {
					health.MetricCacheSetSkippedTotal.WithLabelValues(
						c.projectId,
						req.NetworkId(),
						rpcReq.Method,
						connector.Id(),
						policy.String(),
						ttl.String(),
					).Inc()
				}
				errsMu.Lock()
				errs = append(errs, err)
				errsMu.Unlock()
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
					c.projectId,
					req.NetworkId(),
					rpcReq.Method,
					connector.Id(),
					policy.String(),
					ttl.String(),
					common.ErrorSummary(err),
				).Inc()
				health.MetricCacheSetErrorDuration.WithLabelValues(
					c.projectId,
					req.NetworkId(),
					rpcReq.Method,
					connector.Id(),
					policy.String(),
					ttl.String(),
					common.ErrorSummary(err),
				).Observe(time.Since(start).Seconds())
			} else {
				health.MetricCacheSetSuccessTotal.WithLabelValues(
					c.projectId,
					req.NetworkId(),
					rpcReq.Method,
					connector.Id(),
					policy.String(),
					ttl.String(),
				).Inc()
				health.MetricCacheSetSuccessDuration.WithLabelValues(
					c.projectId,
					req.NetworkId(),
					rpcReq.Method,
					connector.Id(),
					policy.String(),
					ttl.String(),
				).Observe(time.Since(start).Seconds())
			}
		}(policy)
	}
	wg.Wait()

	if len(errs) > 0 {
		if len(errs) == 1 {
			common.SetTraceSpanError(span, errs[0])
			return errs[0]
		}

		// TODO use a new composite error object to keep an array of causes (similar to Upstreams Exhausted error)
		err = fmt.Errorf("failed to set cache for %d policies: %v", len(errs), errs)
		common.SetTraceSpanError(span, err)
		return err
	}

	return nil
}

func (c *EvmJsonRpcCache) MethodConfig(method string) *common.CacheMethodConfig {
	if cfg, ok := c.methods[method]; ok {
		return cfg
	}
	return nil
}

func (c *EvmJsonRpcCache) IsObjectNull() bool {
	return c == nil || c.logger == nil
}

func (c *EvmJsonRpcCache) findSetPolicies(networkId, method string, params []interface{}, finality common.DataFinalityState) ([]*data.CachePolicy, error) {
	var policies []*data.CachePolicy
	for _, policy := range c.policies {
		// Add debug logging for complex param matching
		if c.logger.GetLevel() <= zerolog.TraceLevel {
			c.logger.Trace().
				Str("networkId", networkId).
				Str("method", method).
				Str("finality", finality.String()).
				Interface("params", params).
				Interface("policy", policy).
				Msg("checking policy match for set")
		}

		match, err := policy.MatchesForSet(networkId, method, params, finality)
		if err != nil {
			return nil, err
		}
		if match {
			policies = append(policies, policy)
		}
	}
	return policies, nil
}

func (c *EvmJsonRpcCache) findGetPolicies(networkId, method string, params []interface{}, finality common.DataFinalityState) ([]*data.CachePolicy, error) {
	var policies []*data.CachePolicy
	visitedConnectorsMap := make(map[data.Connector]bool)
	for _, policy := range c.policies {
		// Add debug logging for complex param matching
		if c.logger.GetLevel() <= zerolog.TraceLevel {
			c.logger.Trace().
				Str("networkId", networkId).
				Str("method", method).
				Str("finality", finality.String()).
				Interface("params", params).
				Interface("policy", policy).
				Msg("checking policy match for get")
		}

		match, err := policy.MatchesForGet(networkId, method, params, finality)
		if err != nil {
			return nil, err
		}
		if match {
			if c := policy.GetConnector(); !visitedConnectorsMap[c] {
				policies = append(policies, policy)
				visitedConnectorsMap[c] = true
			}
		}
	}
	return policies, nil
}

func (c *EvmJsonRpcCache) doGet(ctx context.Context, connector data.Connector, req *common.NormalizedRequest, rpcReq *common.JsonRpcRequest) (*common.JsonRpcResponse, error) {
	rpcReq.RLockWithTrace(ctx)
	defer rpcReq.RUnlock()

	blockRef, _, err := ExtractBlockReferenceFromRequest(ctx, req)
	if err != nil {
		return nil, err
	}
	if blockRef == "" {
		if c.logger.GetLevel() <= zerolog.TraceLevel {
			c.logger.Trace().
				Object("request", req).
				Msg("skip fetching from cache because we cannot resolve a block reference")
		} else {
			c.logger.Debug().
				Str("method", rpcReq.Method).
				Msg("skip fetching from cache because we cannot resolve a block reference")
		}
		return nil, nil
	}

	groupKey, requestKey, err := generateKeysForJsonRpcRequest(req, blockRef, ctx)
	if err != nil {
		return nil, err
	}

	c.logger.Trace().Str("pk", groupKey).Str("rk", requestKey).Msg("fetching from cache")

	var resultString string
	if blockRef == "*" {
		resultString, err = connector.Get(ctx, data.ConnectorReverseIndex, groupKey, requestKey)
	} else {
		resultString, err = connector.Get(ctx, data.ConnectorMainIndex, groupKey, requestKey)
	}
	if err != nil {
		return nil, err
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

func (c *EvmJsonRpcCache) getFinalityState(ctx context.Context, req *common.NormalizedRequest, resp *common.NormalizedResponse) (finality common.DataFinalityState) {
	finality = common.DataFinalityStateUnknown

	if req == nil && resp == nil {
		return
	}

	method, _ := req.Method()
	if cfg, ok := c.methods[method]; ok {
		if cfg.Finalized {
			finality = common.DataFinalityStateFinalized
			return
		} else if cfg.Realtime {
			finality = common.DataFinalityStateRealtime
			return
		}
	}

	blockRef, blockNumber, _ := ExtractBlockReferenceFromRequest(ctx, req)

	if blockRef == "latest" || blockRef == "pending" {
		finality = common.DataFinalityStateUnfinalized
	} else if blockRef == "finalized" {
		finality = common.DataFinalityStateFinalized
	} else if resp != nil {
		if blockNumber > 0 {
			upstream := resp.Upstream()
			if upstream != nil {
				if ups, ok := upstream.(common.EvmUpstream); ok {
					if isFinalized, err := ups.EvmIsBlockFinalized(blockNumber); err == nil {
						if isFinalized {
							finality = common.DataFinalityStateFinalized
						} else {
							finality = common.DataFinalityStateUnfinalized
						}
					}
				}
			}
		}
	}

	return
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

	// Check if the response size is within the limits
	if !policy.MatchesSizeLimits(len(rpcResp.Result)) {
		lg.Debug().Int("size", len(rpcResp.Result)).Msg("skip caching because response size does not match policy limits")
		return false, nil
	}

	// Check if we should cache empty results
	isEmpty := resp == nil || rpcResp == nil || rpcResp.Result == nil || resp.IsObjectNull() || resp.IsResultEmptyish()
	switch policy.EmptyState() {
	case common.CacheEmptyBehaviorIgnore:
		return !isEmpty, nil
	case common.CacheEmptyBehaviorAllow:
		return true, nil
	case common.CacheEmptyBehaviorOnly:
		return isEmpty, nil
	default:
		return false, fmt.Errorf("unknown cache empty behavior: %s", policy.EmptyState())
	}
}

func generateKeysForJsonRpcRequest(
	req *common.NormalizedRequest,
	blockRef string,
	ctx ...context.Context,
) (string, string, error) {
	cacheKey, err := req.CacheHash(ctx...)
	if err != nil {
		return "", "", err
	}

	if blockRef != "" {
		return fmt.Sprintf("%s:%s", req.NetworkId(), blockRef), cacheKey, nil
	} else {
		return fmt.Sprintf("%s:nil", req.NetworkId()), cacheKey, nil
	}
}
