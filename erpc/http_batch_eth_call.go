package erpc

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/auth"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/trace"
)

// maxConcurrentCacheWrites limits concurrent multicall3 per-call cache write goroutines.
// This value balances memory usage with throughput - too low causes dropped writes under load,
// too high risks memory pressure. 100 is chosen as a reasonable default for most workloads.
const maxConcurrentCacheWrites = 100

// cacheWriteSem limits concurrent multicall3 per-call cache write goroutines
// to prevent unbounded goroutine growth under high load.
var cacheWriteSem = make(chan struct{}, maxConcurrentCacheWrites)

type ethCallBatchInfo struct {
	networkId  string
	blockRef   string
	blockParam interface{}
}

type ethCallBatchCandidate struct {
	index  int
	ctx    context.Context
	req    *common.NormalizedRequest
	logger zerolog.Logger
}

type ethCallBatchProbe struct {
	Method    string        `json:"method"`
	Params    []interface{} `json:"params"`
	NetworkId string        `json:"networkId"`
}

var (
	forwardBatchNetwork = func(ctx context.Context, network *Network, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		return network.Forward(ctx, req)
	}
	forwardBatchProject = func(ctx context.Context, project *PreparedProject, network *Network, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		return project.doForward(ctx, network, req)
	}
	newBatchJsonRpcResponse = common.NewJsonRpcResponse
)

// detectEthCallBatchInfo checks if a batch request is eligible for Multicall3 aggregation.
// Returns nil (no error) if the batch is not eligible due to:
// - Fewer than 2 requests in the batch
// - Any non-eth_call method in the batch
// - Requests targeting different networks
// - Requests targeting different block references
// - Non-EVM architecture
// Returns an error only for actual parsing/validation failures.
func detectEthCallBatchInfo(requests []json.RawMessage, architecture, chainId string) (*ethCallBatchInfo, error) {
	if len(requests) < 2 {
		return nil, nil
	}
	if architecture != "" && architecture != string(common.ArchitectureEvm) {
		return nil, nil
	}

	defaultNetworkId := ""
	if architecture != "" && chainId != "" {
		defaultNetworkId = fmt.Sprintf("%s:%s", architecture, chainId)
	}

	var networkId string
	var blockRef string
	var blockParam interface{}
	// Track requireCanonical state across requests:
	// 0 = not yet set, 1 = explicitly true, 2 = explicitly false, 3 = not specified (default true)
	var requireCanonicalState int

	for _, raw := range requests {
		var probe ethCallBatchProbe
		if err := common.SonicCfg.Unmarshal(raw, &probe); err != nil {
			return nil, err
		}
		if strings.ToLower(probe.Method) != "eth_call" {
			return nil, nil
		}

		reqNetworkId := defaultNetworkId
		if reqNetworkId == "" {
			reqNetworkId = probe.NetworkId
		}
		if reqNetworkId == "" || !strings.HasPrefix(reqNetworkId, "evm:") {
			return nil, nil
		}
		if networkId == "" {
			networkId = reqNetworkId
		} else if networkId != reqNetworkId {
			return nil, nil
		}

		param := interface{}("latest")
		if len(probe.Params) >= 2 {
			param = probe.Params[1]
		}
		bref, err := evm.NormalizeBlockParam(param)
		if err != nil {
			return nil, err
		}
		if blockRef == "" {
			blockRef = bref
			blockParam = param
		} else if blockRef != bref {
			return nil, nil
		}

		// Check for mixed requireCanonical values in block-hash params (EIP-1898)
		// We need to ensure all requests in a batch have compatible requireCanonical values,
		// otherwise the Multicall3 call won't honor individual semantics.
		// States: 0 = not yet set, 1 = true (explicit or default), 2 = explicitly false
		// Explicit true and absent (default true) are treated as compatible (both = 1)
		if blockObj, ok := param.(map[string]interface{}); ok {
			if _, hasBlockHash := blockObj["blockHash"]; hasBlockHash {
				currentState := 1 // default: true (EIP-1898 default)
				if reqCanonical, hasReqCanonical := blockObj["requireCanonical"]; hasReqCanonical {
					if reqCanonicalBool, ok := reqCanonical.(bool); ok && !reqCanonicalBool {
						currentState = 2 // explicitly false
					}
				}
				if requireCanonicalState == 0 {
					requireCanonicalState = currentState
				} else if requireCanonicalState != currentState {
					// Mixed requireCanonical values - not eligible for batching
					return nil, nil
				}
			}
		}
	}

	if networkId == "" || blockRef == "" {
		return nil, nil
	}

	return &ethCallBatchInfo{
		networkId:  networkId,
		blockRef:   blockRef,
		blockParam: blockParam,
	}, nil
}

