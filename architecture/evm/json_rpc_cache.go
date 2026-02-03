package evm

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/telemetry"
	"github.com/klauspost/compress/zstd"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

type EvmJsonRpcCache struct {
	projectId string
	policies  []*data.CachePolicy
	logger    *zerolog.Logger

	// Compression settings
	compressionEnabled   bool
	compressionThreshold int
	compressionLevel     zstd.EncoderLevel
	encoderPool          *sync.Pool
	decoderPool          *sync.Pool
}

const (
	JsonRpcCacheContext common.ContextKey = "jsonRpcCache"
)

const (
	// Cache envelope format (prefix):
	// [0..3]  magic "ERPC"
	// [4]     version (1)
	// [5..12] big-endian unix seconds (cached-at)
	cacheEnvelopeMagic   = "ERPC"
	cacheEnvelopeVersion = byte(1)
	cacheEnvelopeHeader  = 4 + 1 + 8
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

	cache := &EvmJsonRpcCache{
		policies: policies,
		logger:   logger,
	}

	// Initialize compression if configured
	if cfg.Compression != nil && cfg.Compression.Enabled != nil && *cfg.Compression.Enabled {
		cache.compressionEnabled = true

		// Set compression threshold (default to 512 bytes if not specified)
		cache.compressionThreshold = 512
		if cfg.Compression.Threshold > 0 {
			cache.compressionThreshold = cfg.Compression.Threshold
		}

		// Set compression level
		cache.compressionLevel = zstd.SpeedFastest // Default for optimal caching performance
		if cfg.Compression.ZstdLevel != "" {
			switch strings.ToLower(cfg.Compression.ZstdLevel) {
			case "fastest":
				cache.compressionLevel = zstd.SpeedFastest
			case "default":
				cache.compressionLevel = zstd.SpeedDefault
			case "better":
				cache.compressionLevel = zstd.SpeedBetterCompression
			case "best":
				cache.compressionLevel = zstd.SpeedBestCompression
			default:
				logger.Warn().Str("level", cfg.Compression.ZstdLevel).Msg("unknown compression level, using 'fastest'")
			}
		}

		// Initialize encoder pool
		cache.encoderPool = &sync.Pool{
			New: func() interface{} {
				encoder, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(cache.compressionLevel))
				if err != nil {
					logger.Error().Err(err).Msg("failed to create zstd encoder in pool")
					return nil
				}
				return encoder
			},
		}

		// Initialize decoder pool
		cache.decoderPool = &sync.Pool{
			New: func() interface{} {
				decoder, err := zstd.NewReader(nil)
				if err != nil {
					logger.Error().Err(err).Msg("failed to create zstd decoder in pool")
					return nil
				}
				return decoder
			},
		}

		logger.Info().
			Bool("enabled", cache.compressionEnabled).
			Int("threshold", cache.compressionThreshold).
			Str("level", cache.compressionLevel.String()).
			Msg("cache compression configured")
	}

	return cache, nil
}

