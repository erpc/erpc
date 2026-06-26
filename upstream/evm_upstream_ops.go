package upstream

// EVM-specific methods on *Upstream. These were previously in upstream.go;
// moved here to group all EVM coupling in one file while keeping *Upstream as receiver
// (cross-package move is impossible without a circular import).

import (
	"context"
	"fmt"
	"math"
	"strconv"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
)

func (u *Upstream) EvmGetChainId(ctx context.Context) (string, error) {
	// Always make a real upstream call here. End-user requests can be short-circuited
	// via higher-level hooks (e.g. project/network pre-forward for eth_chainId).
	pr := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":75412,"method":"eth_chainId","params":[]}`))

	resp, err := u.Forward(ctx, pr, true, false)
	if resp != nil {
		defer resp.Release()
	}
	if err != nil {
		return "", err
	}

	jrr, err := resp.JsonRpcResponse()
	if err != nil {
		return "", err
	}
	if jrr.Error != nil {
		return "", jrr.Error
	}
	var chainId string
	err = common.SonicCfg.Unmarshal(jrr.GetResultBytes(), &chainId)
	if err != nil {
		return "", err
	}
	hex, err := common.NormalizeHex(chainId)
	if err != nil {
		return "", err
	}
	dec, err := common.HexToUint64(hex)
	if err != nil {
		return "", err
	}

	return strconv.FormatUint(dec, 10), nil
}

func (u *Upstream) EvmIsBlockFinalized(ctx context.Context, blockNumber int64, forceFreshIfStale bool) (bool, error) {
	if u.evmStatePoller == nil {
		return false, fmt.Errorf("evm state poller not initialized yet")
	}
	isFinalized, err := u.evmStatePoller.IsBlockFinalized(blockNumber)
	if err != nil {
		return false, err
	}
	if !isFinalized && forceFreshIfStale {
		newFinalizedBlock, err := u.evmStatePoller.PollFinalizedBlockNumber(ctx)
		if err != nil {
			return false, err
		}
		return newFinalizedBlock >= blockNumber, nil
	}
	return isFinalized, nil
}

func (u *Upstream) EvmSyncingState() common.EvmSyncingState {
	if u.evmStatePoller == nil {
		return common.EvmSyncingStateUnknown
	}
	return u.evmStatePoller.SyncingState()
}

func (u *Upstream) EvmLatestBlock() (int64, error) {
	if u.evmStatePoller == nil {
		return 0, fmt.Errorf("evm state poller not initialized yet")
	}
	return u.evmStatePoller.LatestBlock(), nil
}

func (u *Upstream) EvmFinalizedBlock() (int64, error) {
	if u.evmStatePoller == nil {
		return 0, fmt.Errorf("evm state poller not initialized yet")
	}
	return u.evmStatePoller.FinalizedBlock(), nil
}

func (u *Upstream) EvmStatePoller() common.EvmStatePoller {
	return u.evmStatePoller
}

