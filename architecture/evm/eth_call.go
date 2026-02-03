package evm

import (
	"context"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
)

// Global batcher manager for network-level Multicall3 batching
var (
	globalBatcherManager *BatcherManager
	batcherManagerOnce   sync.Once
)

// defaultMulticall3AggregationConfig is the default config when Multicall3Aggregation
// is not explicitly configured. Enabled by default to match documented behavior.
var defaultMulticall3AggregationConfig = func() *common.Multicall3AggregationConfig {
	cfg := &common.Multicall3AggregationConfig{Enabled: true}
	cfg.SetDefaults()
	return cfg
}()

// GetBatcherManager returns the global batcher manager.
func GetBatcherManager() *BatcherManager {
	batcherManagerOnce.Do(func() {
		globalBatcherManager = NewBatcherManager()
	})
	return globalBatcherManager
}

// ShutdownBatcherManager shuts down the global batcher manager.
// Should be called during application shutdown.
func ShutdownBatcherManager() {
	if globalBatcherManager != nil {
		globalBatcherManager.Shutdown()
	}
}

// networkForwarder wraps a Network to implement Forwarder interface.
type networkForwarder struct {
	network common.Network
}

func (f *networkForwarder) Forward(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	return f.network.Forward(ctx, req)
}

func (f *networkForwarder) SetCache(ctx context.Context, req *common.NormalizedRequest, resp *common.NormalizedResponse) error {
	cache := f.network.Cache()
	if cache == nil || cache.IsObjectNull() {
		return nil
	}
	return cache.Set(ctx, req, resp)
}

// projectPreForward_eth_call is the pre-forward hook for eth_call requests.
// It handles Multicall3 batching when enabled, aggregating multiple eth_call requests
// into a single Multicall3 call for improved throughput.
//
// Returns:
//   - handled: true if the request was handled (either batched or forwarded directly)
//   - response: the response if handled, nil otherwise
//   - error: any error that occurred
//
// The function will forward the request directly (bypassing batching) when:
//   - Multicall3 aggregation is disabled in config
//   - The request is not eligible for batching (has gas/value/from fields, etc.)
//   - The batcher queue is full or at capacity
//   - The request's deadline is too tight for batching
func projectPreForward_eth_call(ctx context.Context, network common.Network, nq *common.NormalizedRequest) (bool, *common.NormalizedResponse, error) {
	jrq, err := nq.JsonRpcRequest()
	if err != nil {
		return false, nil, nil
	}

	if nq.ParentRequestId() != nil || nq.IsCompositeRequest() {
		return false, nil, nil
	}

	// Normalize params: ensure block param is present
	jrq.RLock()
	paramsLen := len(jrq.Params)
	jrq.RUnlock()

	if paramsLen == 0 {
		return false, nil, nil
	}

	// Add "latest" block param if missing (only 1 param)
	if paramsLen == 1 {
		jrq.Lock()
		jrq.Params = append(jrq.Params, "latest")
		jrq.Unlock()
	}

	if handled, resp, err := handleUserMulticall3(ctx, network, nq); handled || err != nil {
		return true, resp, err
	}

	// Get Multicall3 aggregation config, using defaults if not explicitly configured
	cfg := network.Config()
	var aggCfg *common.Multicall3AggregationConfig
	if cfg != nil && cfg.Evm != nil && cfg.Evm.Multicall3Aggregation != nil {
		aggCfg = cfg.Evm.Multicall3Aggregation
	} else {
		// Use default config (enabled by default)
		aggCfg = defaultMulticall3AggregationConfig
	}

	// Check if Multicall3 aggregation is explicitly disabled
	if !aggCfg.Enabled {
		// Batching disabled, use normal forward
		resp, err := network.Forward(ctx, nq)
		return true, resp, err
	}

	// Check eligibility for batching
	eligible, reason := IsEligibleForBatching(nq, aggCfg)
	if !eligible {
		// Not eligible, forward normally
		if logger := network.Logger(); logger != nil {
			logger.Debug().
				Str("reason", reason).
				Str("method", "eth_call").
				Msg("request not eligible for multicall3 batching")
		}
		resp, err := network.Forward(ctx, nq)
		return true, resp, err
	}

	// Extract call info for batching key
	_, _, blockRef, err := ExtractCallInfo(nq)
	if err != nil {
		resp, err := network.Forward(ctx, nq)
		return true, resp, err
	}

	// Build batching key
	projectId := network.ProjectId()
	if projectId == "" {
		projectId = fmt.Sprintf("network:%s", network.Id())
	}

	userId := ""
	if aggCfg.AllowCrossUserBatching == nil || !*aggCfg.AllowCrossUserBatching {
		userId = nq.UserId()
	}

	key := BatchingKey{
		ProjectId:     projectId,
		NetworkId:     network.Id(),
		BlockRef:      blockRef,
		DirectivesKey: DeriveDirectivesKey(nq.Directives()),
		UserId:        userId,
	}

	// Check cache before batching (unless skip-cache-read is set)
	if !nq.SkipCacheRead() {
		cache := network.Cache()
		if cache != nil && !cache.IsObjectNull() {
			cachedResp, cacheErr := cache.Get(ctx, nq)
			if cacheErr != nil {
				// Log cache errors but continue to batching
				if logger := network.Logger(); logger != nil {
					logger.Warn().
						Err(cacheErr).
						Str("networkId", network.Id()).
						Msg("multicall3 pre-batch cache get failed, continuing to batch")
				}
			} else if cachedResp != nil && !cachedResp.IsObjectNull(ctx) {
				// Cache hit - return cached response directly
				cachedResp.SetFromCache(true)
				return true, cachedResp, nil
			}
		}
	}

	// Get or create batcher for this project+network
	mgr := GetBatcherManager()
	forwarder := &networkForwarder{network: network}
	batcher := mgr.GetOrCreate(projectId, network.Id(), aggCfg, forwarder, network.Logger())
	if batcher == nil {
		// Batching disabled, forward normally
		resp, err := network.Forward(ctx, nq)
		return true, resp, err
	}

	// Enqueue request
	entry, bypass, err := batcher.Enqueue(ctx, key, nq)
	if err != nil || bypass {
		// Log enqueue errors for debugging (bypass without error is normal)
		if err != nil && network.Logger() != nil {
			network.Logger().Debug().
				Err(err).
				Str("projectId", projectId).
				Str("networkId", network.Id()).
				Msg("multicall3 enqueue failed, forwarding normally")
		}
		// Bypass batching, forward normally
		resp, err := network.Forward(ctx, nq)
		return true, resp, err
	}

	// Wait for batch result
	select {
	case result := <-entry.ResultCh:
		return true, result.Response, result.Error
	case <-ctx.Done():
		return true, nil, ctx.Err()
	}
}

