package evm

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// tipRefresher is implemented by *erpc.Network. Optional so test doubles that
// only stub EvmHighestLatestBlockNumber keep compiling.
type tipRefresher interface {
	EvmRefreshHighestLatestBlockNumber(ctx context.Context) int64
}

func refreshHighestLatestBlockNumber(ctx context.Context, network common.Network) int64 {
	if r, ok := network.(tipRefresher); ok {
		return r.EvmRefreshHighestLatestBlockNumber(ctx)
	}
	return common.EvmHighestLatestBlockNumber(network, ctx)
}

func BuildGetBlockByNumberRequest(blockNumberOrTag interface{}, includeTransactions bool) (*common.JsonRpcRequest, error) {
	var bkt string
	var err error

	switch v := blockNumberOrTag.(type) {
	case string:
		bkt = v
		if !strings.HasPrefix(bkt, "0x") {
			switch bkt {
			case "latest", "finalized", "safe", "pending", "earliest":
				// Acceptable tags
			default:
				return nil, fmt.Errorf("invalid block number or tag for eth_getBlockByNumber: %v", v)
			}
		}
	case int, int64, float64:
		bkt, err = common.NormalizeHex(v)
		if err != nil {
			return nil, fmt.Errorf("invalid block number or tag for eth_getBlockByNumber: %v", v)
		}
	default:
		return nil, fmt.Errorf("invalid block number or tag for eth_getBlockByNumber: %v", v)
	}

	return common.NewJsonRpcRequest("eth_getBlockByNumber", []interface{}{bkt, includeTransactions}), nil
}

