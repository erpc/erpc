package evm

import (
	"context"
	"fmt"

	"github.com/erpc/erpc/common"
)

// CheckBlockRangeAvailability verifies that both fromBlock and toBlock are available
// on the given upstream. This is used for range-based methods like eth_getLogs,
// trace_filter, and arbtrace_filter.
//
// It checks:
//  1. toBlock (upper bound) by the one head predicate, with no I/O: a refusal
//     refreshes the upstream's head in the background, as the availability
//     gate does. A toBlock the refusal rule calls future never gets here:
//     the network refuses it before dispatch (spec §6.4).
//  2. fromBlock (lower bound) against the configured window.
//
// Returns nil if both blocks are available, otherwise returns an appropriate error.
func CheckBlockRangeAvailability(
	ctx context.Context,
	up common.EvmUpstream,
	method string,
	fromBlock, toBlock int64,
) error {
	statePoller := up.EvmStatePoller()
	if statePoller == nil || statePoller.IsObjectNull() {
		return common.NewErrUpstreamInitialization(
			fmt.Errorf("upstream evm state poller is not available"),
			up.Id(),
		)
	}

	available, err := up.EvmAssertBlockAvailability(ctx, method, common.AvailbilityConfidenceBlockHead, false, toBlock)
	if err != nil {
		return err
	}
	if !available {
		statePoller.RefreshLatestBlock()
		latestBlock := statePoller.LatestBlock()
		finalizedBlock := statePoller.FinalizedBlock()

		// This is a transient condition (not missing data): the upstream is simply behind its head/finality.
		// Use a retryable upstream-level error code so network-level retries can re-attempt
		// the SAME upstream after a short delay.
		return &common.ErrUpstreamBlockUnavailable{
			BaseError: common.BaseError{
				Code:    common.ErrCodeUpstreamBlockUnavailable,
				Message: fmt.Sprintf("block not found for %s, requested toBlock %d is not yet available on the node (latestBlock: %d, finalizedBlock: %d)", method, toBlock, latestBlock, finalizedBlock),
				Details: map[string]interface{}{
					"upstreamId":     up.Id(),
					"blockNumber":    toBlock,
					"latestBlock":    latestBlock,
					"finalizedBlock": finalizedBlock,
				},
			},
		}
	}

	// Check lower bound (fromBlock) - this is historical data.
	available, err = up.EvmAssertBlockAvailability(ctx, method, common.AvailbilityConfidenceBlockHead, false, fromBlock)
	if err != nil {
		return err
	}
	if !available {
		latestBlock := statePoller.LatestBlock()
		finalizedBlock := statePoller.FinalizedBlock()

		return &common.ErrUpstreamBlockUnavailable{
			BaseError: common.BaseError{
				Code:    common.ErrCodeUpstreamBlockUnavailable,
				Message: fmt.Sprintf("block not found for %s, requested fromBlock %d is not available on the node (latestBlock: %d, finalizedBlock: %d)", method, fromBlock, latestBlock, finalizedBlock),
				Details: map[string]interface{}{
					"upstreamId":     up.Id(),
					"blockNumber":    fromBlock,
					"latestBlock":    latestBlock,
					"finalizedBlock": finalizedBlock,
				},
			},
		}
	}

	return nil
}
