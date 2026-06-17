package evm

import (
	"context"
	"fmt"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// networkPostForward_eth_blockNumber enforces the highest-known block on every
// eth_blockNumber response regardless of its source — a live upstream OR the
// cache. It compares the returned block number against the network's served
// tip and, when the response is behind, replaces it with the tip. The
// correction is a pure in-memory synthesis (poller state), never an extra
// upstream call.
//
// Positioning this at the network post-forward layer (it used to be a project
// pre-forward wrapper that explicitly skipped FromCache responses) is what
// guarantees cache hits are corrected too: a stale value planted in the
// (possibly shared) cache must not be served below the tip this instance
// already advertises, otherwise clients observe the block number moving
// backwards by the full upstream lag for an entire TTL window.
//
// Gating on the request directives (instead of the deprecated
// evm.integrity config) matches eth_getBlockByNumber enforcement: the
// config-level default still applies via directive defaults, and per-request
// overrides (enforce-highest-block=false) are honored.
func networkPostForward_eth_blockNumber(ctx context.Context, network common.Network, nq *common.NormalizedRequest, nr *common.NormalizedResponse, re error) (*common.NormalizedResponse, error) {
	if re != nil || nr == nil {
		return nr, re
	}

	dirs := nq.Directives()
	if dirs == nil || !dirs.EnforceHighestBlock {
		return nr, re
	}

	ctx, span := common.StartDetailSpan(ctx, "Network.PostForward.eth_blockNumber", trace.WithAttributes(
		attribute.String("request.id", fmt.Sprintf("%v", nq.ID())),
		attribute.String("network.id", network.Id()),
	))
	defer span.End()

	blockRef, blockNumber, err := ExtractBlockReferenceFromResponse(ctx, nr)
	if err != nil {
		common.SetTraceSpanError(span, err)
		return nil, err
	}

	// Resolve the tip with the request bound to the context so a
	// use-upstream selector scopes the tip to the targeted subset (the
	// selector-scoped served-tip semantics): a request pinned to a lagging
	// group must not be promised a block that group cannot serve.
	highestBlock := common.EvmHighestLatestBlockNumber(network, context.WithValue(ctx, common.RequestContextKey, nq))
	if common.IsTracingDetailed {
		blockNumberLag := highestBlock - blockNumber
		if blockNumberLag < 0 {
			blockNumberLag = 0
		}
		span.SetAttributes(
			attribute.Int64("block.number", blockNumber),
			attribute.String("block.ref", blockRef),
			attribute.Int64("highest_block", highestBlock),
			attribute.Int64("block.number_lag", blockNumberLag),
		)
	}

	if highestBlock <= blockNumber {
		return nr, re
	}

	ups := nr.Upstream()
	if ups != nil {
		telemetry.MetricUpstreamStaleLatestBlock.WithLabelValues(
			network.ProjectId(),
			ups.VendorName(),
			network.Label(),
			ups.Id(),
			"eth_blockNumber",
		).Inc()
		network.Logger().Debug().
			Str("method", "eth_blockNumber").
			Int64("knownHighestBlock", highestBlock).
			Int64("responseBlockNumber", blockNumber).
			Str("upstreamId", ups.Id()).
			Msg("upstream returned older block than we know, falling back to highest known block")
	} else {
		// No upstream attribution — typically a cache hit carrying a value
		// below the tip (e.g. written before the tip advanced, or by another
		// instance reading from a lagging upstream).
		network.Logger().Debug().
			Str("method", "eth_blockNumber").
			Int64("knownHighestBlock", highestBlock).
			Int64("responseBlockNumber", blockNumber).
			Bool("fromCache", nr.FromCache()).
			Msg("response contains older block than we know, falling back to highest known block")
	}

	hbk, err := common.NormalizeHex(highestBlock)
	if err != nil {
		common.SetTraceSpanError(span, err)
		return nil, err
	}
	jrr, err := common.NewJsonRpcResponse(nq.ID(), hbk, nil)
	if err != nil {
		common.SetTraceSpanError(span, err)
		return nil, err
	}
	corrected := common.NewNormalizedResponse().
		WithRequest(nq).
		WithJsonRpcResponse(jrr)
	if nr.FromCache() {
		// Preserve cache attribution: the value was upgraded in-memory and no
		// upstream call was made to produce this response.
		corrected.WithFromCache(true)
	}
	// We are replacing the original response, so release it to avoid retaining buffers
	nr.Release()
	return corrected, nil
}
