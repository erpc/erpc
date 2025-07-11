package evm

import (
	"context"
	"fmt"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// networkPreForward_eth_blockNumber calls the normal forward, then compares the returned block number,
// to all EVM upstreams' StatePoller for this network and returns the highest block number across them.
func networkPreForward_eth_blockNumber(ctx context.Context, network common.Network, nq *common.NormalizedRequest) (handled bool, resp *common.NormalizedResponse, err error) {
	ctx, span := common.StartDetailSpan(ctx, "Network.PreForwardHook.eth_blockNumber", trace.WithAttributes(
		attribute.String("request.id", fmt.Sprintf("%v", nq.ID())),
		attribute.String("network.id", network.Id()),
	))
	defer span.End()

	ncfg := network.Config()
	if ncfg == nil || ncfg.Evm == nil {
		return false, nil, nil
	}

	if ncfg.Evm.IntentionalBlockLag > 0 {
		latestBlockNumber := network.EvmHighestLatestBlockNumber(ctx)
		if latestBlockNumber <= 0 {
			return false, nil, nil
		}

		laggedBlockNumber := latestBlockNumber - ncfg.Evm.IntentionalBlockLag
		if laggedBlockNumber < 0 {
			laggedBlockNumber = 0
		}

		hbk, err := common.NormalizeHex(laggedBlockNumber)
		if err != nil {
			common.SetTraceSpanError(span, err)
			return false, nil, err
		}

		jrr, err := common.NewJsonRpcResponse(nq.ID(), hbk, nil)
		if err != nil {
			common.SetTraceSpanError(span, err)
			return false, nil, err
		}

		resp := common.NewNormalizedResponse().
			WithRequest(nq).
			WithJsonRpcResponse(jrr)

		network.Logger().Debug().
			Int64("originalLatest", latestBlockNumber).
			Int64("laggedBlock", laggedBlockNumber).
			Int64("intentionalBlockLag", ncfg.Evm.IntentionalBlockLag).
			Msg("applied intentional block lag to eth_blockNumber request")

		return true, resp, nil
	}

	if ncfg.Evm.Integrity == nil ||
		ncfg.Evm.Integrity.EnforceHighestBlock == nil ||
		!*ncfg.Evm.Integrity.EnforceHighestBlock {
		// If integrity check for highest block is disabled, skip this hook.
		return false, nil, nil
	}

	// Step 1: forward the request normally
	resp, err = network.Forward(ctx, nq)
	if err != nil {
		common.SetTraceSpanError(span, err)
		return true, nil, err
	}

	// If response is from cache, skip enforcement otherwise there's no point in caching.
	// As we'll definetely have higher latest block number vs what we have in cache.
	// The correct way to deal with this situation is to set proper TTL for "realtime" cache policy.
	if resp != nil && resp.FromCache() {
		network.Logger().Trace().
			Object("request", nq).
			Object("response", resp).
			Msg("skipping enforcement of highest block number as response is from cache")
		return true, resp, nil
	}

	// Step 2: parse the block number from the existing response
	blockRef, blockNumber, err := ExtractBlockReferenceFromResponse(ctx, resp)
	if err != nil {
		common.SetTraceSpanError(span, err)
		return true, nil, err
	}

	// Step 3: collect the highest block from all EVM upstream pollers for this network
	highestBlock := network.EvmHighestLatestBlockNumber(ctx)
	if common.IsTracingDetailed {
		span.SetAttributes(
			attribute.Int64("block.number", blockNumber),
			attribute.String("block.ref", blockRef),
			attribute.Int64("highest_block", highestBlock),
		)
	}

	// Step 4: if maxBlock is larger than forwardBlock, write that in the response
	if highestBlock > blockNumber {
		ups := resp.Upstream()
		if ups != nil {
			telemetry.MetricUpstreamStaleLatestBlock.WithLabelValues(
				network.ProjectId(),
				ups.VendorName(),
				network.Id(),
				ups.Id(),
				"eth_blockNumber",
			).Inc()
			network.Logger().Debug().
				Str("method", "eth_blockNumber").
				Int64("knownHighestBlock", highestBlock).
				Int64("responseBlockNumber", blockNumber).
				Str("upstreamId", ups.Id()).
				Msg("upstream returned older block than we known, falling back to highest known block")
		} else {
			// This usually shouldn't happen except when reading from cache which is already ignored above.
			network.Logger().Debug().
				Str("method", "eth_blockNumber").
				Int64("knownHighestBlock", highestBlock).
				Int64("responseBlockNumber", blockNumber).
				Object("request", nq).
				Object("response", resp).
				Msg("upstream returned older block than we known, falling back to highest known block")
		}
		hbk, err := common.NormalizeHex(highestBlock)
		if err != nil {
			common.SetTraceSpanError(span, err)
			return true, nil, err
		}
		jrr, err := common.NewJsonRpcResponse(nq.ID(), hbk, nil)
		if err != nil {
			common.SetTraceSpanError(span, err)
			return true, nil, err
		}
		resp := common.NewNormalizedResponse().
			WithRequest(nq).
			WithJsonRpcResponse(jrr)

		return true, resp, nil
	}

	return true, resp, nil
}
