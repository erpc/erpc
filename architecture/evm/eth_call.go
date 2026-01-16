package evm

import (
	"context"
	"fmt"
	"sync"

	"github.com/erpc/erpc/common"
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
