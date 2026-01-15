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

// GetBatcherManager returns the global batcher manager.
func GetBatcherManager() *BatcherManager {
	batcherManagerOnce.Do(func() {
		globalBatcherManager = NewBatcherManager()
	})
	return globalBatcherManager
}

// networkForwarder wraps a Network to implement Forwarder interface.
type networkForwarder struct {
	network common.Network
}

func (f *networkForwarder) Forward(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	return f.network.Forward(ctx, req)
}

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

	// Check if Multicall3 aggregation is enabled
	cfg := network.Config()
	if cfg == nil || cfg.Evm == nil || cfg.Evm.Multicall3Aggregation == nil || !cfg.Evm.Multicall3Aggregation.Enabled {
		// Batching disabled, use normal forward
		resp, err := network.Forward(ctx, nq)
		return true, resp, err
	}

	aggCfg := cfg.Evm.Multicall3Aggregation

	// Check eligibility for batching
	eligible, _ := IsEligibleForBatching(nq, aggCfg)
	if !eligible {
		// Not eligible, forward normally
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

	// Get or create batcher for this network
	mgr := GetBatcherManager()
	forwarder := &networkForwarder{network: network}
	batcher := mgr.GetOrCreate(network.Id(), aggCfg, forwarder)

	// Enqueue request
	entry, bypass, err := batcher.Enqueue(ctx, key, nq)
	if err != nil || bypass {
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
