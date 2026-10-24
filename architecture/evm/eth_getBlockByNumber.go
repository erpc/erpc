package evm

import (
	"context"
	"fmt"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// networkPreForward_eth_getBlockByNumber asks the voters' pollers for a
// debounced background refresh of the axis the request's "latest" or
// "finalized" tag names (spec §7 family 2), as eth_blockNumber does: the
// tag is interpolated to the request's Head, and the next request reads a
// fresher one.
func networkPreForward_eth_getBlockByNumber(ctx context.Context, network common.Network, nq *common.NormalizedRequest) (bool, *common.NormalizedResponse, error) {
	switch requestBlockTag(ctx, nq) {
	case "latest":
		common.EvmRefreshHead(network, ctx, false)
	case "finalized":
		common.EvmRefreshHead(network, ctx, true)
	}
	return false, nil, nil
}

func networkPostForward_eth_getBlockByNumber(ctx context.Context, network common.Network, nq *common.NormalizedRequest, nr *common.NormalizedResponse, re error) (*common.NormalizedResponse, error) {
	ctx, span := common.StartDetailSpan(ctx, "Network.PostForward.eth_getBlockByNumber", trace.WithAttributes(
		attribute.String("request.id", fmt.Sprintf("%v", nq.ID())),
		attribute.String("network.id", network.Id()),
	))
	defer span.End()

	if re != nil {
		common.SetTraceSpanError(span, re)
		return nr, re
	}

	// Track timestamp distance for "latest" blocks and add lag metrics to detailed spans
	if nr != nil && requestBlockTag(ctx, nq) == "latest" {
		_, respBlockNumber, bnErr := ExtractBlockReferenceFromResponse(ctx, nr)
		blockTimestamp, tsErr := ExtractBlockTimestampFromResponse(ctx, nr)

		if common.IsTracingDetailed && bnErr == nil && respBlockNumber > 0 {
			highestBlock := common.EvmHeadOf(network, ctx, nq).Latest
			blockNumberLag := highestBlock - respBlockNumber
			if blockNumberLag < 0 {
				blockNumberLag = 0
			}
			span.SetAttributes(
				attribute.Int64("block.number", respBlockNumber),
				attribute.Int64("highest_block", highestBlock),
				attribute.Int64("block.number_lag", blockNumberLag),
			)
		}

		if tsErr == nil && blockTimestamp > 0 {
			currentTime := time.Now().Unix()
			timestampLag := currentTime - blockTimestamp
			if common.IsTracingDetailed {
				span.SetAttributes(
					attribute.Int64("block.timestamp", blockTimestamp),
					attribute.Int64("current.timestamp", currentTime),
					attribute.Int64("block.timestamp_lag", timestampLag),
				)
			}
			telemetry.MetricNetworkLatestBlockTimestampDistance.WithLabelValues(
				network.ProjectId(),
				network.Label(),
				"network_response",
			).Set(float64(timestampLag))
		}
	}

	return enforceNonNullBlock(ctx, nq, nr)
}

// enforceNonNullBlock checks if the block result is null/empty and returns an appropriate error.
// A request for a tag, interpolated or not (the tag stays in EvmBlockRef,
// spec §7, #477), follows the EnforceNonNullTaggedBlocks directive.
func enforceNonNullBlock(ctx context.Context, nq *common.NormalizedRequest, nr *common.NormalizedResponse) (*common.NormalizedResponse, error) {
	if nr != nil && !nr.IsObjectNull() && !nr.IsResultEmptyish() {
		return nr, nil
	}

	var bnp string
	if rqj, _ := nq.JsonRpcRequest(ctx); rqj != nil {
		rqj.RLock()
		if len(rqj.Params) > 0 {
			bnp = fmt.Sprintf("%v", rqj.Params[0])
		}
		rqj.RUnlock()
	}
	isTag := requestBlockTag(ctx, nq) != ""

	// For tagged blocks, check directive
	if isTag {
		dirs := nq.Directives()
		if dirs == nil || !dirs.EnforceNonNullTaggedBlocks {
			// Directive not set or disabled - allow null tagged blocks
			return nr, nil
		}
	}

	// A null the refusal rule calls final (a future block, or a band block
	// after its one miss) is the truthful answer, not missing data.
	// Mirrors the upstream-level markUnexpectedEmpty guard so the two layers agree.
	if EmptyResultBeyondConfidence(ctx, nq) {
		return nr, nil
	}

	// Create error for:
	// 1. Numeric blocks with null result (always an error - indicates missing/pruned data)
	// 2. Tagged blocks with null result when enforcement is enabled
	details := make(map[string]interface{})
	details["blockNumber"] = bnp
	return nil, common.NewErrEndpointMissingData(
		common.NewErrJsonRpcExceptionInternal(
			0,
			common.JsonRpcErrorMissingData,
			"block not found with number "+bnp,
			nil,
			details,
		),
		nr.Upstream(),
	)
}
