package evm

import (
	"context"
	"fmt"
	"strings"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// headNumberAxis is the Head axis a head-number request asks for (spec §7
// family 1): eth_blockNumber, and erigon_blockNumber with no param or
// "latest", ask for Latest; erigon_blockNumber("finalized") for Finalized.
// ok is false for any other erigon_blockNumber param: it is forwarded as
// is and its answer is not pinned.
func headNumberAxis(ctx context.Context, nq *common.NormalizedRequest) (finalized, ok bool) {
	if method, _ := nq.Method(); !strings.EqualFold(method, "erigon_blockNumber") {
		return false, true
	}
	jrq, err := nq.JsonRpcRequest(ctx)
	if err != nil {
		return false, false
	}
	jrq.RLock()
	defer jrq.RUnlock()
	if len(jrq.Params) == 0 {
		return false, true
	}
	switch jrq.Params[0] {
	case "latest":
		return false, true
	case "finalized":
		return true, true
	}
	return false, false
}

// headNumber is the request's Head on axis finalized.
func headNumber(ctx context.Context, network common.Network, nq *common.NormalizedRequest, finalized bool) int64 {
	head := common.EvmHeadOf(network, ctx, nq)
	if finalized {
		return head.Finalized
	}
	return head.Latest
}

// networkPreForward_eth_blockNumber answers eth_blockNumber and
// erigon_blockNumber with the request's Head on the axis it asks for
// (headNumberAxis) without dispatching whenever enforce-highest-block is on
// and the head is known: the post-forward hook below pins every response to
// that same value EXACTLY, so an upstream's answer is always discarded and
// the call buys nothing but vendor cost and latency. A Latest answer also
// asks the ballot's pollers for a debounced background refresh, so on a
// quiet network the head a client polls for is not held at the ticker
// interval; a Finalized answer already waited for its refresh
// (Network.Forward).
// An unknown head (cold pollers) and enforce-highest-block=false keep
// forwarding.
func networkPreForward_eth_blockNumber(ctx context.Context, network common.Network, nq *common.NormalizedRequest) (handled bool, resp *common.NormalizedResponse, err error) {
	if dirs := nq.Directives(); dirs == nil || !dirs.EnforceHighestBlock {
		return false, nil, nil
	}
	finalized, ok := headNumberAxis(ctx, nq)
	if !ok {
		return false, nil, nil
	}
	ctx = context.WithValue(ctx, common.RequestContextKey, nq)
	tip := headNumber(ctx, network, nq, finalized)
	if tip <= 0 {
		return false, nil, nil
	}
	if !finalized {
		common.EvmRefreshHead(network, ctx)
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
// network's served head EXACTLY, regardless of its source — a live upstream
// (cold head at pre-forward time, or a per-request override path) OR the
// cache. The correction is a pure in-memory synthesis (poller state), never an
// extra upstream call. Enforced answers also ask the ballot's pollers for a
// debounced background refresh here: Network.Forward can return a cache hit
// before networkPreForward_eth_blockNumber runs, and without this the head a
// quiet network serves stays at the ticker interval despite frequent polls.
//
// Exact, not floor-only, because "latest" interpolation (TagBlockNumber)
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
	finalized, ok := headNumberAxis(ctx, nq)
	if !ok {
		return nr, re
	}

	method, _ := nq.Method()
	ctx, span := common.StartDetailSpan(ctx, "Network.PostForward."+method, trace.WithAttributes(
		attribute.String("request.id", fmt.Sprintf("%v", nq.ID())),
		attribute.String("network.id", network.Id()),
	))
	defer span.End()

	blockRef, blockNumber, err := ExtractBlockReferenceFromResponse(ctx, nr)
	if err != nil {
		common.SetTraceSpanError(span, err)
		return nil, err
	}

	// The request's own Head, the one its pre-forward and its interpolation
	// read: a request pinned to a lagging group is never promised a block
	// that group cannot serve.
	reqCtx := context.WithValue(ctx, common.RequestContextKey, nq)
	highestBlock := headNumber(reqCtx, network, nq, finalized)
	if common.IsTracingDetailed() {
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
		// interpolation — never synthesize a block number from nothing.
		return nr, re
	}

	// Same debounced refresh the pre-forward path fires (#1280): a cache hit
	// returns before networkPreForward_eth_blockNumber runs.
	if !finalized {
		common.EvmRefreshHead(network, reqCtx)
	}

	if blockNumber == highestBlock {
		return nr, re
	}
	ups := nr.Upstream()
	if blockNumber > highestBlock {
		// Pin-down: the response is AHEAD of the served head — freshness, not
		// staleness, so the stale-block metric stays out of it.
		evt := network.Logger().Debug().
			Str("method", method).
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
			method,
		).Inc()
		network.Logger().Debug().
			Str("method", method).
			Int64("knownHighestBlock", highestBlock).
			Int64("responseBlockNumber", blockNumber).
			Str("upstreamId", ups.Id()).
			Msg("upstream returned older block than we know, falling back to highest known block")
	} else {
		// No upstream attribution — typically a cache hit carrying a value
		// below the tip (e.g. written before the tip advanced, or by another
		// instance reading from a lagging upstream).
		network.Logger().Debug().
			Str("method", method).
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