func (c *EvmJsonRpcCache) WithProjectId(projectId string) *EvmJsonRpcCache {
	lg := c.logger.With().Str("projectId", projectId).Logger()
	lg.Debug().Msgf("cloning EvmJsonRpcCache for project")
	return &EvmJsonRpcCache{
		logger:               &lg,
		policies:             c.policies,
		projectId:            projectId,
		compressionEnabled:   c.compressionEnabled,
		compressionThreshold: c.compressionThreshold,
		compressionLevel:     c.compressionLevel,
		encoderPool:          c.encoderPool,
		decoderPool:          c.decoderPool,
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
	finState := req.Finality(ctx)
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
		telemetry.MetricCacheGetSkippedTotal.WithLabelValues(
			c.projectId,
			req.NetworkLabel(),
			rpcReq.Method,
		).Inc()
		span.SetAttributes(attribute.Bool("cache.hit", false))
		policySpan.End()
		return nil, nil
	}

	policySpan.End()

	var jrr *common.JsonRpcResponse
	var cachedAt int64
	var connector data.Connector
	var policy *data.CachePolicy
	// Track context for correct miss attribution
	var lastMissConnectorId, lastMissPolicyStr, lastMissTTL string
	var lastRejectConnectorId, lastRejectPolicyStr, lastRejectTTL string
	for _, policy = range policies {
		connector = policy.GetConnector()
		policyCtx, policySpan := common.StartDetailSpan(ctx, "Cache.GetForPolicy", trace.WithAttributes(
			attribute.String("cache.policy_summary", policy.String()),
			attribute.String("cache.connector_id", connector.Id()),
		))
		jrr, cachedAt, err = c.doGet(policyCtx, connector, req, rpcReq)
		if err != nil {
			common.SetTraceSpanError(policySpan, err)
			telemetry.MetricCacheGetErrorTotal.WithLabelValues(
				c.projectId,
				req.NetworkLabel(),
				rpcReq.Method,
				connector.Id(),
				policy.String(),
				policy.GetTTL().String(),
				common.ErrorSummary(err),
			).Inc()
			telemetry.MetricCacheGetErrorDuration.WithLabelValues(
				c.projectId,
				req.NetworkLabel(),
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

		// Record a miss attribution for this attempt if it returned nil without error
		if err == nil && jrr == nil && policy != nil {
			lastMissConnectorId = connector.Id()
			lastMissPolicyStr = policy.String()
			lastMissTTL = policy.GetTTL().String()
		}

		policySpan.End()
		if jrr != nil {
			// Validate the cached result's age against the policy's TTL
			if c.shouldAcceptCachedResult(ctx, req, jrr, policy) {
				// Result is acceptable, use it
				break
			} else {
				// Result is too old, reject it and try the next policy
				c.logger.Debug().Str("connector", connector.Id()).Interface("id", req.ID()).Msg("cached result rejected due to age exceeding TTL")
				// Record last rejection context to attribute miss correctly
				lastRejectConnectorId = connector.Id()
				lastRejectPolicyStr = policy.String()
				lastRejectTTL = policy.GetTTL().String()
				jrr = nil
				continue
			}
		}
	}

	if jrr == nil {
		// Prefer attributing miss to age-guard rejection if any, otherwise the last miss
		labelConnectorId := connector.Id()
		labelPolicyStr := policy.String()
		labelTTL := policy.GetTTL().String()
		if lastRejectConnectorId != "" {
			labelConnectorId = lastRejectConnectorId
			labelPolicyStr = lastRejectPolicyStr
			labelTTL = lastRejectTTL
		} else if lastMissConnectorId != "" {
			labelConnectorId = lastMissConnectorId
			labelPolicyStr = lastMissPolicyStr
			labelTTL = lastMissTTL
		}

		telemetry.MetricCacheGetSuccessMissTotal.WithLabelValues(
			c.projectId,
			req.NetworkLabel(),
			rpcReq.Method,
			labelConnectorId,
			labelPolicyStr,
			labelTTL,
		).Inc()
		telemetry.MetricCacheGetSuccessMissDuration.WithLabelValues(
			c.projectId,
			req.NetworkLabel(),
			rpcReq.Method,
			labelConnectorId,
			labelPolicyStr,
			labelTTL,
		).Observe(time.Since(start).Seconds())
		span.SetAttributes(attribute.Bool("cache.hit", false))
		return nil, nil
	}

	if jrr.IsResultEmptyish() {
		switch policy.EmptyState() {
		case common.CacheEmptyBehaviorIgnore:
			// Treat as cache miss - return nil to indicate no cached data
			telemetry.MetricCacheGetSuccessMissTotal.WithLabelValues(
				c.projectId,
				req.NetworkLabel(),
				rpcReq.Method,
				connector.Id(),
				policy.String(),
				policy.GetTTL().String(),
			).Inc()
			telemetry.MetricCacheGetSuccessMissDuration.WithLabelValues(
				c.projectId,
				req.NetworkLabel(),
				rpcReq.Method,
				connector.Id(),
				policy.String(),
				policy.GetTTL().String(),
			).Observe(time.Since(start).Seconds())
			span.SetAttributes(attribute.Bool("cache.hit", false))
			return nil, nil
		case common.CacheEmptyBehaviorAllow, common.CacheEmptyBehaviorOnly:
			// Continue to create and return the response
			break
		}
	}

	resp := common.NewNormalizedResponse().
		WithRequest(req).
		WithFromCache(true).
		WithJsonRpcResponse(jrr)
	if cachedAt > 0 {
		resp.SetCacheStoredAtUnix(cachedAt)
	}

	telemetry.MetricCacheGetSuccessHitTotal.WithLabelValues(
		c.projectId,
		req.NetworkLabel(),
		rpcReq.Method,
		connector.Id(),
		policy.String(),
		policy.GetTTL().String(),
	).Inc()
	telemetry.MetricCacheGetSuccessHitDuration.WithLabelValues(
		c.projectId,
		req.NetworkLabel(),
		rpcReq.Method,
		connector.Id(),
		policy.String(),
		policy.GetTTL().String(),
	).Observe(time.Since(start).Seconds())
	span.SetAttributes(attribute.Bool("cache.hit", true))
	if c.logger.GetLevel() <= zerolog.DebugLevel {
		result := jrr.GetResultBytes()
		if common.IsSemiValidJson(result) {
			c.logger.Trace().Str("method", rpcReq.Method).Interface("id", req.ID()).RawJSON("result", result).Msg("returning cached response")
		} else {
			c.logger.Trace().Str("method", rpcReq.Method).Interface("id", req.ID()).Str("result", jrr.GetResultString()).Msg("returning cached response")
		}
	} else {
		c.logger.Debug().Str("method", rpcReq.Method).Interface("id", req.ID()).Msg("returning cached response")
	}

	return resp, nil
}

func (c *EvmJsonRpcCache) Set(ctx context.Context, req *common.NormalizedRequest, resp *common.NormalizedResponse) error {
	upsId := "n/a"
	if resp != nil && resp.Upstream() != nil {
		upsId = resp.Upstream().Id()
	}
	ctx, span := common.StartSpan(ctx, "Cache.Set", trace.WithAttributes(
		attribute.String("upstream.id", upsId),
	))
	defer span.End()

	if common.IsTracingDetailed {
		span.SetAttributes(
			attribute.String("request.id", fmt.Sprintf("%v", req.ID())),
		)
	}

	// TODO after subscription epic this method can be called for every new block data to pre-populate the cache,
	// based on the evmJsonRpcCache.hydration.filters which is only the data (logs, txs) that user cares about.
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

	// Use response finality if available, otherwise fall back to request finality
	var finState common.DataFinalityState
	if resp != nil {
		finState = resp.Finality(ctx)
	} else {
		finState = req.Finality(ctx)
	}
	isEmptyish := resp == nil || resp.IsResultEmptyish()
	policies, err := c.findSetPolicies(ntwId, rpcReq.Method, rpcReq.Params, finState, isEmptyish)
	span.SetAttributes(
		attribute.String("block.finality", finState.String()),
		attribute.Int("cache.policies_matched", len(policies)),
		attribute.Bool("response.emptyish", isEmptyish),
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
			RawJSON("result", rpcResp.GetResultBytes()).
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
					telemetry.MetricCacheSetErrorTotal.WithLabelValues(
						c.projectId,
						req.NetworkLabel(),
						rpcReq.Method,
						connector.Id(),
						policy.String(),
						ttl.String(),
						common.ErrorSummary(err),
					).Inc()
					telemetry.MetricCacheSetErrorDuration.WithLabelValues(
						c.projectId,
						req.NetworkLabel(),
						rpcReq.Method,
						connector.Id(),
						policy.String(),
						ttl.String(),
						common.ErrorSummary(err),
					).Observe(time.Since(start).Seconds())
					errsMu.Lock()
					errs = append(errs, err)
					errsMu.Unlock()
				} else {
					telemetry.MetricCacheSetSkippedTotal.WithLabelValues(
						c.projectId,
						req.NetworkLabel(),
						rpcReq.Method,
						connector.Id(),
						policy.String(),
						ttl.String(),
					).Inc()
				}
				return
			}

			// Compress the value before storing if compression is enabled
			valueToStore := rpcResp.GetResultBytes()
			telemetry.MetricCacheSetOriginalBytes.WithLabelValues(
				c.projectId,
				req.NetworkLabel(),
				rpcReq.Method,
				connector.Id(),
				policy.String(),
				ttl.String(),
			).Add(float64(len(valueToStore)))

			storedValue := wrapCacheEnvelope(valueToStore)
			if c.compressionEnabled && len(storedValue) >= c.compressionThreshold {
				compressedValue, isCompressed := c.compressValueBytes(storedValue)
				if isCompressed {
					originalSize := len(storedValue)
					compressedSize := len(compressedValue)
					savings := float64(originalSize-compressedSize) / float64(originalSize) * 100
					lg.Debug().
						Int("originalSize", originalSize).
						Int("compressedSize", compressedSize).
						Float64("savings", savings).
						Msg("compressed cache value")
					telemetry.MetricCacheSetCompressedBytes.WithLabelValues(
						c.projectId,
						req.NetworkLabel(),
						rpcReq.Method,
						connector.Id(),
						policy.String(),
						ttl.String(),
					).Add(float64(compressedSize))
					storedValue = compressedValue
				}
			}

			ctx, cancel := context.WithTimeoutCause(ctx, 5*time.Second, errors.New("evm json-rpc cache driver timeout during set"))
			defer cancel()
			err = connector.Set(ctx, pk, rk, storedValue, ttl)
			if err != nil {
				errsMu.Lock()
				errs = append(errs, err)
				errsMu.Unlock()
				telemetry.MetricCacheSetErrorTotal.WithLabelValues(
					c.projectId,
					req.NetworkLabel(),
					rpcReq.Method,
					connector.Id(),
					policy.String(),
					ttl.String(),
					common.ErrorSummary(err),
				).Inc()
				telemetry.MetricCacheSetErrorDuration.WithLabelValues(
					c.projectId,
					req.NetworkLabel(),
					rpcReq.Method,
					connector.Id(),
					policy.String(),
					ttl.String(),
					common.ErrorSummary(err),
				).Observe(time.Since(start).Seconds())
			} else {
				telemetry.MetricCacheSetSuccessTotal.WithLabelValues(
					c.projectId,
					req.NetworkLabel(),
					rpcReq.Method,
					connector.Id(),
					policy.String(),
					ttl.String(),
				).Inc()
				telemetry.MetricCacheSetSuccessDuration.WithLabelValues(
					c.projectId,
					req.NetworkLabel(),
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

func (c *EvmJsonRpcCache) IsObjectNull() bool {
	return c == nil || c.logger == nil
}

// shouldAcceptCachedResult checks if a cached result should be accepted based on its age
// It compares the block timestamp against the policy's TTL to ensure freshness.
// This validation only applies to realtime finality data (e.g., eth_gasPrice, latest block).
// For finalized/unfinalized/unknown finality, block data is immutable and should always be accepted
// regardless of how old the block timestamp is.
func (c *EvmJsonRpcCache) shouldAcceptCachedResult(
	ctx context.Context,
	req *common.NormalizedRequest,
	jrr *common.JsonRpcResponse,
	policy *data.CachePolicy,
) bool {
	// Only apply age guard for realtime finality.
	// Finalized/unfinalized/unknown data is immutable - a block from 2022 is still valid today.
	// The age guard is only meaningful for realtime queries (eth_gasPrice, latest block, etc.)
	// where users expect fresh data that changes with each new block.
	finality := req.Finality(ctx)
	if finality != common.DataFinalityStateRealtime {
		return true
	}

	// If no TTL is set, accept the result
	ttl := policy.GetTTL()
	if ttl == nil || *ttl <= 0 {
		return true
	}

	// Try to extract block timestamp from the response
	// We need to create a temporary NormalizedResponse to use the existing extraction logic
	nr := common.NewNormalizedResponse().
		WithRequest(req).
		WithJsonRpcResponse(jrr)

	blockTimestamp, err := ExtractBlockTimestampFromResponse(ctx, nr)
	if err != nil || blockTimestamp <= 0 {
		// If we can't extract a timestamp (e.g., for methods that don't have block data),
		// we can't enforce age-based validation, so accept the result
		if c.logger.GetLevel() <= zerolog.TraceLevel {
			method, _ := req.Method()
			c.logger.Trace().
				Err(err).
				Str("method", method).
				Msg("cannot extract block timestamp for age validation, accepting cached result")
		}
		return true
	}

	// Calculate the age of the block
	now := time.Now().Unix()
	age := time.Duration(now-blockTimestamp) * time.Second

	// Check if the age exceeds the TTL
	if age > *ttl {
		if c.logger.GetLevel() <= zerolog.DebugLevel {
			c.logger.Debug().
				Dur("age", age).
				Dur("ttl", *ttl).
				Int64("blockTimestamp", blockTimestamp).
				Int64("now", now).
				Str("policy", policy.String()).
				Msg("rejecting cached result because block age exceeds policy TTL")
		}

		// Record metric for age-guard rejection
		method, _ := req.Method()
		telemetry.MetricCacheGetAgeGuardRejectTotal.WithLabelValues(
			c.projectId,
			req.NetworkLabel(),
			method,
			policy.GetConnector().Id(),
			policy.String(),
			ttl.String(),
		).Inc()

		return false
	}

	// Accept the result as it's within the acceptable age
	return true
}

func (c *EvmJsonRpcCache) findSetPolicies(networkId, method string, params []interface{}, finality common.DataFinalityState, isEmptyish bool) ([]*data.CachePolicy, error) {
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

		match, err := policy.MatchesForSet(networkId, method, params, finality, isEmptyish)
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

func (c *EvmJsonRpcCache) doGet(ctx context.Context, connector data.Connector, req *common.NormalizedRequest, rpcReq *common.JsonRpcRequest) (*common.JsonRpcResponse, int64, error) {
	rpcReq.RLockWithTrace(ctx)
	defer rpcReq.RUnlock()

	blockRef, _, err := ExtractBlockReferenceFromRequest(ctx, req)
	if err != nil {
		return nil, 0, err
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
		return nil, 0, nil
	}

	groupKey, requestKey, err := generateKeysForJsonRpcRequest(req, blockRef, ctx)
	if err != nil {
		return nil, 0, err
	}

	c.logger.Trace().Str("pk", groupKey).Str("rk", requestKey).Msg("fetching from cache")

	var resultBytes []byte
	if blockRef == "*" {
		resultBytes, err = connector.Get(ctx, data.ConnectorReverseIndex, groupKey, requestKey, req)
	} else {
		resultBytes, err = connector.Get(ctx, data.ConnectorMainIndex, groupKey, requestKey, req)
	}
	if err != nil {
		return nil, 0, err
	}
	if len(resultBytes) == 0 {
		return nil, 0, nil
	}

	// Check if it's compressed data
	if c.compressionEnabled && c.isCompressed(resultBytes) {
		decompressed, err := c.decompressValueBytes(resultBytes)
		if err != nil {
			c.logger.Error().Err(err).Msg("failed to decompress cached value")
			return nil, 0, fmt.Errorf("failed to decompress cached value: %w", err)
		}
		c.logger.Debug().
			Int("compressedSize", len(resultBytes)).
			Int("decompressedSize", len(decompressed)).
			Msg("decompressed cache value")
		resultBytes = decompressed
	}

	resultBytes, cachedAt, _ := unwrapCacheEnvelope(resultBytes)
	if c.isCacheEntryStale(req, cachedAt) {
		// Best-effort delete of stale entry
		if delErr := connector.Delete(ctx, groupKey, requestKey); delErr != nil {
			c.logger.Debug().Err(delErr).Str("pk", groupKey).Str("rk", requestKey).Msg("failed to delete stale cache entry")
		}
		return nil, 0, nil
	}

	jrr, err := common.NewJsonRpcResponseFromBytes(nil, resultBytes, nil)
	if err != nil {
		return nil, 0, err
	}
	_ = jrr.SetID(rpcReq.ID)

	return jrr, cachedAt, nil
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

	size := rpcResp.ResultLength()
	// Check if the response size is within the limits
	if !policy.MatchesSizeLimits(size) {
		lg.Debug().Int("size", size).Msg("skip caching because response size does not match policy limits")
		return false, nil
	}
	result := rpcResp.GetResultBytes()
	// Check if we should cache empty results
	isEmpty := resp == nil || rpcResp == nil || result == nil || resp.IsObjectNull() || resp.IsResultEmptyish()
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

// wrapCacheEnvelope prefixes cached result bytes with envelope metadata.
func wrapCacheEnvelope(result []byte) []byte {
	cachedAt := time.Now().Unix()
	if len(result) > math.MaxInt-cacheEnvelopeHeader {
		return result
	}
	out := make([]byte, cacheEnvelopeHeader+len(result))
	copy(out[:4], []byte(cacheEnvelopeMagic))
	out[4] = cacheEnvelopeVersion
	binary.BigEndian.PutUint64(out[5:13], uint64(cachedAt))
	copy(out[cacheEnvelopeHeader:], result)
	return out
}

// unwrapCacheEnvelope returns (payload, cachedAtUnix, ok).
func unwrapCacheEnvelope(data []byte) ([]byte, int64, bool) {
	if len(data) < cacheEnvelopeHeader {
		return data, 0, false
	}
	if string(data[:4]) != cacheEnvelopeMagic {
		return data, 0, false
	}
	if data[4] != cacheEnvelopeVersion {
		return data, 0, false
	}
	cachedAt := int64(binary.BigEndian.Uint64(data[5:13]))
	return data[cacheEnvelopeHeader:], cachedAt, true
}

func (c *EvmJsonRpcCache) isCacheEntryStale(req *common.NormalizedRequest, cachedAt int64) bool {
	if req == nil {
		return false
	}
	maxAge := req.CacheMaxAgeSeconds()
	if maxAge == nil {
		return false
	}
	if *maxAge < 0 {
		return false
	}
	if cachedAt <= 0 {
		return true
	}
	age := time.Now().Unix() - cachedAt
	if age < 0 {
		age = 0
	}
	return age > *maxAge
}

// compressValueBytes compresses byte data using zstd
func (c *EvmJsonRpcCache) compressValueBytes(value []byte) ([]byte, bool) {
	if !c.compressionEnabled || len(value) < c.compressionThreshold {
		return value, false
	}

	// Get encoder from pool
	encoderInterface := c.encoderPool.Get()
	if encoderInterface == nil {
		c.logger.Warn().Msg("failed to get encoder from pool, storing uncompressed")
		return value, false
	}
	encoder := encoderInterface.(*zstd.Encoder)
	defer c.encoderPool.Put(encoder)

	// Compress using the pooled encoder
	var buf bytes.Buffer
	encoder.Reset(&buf)
	if _, err := encoder.Write(value); err != nil {
		c.logger.Warn().Err(err).Msg("failed to compress value, storing uncompressed")
		return value, false
	}

	if err := encoder.Close(); err != nil {
		c.logger.Warn().Err(err).Msg("failed to close zstd encoder, storing uncompressed")
		return value, false
	}

	compressed := buf.Bytes()

	// Only use compression if it actually saves space
	if len(compressed) < len(value) {
		return compressed, true
	}

	return value, false
}

// isCompressed checks if data starts with zstd magic number
func (c *EvmJsonRpcCache) isCompressed(data []byte) bool {
	return len(data) >= 4 &&
		data[0] == 0x28 &&
		data[1] == 0xB5 &&
		data[2] == 0x2F &&
		data[3] == 0xFD
}

// decompressValueBytes decompresses zstd-compressed byte data
func (c *EvmJsonRpcCache) decompressValueBytes(compressedData []byte) ([]byte, error) {
	if !c.isCompressed(compressedData) {
		// Not compressed, return as-is
		return compressedData, nil
	}

	// Get decoder from pool
	decoderInterface := c.decoderPool.Get()
	if decoderInterface == nil {
		return nil, fmt.Errorf("failed to get decoder from pool")
	}
	decoder := decoderInterface.(*zstd.Decoder)
	defer c.decoderPool.Put(decoder)

	// Reset decoder with the compressed data
	if err := decoder.Reset(bytes.NewReader(compressedData)); err != nil {
		return nil, fmt.Errorf("failed to reset zstd decoder: %w", err)
	}

	// Read all decompressed data
	decompressed, err := io.ReadAll(decoder)
	if err != nil {
		return nil, fmt.Errorf("failed to decompress value: %w", err)
	}

	return decompressed, nil
}