// forwardEthCallBatchCandidates forwards individual eth_call requests in parallel as a fallback
// when Multicall3 aggregation fails or is not applicable.
//
// Rate limiting note: The network rate limit is skipped (withSkipNetworkRateLimit) because
// rate limits were already acquired for each request during the aggregation flow preparation.
// This prevents double-counting rate limits when falling back to individual requests.
func (s *HttpServer) forwardEthCallBatchCandidates(
	startedAt *time.Time,
	project *PreparedProject,
	network *Network,
	candidates []ethCallBatchCandidate,
	responses []interface{},
) {
	if project == nil || network == nil {
		err := common.NewErrInvalidRequest(fmt.Errorf("network not available for batch eth_call fallback"))
		for _, cand := range candidates {
			responses[cand.index] = processErrorBody(&cand.logger, startedAt, cand.req, err, s.serverCfg.IncludeErrorDetails)
			common.EndRequestSpan(cand.ctx, nil, err)
		}
		return
	}

	// Process candidates in parallel for better performance during fallback
	var wg sync.WaitGroup
	for _, cand := range candidates {
		wg.Add(1)
		go func(c ethCallBatchCandidate) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					panicErr := common.NewErrJsonRpcExceptionInternal(
						0,
						common.JsonRpcErrorServerSideException,
						fmt.Sprintf("internal error: panic in batch fallback: %v", r),
						nil,
						nil,
					)
					c.logger.Error().
						Str("panic", fmt.Sprintf("%v", r)).
						Str("stack", string(debug.Stack())).
						Msg("panic in forwardEthCallBatchCandidates goroutine")
					responses[c.index] = processErrorBody(&c.logger, startedAt, c.req, panicErr, s.serverCfg.IncludeErrorDetails)
					common.EndRequestSpan(c.ctx, nil, panicErr)
				}
			}()
			resp, err := forwardBatchProject(withSkipNetworkRateLimit(c.ctx), project, network, c.req)
			if err != nil {
				if resp != nil {
					resp.Release()
				}
				responses[c.index] = processErrorBody(&c.logger, startedAt, c.req, err, s.serverCfg.IncludeErrorDetails)
				common.EndRequestSpan(c.ctx, nil, err)
				return
			}

			responses[c.index] = resp
			common.EndRequestSpan(c.ctx, resp, nil)
		}(cand)
	}
	wg.Wait()
}

