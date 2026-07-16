package evm

import (
	"context"
	"fmt"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// networkPostForward_eth_blockNumber enforces the network's served tip on
// every eth_blockNumber response regardless of its source — a live upstream OR
// the cache. The correction is a pure in-memory synthesis (poller state),
// never an extra upstream call.
//
// In the default max mode the served tip is the MAX head across upstreams, so
// the enforcement is floor-only: a response behind the tip is raised to it and
// a response at/above it is fresher truth that passes through. In majority
// served-tip mode (EvmServedTipConfig.EnabledFor "latest") the response is
// pinned to the tip EXACTLY — above-tip responses are capped too — because
// "latest" interpolation (resolveBlockTagToHex) anchors block-tagged methods
// (eth_call, eth_getLogs, ...) to that same majority tip: letting a fresher
// head, or a cache entry written from one, through the floor-only check makes
// clients observe eth_blockNumber AHEAD of the block "latest" actually
// executes at, even when the same upstream serves both calls.
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
	highestBlock := network.EvmHighestLatestBlockNumber(context.WithValue(ctx, common.RequestContextKey, nq))
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

	pinToTip := false
	if cfg := network.Config(); cfg != nil && cfg.Evm != nil {
		pinToTip = cfg.Evm.ServedTipEnabledFor("latest")
	}
	if pinToTip {
		if blockNumber == highestBlock {
			return nr, re
		}
	} else if highestBlock <= blockNumber {
		return nr, re
	}

	ups := nr.Upstream()
	if blockNumber > highestBlock {
		// Pin-down (served-tip mode only): the response is AHEAD of the
		// majority tip — freshness, not staleness, so the stale-block metric
		// stays out of it.
		evt := network.Logger().Debug().
			Str("method", "eth_blockNumber").
			Int64("servedTip", highestBlock).
			Int64("responseBlockNumber", blockNumber).
			Bool("fromCache", nr.FromCache())
		if ups != nil {
			evt = evt.Str("upstreamId", ups.Id())
		}
		evt.Msg("response is ahead of the served tip, pinning to the majority tip")
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
	if nr.FromCache() {
		// Preserve cache attribution: the value was upgraded in-memory and no
		// upstream call was made to produce this response.
		corrected.WithFromCache(true)
	}
	// We are replacing the original response, so release it to avoid retaining buffers
	nr.Release()
	return corrected, nil
}
