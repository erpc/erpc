package evm

import (
	"context"
	"fmt"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// networkPreForward_eth_blockNumber answers eth_blockNumber from the served
// head without dispatching whenever enforce-highest-block is on and the head is
// known: the post-forward hook below pins every response to that head EXACTLY,
// so an upstream's answer is always discarded and the call buys nothing but
// vendor cost and latency. An unknown head (cold pollers) and
// enforce-highest-block=false keep forwarding.
func networkPreForward_eth_blockNumber(ctx context.Context, network common.Network, nq *common.NormalizedRequest) (handled bool, resp *common.NormalizedResponse, err error) {
	if dirs := nq.Directives(); dirs == nil || !dirs.EnforceHighestBlock {
		return false, nil, nil
	}
	ctx = context.WithValue(ctx, common.RequestContextKey, nq)
	tip := common.EvmHighestLatestBlockNumber(network, ctx)
	if tip <= 0 {
		return false, nil, nil
	}
	hex, err := common.NormalizeHex(tip)
	if err != nil {
		return true, nil, err
	}
	jrr, err := common.NewJsonRpcResponse(nq.ID(), hex, nil)
	if err != nil {
		return true, nil, err
	}
	return true, common.NewNormalizedResponse().WithRequest(nq).WithJsonRpcResponse(jrr), nil
}

// networkPostForward_eth_blockNumber pins every eth_blockNumber response to the
// network's served head EXACTLY, regardless of its source — the pre-forward
// answer above, a live upstream (cold head at pre-forward time, or a
// per-request override path) OR the cache. The correction is a pure in-memory
// synthesis (poller state), never an extra upstream call. Every enforced answer
// also asks the ballot's pollers for a debounced background refresh here, the
// one hook every path reaches: Network.Forward returns a cache hit before
// networkPreForward_eth_blockNumber runs, and without the refresh the head a
// quiet network serves stays at the ticker interval despite frequent polls.
//
// Exact, not floor-only, because "latest" interpolation (resolveBlockTagToHex)
// anchors block-tagged methods (eth_call, eth_getLogs, ...) to that same served
// head: a fresher upstream answer, or a cache entry written from one, would
// show clients an eth_blockNumber AHEAD of the block "latest" actually executes
// at; a staler one (e.g. a value planted in a shared cache) would show the
// block number moving backwards for an entire TTL window.
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
	reqCtx := context.WithValue(ctx, common.RequestContextKey, nq)
	highestBlock := common.EvmHighestLatestBlockNumber(network, reqCtx)
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

	if highestBlock <= 0 {
		// Unknown tip (pollers cold, all upstreams syncing): fail open like
		// resolveBlockTagToHex — never synthesize a block number from nothing.
		return nr, re
	}

	// The one debounced refresh per enforced answer, whatever produced it.
	common.EvmRefreshLatestBlock(network, reqCtx)

	if blockNumber == highestBlock {
		return nr, re
	}
	ups := nr.Upstream()
	if blockNumber > highestBlock {
		// Pin-down: the response is AHEAD of the served head — freshness, not
		// staleness, so the stale-block metric stays out of it.
		evt := network.Logger().Debug().
			Str("method", "eth_blockNumber").
			Int64("servedTip", highestBlock).
			Int64("responseBlockNumber", blockNumber).
			Bool("fromCache", nr.FromCache())
		if ups != nil {
			evt = evt.Str("upstreamId", ups.Id())
		}
		evt.Msg("response is ahead of the served tip, pinning to the served tip")
	} else if ups != nil {
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
	if ups != nil {
		corrected.SetUpstream(ups)
	}
	if nr.FromCache() {
		// Preserve cache attribution: the value was upgraded in-memory and no
		// upstream call was made to produce this response.
		corrected.WithFromCache(true)
	}
	// We are replacing the original response, so release it to avoid retaining buffers
	nr.Release()
	return corrected, nil
}
