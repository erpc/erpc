package evm

import (
	"context"
	"fmt"

	"github.com/erpc/erpc/common"
)

// rangeHolder is the network's side of the range check (spec §6.4): the
// wait for an upstream to hold a range's toBlock, its counter below it.
type rangeHolder interface {
	EvmAwaitRangeHolder(ctx context.Context, req *common.NormalizedRequest, u common.Upstream, toBlock int64, likely bool) bool
}

// CheckBlockRangeAvailability verifies that both fromBlock and toBlock are available
// on the given upstream. This is used for range-based methods like eth_getLogs,
// trace_filter, and arbtrace_filter.
//
// It checks:
//  1. toBlock (upper bound) by the one head predicate on the upstream's
//     counter; below it, within its configured bounds, the network waits
//     until the upstream holds toBlock, at most W (EvmAwaitRangeHolder,
//     spec §6.4 range check): its evidence, or, for a one-block range,
//     its held wait since toBlock's first sight. A toBlock above the
//     lane's Latest is the lane's read ahead, whose one shared fetch
//     brings it; once the lane holds it, an untried holder goes first,
//     else the upstream is Asked for it once, one Ask per upstream and
//     block. A toBlock the refusal rule calls future never gets here:
//     the network refuses it before dispatch.
//  2. fromBlock (lower bound) against the configured window.
//
// Returns nil if both blocks are available, otherwise returns an appropriate error.
func CheckBlockRangeAvailability(
	ctx context.Context,
	nw common.Network,
	req *common.NormalizedRequest,
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
		// Behind by its counter only, not by its configured bounds: a
		// latestBlockMinus upper bound moves with the head, so the
		// upstream holds toBlock once its head reaches toBlock plus that
		// margin. A node lacking the top of a longer range answers the
		// part it has, so only a one-block range, which it answers empty
		// (a miss the refusal rule retries), goes on a likely hold.
		need := toBlock
		minBound, maxBound := up.EvmBlockAvailabilityBounds()
		if m := upperLatestMinus(up); m != nil {
			need, maxBound = toBlock+*m, toBlock
		}
		if toBlock >= minBound && toBlock <= maxBound && !common.EvmHeadReaches(up, statePoller.LatestBlock(), need) {
			if h, ok := nw.(rangeHolder); ok {
				available = h.EvmAwaitRangeHolder(ctx, req, up, need, fromBlock == toBlock)
			}
		}
	}
	if !available {
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

// upperLatestMinus is up's latestBlockMinus upper bound, nil when its
// upper bound is none or of another kind.
func upperLatestMinus(up common.Upstream) *int64 {
	cfg := up.Config()
	if cfg == nil || cfg.Evm == nil || cfg.Evm.BlockAvailability == nil {
		return nil
	}
	if b := cfg.Evm.BlockAvailability.Upper; b != nil && b.ExactBlock == nil && b.EarliestBlockPlus == nil {
		return b.LatestBlockMinus
	}
	return nil
}
