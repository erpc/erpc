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

// requireCanonical consistency states across batch requests
const (
	requireCanonicalUnset = 0
	requireCanonicalTrue  = 1
	requireCanonicalFalse = 2

	multicallFallbackModeFull     = "full"
	multicallFallbackModePartial  = "partial"
	multicallFallbackModeTerminal = "terminal"
)

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
	Method    string            `json:"method"`
	Params    []json.RawMessage `json:"params"`
	NetworkId string            `json:"networkId"`
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
	var requireCanonicalState int // one of requireCanonicalUnset, requireCanonicalTrue, requireCanonicalFalse

	for _, raw := range requests {
		var probe ethCallBatchProbe
		if err := common.SonicCfg.Unmarshal(raw, &probe); err != nil {
			return nil, err
		}
		if !strings.EqualFold(probe.Method, "eth_call") {
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

		var param interface{} = "latest"
		if len(probe.Params) >= 2 {
			if err := common.SonicCfg.Unmarshal(probe.Params[1], &param); err != nil {
				return nil, err
			}
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
		// Explicit true and absent (default true) are treated as compatible (both = requireCanonicalTrue)
		if blockObj, ok := param.(map[string]interface{}); ok {
			if _, hasBlockHash := blockObj["blockHash"]; hasBlockHash {
				currentState := requireCanonicalTrue // default: true (EIP-1898 default)
				if reqCanonical, hasReqCanonical := blockObj["requireCanonical"]; hasReqCanonical {
					if reqCanonicalBool, ok := reqCanonical.(bool); ok && !reqCanonicalBool {
						currentState = requireCanonicalFalse
					}
				}
				if requireCanonicalState == requireCanonicalUnset {
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
	reason string,
	mode string,
) {
	projectId := ""
	networkId := ""
	if len(candidates) > 0 {
		method := ""
		if project != nil && project.Config != nil {
			projectId = project.Config.Id
		}
		if network != nil {
			networkId = network.Id()
		}
		if reason == "" {
			reason = "unspecified"
		}
		if mode == "" {
			mode = multicallFallbackModeFull
		}
		telemetry.MetricMulticall3FallbackTotal.WithLabelValues(projectId, networkId, reason).Inc()
		telemetry.MetricMulticall3FallbackReasonMatrixTotal.WithLabelValues(projectId, networkId, reason, mode).Inc()
		if candidates[0].req != nil {
			method, _ = candidates[0].req.Method()
		}
		telemetry.AddNetworkAttemptReason(
			projectId,
			networkId,
			method,
			telemetry.AttemptReasonBatchFallback,
			len(candidates),
		)
	}

	if project == nil || network == nil {
		err := common.NewErrInvalidRequest(fmt.Errorf("network not available for batch eth_call fallback"))
		for _, cand := range candidates {
			responses[cand.index] = processErrorBody(&cand.logger, startedAt, cand.req, err, s.serverCfg.IncludeErrorDetails)
			common.EndRequestSpan(cand.ctx, nil, err)
			telemetry.MetricMulticall3FallbackRequestsTotal.WithLabelValues(projectId, networkId, "error").Inc()
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
					telemetry.MetricMulticall3FallbackRequestsTotal.WithLabelValues(projectId, networkId, "error").Inc()
				}
			}()
			resp, err := forwardBatchProject(withSkipNetworkRateLimit(c.ctx), project, network, c.req)
			if err != nil {
				if resp != nil {
					resp.Release()
				}
				responses[c.index] = processErrorBody(&c.logger, startedAt, c.req, err, s.serverCfg.IncludeErrorDetails)
				common.EndRequestSpan(c.ctx, nil, err)
				telemetry.MetricMulticall3FallbackRequestsTotal.WithLabelValues(projectId, networkId, "error").Inc()
				return
			}

			responses[c.index] = resp
			common.EndRequestSpan(c.ctx, resp, nil)
			telemetry.MetricMulticall3FallbackRequestsTotal.WithLabelValues(projectId, networkId, "success").Inc()
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
			telemetry.MetricMulticall3CacheHitsTotal.WithLabelValues(projectId, batchInfo.networkId, candidates[0].req.UserId()).Add(float64(cacheHits))
			baseLogger.Debug().
				Int("cacheHits", cacheHits).
				Int("uncached", len(uncachedCandidates)).
				Int("total", len(candidates)).
				Str("networkId", batchInfo.networkId).
				Msg("multicall3 pre-aggregation cache check")
		}
		if len(uncachedCandidates) > 0 {
			telemetry.MetricMulticall3BatchPercallCacheMissTotal.WithLabelValues(projectId, batchInfo.networkId, candidates[0].req.UserId()).Add(float64(len(uncachedCandidates)))
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
		s.forwardEthCallBatchCandidates(startedAt, project, network, candidates, responses, "single_candidate", multicallFallbackModeFull)
		return true
	}

	reqs := make([]*common.NormalizedRequest, len(candidates))
	for i, cand := range candidates {
		reqs[i] = cand.req
	}

	propagateTerminal := func(reason string, terminalErr error) bool {
		telemetry.MetricMulticall3AggregationTotal.WithLabelValues(projectId, batchInfo.networkId, "error").Inc()
		telemetry.MetricMulticall3FallbackReasonMatrixTotal.WithLabelValues(
			projectId,
			batchInfo.networkId,
			reason,
			multicallFallbackModeTerminal,
		).Inc()
		for _, cand := range candidates {
			responses[cand.index] = processErrorBody(&cand.logger, startedAt, cand.req, terminalErr, s.serverCfg.IncludeErrorDetails)
			common.EndRequestSpan(cand.ctx, nil, terminalErr)
		}
		return true
	}

	mcReq, calls, err := evm.BuildMulticall3Request(reqs, batchInfo.blockParam)
	if err != nil {
		baseLogger.Debug().Err(err).
			Int("candidateCount", len(candidates)).
			Str("networkId", batchInfo.networkId).
			Msg("multicall3 build failed, falling back")
		s.forwardEthCallBatchCandidates(startedAt, project, network, candidates, responses, "build_failed", multicallFallbackModeFull)
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
			baseLogger.Debug().Err(mcErr).
				Int("candidateCount", len(candidates)).
				Str("networkId", batchInfo.networkId).
				Msg("multicall3 request failed, falling back")
			s.forwardEthCallBatchCandidates(startedAt, project, network, candidates, responses, "forward_failed", multicallFallbackModeFull)
			return true
		}
		return propagateTerminal("forward_terminal", mcErr)
	}
	if mcResp == nil {
		baseLogger.Debug().
			Int("candidateCount", len(candidates)).
			Str("networkId", batchInfo.networkId).
			Msg("multicall3 response missing, treating as terminal")
		return propagateTerminal("nil_response", common.NewErrInternalServerError(errors.New("multicall3 response missing")))
	}

	jrr, err := mcResp.JsonRpcResponse(mcCtx)
	if err != nil || jrr == nil {
		mcResp.Release()
		baseLogger.Debug().Err(err).
			Int("candidateCount", len(candidates)).
			Str("networkId", batchInfo.networkId).
			Bool("missingResponse", jrr == nil).
			Msg("multicall3 response invalid, treating as terminal")
		return propagateTerminal("invalid_response", common.NewErrInternalServerError(fmt.Errorf("invalid multicall3 response: %w", err)))
	}
	if jrr.Error != nil {
		mcErr = common.NewErrEndpointExecutionException(jrr.Error)
		if evm.ShouldFallbackMulticall3(mcErr) {
			mcResp.Release()
			baseLogger.Debug().
				Int("candidateCount", len(candidates)).
				Int("rpcErrorCode", jrr.Error.Code).
				Str("rpcErrorMessage", jrr.Error.Message).
				Str("networkId", batchInfo.networkId).
				Msg("multicall3 rpc error recoverable, falling back")
			s.forwardEthCallBatchCandidates(startedAt, project, network, candidates, responses, "rpc_error_recoverable", multicallFallbackModeFull)
			return true
		}
		mcResp.Release()
		baseLogger.Debug().
			Int("candidateCount", len(candidates)).
			Int("rpcErrorCode", jrr.Error.Code).
			Str("rpcErrorMessage", jrr.Error.Message).
			Str("networkId", batchInfo.networkId).
			Msg("multicall3 rpc error terminal, not falling back")
		return propagateTerminal("rpc_error_terminal", mcErr)
	}

	var resultHex string
	if err := common.SonicCfg.Unmarshal(jrr.GetResultBytes(), &resultHex); err != nil {
		mcResp.Release()
		baseLogger.Debug().Err(err).
			Int("candidateCount", len(candidates)).
			Str("networkId", batchInfo.networkId).
			Msg("multicall3 result unmarshal failed, treating as terminal")
		return propagateTerminal("unmarshal_failed", common.NewErrInternalServerError(fmt.Errorf("multicall3 result unmarshal failed: %w", err)))
	}
	resultBytes, err := common.HexToBytes(resultHex)
	if err != nil {
		mcResp.Release()
		baseLogger.Debug().Err(err).
			Int("candidateCount", len(candidates)).
			Str("networkId", batchInfo.networkId).
			Msg("multicall3 result decode failed, treating as terminal")
		return propagateTerminal("hex_decode_failed", common.NewErrInternalServerError(fmt.Errorf("multicall3 result decode failed: %w", err)))
	}

	decoded, err := evm.DecodeMulticall3Aggregate3Result(resultBytes)
	if err != nil {
		mcResp.Release()
		baseLogger.Debug().Err(err).
			Int("candidateCount", len(candidates)).
			Str("networkId", batchInfo.networkId).
			Msg("multicall3 result parsing failed, treating as terminal")
		return propagateTerminal("abi_decode_failed", common.NewErrInternalServerError(fmt.Errorf("multicall3 result parsing failed: %w", err)))
	}
	if len(decoded) != len(calls) {
		mcResp.Release()
		baseLogger.Error().
			Int("candidateCount", len(candidates)).
			Int("decodedCount", len(decoded)).
			Int("callCount", len(calls)).
			Str("networkId", batchInfo.networkId).
			Msg("CRITICAL: multicall3 result length mismatch - possible data corruption, terminal")
		return propagateTerminal("length_mismatch", common.NewErrInternalServerError(fmt.Errorf("multicall3 result length mismatch decoded=%d calls=%d", len(decoded), len(calls))))
	}

	shouldCache := !mcResp.FromCache() && cacheDal != nil && !cacheDal.IsObjectNull()
	failedCandidates := make([]ethCallBatchCandidate, 0, len(decoded))
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
			nr.SetCacheStoredAtUnix(mcResp.CacheStoredAtUnix())
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
						} else {
							telemetry.MetricMulticall3BatchPercallCacheSetTotal.WithLabelValues(projectId, batchInfo.networkId, req.UserId()).Inc()
						}
					}(nr, cand.req, cand.ctx, cand.logger)
				default:
					// Semaphore full - skip cache write to avoid unbounded goroutine growth
					telemetry.MetricMulticall3CacheWriteDroppedTotal.WithLabelValues(projectId, batchInfo.networkId).Inc()
					cand.logger.Warn().Msg("skipping multicall3 per-call cache write due to backpressure")
				}
			}
			continue
		}
		failedCandidates = append(failedCandidates, cand)
	}

	if len(failedCandidates) > 0 {
		telemetry.MetricMulticall3AggregationTotal.WithLabelValues(projectId, batchInfo.networkId, "partial_fallback").Inc()
		mcResp.Release()
		s.forwardEthCallBatchCandidates(
			startedAt,
			project,
			network,
			failedCandidates,
			responses,
			"subcall_failed",
			multicallFallbackModePartial,
		)
		return true
	}

	// Aggregation succeeded
	telemetry.MetricMulticall3AggregationTotal.WithLabelValues(projectId, batchInfo.networkId, "success").Inc()
	mcResp.Release()
	return true
}