func networkPostForward_eth_getBlockByNumber(ctx context.Context, network common.Network, nq *common.NormalizedRequest, nr *common.NormalizedResponse, re error) (*common.NormalizedResponse, error) {
	ctx, span := common.StartDetailSpan(ctx, "Network.PostForward.eth_getBlockByNumber", trace.WithAttributes(
		attribute.String("request.id", fmt.Sprintf("%v", nq.ID())),
		attribute.String("network.id", network.Id()),
	))
	defer span.End()

	nr, err := enforceHighestBlock(ctx, network, nq, nr, re)
	if err != nil {
		common.SetTraceSpanError(span, err)
		return nr, err
	}

	// Track timestamp distance for "latest" blocks and add lag metrics to detailed spans
	if nr != nil && re == nil {
		rqj, rqErr := nq.JsonRpcRequest(ctx)
		if rqErr == nil && rqj != nil {
			rqj.RLock()
			if len(rqj.Params) >= 1 {
				if bnp, ok := rqj.Params[0].(string); ok && bnp == "latest" {
					// Extract block number and timestamp from the response
					_, respBlockNumber, bnErr := ExtractBlockReferenceFromResponse(ctx, nr)
					blockTimestamp, tsErr := ExtractBlockTimestampFromResponse(ctx, nr)

					// Calculate block number lag
					if common.IsTracingDetailed && bnErr == nil && respBlockNumber > 0 {
						highestBlock := common.EvmHighestLatestBlockNumber(network, ctx)
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

					// Calculate timestamp lag and record metric
					if tsErr == nil && blockTimestamp > 0 {
						currentTime := time.Now().Unix()
						timestampLag := currentTime - blockTimestamp

						// Add to detailed span
						if common.IsTracingDetailed {
							span.SetAttributes(
								attribute.Int64("block.timestamp", blockTimestamp),
								attribute.Int64("current.timestamp", currentTime),
								attribute.Int64("block.timestamp_lag", timestampLag),
							)
						}

						// Record prometheus metric
						telemetry.MetricNetworkLatestBlockTimestampDistance.WithLabelValues(
							network.ProjectId(),
							network.Label(),
							"network_response",
						).Set(float64(timestampLag))
					}
				}
			}
			rqj.RUnlock()
		}
	}

	return enforceNonNullBlock(ctx, nq, nr)
}

func enforceHighestBlock(ctx context.Context, network common.Network, nq *common.NormalizedRequest, nr *common.NormalizedResponse, re error) (*common.NormalizedResponse, error) {
	if re != nil {
		return nr, re
	}

	// Check directive - this is the new way to control this behavior
	dirs := nq.Directives()
	if dirs == nil || !dirs.EnforceHighestBlock {
		return nr, re
	}

	logger := network.Logger().With().Str("method", "eth_getBlockByNumber").Logger()

	// Cached "latest" can lag TipHW across pods / tip races. Only skip
	// enforcement when the cached payload already meets the tip floor.
	if nr.FromCache() {
		highestBlockNumber := common.EvmHighestLatestBlockNumber(network, ctx)
		_, cachedBN, cerr := ExtractBlockReferenceFromResponse(ctx, nr)
		if cerr == nil && cachedBN >= highestBlockNumber {
			if refreshed := refreshHighestLatestBlockNumber(ctx, network); refreshed > highestBlockNumber {
				highestBlockNumber = refreshed
			}
		}
		if cerr == nil && cachedBN >= highestBlockNumber {
			logger.Trace().
				Object("request", nq).
				Object("response", nr).
				Msg("skipping enforcement of highest block number as cached response meets tip")
			return nr, re
		}
		logger.Debug().
			Int64("highestBlockNumber", highestBlockNumber).
			Int64("cachedBlockNumber", cachedBN).
			Msg("cached latest lags tip; enforcing highest block")
	}

	rqj, err := nq.JsonRpcRequest(ctx)
	if err != nil {
		return nil, err
	}
	rqj.RLock()
	if len(rqj.Params) < 1 {
		rqj.RUnlock()
		return nr, re
	}
	bnp, ok := rqj.Params[0].(string)
	if !ok {
		rqj.RUnlock()
		return nr, re
	}
	var itx bool
	if len(rqj.Params) > 1 {
		itx, _ = rqj.Params[1].(bool)
	}
	rqj.RUnlock()

	if bnp != "latest" && bnp != "finalized" {
		return nr, re
	}

	// Resolve tips with the request bound to the context so a use-upstream
	// selector scopes the tip to the targeted subset (the selector-scoped
	// served-tip semantics): a request pinned to a lagging group must not be
	// re-forwarded towards a block that group cannot serve.
	tipCtx := context.WithValue(ctx, common.RequestContextKey, nq)

	switch bnp {
	case "latest":
		highestBlockNumber := common.EvmHighestLatestBlockNumber(network, tipCtx)
		_, respBlockNumber, err := ExtractBlockReferenceFromResponse(ctx, nr)
		if err != nil {
			return nil, err
		}
		if highestBlockNumber <= respBlockNumber {
			// The local tip appears caught up — but a sibling instance may have
			// published a higher tip to shared state that this process has not
			// yet adopted via async pub/sub. Refresh once before skipping
			// enforcement; this is the cross-instance race that makes a strict
			// client mark the gateway as out of sync.
			if refreshed := refreshHighestLatestBlockNumber(ctx, network); refreshed > highestBlockNumber {
				highestBlockNumber = refreshed
			}
			if highestBlockNumber <= respBlockNumber {
				return nr, re
			}
			logger.Debug().
				Str("blockTag", bnp).
				Int64("highestBlockNumber", highestBlockNumber).
				Int64("respBlockNumber", respBlockNumber).
				Msg("tip refresh from remote raised TipHW; enforcing highest latest block")
		} else {
			logger.Debug().
				Str("blockTag", bnp).
				Object("request", nq).
				Object("response", nr).
				Interface("highestBlockNumber", highestBlockNumber).
				Interface("respBlockNumber", respBlockNumber).
				Msg("enforcing highest latest block")
		}
		// fall through to tip re-fetch / refuse-stale (logger already emitted)
		if respBlockNumber > 0 {
			ups := nr.Upstream()
			telemetry.MetricUpstreamStaleLatestBlock.WithLabelValues(
				network.ProjectId(),
				ups.VendorName(),
				network.Label(),
				ups.Id(),
				"eth_getBlockByNumber",
			).Inc()
		}

		// Prefer the upstream whose poller already owns this tip
		// (EvmLeaderUpstream — typically the WS ingress that called
		// SuggestLatestBlock). If TipHW advanced via Redis/WS while
		// local pollers lag inside their debounce window, force-poll
		// the leader once before deciding. Fall back to excluding the
		// stale responder when no local poller has caught up yet.
		useUpstream := ""
		if leader := common.EvmLeaderUpstream(network, ctx); leader != nil {
			if eu, ok := leader.(common.EvmUpstream); ok {
				if sp := eu.EvmStatePoller(); sp != nil && !sp.IsObjectNull() {
					if sp.LatestBlock() < highestBlockNumber {
						_, _ = sp.PollLatestBlockNumberNow(ctx)
					}
					if sp.LatestBlock() >= highestBlockNumber {
						useUpstream = leader.Id()
					}
				}
			}
		}
		if useUpstream == "" && respBlockNumber > 0 {
			useUpstream = fmt.Sprintf("!%s", nr.UpstreamId())
		}

		// Do not use pickHighestBlock against the stale "latest" response —
		// that helper fail-opens to stale when the tip re-fetch misses, which
		// is exactly what strict clients (repeatable-read / head-sync checks)
		// reject.
		nnr, ferr := forwardGetBlockByNumber(ctx, network, nq, highestBlockNumber, itx, useUpstream)
		if meetsTipFloor(ctx, nnr, highestBlockNumber) {
			if nr != nil {
				nr.Release()
			}
			return nnr, nil
		}
		if nnr != nil {
			nnr.Release()
		}

		// Pinned / excluded re-fetch missed the tip (sibling fullnode
		// lag, WS JSON-RPC miss, etc.). Retry with no UseUpstream pin
		// so every upstream (including fallbacks via escape) can serve
		// the concrete TipHW block.
		nnr2, ferr2 := forwardGetBlockByNumber(ctx, network, nq, highestBlockNumber, itx, "")
		if meetsTipFloor(ctx, nnr2, highestBlockNumber) {
			if nr != nil {
				nr.Release()
			}
			return nnr2, nil
		}
		if nnr2 != nil {
			nnr2.Release()
		}

		// NEVER fail-open to a tip below TipHW. Prefer an error over stale.
		logger.Warn().
			Int64("highestBlockNumber", highestBlockNumber).
			Int64("staleBlockNumber", respBlockNumber).
			Err(ferr2).
			Msg("tip re-fetch could not reach TipHW; refusing stale latest")
		if nr != nil {
			nr.Release()
		}
		if ferr2 != nil {
			return nil, ferr2
		}
		if ferr != nil {
			return nil, ferr
		}
		details := map[string]interface{}{"blockNumber": highestBlockNumber}
		return nil, common.NewErrEndpointMissingData(
			common.NewErrJsonRpcExceptionInternal(
				0,
				common.JsonRpcErrorMissingData,
				fmt.Sprintf("block not found with number %d", highestBlockNumber),
				nil,
				details,
			),
			nil,
		)
	case "finalized":
		highestBlockNumber := common.EvmHighestFinalizedBlockNumber(network, tipCtx)
		_, respBlockNumber, err := ExtractBlockReferenceFromResponse(ctx, nr)
		if err != nil {
			return nil, err
		}
		if highestBlockNumber <= respBlockNumber {
			return nr, re
		}
		logger.Debug().
			Str("blockTag", bnp).
			Interface("highestBlockNumber", highestBlockNumber).
			Interface("respBlockNumber", respBlockNumber).
			Msg("enforcing highest finalized block")
		if respBlockNumber > 0 {
			ups := nr.Upstream()
			telemetry.MetricUpstreamStaleFinalizedBlock.WithLabelValues(
				network.ProjectId(),
				ups.VendorName(),
				network.Label(),
				ups.Id(),
			).Inc()
		}
		useUpstream := ""
		if respBlockNumber > 0 {
			useUpstream = fmt.Sprintf("!%s", nr.UpstreamId())
		}
		nnr, err := forwardGetBlockByNumber(ctx, network, nq, highestBlockNumber, itx, useUpstream)
		return pickHighestBlock(ctx, nnr, nr, err)
	default:
		return nr, re
	}
}

// enforceNonNullBlock checks if the block result is null/empty and returns an appropriate error
// This is now controlled by the EnforceNonNullTaggedBlocks directive
func enforceNonNullBlock(ctx context.Context, nq *common.NormalizedRequest, nr *common.NormalizedResponse) (*common.NormalizedResponse, error) {
	if nr != nil && !nr.IsObjectNull() && !nr.IsResultEmptyish() {
		return nr, nil
	}

	// Response is null/empty - extract block parameter to determine if it's a tag or numeric
	rq := nr.Request()
	var bnp string
	var isTag bool
	if rq != nil {
		rqj, _ := rq.JsonRpcRequest()
		if rqj != nil && len(rqj.Params) > 0 {
			bnp, _ = rqj.Params[0].(string)
			// Check if it's a block tag (not a hex number)
			// Tags: "latest", "pending", "finalized", "safe", "earliest"
			// Numeric: starts with "0x"
			isTag = bnp != "" && !strings.HasPrefix(bnp, "0x")
		}
	}

	// For tagged blocks, check directive
	if isTag {
		dirs := nq.Directives()
		if dirs == nil || !dirs.EnforceNonNullTaggedBlocks {
			// Directive not set or disabled - allow null tagged blocks
			return nr, nil
		}
	}

	// A block beyond the network's confidence head (latest by default, or finalized)
	// isn't produced/confirmed yet and legitimately returns null on every upstream —
	// it isn't missing/pruned data, so don't convert it to an error and churn retries.
	// Mirrors the upstream-level markUnexpectedEmpty guard so the two layers agree.
	if emptyResultBeyondConfidence(ctx, nq) {
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

func forwardGetBlockByNumber(
	ctx context.Context,
	network common.Network,
	original *common.NormalizedRequest,
	blockNumber int64,
	includeTx bool,
	useUpstream string,
) (*common.NormalizedResponse, error) {
	request, err := BuildGetBlockByNumberRequest(blockNumber, includeTx)
	if err != nil {
		return nil, err
	}
	if err := request.SetID(original.ID()); err != nil {
		return nil, err
	}
	newReq := common.NewNormalizedRequestFromJsonRpcRequest(request)
	dr := original.Directives().Clone()
	dr.SkipCacheRead = "true"
	dr.UseUpstream = useUpstream
	newReq.SetDirectives(dr)
	newReq.SetNetwork(network)
	newReq.CopyHttpContextFrom(original)
	return network.Forward(ctx, newReq)
}

// meetsTipFloor reports whether resp carries a block number >= minBlock.
func meetsTipFloor(ctx context.Context, resp *common.NormalizedResponse, minBlock int64) bool {
	if resp == nil || resp.IsObjectNull() || resp.IsResultEmptyish() || minBlock <= 0 {
		return false
	}
	// Peek number directly — ExtractBlockReferenceFromResponse can fail on
	// incomplete header fields (hash/parentHash) even when number is present.
	jrr, err := resp.JsonRpcResponse(ctx)
	if err != nil || jrr == nil {
		return false
	}
	numStr, err := jrr.PeekStringByPath(ctx, "number")
	if err != nil || numStr == "" {
		return false
	}
	bn, err := common.HexToInt64(numStr)
	return err == nil && bn >= minBlock
}

func pickHighestBlock(ctx context.Context, x *common.NormalizedResponse, y *common.NormalizedResponse, err error) (*common.NormalizedResponse, error) {
	ctx, span := common.StartDetailSpan(ctx, "Evm.PickHighestBlock")
	defer span.End()

	xnull := x == nil || x.IsObjectNull() || x.IsResultEmptyish()
	ynull := y == nil || y.IsObjectNull() || y.IsResultEmptyish()
	if xnull && ynull && err != nil {
		// both emptyish; nothing to keep
		if x != nil {
			x.Release()
		}
		if y != nil {
			y.Release()
		}
		return nil, err
	} else if xnull && !ynull {
		if x != nil {
			x.Release()
		}
		return y, nil
	} else if !xnull && ynull {
		if y != nil {
			y.Release()
		}
		return x, nil
	}
	xjrr, err := x.JsonRpcResponse(ctx)
	if err != nil || xjrr == nil {
		if x != nil && x != y {
			x.Release()
		}
		return y, nil
	}
	yjrr, err := y.JsonRpcResponse(ctx)
	if err != nil || yjrr == nil {
		if y != nil && y != x {
			y.Release()
		}
		return x, nil
	}
	xbn, err := xjrr.PeekStringByPath(ctx, "number")
	if err != nil {
		if x != nil && x != y {
			x.Release()
		}
		return y, nil
	}
	span.SetAttributes(attribute.String("block_number_1", xbn))
	ybn, err := yjrr.PeekStringByPath(ctx, "number")
	if err != nil {
		if y != nil && y != x {
			y.Release()
		}
		return x, nil
	}
	span.SetAttributes(attribute.String("block_number_2", ybn))
	xbnInt, err := strconv.ParseInt(xbn, 0, 64)
	if err != nil {
		if x != nil && x != y {
			x.Release()
		}
		return y, nil
	}
	ybnInt, err := strconv.ParseInt(ybn, 0, 64)
	if err != nil {
		if y != nil && y != x {
			y.Release()
		}
		return x, nil
	}
	if xbnInt > ybnInt {
		if y != nil && y != x {
			y.Release()
		}
		return x, nil
	}
	if x != nil && x != y {
		x.Release()
	}
	return y, nil
}