func handleUserMulticall3(ctx context.Context, network common.Network, nq *common.NormalizedRequest) (bool, *common.NormalizedResponse, error) {
	if nq == nil || network == nil {
		return false, nil, nil
	}
	logger := network.Logger()
	logDebug := func(err error, msg string) {
		if err == nil || logger == nil {
			return
		}
		logger.Debug().Err(err).Msg(msg)
	}
	jrq, err := nq.JsonRpcRequest()
	if err != nil {
		logDebug(err, "handleUserMulticall3: json-rpc request parse failed, falling back")
		return false, nil, nil
	}

	jrq.RLock()
	method := strings.ToLower(jrq.Method)
	params := jrq.Params
	jrq.RUnlock()
	if method != "eth_call" || len(params) < 1 {
		return false, nil, nil
	}
	if len(params) > 2 {
		if logger != nil {
			logger.Debug().Msg("handleUserMulticall3: state override present, skipping optimization")
		}
		return false, nil, nil
	}

	callObj, ok := params[0].(map[string]interface{})
	if !ok {
		return false, nil, nil
	}
	toVal, ok := callObj["to"]
	if !ok {
		return false, nil, nil
	}
	toStr, ok := toVal.(string)
	if !ok || toStr == "" || !strings.EqualFold(toStr, multicall3Address) {
		return false, nil, nil
	}

	dataStr := ""
	if v, ok := callObj["data"].(string); ok {
		dataStr = v
	} else if v, ok := callObj["input"].(string); ok {
		dataStr = v
	}
	if dataStr == "" {
		return false, nil, nil
	}
	calldata, err := common.HexToBytes(dataStr)
	if err != nil {
		logDebug(err, "handleUserMulticall3: calldata hex decode failed, falling back")
		return false, nil, nil
	}

	decodedCalls, err := DecodeMulticall3Aggregate3Calls(calldata)
	if err != nil {
		logDebug(err, "handleUserMulticall3: multicall3 calldata decode failed, falling back")
		return false, nil, nil
	}
	for _, call := range decodedCalls {
		if !call.AllowFailure {
			if logger != nil {
				logger.Debug().Msg("handleUserMulticall3: allowFailure=false detected, skipping optimization")
			}
			return false, nil, nil
		}
	}

	blockParam := interface{}("latest")
	if len(params) >= 2 {
		blockParam = params[1]
	}

	perCallReqs := make([]*common.NormalizedRequest, len(decodedCalls))
	for i, call := range decodedCalls {
		callObj := map[string]interface{}{
			"to":   "0x" + hex.EncodeToString(call.Target),
			"from": multicall3Address,
			"data": "0x" + hex.EncodeToString(call.CallData),
		}
		callReq := common.NewJsonRpcRequest("eth_call", []interface{}{callObj, blockParam})
		callReq.ID = util.RandomID()

		nr := common.NewNormalizedRequestFromJsonRpcRequest(callReq)
		nr.CopyHttpContextFrom(nq)
		if dirs := nq.Directives(); dirs != nil {
			nr.SetDirectives(dirs.Clone())
		}
		nr.SetNetwork(network)
		nr.SetParentRequestId(nq.ID())
		perCallReqs[i] = nr
	}

	cacheDal := network.Cache()
	cachedResponses := make([]*common.NormalizedResponse, len(decodedCalls))
	missingReqs := make([]*common.NormalizedRequest, 0)
	missingIdx := make([]int, 0)
	if cacheDal != nil && !cacheDal.IsObjectNull() && !nq.SkipCacheRead() {
		for i, req := range perCallReqs {
			cachedResp, cacheErr := cacheDal.Get(ctx, req)
			if cacheErr != nil {
				logDebug(cacheErr, "handleUserMulticall3: per-call cache get failed, treating as miss")
			} else if cachedResp != nil && !cachedResp.IsObjectNull(ctx) {
				cachedResp.SetFromCache(true)
				cachedResponses[i] = cachedResp
				continue
			}
			missingReqs = append(missingReqs, req)
			missingIdx = append(missingIdx, i)
		}
	} else {
		missingReqs = append(missingReqs, perCallReqs...)
		for i := range perCallReqs {
			missingIdx = append(missingIdx, i)
		}
	}

	if len(missingReqs) == 0 {
		results := make([]Multicall3Result, len(decodedCalls))
		oldestCacheAt := int64(0)
		allCacheAt := true
		for i, resp := range cachedResponses {
			if resp == nil {
				if logger != nil {
					logger.Debug().Msg("handleUserMulticall3: cached response missing, falling back")
				}
				return false, nil, nil
			}
			jrr, err := resp.JsonRpcResponse(ctx)
			if err != nil || jrr == nil || jrr.Error != nil {
				if err != nil {
					logDebug(err, "handleUserMulticall3: cached response parse failed, falling back")
				} else if logger != nil {
					if jrr == nil {
						logger.Debug().Msg("handleUserMulticall3: cached response missing, falling back")
					} else {
						logger.Debug().Interface("jsonRpcError", jrr.Error).Msg("handleUserMulticall3: cached response error, falling back")
					}
				}
				return false, nil, nil
			}
			resultHex, err := getJsonRpcResultHex(jrr)
			if err != nil {
				logDebug(err, "handleUserMulticall3: cached result decode failed, falling back")
				return false, nil, nil
			}
			resultBytes, err := common.HexToBytes(resultHex)
			if err != nil {
				logDebug(err, "handleUserMulticall3: cached result hex decode failed, falling back")
				return false, nil, nil
			}
			results[i] = Multicall3Result{Success: true, ReturnData: resultBytes}
			if cachedAt := resp.CacheStoredAtUnix(); cachedAt > 0 {
				if oldestCacheAt == 0 || cachedAt < oldestCacheAt {
					oldestCacheAt = cachedAt
				}
			} else {
				allCacheAt = false
			}
		}

		encoded, err := EncodeMulticall3Aggregate3Results(results)
		if err != nil {
			logDebug(err, "handleUserMulticall3: encode cached results failed, falling back")
			return false, nil, nil
		}
		jrr, err := common.NewJsonRpcResponse(nq.ID(), "0x"+hex.EncodeToString(encoded), nil)
		if err != nil {
			logDebug(err, "handleUserMulticall3: build cached json-rpc response failed, falling back")
			return false, nil, nil
		}
		resp := common.NewNormalizedResponse().WithRequest(nq).WithJsonRpcResponse(jrr)
		resp.SetFromCache(true)
		if allCacheAt && oldestCacheAt > 0 {
			resp.SetCacheStoredAtUnix(oldestCacheAt)
		}
		return true, resp, nil
	}

	missingBatchReqs := make([]*common.NormalizedRequest, len(missingReqs))
	for i, req := range missingReqs {
		jrq, err := req.JsonRpcRequest()
		if err != nil {
			logDebug(err, "handleUserMulticall3: per-call request parse failed, falling back")
			return false, nil, nil
		}
		jrq.RLock()
		params := jrq.Params
		jrq.RUnlock()
		if len(params) < 1 {
			logDebug(fmt.Errorf("missing eth_call params"), "handleUserMulticall3: per-call params missing, falling back")
			return false, nil, nil
		}
		callObj, ok := params[0].(map[string]interface{})
		if !ok {
			logDebug(fmt.Errorf("invalid call object"), "handleUserMulticall3: per-call params invalid, falling back")
			return false, nil, nil
		}
		target, ok := callObj["to"].(string)
		if !ok || target == "" {
			logDebug(fmt.Errorf("missing call target"), "handleUserMulticall3: per-call target missing, falling back")
			return false, nil, nil
		}
		dataStr, ok := callObj["data"].(string)
		if !ok || dataStr == "" {
			if inputStr, ok := callObj["input"].(string); ok {
				dataStr = inputStr
			}
		}
		if dataStr == "" {
			logDebug(fmt.Errorf("missing call data"), "handleUserMulticall3: per-call data missing, falling back")
			return false, nil, nil
		}
		batchCallObj := map[string]interface{}{
			"to":   target,
			"data": dataStr,
		}
		batchReq := common.NewJsonRpcRequest("eth_call", []interface{}{batchCallObj})
		batchReq.ID = util.RandomID()
		missingBatchReqs[i] = common.NewNormalizedRequestFromJsonRpcRequest(batchReq)
	}

	mcReq, _, err := BuildMulticall3Request(missingBatchReqs, blockParam)
	if err != nil {
		logDebug(err, "handleUserMulticall3: build multicall3 request failed, falling back")
		return false, nil, nil
	}
	mcReq.SetNetwork(network)
	mcReq.SetParentRequestId(nq.ID())
	mcReq.SetCompositeType(common.CompositeTypeMulticall3)
	mcReq.CopyHttpContextFrom(nq)
	if dirs := nq.Directives(); dirs != nil {
		mcReq.SetDirectives(dirs.Clone())
	}

	mcResp, err := network.Forward(ctx, mcReq)
	if err != nil || mcResp == nil {
		logDebug(err, "handleUserMulticall3: multicall3 forward failed, falling back")
		return false, nil, nil
	}
	mcUpstream := mcResp.Upstream()
	mcJrr, err := mcResp.JsonRpcResponse(ctx)
	if err != nil || mcJrr == nil || mcJrr.Error != nil {
		if err != nil {
			logDebug(err, "handleUserMulticall3: multicall3 response parse failed, falling back")
		} else if logger != nil {
			if mcJrr == nil {
				logger.Debug().Msg("handleUserMulticall3: multicall3 response missing, falling back")
			} else {
				logger.Debug().Interface("jsonRpcError", mcJrr.Error).Msg("handleUserMulticall3: multicall3 response error, falling back")
			}
		}
		mcResp.Release()
		return false, nil, nil
	}
	mcResultHex, err := getJsonRpcResultHex(mcJrr)
	if err != nil {
		logDebug(err, "handleUserMulticall3: multicall3 result decode failed, falling back")
		mcResp.Release()
		return false, nil, nil
	}
	mcResultBytes, err := common.HexToBytes(mcResultHex)
	if err != nil {
		logDebug(err, "handleUserMulticall3: multicall3 result hex decode failed, falling back")
		mcResp.Release()
		return false, nil, nil
	}
	missingResults, err := DecodeMulticall3Aggregate3Result(mcResultBytes)
	if err != nil || len(missingResults) != len(missingReqs) {
		if err != nil {
			logDebug(err, "handleUserMulticall3: multicall3 result decode failed, falling back")
		} else if logger != nil {
			logger.Debug().
				Int("expected", len(missingReqs)).
				Int("actual", len(missingResults)).
				Msg("handleUserMulticall3: multicall3 result length mismatch, falling back")
		}
		mcResp.Release()
		return false, nil, nil
	}

	fullResults := make([]Multicall3Result, len(decodedCalls))
	for i, resp := range cachedResponses {
		if resp == nil {
			continue
		}
		jrr, err := resp.JsonRpcResponse(ctx)
		if err != nil || jrr == nil || jrr.Error != nil {
			if err != nil {
				logDebug(err, "handleUserMulticall3: cached response parse failed, falling back")
			} else if logger != nil {
				if jrr == nil {
					logger.Debug().Msg("handleUserMulticall3: cached response missing, falling back")
				} else {
					logger.Debug().Interface("jsonRpcError", jrr.Error).Msg("handleUserMulticall3: cached response error, falling back")
				}
			}
			mcResp.Release()
			return false, nil, nil
		}
		resultHex, err := getJsonRpcResultHex(jrr)
		if err != nil {
			logDebug(err, "handleUserMulticall3: cached result decode failed, falling back")
			mcResp.Release()
			return false, nil, nil
		}
		resultBytes, err := common.HexToBytes(resultHex)
		if err != nil {
			logDebug(err, "handleUserMulticall3: cached result hex decode failed, falling back")
			mcResp.Release()
			return false, nil, nil
		}
		fullResults[i] = Multicall3Result{Success: true, ReturnData: resultBytes}
	}
	for i, res := range missingResults {
		fullResults[missingIdx[i]] = res
	}

	cachePerCall := true
	if cfg := network.Config(); cfg != nil && cfg.Evm != nil && cfg.Evm.Multicall3Aggregation != nil && cfg.Evm.Multicall3Aggregation.CachePerCall != nil {
		cachePerCall = *cfg.Evm.Multicall3Aggregation.CachePerCall
	}
	if cachePerCall && cacheDal != nil && !cacheDal.IsObjectNull() {
		for i, res := range missingResults {
			if !res.Success {
				continue
			}
			req := missingReqs[i]
			resultHex := "0x" + hex.EncodeToString(res.ReturnData)
			jrr, err := common.NewJsonRpcResponse(req.ID(), resultHex, nil)
			if err != nil {
				continue
			}
			resp := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
			resp.SetUpstream(mcUpstream)
			cacheCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			if err := cacheDal.Set(cacheCtx, req, resp); err != nil {
				if logger != nil {
					logger.Warn().Err(err).Msg("handleUserMulticall3: failed to write per-call cache entry")
				}
			}
			cancel()
		}
	}

	encoded, err := EncodeMulticall3Aggregate3Results(fullResults)
	if err != nil {
		logDebug(err, "handleUserMulticall3: encode results failed, falling back")
		mcResp.Release()
		return false, nil, nil
	}
	jrr, err := common.NewJsonRpcResponse(nq.ID(), "0x"+hex.EncodeToString(encoded), nil)
	if err != nil {
		logDebug(err, "handleUserMulticall3: build json-rpc response failed, falling back")
		mcResp.Release()
		return false, nil, nil
	}
	resp := common.NewNormalizedResponse().WithRequest(nq).WithJsonRpcResponse(jrr)
	resp.SetUpstream(mcUpstream)
	if mcResp.FromCache() {
		resp.SetFromCache(true)
		if mcCacheAt := mcResp.CacheStoredAtUnix(); mcCacheAt > 0 {
			oldestCacheAt := mcCacheAt
			allCacheAt := true
			for _, cached := range cachedResponses {
				if cached == nil {
					continue
				}
				cachedAt := cached.CacheStoredAtUnix()
				if cachedAt <= 0 {
					allCacheAt = false
					break
				}
				if cachedAt < oldestCacheAt {
					oldestCacheAt = cachedAt
				}
			}
			if allCacheAt {
				resp.SetCacheStoredAtUnix(oldestCacheAt)
			}
		}
	}
	mcResp.Release()
	return true, resp, nil
}

func getJsonRpcResultHex(jrr *common.JsonRpcResponse) (string, error) {
	if jrr == nil {
		return "", fmt.Errorf("nil response")
	}
	resultStr := strings.TrimSpace(jrr.GetResultString())
	if resultStr == "" {
		return "", fmt.Errorf("empty result")
	}
	if strings.HasPrefix(resultStr, "\"") {
		unquoted, err := strconv.Unquote(resultStr)
		if err != nil {
			return "", err
		}
		resultStr = unquoted
	}
	return resultStr, nil
}