func (s *HttpServer) handleEthCallBatchAggregation(
	httpCtx context.Context,
	startedAt *time.Time,
	r *http.Request,
	project *PreparedProject,
	baseLogger zerolog.Logger,
	batchInfo *ethCallBatchInfo,
	requests []json.RawMessage,
	headers http.Header,
	queryArgs map[string][]string,
	responses []interface{},
) bool {
	if batchInfo == nil || project == nil {
		return false
	}

	network, networkErr := project.GetNetwork(httpCtx, batchInfo.networkId)
	uaMode := common.UserAgentTrackingModeSimplified
	if project.Config != nil && project.Config.UserAgentMode != "" {
		uaMode = project.Config.UserAgentMode
	}

	candidates := make([]ethCallBatchCandidate, 0, len(requests))
	for i, rawReq := range requests {
		nq := common.NewNormalizedRequest(rawReq)
		rawReq = nil
		requestCtx := common.StartRequestSpan(httpCtx, nq)

		clientIP := s.resolveRealClientIP(r)
		nq.SetClientIP(clientIP)

		if err := nq.Validate(); err != nil {
			responses[i] = processErrorBody(&baseLogger, startedAt, nq, err, &common.TRUE)
			common.EndRequestSpan(requestCtx, nil, err)
			continue
		}

		method, methodErr := nq.Method()
		if methodErr != nil {
			responses[i] = processErrorBody(&baseLogger, startedAt, nq, methodErr, &common.TRUE)
			common.EndRequestSpan(requestCtx, nil, methodErr)
			continue
		}
		rlg := baseLogger.With().Str("method", method).Logger()

		ap, err := auth.NewPayloadFromHttp(method, r.RemoteAddr, headers, queryArgs)
		if err != nil {
			responses[i] = processErrorBody(&rlg, startedAt, nq, err, &common.TRUE)
			common.EndRequestSpan(requestCtx, nil, err)
			continue
		}

		user, err := project.AuthenticateConsumer(requestCtx, nq, method, ap)
		if err != nil {
			responses[i] = processErrorBody(&rlg, startedAt, nq, err, s.serverCfg.IncludeErrorDetails)
			common.EndRequestSpan(requestCtx, nil, err)
			continue
		}
		if user != nil {
			rlg = rlg.With().Str("userId", user.Id).Logger()
		}
		nq.SetUser(user)

		if networkErr != nil || network == nil {
			err := networkErr
			if err == nil {
				err = common.NewErrNetworkNotFound(batchInfo.networkId)
			}
			responses[i] = processErrorBody(&rlg, startedAt, nq, err, s.serverCfg.IncludeErrorDetails)
			common.EndRequestSpan(requestCtx, nil, err)
			continue
		}

		nq.SetNetwork(network)
		nq.SetCacheDal(network.Cache())
		nq.ApplyDirectiveDefaults(network.Config().DirectiveDefaults)
		nq.EnrichFromHttp(headers, queryArgs, uaMode)
		rlg.Trace().Interface("directives", nq.Directives()).Msgf("applied request directives")

		// Acquire project rate limit early (for billing/analytics purposes)
		if err := project.acquireRateLimitPermit(requestCtx, nq); err != nil {
			responses[i] = processErrorBody(&rlg, startedAt, nq, err, s.serverCfg.IncludeErrorDetails)
			common.EndRequestSpan(requestCtx, nil, err)
			continue
		}

		// Record per-request metric for billing/analytics
		// This ensures aggregated requests are counted individually
		reqFinality := nq.Finality(requestCtx)
		telemetry.CounterHandle(telemetry.MetricNetworkRequestsReceived,
			project.Config.Id, network.Label(), method, reqFinality.String(), nq.UserId(), nq.AgentName(),
		).Inc()

		candidates = append(candidates, ethCallBatchCandidate{
			index:  i,
			ctx:    requestCtx,
			req:    nq,
			logger: rlg,
		})
	}

	if len(candidates) == 0 {
		return true
	}

	projectId := ""
	if project.Config != nil {
		projectId = project.Config.Id
	}

	// Check cache for individual requests before aggregating
	// Respects skip-cache-read directive - requests with this directive skip cache probe
	cacheDal := network.Cache()
	var uncachedCandidates []ethCallBatchCandidate
	if cacheDal != nil && !cacheDal.IsObjectNull() {
		uncachedCandidates = make([]ethCallBatchCandidate, 0, len(candidates))
		cacheHits := 0
		for _, cand := range candidates {
			// Respect skip-cache-read directive - if set, skip cache probe entirely
			if cand.req.SkipCacheRead() {
				uncachedCandidates = append(uncachedCandidates, cand)
				continue
			}
			cachedResp, err := cacheDal.Get(cand.ctx, cand.req)
			if err != nil {
				// Log and track cache errors - they're non-fatal but indicate potential cache issues
				telemetry.MetricMulticall3CacheReadErrorsTotal.WithLabelValues(projectId, batchInfo.networkId).Inc()
				cand.logger.Warn().
					Err(err).
					Str("networkId", batchInfo.networkId).
					Msg("multicall3 pre-aggregation cache get failed, treating as miss")
			}
			if err == nil && cachedResp != nil && !cachedResp.IsObjectNull(cand.ctx) {
				// Cache hit - use cached response directly
				cachedResp.SetFromCache(true)
				responses[cand.index] = cachedResp
				common.EndRequestSpan(cand.ctx, cachedResp, nil)
				cacheHits++
				continue
			}
			// Cache miss - needs to be fetched
			uncachedCandidates = append(uncachedCandidates, cand)
		}
		if cacheHits > 0 {
			telemetry.MetricMulticall3CacheHitsTotal.WithLabelValues(projectId, batchInfo.networkId).Add(float64(cacheHits))
			baseLogger.Debug().
				Int("cacheHits", cacheHits).
				Int("uncached", len(uncachedCandidates)).
				Int("total", len(candidates)).
				Str("networkId", batchInfo.networkId).
				Msg("multicall3 pre-aggregation cache check")
		}
		candidates = uncachedCandidates
	}

	// All requests served from cache
	if len(candidates) == 0 {
		telemetry.MetricMulticall3AggregationTotal.WithLabelValues(projectId, batchInfo.networkId, "all_cached").Inc()
		return true
	}

	// Acquire network rate limits only for uncached requests that will hit the network
	// This prevents wasting rate limit permits on cache hits
	var rateLimitedCandidates []ethCallBatchCandidate
	for _, cand := range candidates {
		if err := network.acquireRateLimitPermit(cand.ctx, cand.req); err != nil {
			responses[cand.index] = processErrorBody(&cand.logger, startedAt, cand.req, err, s.serverCfg.IncludeErrorDetails)
			common.EndRequestSpan(cand.ctx, nil, err)
			continue
		}
		rateLimitedCandidates = append(rateLimitedCandidates, cand)
	}
	candidates = rateLimitedCandidates

	// All uncached requests were rate limited
	if len(candidates) == 0 {
		return true
	}

	if len(candidates) < 2 {
		s.forwardEthCallBatchCandidates(startedAt, project, network, candidates, responses)
		return true
	}

	reqs := make([]*common.NormalizedRequest, len(candidates))
	for i, cand := range candidates {
		reqs[i] = cand.req
	}

	mcReq, calls, err := evm.BuildMulticall3Request(reqs, batchInfo.blockParam)
	if err != nil {
		telemetry.MetricMulticall3FallbackTotal.WithLabelValues(projectId, batchInfo.networkId, "build_failed").Inc()
		baseLogger.Debug().Err(err).
			Int("candidateCount", len(candidates)).
			Str("networkId", batchInfo.networkId).
			Msg("multicall3 build failed, falling back")
		s.forwardEthCallBatchCandidates(startedAt, project, network, candidates, responses)
		return true
	}

	// Mark as composite to disable hedging - multicall3 requests should not be
	// hedged like normal requests as this would create duplicate batches
	mcReq.SetCompositeType(common.CompositeTypeMulticall3)

	mcCtx := withSkipNetworkRateLimit(httpCtx)
	mcResp, mcErr := forwardBatchNetwork(mcCtx, network, mcReq)
	if mcErr != nil {
		if mcResp != nil {
			mcResp.Release()
		}
		if evm.ShouldFallbackMulticall3(mcErr) {
			telemetry.MetricMulticall3FallbackTotal.WithLabelValues(projectId, batchInfo.networkId, "forward_failed").Inc()
			baseLogger.Debug().Err(mcErr).
				Int("candidateCount", len(candidates)).
				Str("networkId", batchInfo.networkId).
				Msg("multicall3 request failed, falling back")
			s.forwardEthCallBatchCandidates(startedAt, project, network, candidates, responses)
			return true
		}
		// Non-recoverable error - don't fallback, propagate to all candidates
		telemetry.MetricMulticall3AggregationTotal.WithLabelValues(projectId, batchInfo.networkId, "error").Inc()
		for _, cand := range candidates {
			responses[cand.index] = processErrorBody(&cand.logger, startedAt, cand.req, mcErr, s.serverCfg.IncludeErrorDetails)
			common.EndRequestSpan(cand.ctx, nil, mcErr)
		}
		return true
	}
	if mcResp == nil {
		telemetry.MetricMulticall3FallbackTotal.WithLabelValues(projectId, batchInfo.networkId, "nil_response").Inc()
		baseLogger.Debug().
			Int("candidateCount", len(candidates)).
			Str("networkId", batchInfo.networkId).
			Msg("multicall3 response missing, falling back")
		s.forwardEthCallBatchCandidates(startedAt, project, network, candidates, responses)
		return true
	}

	jrr, err := mcResp.JsonRpcResponse(mcCtx)
	if err != nil || jrr == nil || jrr.Error != nil {
		mcResp.Release()
		telemetry.MetricMulticall3FallbackTotal.WithLabelValues(projectId, batchInfo.networkId, "invalid_response").Inc()
		logEvent := baseLogger.Debug().Err(err).
			Int("candidateCount", len(candidates)).
			Str("networkId", batchInfo.networkId).
			Bool("missingResponse", jrr == nil).
			Bool("hasRpcError", jrr != nil && jrr.Error != nil)
		if jrr != nil && jrr.Error != nil {
			logEvent = logEvent.Int("rpcErrorCode", jrr.Error.Code).
				Str("rpcErrorMessage", jrr.Error.Message)
		}
		logEvent.Msg("multicall3 response invalid, falling back")
		s.forwardEthCallBatchCandidates(startedAt, project, network, candidates, responses)
		return true
	}

	var resultHex string
	if err := common.SonicCfg.Unmarshal(jrr.GetResultBytes(), &resultHex); err != nil {
		mcResp.Release()
		telemetry.MetricMulticall3FallbackTotal.WithLabelValues(projectId, batchInfo.networkId, "unmarshal_failed").Inc()
		baseLogger.Debug().Err(err).
			Int("candidateCount", len(candidates)).
			Str("networkId", batchInfo.networkId).
			Msg("multicall3 result unmarshal failed, falling back")
		s.forwardEthCallBatchCandidates(startedAt, project, network, candidates, responses)
		return true
	}
	resultBytes, err := common.HexToBytes(resultHex)
	if err != nil {
		mcResp.Release()
		telemetry.MetricMulticall3FallbackTotal.WithLabelValues(projectId, batchInfo.networkId, "hex_decode_failed").Inc()
		baseLogger.Debug().Err(err).
			Int("candidateCount", len(candidates)).
			Str("networkId", batchInfo.networkId).
			Msg("multicall3 result decode failed, falling back")
		s.forwardEthCallBatchCandidates(startedAt, project, network, candidates, responses)
		return true
	}

	decoded, err := evm.DecodeMulticall3Aggregate3Result(resultBytes)
	if err != nil {
		mcResp.Release()
		telemetry.MetricMulticall3FallbackTotal.WithLabelValues(projectId, batchInfo.networkId, "abi_decode_failed").Inc()
		baseLogger.Debug().Err(err).
			Int("candidateCount", len(candidates)).
			Str("networkId", batchInfo.networkId).
			Msg("multicall3 result parsing failed, falling back")
		s.forwardEthCallBatchCandidates(startedAt, project, network, candidates, responses)
		return true
	}
	if len(decoded) != len(calls) {
		mcResp.Release()
		// Length mismatch is a critical issue - use separate metric and Error level logging
		telemetry.MetricMulticall3FallbackTotal.WithLabelValues(projectId, batchInfo.networkId, "length_mismatch").Inc()
		baseLogger.Error().
			Int("candidateCount", len(candidates)).
			Int("decodedCount", len(decoded)).
			Int("callCount", len(calls)).
			Str("networkId", batchInfo.networkId).
			Msg("CRITICAL: multicall3 result length mismatch - possible data corruption, falling back")
		s.forwardEthCallBatchCandidates(startedAt, project, network, candidates, responses)
		return true
	}

	shouldCache := !mcResp.FromCache() && cacheDal != nil && !cacheDal.IsObjectNull()
	for i, result := range decoded {
		cand := candidates[i]
		if result.Success {
			returnHex := "0x" + hex.EncodeToString(result.ReturnData)
			jrr, err := newBatchJsonRpcResponse(cand.req.ID(), returnHex, nil)
			if err != nil {
				responses[cand.index] = processErrorBody(&cand.logger, startedAt, cand.req, err, s.serverCfg.IncludeErrorDetails)
				common.EndRequestSpan(cand.ctx, nil, err)
				continue
			}

			nr := common.NewNormalizedResponse().WithRequest(cand.req).WithJsonRpcResponse(jrr)
			nr.SetUpstream(mcResp.Upstream())
			nr.SetFromCache(mcResp.FromCache())
			nr.SetAttempts(mcResp.Attempts())
			nr.SetRetries(mcResp.Retries())
			nr.SetHedges(mcResp.Hedges())
			nr.SetEvmBlockRef(mcResp.EvmBlockRef())
			nr.SetEvmBlockNumber(mcResp.EvmBlockNumber())
			responses[cand.index] = nr
			common.EndRequestSpan(cand.ctx, nr, nil)

			// Cache individual response asynchronously with backpressure
			if shouldCache {
				// Try to acquire semaphore (non-blocking to avoid blocking response path)
				select {
				case cacheWriteSem <- struct{}{}:
					nr.RLockWithTrace(cand.ctx)
					nr.AddRef()
					go func(resp *common.NormalizedResponse, req *common.NormalizedRequest, reqCtx context.Context, lg zerolog.Logger) {
						defer func() { <-cacheWriteSem }() // Release semaphore
						defer func() {
							if rec := recover(); rec != nil {
								telemetry.MetricUnexpectedPanicTotal.WithLabelValues(
									"multicall3-cache-write",
									fmt.Sprintf("network:%s", batchInfo.networkId),
									common.ErrorFingerprint(rec),
								).Inc()
								lg.Error().
									Interface("panic", rec).
									Str("stack", string(debug.Stack())).
									Msg("unexpected panic on multicall3 per-call cache write")
							}
						}()
						defer resp.RUnlock()
						defer resp.DoneRef()

						timeoutCtx, timeoutCtxCancel := context.WithTimeoutCause(network.AppCtx(), 10*time.Second, errors.New("cache driver timeout during multicall3 per-call cache write"))
						defer timeoutCtxCancel()
						tracedCtx := trace.ContextWithSpanContext(timeoutCtx, trace.SpanContextFromContext(reqCtx))
						if err := cacheDal.Set(tracedCtx, req, resp); err != nil {
							lg.Warn().Err(err).Msg("could not store multicall3 per-call response in cache")
						}
					}(nr, cand.req, cand.ctx, cand.logger)
				default:
					// Semaphore full - skip cache write to avoid unbounded goroutine growth
					telemetry.MetricMulticall3CacheWriteDroppedTotal.WithLabelValues(projectId, batchInfo.networkId).Inc()
					cand.logger.Debug().Msg("skipping multicall3 per-call cache write due to backpressure")
				}
			}
			continue
		}

		dataHex := "0x" + hex.EncodeToString(result.ReturnData)
		callErr := common.NewErrEndpointExecutionException(
			common.NewErrJsonRpcExceptionInternal(
				0,
				common.JsonRpcErrorEvmReverted,
				"execution reverted",
				nil,
				map[string]interface{}{
					"data": dataHex,
				},
			),
		)
		responses[cand.index] = processErrorBody(&cand.logger, startedAt, cand.req, callErr, s.serverCfg.IncludeErrorDetails)
		common.EndRequestSpan(cand.ctx, nil, callErr)
	}

	// Aggregation succeeded
	telemetry.MetricMulticall3AggregationTotal.WithLabelValues(projectId, batchInfo.networkId, "success").Inc()
	mcResp.Release()
	return true
}
