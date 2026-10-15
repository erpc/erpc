package svm

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/erpc/erpc/common"
)

// DeclaredConfidenceLabel is the `confidence` label used on
// MetricUpstreamStaleLowerBound / MetricUpstreamStaleUpperBound for a skip
// driven by operator-configured svm.blockAvailability, not by an observed
// floor.
const DeclaredConfidenceLabel = "configured"

// maxRetryableSlotDistance mirrors erpc.defaultMaxRetryableBlockDistance as a
// COUNT of slots, not a wall-clock horizon: near the tip a skip is worth a
// retry round (the tip may simply not have reached the requested slot yet),
// beyond it the miss is treated as real. Kept local: architecture/svm cannot
// import erpc. It deliberately does not derive from any chain's slot time —
// that would be machinery for SVM chains this codebase does not yet serve —
// and there is no SVM operator override (EVM's
// networks[].evm.maxRetryableBlockDistance is not mirrored): a knob whose
// only non-default use is tuning a wait window nobody has needed.
const maxRetryableSlotDistance = int64(128)

// ResolveDeclaredSlotBounds resolves the upstream's configured
// svm.blockAvailability against the poller. Returns MinInt64/MaxInt64 when a
// side is unbounded. Fail-open (both unbounded) when the computed range is
// invalid (min > max), matching EVM resolveAvailabilityBounds.
//
// firstAvailableSlotPlus with an unobserved floor is unbounded — unknown floor
// is not slot 0. EVM's earliestBlockPlus computes 0+N before detection; SVM
// must not, because absence of a floor observation is not evidence of genesis.
func ResolveDeclaredSlotBounds(u common.Upstream) (int64, int64) {
	if u == nil || u.Config() == nil || u.Config().Svm == nil || u.Config().Svm.BlockAvailability == nil {
		return math.MinInt64, math.MaxInt64
	}
	cfg := u.Config().Svm.BlockAvailability

	var poller common.SvmStatePoller
	if su, ok := u.(common.SvmUpstream); ok {
		poller = su.SvmStatePoller()
	}

	minVal := resolveDeclaredBound(cfg.Lower, true, poller)
	maxVal := resolveDeclaredBound(cfg.Upper, false, poller)
	if minVal != math.MinInt64 && maxVal != math.MaxInt64 && minVal > maxVal {
		return math.MinInt64, math.MaxInt64
	}
	return minVal, maxVal
}

func resolveDeclaredBound(bound *common.SvmAvailabilityBoundConfig, isLower bool, poller common.SvmStatePoller) int64 {
	unbounded := func() int64 {
		if isLower {
			return math.MinInt64
		}
		return math.MaxInt64
	}
	if bound == nil {
		return unbounded()
	}
	if bound.ExactSlot != nil {
		return *bound.ExactSlot
	}
	if bound.LatestSlotMinus != nil {
		if poller == nil || poller.IsObjectNull() {
			return unbounded()
		}
		latest := poller.LatestSlot()
		if latest <= 0 {
			return unbounded()
		}
		return latest - *bound.LatestSlotMinus
	}
	if bound.FirstAvailableSlotPlus != nil {
		if poller == nil || poller.IsObjectNull() {
			return unbounded()
		}
		floor, known := poller.FirstAvailableSlot()
		if !known {
			return unbounded()
		}
		// floor >= 0 and the offset >= 0 (validation), so the addition can
		// only overflow upward — and a wrapped bound silently widens a lower
		// restriction or rejects every slot on an upper. Either way the bound
		// is unresolvable, and this function's contract is fail-open.
		if *bound.FirstAvailableSlotPlus > math.MaxInt64-floor {
			return unbounded()
		}
		return floor + *bound.FirstAvailableSlotPlus
	}
	return unbounded()
}

