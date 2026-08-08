package evm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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

func NewEvmJsonRpcCache(ctx context.Context, logger *zerolog.Logger, cfg *common.CacheConfig) (*EvmJsonRpcCache, error) {
	logger.Info().Msg("initializing evm json rpc cache...")

	// Create connectors map
	connectors := make(map[string]data.Connector)
	connectorTags := make(map[string][]string)
	for _, connCfg := range cfg.Connectors {
		c, err := data.NewConnector(ctx, logger, connCfg)
		if err != nil {
			return nil, fmt.Errorf("failed to create connector %s: %w", connCfg.Id, err)
		}
		connectors[connCfg.Id] = c
		connectorTags[connCfg.Id] = connCfg.Tags
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
		// Connector tags drive use-upstream gating of this policy's cache.
		policy.SetConnectorTags(connectorTags[policyCfg.Connector])
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

// observeGetLogsRange records the concrete block-range size of an eth_getLogs
// request into MetricCacheEvmGetLogsRange, tagged by the connector/policy/ttl
// involved and the hit/miss outcome. It is a no-op for non-getLogs methods and
// for requests whose range is not concrete (block tags, blockHash, malformed).
func (c *EvmJsonRpcCache) observeGetLogsRange(ctx context.Context, req *common.NormalizedRequest, rpcReq *common.JsonRpcRequest, connectorId, policy, ttl, outcome string) {
	if rpcReq == nil || rpcReq.Method != "eth_getLogs" {
		return
	}
	rangeSize, ok := getLogsConcreteRangeSize(ctx, rpcReq)
	if !ok {
		return
	}
	telemetry.MetricCacheEvmGetLogsRange.WithLabelValues(
		c.projectId,
		req.NetworkLabel(),
		connectorId,
		policy,
		ttl,
		outcome,
	).Observe(rangeSize)
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

	// Fan out cache reads in parallel across matching connectors. findGetPolicies
	// already deduped by connector, so each policy here represents a unique
	// connector. First accepted hit cancels peers; if every connector confirms
	// a miss (or errors/rejects), the request falls through to the upstream layer.
	type fanResult struct {
		jrr        *common.JsonRpcResponse
		policy     *data.CachePolicy
		connector  data.Connector
		err        error
		missReason string
	}

	fanCtx, cancelFan := context.WithCancel(ctx)
	defer cancelFan()

	// Defensive backstop: if the caller's context has no deadline and a
	// connector lacks a failsafe timeout, a hung connector could pin the
	// fan-out goroutine indefinitely — over time, FDs/connection-pool slots
	// leak per request. Cap the fan-out at 30s. Properly configured
	// connectors exit far earlier via their own failsafe timeout.
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var bsCancel context.CancelFunc
		fanCtx, bsCancel = context.WithTimeout(fanCtx, 30*time.Second)
		defer bsCancel()
	}

	// Buffer sized to the worst-case spawn count so late peers (after we've
	// already taken a winner) can post their result without blocking — we
	// don't drain stragglers; we let them GC with the channel.
	results := make(chan fanResult, len(policies))
	spawned := 0

	useUpstream := useUpstreamSelector(req)
	for _, p := range policies {
		conn := p.GetConnector()
		if req.ShouldSkipCacheRead(conn.Id()) {
			c.logger.Debug().Str("connector", conn.Id()).Interface("id", req.ID()).Msg("skipping cache connector due to skip-cache-read directive pattern")
			continue
		}
		if eligible, _ := p.MatchesUpstreamSelector(useUpstream); !eligible {
			c.logger.Debug().Str("connector", conn.Id()).Str("useUpstream", useUpstream).Interface("id", req.ID()).Msg("skipping cache connector due to use-upstream directive selector")
			continue
		}
		spawned++
		go func(policy *data.CachePolicy, connector data.Connector) {
			policyCtx, policySpan := common.StartDetailSpan(fanCtx, "Cache.GetForPolicy", trace.WithAttributes(
				attribute.String("cache.policy_summary", policy.String()),
				attribute.String("cache.connector_id", connector.Id()),
				attribute.String("cache.method", rpcReq.Method),
			))
			defer policySpan.End()

			jrr, err := c.doGet(policyCtx, connector, req, rpcReq)
			// Unconditional cancellation guard — runs regardless of whether
			// doGet returned an error. fanCtx is done either because a peer
			// connector already won (cancelFan), the caller's context was
			// cancelled, or the 30s defensive backstop expired. We treat any
			// outcome that arrives once fanCtx is done as "cancelled":
			//   - (err != nil): the inner failsafe may wrap the context error
			//     in a typed error that errors.Is can't unwind to
			//     context.Canceled — fanCtx.Err() is the authoritative signal
			//     so wrapped cancellation doesn't inflate connector_error.
			//   - (nil, nil): a buggy connector that swallows ctx cancellation
			//     internally and returns a silent miss — we shouldn't credit
			//     it as a genuine miss against this connector's policy.
			//   - (jrr, nil): a late-arriving hit after the winner already
			//     sent. The consumer will discard it anyway (jrr already set);
			//     marking cancelled avoids running shouldAcceptCachedResult /
			//     emptyish checks for a result that won't be used.
			if fanCtx.Err() != nil {
				policySpan.SetAttributes(attribute.String("cache.get_outcome", "cancelled"))
				return
			}
			if err != nil {
				// Semantic-miss errors: the connector is signalling
				// "no key" / "expired" / "data not available here", not a
				// real failure. Classify as miss so we don't inflate
				// connector_error metrics with normal cache misses.
				//   ErrRecordNotFound  — generic data connector miss
				//   ErrRecordExpired   — connector miss past TTL
				//   ErrEndpointMissingData — gRPC connector (e.g. prism)
				//     translation of "range outside available" / cold
				//     storage range, see common/grpc_errors.go
				if common.HasErrorCode(err, common.ErrCodeRecordNotFound) ||
					common.HasErrorCode(err, common.ErrCodeRecordExpired) ||
					common.HasErrorCode(err, common.ErrCodeEndpointMissingData) {
					policySpan.SetAttributes(attribute.String("cache.get_outcome", "miss"))
					select {
					case results <- fanResult{policy: policy, connector: connector, missReason: "empty_result"}:
					case <-fanCtx.Done():
					}
					return
				}
				common.SetTraceSpanError(policySpan, err)
				policySpan.SetAttributes(
					attribute.String("cache.get_outcome", "error"),
					attribute.String("cache.error", common.ErrorSummary(err)),
				)
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
				if c.logger.GetLevel() <= zerolog.DebugLevel {
					c.logger.Debug().Str("connector", connector.Id()).Interface("id", req.ID()).Err(err).Msg("cache connector errored during GET")
				}
				select {
				case results <- fanResult{policy: policy, connector: connector, err: err, missReason: "connector_error"}:
				case <-fanCtx.Done():
				}
				return
			}
			if jrr == nil {
				policySpan.SetAttributes(attribute.String("cache.get_outcome", "miss"))
				select {
				case results <- fanResult{policy: policy, connector: connector, missReason: "empty_result"}:
				case <-fanCtx.Done():
				}
				return
			}
			if !c.shouldAcceptCachedResult(ctx, req, jrr, policy) {
				c.logger.Debug().Str("connector", connector.Id()).Interface("id", req.ID()).Msg("cached result rejected due to age exceeding TTL")
				policySpan.SetAttributes(attribute.String("cache.get_outcome", "ttl_rejected"))
				select {
				case results <- fanResult{policy: policy, connector: connector, missReason: "ttl_rejected"}:
				case <-fanCtx.Done():
				}
				return
			}
			// An emptyish result under EmptyState=Ignore is a miss for THIS
			// policy — report as miss and let peer connectors keep racing.
			// Without this, the first emptyish result would win the fan-out,
			// cancel peers, and only THEN get reclassified as a miss by the
			// post-fan-out emptyish handling — losing the chance for a peer
			// with non-empty data or Allow policy to serve a real hit.
			if jrr.IsResultEmptyish() && policy.EmptyState() == common.CacheEmptyBehaviorIgnore {
				policySpan.SetAttributes(attribute.String("cache.get_outcome", "empty_ignored"))
				select {
				case results <- fanResult{policy: policy, connector: connector, missReason: "empty_result"}:
				case <-fanCtx.Done():
				}
				return
			}
			policySpan.SetAttributes(attribute.String("cache.get_outcome", "found"))
			select {
			case results <- fanResult{jrr: jrr, policy: policy, connector: connector}:
				cancelFan()
			case <-fanCtx.Done():
			}
		}(p, conn)
	}

	// Drain results until we get the first acceptable hit OR every spawned
	// goroutine has reported back OR the caller's context is cancelled. We
	// never wait for stragglers after a hit lands — they post into the
	// buffered channel and exit on their own. Slow peers no longer pin the
	// user-visible latency of a fast winner.
	var (
		jrr        *common.JsonRpcResponse
		policy     *data.CachePolicy
		connector  data.Connector
		lastMiss   *fanResult
		lastReject *fanResult
		lastError  *fanResult
		aborted    bool
	)
drain:
	for received := 0; received < spawned && jrr == nil; {
		select {
		case r := <-results:
			received++
			if r.jrr != nil {
				rr := r
				jrr = rr.jrr
				policy = rr.policy
				connector = rr.connector
				continue
			}
			switch r.missReason {
			case "ttl_rejected":
				rr := r
				lastReject = &rr
			case "empty_result":
				rr := r
				lastMiss = &rr
			case "connector_error":
				rr := r
				lastError = &rr
			}
		case <-fanCtx.Done():
			// fanCtx fires from any of: (a) caller cancelled the parent
			// ctx, (b) the 30s defensive backstop fired, (c) a winner
			// called cancelFan() AFTER sending its hit into the buffer.
			// Listening on fanCtx (not ctx) is required: if we only
			// watched ctx, the backstop timeout in case (b) would cancel
			// goroutines (so they return without sending) while leaving
			// this loop blocked forever on a parent that never deadlines.
			//
			// Before bailing, drain any results already in the buffer.
			// In case (c) the winner's send happened-before its cancelFan,
			// so the hit IS in the channel — Go's select just happened to
			// pick the Done branch over the receive branch. Picking up
			// that hit here avoids a phantom miss under the race.
			//
			// Mark the fan-out aborted: if no hit surfaces from the buffer
			// below, we exited because of cancellation (a)/(b), not because
			// every connector confirmed a genuine miss. The post-fan-out
			// block uses this to avoid recording a cancelled read as a
			// success_miss (which would inflate the miss count and attribute
			// the cancellation latency — e.g. a request-level failsafe
			// timeout ceiling — to the connector). Case (c) sets jrr below,
			// so this flag is irrelevant there.
			aborted = true
		drainBuffer:
			for {
				select {
				case r := <-results:
					received++
					if r.jrr != nil && jrr == nil {
						rr := r
						jrr = rr.jrr
						policy = rr.policy
						connector = rr.connector
					} else {
						switch r.missReason {
						case "ttl_rejected":
							rr := r
							lastReject = &rr
						case "empty_result":
							rr := r
							lastMiss = &rr
						case "connector_error":
							rr := r
							lastError = &rr
						}
					}
				default:
					break drainBuffer
				}
			}
			break drain
		}
	}

	if jrr == nil {
		// The fan-out was aborted by context cancellation (caller cancelled
		// the parent ctx, or the 30s defensive backstop fired) rather than
		// every connector confirming a genuine miss. This is NOT a cache
		// miss: counting it inflates success_miss with cancelled reads and
		// records the cancellation latency (often a fixed request-level
		// failsafe/hedge timeout ceiling) against the connector. Fall through
		// to the upstream layer without emitting a miss metric — mirroring the
		// per-goroutine "cancelled" guard above.
		if aborted {
			span.SetAttributes(
				attribute.Bool("cache.hit", false),
				attribute.String("cache.miss_reason", "cancelled"),
			)
			return nil, nil
		}

		// All connectors confirmed miss / errored / age-rejected. Attribute the
		// fall-through metric to the most informative outcome we observed,
		// preferring rejections over plain misses over errors.
		var labelConnector data.Connector
		var labelPolicy *data.CachePolicy
		missReason := "empty_result"
		switch {
		case lastReject != nil:
			labelConnector = lastReject.connector
			labelPolicy = lastReject.policy
			missReason = "ttl_rejected"
		case lastMiss != nil:
			labelConnector = lastMiss.connector
			labelPolicy = lastMiss.policy
			missReason = "connector_miss"
		case lastError != nil:
			labelConnector = lastError.connector
			labelPolicy = lastError.policy
			missReason = "connector_error"
		default:
			if len(policies) > 0 {
				labelPolicy = policies[0]
				labelConnector = labelPolicy.GetConnector()
			}
		}

		if labelConnector == nil || labelPolicy == nil {
			span.SetAttributes(attribute.Bool("cache.hit", false))
			return nil, nil
		}

		labelConnectorId := labelConnector.Id()
		labelPolicyStr := labelPolicy.String()
		labelTTL := labelPolicy.GetTTL().String()

		span.SetAttributes(
			attribute.String("cache.miss_reason", missReason),
			attribute.String("cache.miss_connector_id", labelConnectorId),
			attribute.String("cache.miss_policy", labelPolicyStr),
		)
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
		c.observeGetLogsRange(ctx, req, rpcReq, labelConnectorId, labelPolicyStr, labelTTL, "miss")
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
			c.observeGetLogsRange(ctx, req, rpcReq, connector.Id(), policy.String(), policy.GetTTL().String(), "miss")
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
	c.observeGetLogsRange(ctx, req, rpcReq, connector.Id(), policy.String(), policy.GetTTL().String(), "hit")
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
	useUpstream := useUpstreamSelector(req)
	for _, policy := range policies {
		// Don't write a response into a cache the request's use-upstream selector
		// excludes, so a source-tagged connector only stores matching data.
		if eligible, _ := policy.MatchesUpstreamSelector(useUpstream); !eligible {
			lg.Debug().Str("connector", policy.GetConnector().Id()).Str("useUpstream", useUpstream).Msg("skipping cache write due to use-upstream directive selector")
			continue
		}
		wg.Add(1)
		go func(policy *data.CachePolicy) {
			defer wg.Done()
			connector := policy.GetConnector()
			// Fixed TTL component for telemetry labels (stable values only).
			ttl := policy.GetTTL()
			// Storage expiry must match the read-side window: for a block-time
			// dynamic realtime TTL, the resolved value can exceed the fixed
			// fallback, and writing with the fallback would evict entries long
			// before the read-side age guard stops serving them.
			storageTTL := ttl
			if resolved := policy.ResolveTTL(networkBlockTime(req), defaultRealtimeColdStartTTL); resolved > 0 {
				storageTTL = &resolved
			}

			shouldCache, err := shouldCacheResponse(ctx, lg, resp, rpcResp, policy, finState)
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

			if c.compressionEnabled && len(valueToStore) >= c.compressionThreshold {
				compressedValue, isCompressed := c.compressValueBytes(valueToStore)
				if isCompressed {
					originalSize := len(valueToStore)
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
					valueToStore = compressedValue
				}
			}

			ctx, cancel := context.WithTimeoutCause(ctx, 5*time.Second, errors.New("evm json-rpc cache driver timeout during set"))
			defer cancel()
			err = connector.Set(ctx, pk, rk, valueToStore, storageTTL)
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

// defaultRealtimeColdStartTTL bounds realtime staleness when a policy sets
// ttlBlockTimeMultiplier but has no static ttl and the network's block time
// isn't known yet (cold start / not head-tracked), so the guard never accepts
// an unbounded-stale head.
const defaultRealtimeColdStartTTL = 2 * time.Second

// shouldAcceptCachedResult checks if a cached realtime result is still fresh enough to serve, by
// comparing a block timestamp against the policy's TTL. The timestamp is taken from the response
// when present; for responses that carry none (eth_blockNumber, eth_gasPrice, eth_getLogs) it falls
// back to the serving connector's reported latest-block timestamp (read-through connectors that
// implement data.CacheHeadReporter), so a lagging source is still caught for those methods.
// Applies only to realtime finality — finalized/unfinalized/unknown block data is immutable and is
// always accepted regardless of age.
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

	// Resolve the realtime age limit from the policy TTL: a fixed value, or one
	// derived from the network's estimated block time (object form). When
	// block-time-dynamic but the block time isn't known yet (cold start, or a
	// network without an estimate) it falls back to the configured value, or to
	// a safe default — so the guard always bounds staleness. No limit -> accept.
	effectiveTTL := policy.ResolveTTL(networkBlockTime(req), defaultRealtimeColdStartTTL)
	if effectiveTTL <= 0 {
		return true
	}

	// Try to extract block timestamp from the response
	// We need to create a temporary NormalizedResponse to use the existing extraction logic
	nr := common.NewNormalizedResponse().
		WithRequest(req).
		WithJsonRpcResponse(jrr)

	blockTimestamp, err := ExtractBlockTimestampFromResponse(ctx, nr)
	if err != nil || blockTimestamp <= 0 {
		// The response carries no block timestamp (e.g. eth_blockNumber, eth_gasPrice, eth_getLogs).
		// Fall back to the serving connector's reported latest-block timestamp so realtime freshness
		// can still be enforced for these methods when the connector is head-aware (read-through).
		blockTimestamp = 0
		if reporter, ok := policy.GetConnector().(data.CacheHeadReporter); ok {
			if ts, known := reporter.CacheLatestBlockTimestamp(req.NetworkId()); known && ts > 0 {
				blockTimestamp = ts
			}
		}
		if blockTimestamp <= 0 {
			// Still can't determine the age (connector not head-aware or head unknown), so accept.
			if c.logger.GetLevel() <= zerolog.TraceLevel {
				method, _ := req.Method()
				c.logger.Trace().
					Err(err).
					Str("method", method).
					Msg("cannot determine block timestamp for age validation, accepting cached result")
			}
			return true
		}
	}

	// Calculate the age of the block
	now := time.Now().Unix()
	age := time.Duration(now-blockTimestamp) * time.Second

	// Check if the age exceeds the TTL
	if age > effectiveTTL {
		if c.logger.GetLevel() <= zerolog.DebugLevel {
			c.logger.Debug().
				Dur("age", age).
				Dur("ttl", effectiveTTL).
				Int64("blockTimestamp", blockTimestamp).
				Int64("now", now).
				Str("policy", policy.String()).
				Msg("rejecting cached result because block age exceeds policy TTL")
		}

		// Record metric for age-guard rejection. Label with the policy's fixed
		// TTL component, not the block-time-resolved value — the latter varies
		// per sample (EMA-derived) and would explode label cardinality.
		method, _ := req.Method()
		telemetry.MetricCacheGetAgeGuardRejectTotal.WithLabelValues(
			c.projectId,
			req.NetworkLabel(),
			method,
			policy.GetConnector().Id(),
			policy.String(),
			policy.GetTTL().String(),
		).Inc()

		return false
	}

	// Accept the result as it's within the acceptable age
	return true
}

// networkBlockTime returns the request network's estimated block time, or 0 if
// it's not available (network unset, not head-tracked, or not yet warmed up).
func networkBlockTime(req *common.NormalizedRequest) time.Duration {
	ntw := req.Network()
	if ntw == nil {
		return 0
	}
	if p, ok := ntw.(interface{ EvmBlockTime() time.Duration }); ok {
		return p.EvmBlockTime()
	}
	return 0
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

func (c *EvmJsonRpcCache) doGet(ctx context.Context, connector data.Connector, req *common.NormalizedRequest, rpcReq *common.JsonRpcRequest) (*common.JsonRpcResponse, error) {
	rpcReq.RLockWithTrace(ctx)
	defer rpcReq.RUnlock()

	blockRef, _, err := ExtractBlockReferenceFromRequest(ctx, req)
	if err != nil {
		return nil, err
	}
	if blockRef == "" {
		// Add trace attribute for empty blockRef so we know WHY cache was skipped
		span := trace.SpanFromContext(ctx)
		span.SetAttributes(
			attribute.String("cache.skip_reason", "empty_block_ref"),
			attribute.String("cache.method", rpcReq.Method),
		)
		return nil, nil
	}

	groupKey, requestKey, err := generateKeysForJsonRpcRequest(req, blockRef, ctx)
	if err != nil {
		return nil, err
	}

	// Annotate the span with cache lookup details for debugging
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(
		attribute.String("cache.block_ref", blockRef),
		attribute.String("cache.group_key", groupKey),
		attribute.String("cache.request_key", requestKey),
		attribute.String("cache.connector_driver", connector.Id()),
	)

	var resultBytes []byte
	if blockRef == "*" {
		resultBytes, err = connector.Get(ctx, data.ConnectorReverseIndex, groupKey, requestKey, req)
	} else {
		resultBytes, err = connector.Get(ctx, data.ConnectorMainIndex, groupKey, requestKey, req)
	}
	if err != nil {
		span.SetAttributes(attribute.String("cache.connector_error", common.ErrorSummary(err)))
		return nil, err
	}
	if len(resultBytes) == 0 {
		span.SetAttributes(attribute.String("cache.connector_result", "empty_bytes"))
		return nil, nil
	}
	span.SetAttributes(
		attribute.String("cache.connector_result", "found"),
		attribute.Int("cache.result_bytes", len(resultBytes)),
	)

	// Check if it's compressed data
	if c.compressionEnabled && c.isCompressed(resultBytes) {
		decompressed, err := c.decompressValueBytes(resultBytes)
		if err != nil {
			c.logger.Error().Err(err).Msg("failed to decompress cached value")
			return nil, fmt.Errorf("failed to decompress cached value: %w", err)
		}
		c.logger.Debug().
			Int("compressedSize", len(resultBytes)).
			Int("decompressedSize", len(decompressed)).
			Msg("decompressed cache value")
		resultBytes = decompressed
	}

	jrr, err := common.NewJsonRpcResponseFromBytes(nil, resultBytes, nil)
	if err != nil {
		return nil, err
	}
	_ = jrr.SetID(rpcReq.ID)

	return jrr, nil
}

func shouldCacheResponse(
	ctx context.Context,
	lg zerolog.Logger,
	resp *common.NormalizedResponse,
	rpcResp *common.JsonRpcResponse,
	policy *data.CachePolicy,
	finality common.DataFinalityState,
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

	// Never persist a realtime response that is already behind the network
	// tip while the request runs under enforceHighestBlock: enforcement will
	// never serve such a value as-is, so caching it can only poison future
	// reads (e.g. the eth_blockNumber sawtooth: a lagging upstream's value
	// lands in the cache and is then served for a full TTL window). The tip
	// is resolved network-wide on purpose — the cache entry is shared by all
	// requests regardless of any use-upstream selector — and the guard fails
	// open when pollers don't know a tip yet.
	if finality == common.DataFinalityStateRealtime && resp != nil {
		if req := resp.Request(); req != nil {
			if dirs := req.Directives(); dirs != nil && dirs.EnforceHighestBlock {
				if ntw := req.Network(); ntw != nil {
					if _, respBlock, err := ExtractBlockReferenceFromResponse(ctx, resp); err == nil && respBlock > 0 {
						if tip := common.EvmHighestLatestBlockNumber(ntw, ctx); tip > respBlock {
							lg.Debug().
								Int64("responseBlockNumber", respBlock).
								Int64("knownHighestBlock", tip).
								Msg("skip caching realtime response older than the known highest block")
							return false, nil
						}
					}
				}
			}
		}
	}
	result := rpcResp.GetResultBytes()
	// Check if we should cache empty results
	isEmpty := resp == nil || rpcResp == nil || result == nil || resp.IsObjectNull() || resp.IsResultEmptyish()
	// Never cache an empty result for a not-yet-produced (future) block: the block
	// will exist later, so a cached null would be served as a wrong answer until the
	// TTL expires. This holds regardless of the policy's empty behavior.
	if isEmpty && resp != nil && emptyResultBeyondConfidence(ctx, resp.Request()) {
		lg.Debug().Msg("skip caching empty result for a not-yet-produced (future) block")
		return false, nil
	}
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

// useUpstreamSelector returns the request's use-upstream directive, used to gate
// which cache connectors may serve/store it (empty = no gating).
func useUpstreamSelector(req *common.NormalizedRequest) string {
	if d := req.Directives(); d != nil {
		return d.UseUpstream
	}
	return ""
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