// EvmAssertBlockAvailability checks if the upstream is supposed to have the data for a certain block number.
// For full nodes it will check the first available block number, and for archive nodes it will check if the block is less than the latest block number.
// If the requested block is beyond the current latest block, it will force-poll the latest block number once.
// This method also increments appropriate metrics when the upstream cannot handle the block.
func (u *Upstream) EvmAssertBlockAvailability(ctx context.Context, forMethod string, confidence common.AvailbilityConfidence, forceFreshIfStale bool, blockNumber int64) (bool, error) {
	if u == nil || u.config == nil {
		return false, fmt.Errorf("upstream or config is nil")
	}

	// Get the state poller
	statePoller := u.EvmStatePoller()
	if statePoller == nil || statePoller.IsObjectNull() {
		return false, fmt.Errorf("upstream evm state poller is not available")
	}

	cfg := u.config
	if cfg.Type != common.UpstreamTypeEvm || cfg.Evm == nil {
		// If not an EVM upstream, we can't determine block handling capability
		return false, fmt.Errorf("upstream is not an EVM type")
	}

	// Resolve configured availability bounds (min/max) and enforce before legacy logic
	minBound, maxBound := u.resolveAvailabilityBounds()
	if minBound != math.MinInt64 && blockNumber < minBound {
		telemetry.MetricUpstreamStaleLowerBound.WithLabelValues(
			u.ProjectId,
			u.VendorName(),
			u.NetworkLabel(),
			u.Id(),
			forMethod,
			confidence.String(),
		).Inc()
		u.logger.Debug().
			Int64("blockNumber", blockNumber).
			Int64("minBound", minBound).
			Str("method", forMethod).
			Str("upstreamId", u.config.Id).
			Msg("block rejected: below lower availability bound")
		return false, nil
	}
	if maxBound != math.MaxInt64 && blockNumber > maxBound {
		telemetry.MetricUpstreamStaleUpperBound.WithLabelValues(
			u.ProjectId,
			u.VendorName(),
			u.NetworkLabel(),
			u.Id(),
			forMethod,
			confidence.String(),
		).Inc()
		u.logger.Debug().
			Int64("blockNumber", blockNumber).
			Int64("maxBound", maxBound).
			Str("method", forMethod).
			Str("upstreamId", u.config.Id).
			Msg("block rejected: above upper availability bound")
		return false, nil
	}

	switch confidence {
	case common.AvailbilityConfidenceFinalized:
		//
		// UPPER BOUND: Check if the block is finalized
		//
		isFinalized, err := u.EvmIsBlockFinalized(ctx, blockNumber, forceFreshIfStale)
		if err != nil {
			return false, fmt.Errorf("failed to check if block is finalized: %w", err)
		}
		if !isFinalized {
			telemetry.MetricUpstreamStaleUpperBound.WithLabelValues(
				u.ProjectId,
				u.VendorName(),
				u.NetworkLabel(),
				u.Id(),
				forMethod,
				confidence.String(),
			).Inc()
			return false, nil
		}

		//
		// LOWER BOUND: For full nodes, also check if the block is within the available range
		//
		if cfg.Evm.MaxAvailableRecentBlocks > 0 {
			// First check with current data
			available, err := u.assertUpstreamLowerBound(ctx, statePoller, blockNumber, cfg.Evm.MaxAvailableRecentBlocks, forMethod, confidence)
			if err != nil {
				return false, err
			}
			if !available {
				// If it can't handle, return immediately
				return false, nil
			}
		}

		// Block is finalized and within range (or archive node)
		return true, nil
	case common.AvailbilityConfidenceBlockHead:
		//
		// UPPER BOUND: Check if block is before the latest block
		//
		latestBlock := statePoller.LatestBlock()
		// If the requested block is beyond the current latest block, try force-polling once
		if blockNumber > latestBlock && forceFreshIfStale {
			var err error
			latestBlock, err = statePoller.PollLatestBlockNumber(ctx)
			if err != nil {
				return false, fmt.Errorf("failed to poll latest block number: %w", err)
			}
		}
		// Check if the requested block is still beyond the latest known block
		if blockNumber > latestBlock {
			// Upper bound issue - block is beyond latest
			telemetry.MetricUpstreamStaleUpperBound.WithLabelValues(
				u.ProjectId,
				u.VendorName(),
				u.NetworkLabel(),
				u.Id(),
				forMethod,
				confidence.String(),
			).Inc()
			return false, nil
		}

		//
		// LOWER BOUND: For full nodes, check if the block is within the available range
		//
		if cfg.Evm.MaxAvailableRecentBlocks > 0 {
			available, err := u.assertUpstreamLowerBound(ctx, statePoller, blockNumber, cfg.Evm.MaxAvailableRecentBlocks, forMethod, confidence)
			if err != nil {
				return false, err
			}
			if !available {
				return false, nil
			}
		}

		// If MaxAvailableRecentBlocks is not configured, assume the node can handle the block if it's <= latest
		return blockNumber <= latestBlock, nil
	default:
		return false, fmt.Errorf("unsupported block availability confidence: %s", confidence)
	}
}