// CheckUpstreamDeclaredSlotBounds reports an error when the request names a
// slot outside this upstream's declared svm.blockAvailability window.
//
// Below the declared lower: permanent skip, same error shape as the observed
// floor skip (ErrEndpointMissingData wrapping agave -32001) so the client sees
// one contract whether the bound is observed or configured.
//
// Above the declared upper: -32004 BlockNotAvailable, evaluated against the
// highest slot the request can touch (the END for getBlocks/getBlocksWithLimit,
// not just the start). Retryable iff the upper kind is dynamic AND the slot is
// ahead of LatestSlot() by at most maxRetryableSlotDistance, matching EVM's
// near-tip classification — an exactSlot cap can never reopen by waiting.
// Non-retryable upper skips are permanent (like lower skips) so the failsafe
// executor does not treat an all-upstream declared-window rejection as
// transient missing_data.
//
// Fail-open when no slot is named, no bounds are configured, or resolution
// produced an unbounded range (cold poller, unobserved floor, invalid min>max).
func CheckUpstreamDeclaredSlotBounds(ctx context.Context, u common.Upstream, r *common.NormalizedRequest) (error, bool) {
	slot, named := RequestLowerBoundSlot(ctx, r)
	if !named {
		return nil, false
	}
	minBound, maxBound := ResolveDeclaredSlotBounds(u)
	if minBound == math.MinInt64 && maxBound == math.MaxInt64 {
		return nil, false
	}
	if minBound != math.MinInt64 && slot < minBound {
		err := common.NewErrEndpointMissingData(
			common.NewErrJsonRpcExceptionInternal(
				svmCodeBlockCleanedUp,
				common.JsonRpcErrorNumber(svmCodeBlockCleanedUp),
				fmt.Sprintf("Slot %d cleaned up, does not exist on upstream %s. First available block: %d",
					slot, u.Id(), minBound),
				nil, nil,
			),
			u,
		)
		if me, ok := err.(*common.ErrEndpointMissingData); ok {
			me.WithPermanentMissingData(true)
		}
		return err, false
	}
	if maxBound != math.MaxInt64 {
		latest := int64(0)
		if su, ok := u.(common.SvmUpstream); ok {
			if p := su.SvmStatePoller(); p != nil && !p.IsObjectNull() {
				latest = p.LatestSlot()
			}
		}
		// The UPPER bound evaluates the highest slot the request can touch:
		// for range methods that is the END, not the start. getBlocks(50, 1000)
		// against a window ending at 100 must skip — serving the truncated
		// prefix here would hide the full range another upstream holds.
		upperSlot, upperOk := RequestRangeEndSlot(ctx, r, slot, latest)
		if upperOk && upperSlot > maxBound {
			err := common.NewErrEndpointMissingData(
				common.NewErrJsonRpcExceptionInternal(
					svmCodeBlockNotAvailable,
					common.JsonRpcErrorNumber(svmCodeBlockNotAvailable),
					fmt.Sprintf("Block not available for slot %d on upstream %s (configured window ends at %d)",
						upperSlot, u.Id(), maxBound),
					nil, nil,
				),
				u,
			)
			// exactSlot is a FIXED cap: the tip advancing can never reopen the
			// window, so a near-tip retry would burn rounds for nothing. Only
			// dynamic uppers (latestSlotMinus, firstAvailableSlotPlus) move with
			// observed state and can become servable by waiting.
			fixedUpper := false
			if cfg := u.Config(); cfg != nil && cfg.Svm != nil && cfg.Svm.BlockAvailability != nil &&
				cfg.Svm.BlockAvailability.Upper != nil && cfg.Svm.BlockAvailability.Upper.ExactSlot != nil {
				fixedUpper = true
			}
			retryable := false
			if !fixedUpper && latest > 0 && upperSlot > latest {
				retryable = upperSlot-latest <= maxRetryableSlotDistance
			}
			if !retryable {
				if me, ok := err.(*common.ErrEndpointMissingData); ok {
					me.WithPermanentMissingData(true)
				}
			}
			return err, retryable
		}
	}
	return nil, false
}

// IsDeclaredUpperBoundSkip reports whether err is a skip from a declared
// svm.blockAvailability.upper (agave -32004), as opposed to a declared or
// observed lower-bound skip (-32001).
func IsDeclaredUpperBoundSkip(err error) bool {
	var jre *common.ErrJsonRpcExceptionInternal
	return errors.As(err, &jre) && jre.OriginalCode() == svmCodeBlockNotAvailable
}
