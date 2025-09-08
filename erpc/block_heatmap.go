package erpc

import (
	"context"
	"fmt"
	"math"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
)

// recordEvmBlockRangeHeatmap records MetricNetworkEvmBlockRangeRequested using dynamic bucket sizes
// based on the distance of the referenced block to the network's highest latest block. It also
// emits a human-readable bucket label in the form "start-end" using k/m/b suffixes.
func recordEvmBlockRangeHeatmap(ctx context.Context, projectId string, network *Network, method string, req *common.NormalizedRequest, resp *common.NormalizedResponse) {
	if network == nil || resp == nil {
		return
	}

	// First try to extract both blockRef and blockNumber from request
	var blockRef string
	var blockNumber int64
	if req != nil {
		blockRef, blockNumber, _ = evm.ExtractBlockReferenceFromRequest(ctx, req)
	}

	// Only if blockNumber is not available from request, try to get it from response
	if blockNumber <= 0 {
		_, blockNumber, _ = evm.ExtractBlockReferenceFromResponse(ctx, resp)
		if blockNumber <= 0 {
			return
		}
	}

	tip := network.EvmHighestLatestBlockNumber(ctx)
	finalityStr := resp.Finality(ctx).String()

	start, end, size, label := ComputeBlockHeatmapBucket(blockNumber, tip, blockRef)
	if size <= 0 {
		// Fallback: tip unknown or sizing failure, use default sizing
		size = telemetry.EvmBlockRangeBucketSize
		if size <= 0 {
			size = 100000
		}
		start = (blockNumber / size) * size
		end = start + size
		label = formatAbsoluteBucketLabel(start, end, size)
	}

	vendor := "n/a"
	upstreamId := "n/a"
	if resp.Upstream() != nil {
		u := resp.Upstream()
		vendor = u.VendorName()
		upstreamId = u.Id()
	}

	telemetry.MetricNetworkEvmBlockRangeRequested.
		WithLabelValues(
			projectId,
			network.Label(),
			vendor,
			upstreamId,
			method,
			req.UserId(),
			finalityStr,
			label,
			fmt.Sprintf("%d", size),
		).
		Inc()
}

// ComputeBlockHeatmapBucket computes the bucket [start,end] (inclusive end snapped to tip),
// bucket size, and the human-readable label for a given blockNumber and reference tip.
// The blockRef parameter is optional and used to detect special tags like "latest" or "finalized".
// This helper is pure and suitable for unit testing without mocks.
func ComputeBlockHeatmapBucket(blockNumber int64, tip int64, blockRef string) (int64, int64, int64, string) {
	if blockNumber <= 0 {
		return 0, 0, 0, ""
	}

	// If tip is unknown, return zeros to let caller handle fallback
	if tip <= 0 {
		return 0, 0, 0, ""
	}

	// Distance to tip (non-negative)
	distance := tip - blockNumber
	if distance < 0 {
		distance = 0
	}

	size := selectDynamicBucketSize(distance)
	if size <= 0 {
		return 0, 0, 0, ""
	}

	start := (blockNumber / size) * size
	end := start + size

	// Snap to deterministic boundaries relative to tip if known
	if end > tip {
		end = tip
	}
	if start >= tip {
		// If start is beyond tip due to blockNumber > tip, snap the bucket to the last aligned bucket ending at or before tip
		// We want the last bucket that is aligned to size boundaries
		start = ((tip - 1) / size) * size
		end = tip
		// Ensure the bucket still has the expected size if possible
		if end-start > size {
			start = end - size
		}
	}
	if end < start {
		end = start
	}

	var label string
	// Special cases for tags, exact tip, and future blocks
	if blockRef == "latest" {
		label = "LATEST"
	} else if blockRef == "finalized" {
		label = "FINALIZED"
	} else if blockRef == "safe" {
		label = "SAFE"
	} else if blockRef == "pending" {
		label = "PENDING"
	} else if math.Abs(float64(blockNumber-tip)) <= 4 {
		label = "TIP"
	} else if blockNumber > tip {
		label = "FUTURE"
	} else if (tip - start) <= 5_000_000 {
		// Near-tip labeling up to last 5m blocks.
		// Always use bucket boundaries for consistent labeling.
		if end == tip {
			// Bucket touches tip: use L<size>
			label = fmt.Sprintf("L%s", humanizeDistance(size))
		} else {
			// Bucket doesn't touch tip: round distances to nearest bucket size for clean labels
			dStart := tip - start
			dEnd := tip - end
			if dStart < 0 {
				dStart = 0
			}
			if dEnd < 0 {
				dEnd = 0
			}
			// Round to nearest bucket size multiple for cleaner labels
			roundedStart := ((dStart + size/2) / size) * size
			roundedEnd := ((dEnd + size/2) / size) * size

			if roundedStart == roundedEnd || roundedEnd == 0 {
				// If they round to the same value, or end rounds to 0 (at tip), just show one
				label = fmt.Sprintf("L%s", humanizeDistance(roundedStart))
			} else {
				label = fmt.Sprintf("L%s-L%s", humanizeDistance(roundedStart), humanizeDistance(roundedEnd))
			}
		}
	} else {
		label = formatAbsoluteBucketLabel(start, end, size)
	}

	return start, end, size, label
}

// formatAbsoluteBucketLabel renders start-end using an integer suffix based on size (k/m/b), never decimals.
func formatAbsoluteBucketLabel(start, end, size int64) string {
	// Choose unit based on size to avoid decimals
	if size < 1_000_000 {
		// use thousands
		return fmt.Sprintf("%dk-%dk", start/1_000, end/1_000)
	}
	if size < 1_000_000_000 {
		// use millions
		return fmt.Sprintf("%dm-%dm", start/1_000_000, end/1_000_000)
	}
	// billions
	return fmt.Sprintf("%db-%db", start/1_000_000_000, end/1_000_000_000)
}

// selectDynamicBucketSize chooses bucket sizes that increase with distance from the tip.
// The thresholds are intentionally small near the tip to provide finer resolution.
//
// distance thresholds (from tip) => size
func selectDynamicBucketSize(distance int64) int64 {
	switch {
	case distance <= 1_000:
		return 100
	case distance <= 5_000:
		return 1_000
	case distance <= 20_000:
		return 5_000
	case distance <= 50_000:
		return 10_000
	case distance <= 100_000:
		return 30_000
	case distance <= 300_000:
		return 50_000
	case distance <= 1_000_000:
		return 100_000
	case distance <= 10_000_000:
		return 1_000_000
	case distance <= 30_000_000:
		return 10_000_000
	case distance <= 100_000_000:
		return 50_000_000
	default:
		return 50_000_000
	}
}

// humanizeDistance produces whole-number labels without decimals.
// For values < 1000: raw digits.
// For values >= 1000: use k if < 1m, otherwise m, always whole numbers.
func humanizeDistance(value int64) string {
	if value < 0 {
		value = 0
	}
	if value < 1_000 {
		return fmt.Sprintf("%d", value)
	}
	if value < 1_000_000 {
		// Round to nearest thousand for k values
		return fmt.Sprintf("%dk", (value+500)/1_000)
	}
	// Round to nearest million for m values
	return fmt.Sprintf("%dm", (value+500_000)/1_000_000)
}
