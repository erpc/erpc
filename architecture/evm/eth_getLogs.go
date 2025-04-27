package evm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog/log"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// LowerBoundBlocksSafetyMargin is the number of blocks to subtract from the last available block to be on the safe side,
// when looking for data too close to the lower-end of Full nodes, because they might have pruned the data already.
var LowerBoundBlocksSafetyMargin int64 = 10

func BuildGetLogsRequest(fromBlock, toBlock int64, address interface{}, topics interface{}) (*common.JsonRpcRequest, error) {
	fb, err := common.NormalizeHex(fromBlock)
	if err != nil {
		return nil, err
	}
	tb, err := common.NormalizeHex(toBlock)
	if err != nil {
		return nil, err
	}
	filter := map[string]interface{}{
		"fromBlock": fb,
		"toBlock":   tb,
	}
	if address != nil {
		filter["address"] = address
	}
	if topics != nil {
		filter["topics"] = topics
	}
	jrq := common.NewJsonRpcRequest("eth_getLogs", []interface{}{filter})
	err = jrq.SetID(util.RandomID())
	if err != nil {
		return nil, err
	}

	return jrq, nil
}

func upstreamPreForward_eth_getLogs(ctx context.Context, n common.Network, u common.Upstream, r *common.NormalizedRequest) (handled bool, resp *common.NormalizedResponse, err error) {
	up, ok := u.(common.EvmUpstream)
	if !ok {
		log.Warn().Interface("upstream", u).Object("request", r).Msg("passed upstream is not a common.EvmUpstream")
		return false, nil, nil
	}

	ncfg := n.Config()
	if ncfg == nil ||
		ncfg.Evm == nil ||
		ncfg.Evm.Integrity == nil ||
		ncfg.Evm.Integrity.EnforceGetLogsBlockRange == nil ||
		!*ncfg.Evm.Integrity.EnforceGetLogsBlockRange {
		// If integrity check for eth_getLogs block range is disabled, skip this hook.
		return false, nil, nil
	}
	ctx, span := common.StartDetailSpan(ctx, "Upstream.PreForwardHook.eth_getLogs", trace.WithAttributes(
		attribute.String("request.id", fmt.Sprintf("%v", r.ID())),
		attribute.String("network.id", n.Id()),
		attribute.String("upstream.id", up.Config().Id),
	))
	defer span.End()

	logger := up.Logger().With().Str("method", "eth_getLogs").Interface("id", r.ID()).Logger()

	jrq, err := r.JsonRpcRequest(ctx)
	if err != nil {
		return true, nil, err
	}

	jrq.RLock()
	if len(jrq.Params) < 1 {
		jrq.RUnlock()
		return false, nil, nil
	}
	filter, ok := jrq.Params[0].(map[string]interface{})
	if !ok {
		jrq.RUnlock()
		return false, nil, nil
	}

	// EIP 234: If blockHash is present, we set handled to false to directly forward to upstream since there
	// is no need to break the request into sub-requests.
	blockHash, ok := filter["blockHash"].(string)
	if ok && blockHash != "" {
		jrq.RUnlock()
		return false, nil, nil
	}

	fb, ok := filter["fromBlock"].(string)
	if !ok || !strings.HasPrefix(fb, "0x") {
		jrq.RUnlock()
		return false, nil, nil
	}
	fromBlock, err := strconv.ParseInt(fb, 0, 64)
	if err != nil {
		jrq.RUnlock()
		return true, nil, err
	}
	tb, ok := filter["toBlock"].(string)
	if !ok || !strings.HasPrefix(tb, "0x") {
		jrq.RUnlock()
		return false, nil, nil
	}
	toBlock, err := strconv.ParseInt(tb, 0, 64)
	if err != nil {
		jrq.RUnlock()
		return true, nil, err
	}
	jrq.RUnlock()

	if fromBlock > toBlock {
		return true, nil, errors.New("fromBlock must be less than or equal to toBlock")
	}
	requestRange := toBlock - fromBlock + 1
	cfg := up.Config()
	if common.IsTracingDetailed {
		span.SetAttributes(
			attribute.Int64("request_range", requestRange),
			attribute.Int64("max_allowed_range", cfg.Evm.GetLogsMaxAllowedRange),
		)
	}
	// check if the log range is beyond the hard limit
	if requestRange > 0 && cfg != nil && cfg.Evm != nil && cfg.Evm.GetLogsMaxAllowedRange > 0 {
		if requestRange > cfg.Evm.GetLogsMaxAllowedRange {
			return true, nil, common.NewErrUpstreamGetLogsExceededMaxAllowedRange(
				up.Config().Id,
				requestRange,
				cfg.Evm.GetLogsMaxAllowedRange,
			)
		}
	}

	// check if the number of addresses is beyond the hard limit
	addresses, hasAddresses := filter["address"].([]interface{})
	if hasAddresses && cfg != nil && cfg.Evm != nil && cfg.Evm.GetLogsMaxAllowedAddresses > 0 &&
		int64(len(addresses)) > cfg.Evm.GetLogsMaxAllowedAddresses {
		return true, nil, common.NewErrUpstreamGetLogsExceededMaxAllowedAddresses(
			up.Config().Id,
			int64(len(addresses)),
			cfg.Evm.GetLogsMaxAllowedAddresses,
		)
	}

	// check if the number of topics is beyond the hard limit
	topics, hasTopics := filter["topics"].([]interface{})
	if hasTopics && cfg != nil && cfg.Evm != nil && cfg.Evm.GetLogsMaxAllowedTopics > 0 &&
		int64(len(topics)) > cfg.Evm.GetLogsMaxAllowedTopics {
		return true, nil, common.NewErrUpstreamGetLogsExceededMaxAllowedTopics(
			up.Config().Id,
			int64(len(topics)),
			cfg.Evm.GetLogsMaxAllowedTopics,
		)
	}

	statePoller := up.EvmStatePoller()
	if statePoller == nil || statePoller.IsObjectNull() {
		return true, nil, common.NewErrUpstreamRequestSkipped(
			fmt.Errorf("upstream evm state poller is not available"),
			up.Config().Id,
		)
	}

	// Check if upstream state poller has the last block >= logs range end,
	// if not force a poller update, and if still not enough, skip the request.
	latestBlock := statePoller.LatestBlock()
	if common.IsTracingDetailed {
		span.SetAttributes(
			attribute.Int64("upstream_latest_block", latestBlock),
			attribute.Int64("from_block", fromBlock),
			attribute.Int64("to_block", toBlock),
		)
	}
	if latestBlock > 0 {
		logger.Debug().Int64("fromBlock", fromBlock).Int64("toBlock", toBlock).Int64("latestBlock", latestBlock).Msg("checking eth_getLogs block range integrity")

		if latestBlock < toBlock {
			latestBlock, err = statePoller.PollLatestBlockNumber(ctx)
			if err != nil {
				return true, nil, err
			}
		}
		if latestBlock < toBlock {
			telemetry.MetricUpstreamEvmGetLogsStaleUpperBound.WithLabelValues(
				n.ProjectId(),
				up.NetworkId(),
				up.Config().Id,
			).Inc()
			return true, nil, common.NewErrUpstreamRequestSkipped(
				fmt.Errorf("upstream latest block %d is less than toBlock %d", latestBlock, toBlock),
				up.Config().Id,
			)
		}
	} else {
		logger.Debug().Msg("upstream latest block is not available, skipping integrity check")
	}

	// Check if the log range start is higher than node's max available block range,
	// if not, skip the request.
	if cfg != nil && cfg.Evm != nil && cfg.Evm.MaxAvailableRecentBlocks > 0 {
		lastAvailableBlock := latestBlock - cfg.Evm.MaxAvailableRecentBlocks
		if common.IsTracingDetailed {
			span.SetAttributes(
				attribute.Int64("last_available_block", lastAvailableBlock),
			)
		}
		// If range is beyond the last available block, or too close to the last available block, skip the request for safety.
		if fromBlock < (lastAvailableBlock + LowerBoundBlocksSafetyMargin) {
			telemetry.MetricUpstreamEvmGetLogsStaleLowerBound.WithLabelValues(
				n.ProjectId(),
				up.NetworkId(),
				up.Config().Id,
			).Inc()
			return true, nil, common.NewErrUpstreamRequestSkipped(
				fmt.Errorf("requested fromBlock %d is < than upstream latest block %d minus max available recent blocks %d plus safety margin %d", fromBlock, latestBlock, cfg.Evm.MaxAvailableRecentBlocks, LowerBoundBlocksSafetyMargin),
				up.Config().Id,
			)
		}
	}

	// Check evmGetLogsMaxRange and if the range is already bigger try to split in multiple smaller requests, and merge the final result
	// For better performance try to use byte merging (not JSON parsing/encoding)
	if requestRange > 0 && cfg != nil && cfg.Evm != nil && cfg.Evm.GetLogsAutoSplittingRangeThreshold > 0 {
		if requestRange > cfg.Evm.GetLogsAutoSplittingRangeThreshold {
			telemetry.MetricUpstreamEvmGetLogsRangeExceededAutoSplittingThreshold.WithLabelValues(
				n.ProjectId(),
				up.NetworkId(),
				up.Config().Id,
			).Inc()

			var subRequests []ethGetLogsSubRequest
			sb := fromBlock
			for sb <= toBlock {
				eb := min(sb+cfg.Evm.GetLogsAutoSplittingRangeThreshold-1, toBlock)
				subRequests = append(subRequests, ethGetLogsSubRequest{
					fromBlock: sb,
					toBlock:   eb,
					address:   filter["address"],
					topics:    filter["topics"],
				})
				sb = eb + 1
			}
			logger.Debug().
				Int64("requestRange", requestRange).
				Int64("maxBlockRange", cfg.Evm.GetLogsAutoSplittingRangeThreshold).
				Int("subRequests", len(subRequests)).
				Msg("eth_getLogs block range exceeded, splitting")

			mergedResponse, err := executeGetLogsSubRequests(ctx, n, u, r, subRequests, r.Directives().SkipCacheRead)
			if err != nil {
				return true, nil, err
			}

			mergedNR := common.NewNormalizedResponse().
				WithRequest(r).
				WithJsonRpcResponse(mergedResponse)
			r.SetLastValidResponse(mergedNR)
			mergedNR.SetUpstream(u)

			return true, mergedNR, nil
		}
	}

	// Continue with the original forward flow
	return false, nil, nil
}

func upstreamPostForward_eth_getLogs(ctx context.Context, n common.Network, u common.Upstream, rq *common.NormalizedRequest, rs *common.NormalizedResponse, re error, skipCacheRead bool) (*common.NormalizedResponse, error) {
	ctx, span := common.StartDetailSpan(ctx, "Upstream.PostForwardHook.eth_getLogs", trace.WithAttributes(
		attribute.String("request.id", fmt.Sprintf("%v", rq.ID())),
		attribute.String("network.id", n.Id()),
		attribute.String("upstream.id", u.Config().Id),
	))
	defer span.End()

	if re != nil {
		if common.HasErrorCode(re, common.ErrCodeEndpointRequestTooLarge) {
			logger := u.Logger().With().Str("method", "eth_getLogs").Interface("id", rq.ID()).Logger()
			// Split the request in half, first on block range, if 1 block then on addresses, if 1 address then on topics
			subRequests, err := splitEthGetLogsRequest(rq)
			// TODO Update the evmGetLogsMaxRange accordingly?
			if err != nil {
				logger.Warn().Err(err).Object("request", rq).Msg("could not split eth_getLogs request, returning original response")
				return rs, re
			}
			mergedResponse, err := executeGetLogsSubRequests(ctx, n, u, rq, subRequests, skipCacheRead)
			if err != nil {
				logger.Warn().Err(err).Object("request", rq).Msg("could not execute eth_getLogs sub-requests, returning original response")
				return rs, re
			}

			return common.NewNormalizedResponse().
				WithRequest(rq).
				WithJsonRpcResponse(mergedResponse), nil
		}
	} else if rs != nil && rs.IsResultEmptyish(ctx) {
		// This is to normalize empty logs responses (e.g. instead of returning "null")
		jrr, err := common.NewJsonRpcResponse(rq.ID(), []interface{}{}, nil)
		if err != nil {
			return nil, err
		}
		nnr := common.NewNormalizedResponse().WithRequest(rq).WithJsonRpcResponse(jrr)
		nnr.SetUpstream(u)
		nnr.SetFromCache(rs.FromCache())
		nnr.SetEvmBlockRef(rs.EvmBlockRef())
		nnr.SetEvmBlockNumber(rs.EvmBlockNumber())
		nnr.SetAttempts(rs.Attempts())
		nnr.SetRetries(rs.Retries())
		nnr.SetHedges(rs.Hedges())
		rq.SetLastValidResponse(nnr)
		return nnr, nil
	}

	return rs, re
}

type GetLogsMultiResponseWriter struct {
	responses []*common.JsonRpcResponse
}

func NewGetLogsMultiResponseWriter(responses []*common.JsonRpcResponse) *GetLogsMultiResponseWriter {
	return &GetLogsMultiResponseWriter{
		responses: responses,
	}
}

func (g *GetLogsMultiResponseWriter) WriteTo(w io.Writer, trimSides bool) (n int64, err error) {
	// Write opening bracket
	if !trimSides {
		nn, err := w.Write([]byte{'['})
		if err != nil {
			return int64(nn), err
		}
		n += int64(nn)
	}

	first := true
	for _, response := range g.responses {
		if response == nil || response.IsResultEmptyish() {
			continue // Skip empty results
		}

		if !first {
			// Write comma separator
			nn, err := w.Write([]byte{','})
			if err != nil {
				return n + int64(nn), err
			}
			n += int64(nn)
		}
		first = false

		// Write the inner content, skipping the outer brackets
		nw, err := response.WriteResultTo(w, true)
		if err != nil {
			return n + nw, err
		}
		n += nw
	}

	if !trimSides {
		// Write closing bracket
		nn, err := w.Write([]byte{']'})
		return n + int64(nn), err
	}

	return n, nil
}

func (g *GetLogsMultiResponseWriter) IsResultEmptyish() bool {
	if len(g.responses) == 0 {
		return true
	}

	for _, response := range g.responses {
		if response == nil {
			continue
		}
		if !response.IsResultEmptyish() {
			return false
		}
	}

	return true
}

type ethGetLogsSubRequest struct {
	fromBlock int64
	toBlock   int64
	address   interface{}
	topics    interface{}
}

func splitEthGetLogsRequest(r *common.NormalizedRequest) ([]ethGetLogsSubRequest, error) {
	jrq, err := r.JsonRpcRequest()
	if err != nil {
		return nil, err
	}
	jrq.RLock()
	defer jrq.RUnlock()

	if len(jrq.Params) < 1 {
		return nil, fmt.Errorf("invalid params length")
	}

	filter, ok := jrq.Params[0].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("invalid filter format")
	}

	fb, tb, err := extractBlockRange(filter)
	if err != nil {
		return nil, err
	}

	n := r.Network()
	var u common.Upstream
	var c *common.UpstreamConfig
	if p := r.LastValidResponse(); p != nil {
		if s := p.Upstream(); s != nil {
			u = s
			if f := u.Config(); f != nil {
				c = f
			}
		}
	}

	// First try splitting by block range
	blockRange := tb - fb + 1
	if blockRange > 1 {
		if n != nil && u != nil && c != nil {
			telemetry.MetricUpstreamEvmGetLogsForcedSplits.WithLabelValues(
				n.ProjectId(),
				u.NetworkId(),
				c.Id,
				"block_range",
			).Inc()
		}
		mid := fb + (blockRange / 2)
		return []ethGetLogsSubRequest{
			{fromBlock: fb, toBlock: mid - 1, address: filter["address"], topics: filter["topics"]},
			{fromBlock: mid, toBlock: tb, address: filter["address"], topics: filter["topics"]},
		}, nil
	}

	// If single block, try splitting by address
	addresses, ok := filter["address"].([]interface{})
	if ok && len(addresses) > 1 {
		mid := len(addresses) / 2
		if n != nil && u != nil && c != nil {
			telemetry.MetricUpstreamEvmGetLogsForcedSplits.WithLabelValues(
				n.ProjectId(),
				u.NetworkId(),
				c.Id,
				"addresses",
			).Inc()
		}
		return []ethGetLogsSubRequest{
			{fromBlock: fb, toBlock: tb, address: addresses[:mid], topics: filter["topics"]},
			{fromBlock: fb, toBlock: tb, address: addresses[mid:], topics: filter["topics"]},
		}, nil
	}

	// If single address or no address, try splitting by topics
	topics, ok := filter["topics"].([]interface{})
	if ok && len(topics) > 1 {
		mid := len(topics) / 2
		if n != nil && u != nil && c != nil {
			telemetry.MetricUpstreamEvmGetLogsForcedSplits.WithLabelValues(
				n.ProjectId(),
				u.NetworkId(),
				c.Id,
				"topics",
			).Inc()
		}
		return []ethGetLogsSubRequest{
			{fromBlock: fb, toBlock: tb, address: filter["address"], topics: topics[:mid]},
			{fromBlock: fb, toBlock: tb, address: filter["address"], topics: topics[mid:]},
		}, nil
	}

	return nil, fmt.Errorf("request cannot be split further")
}

func extractBlockRange(filter map[string]interface{}) (fromBlock, toBlock int64, err error) {
	fb, ok := filter["fromBlock"].(string)
	if !ok || !strings.HasPrefix(fb, "0x") {
		return 0, 0, fmt.Errorf("invalid fromBlock")
	}
	fromBlock, err = strconv.ParseInt(fb, 0, 64)
	if err != nil {
		return 0, 0, err
	}

	tb, ok := filter["toBlock"].(string)
	if !ok || !strings.HasPrefix(tb, "0x") {
		return 0, 0, fmt.Errorf("invalid toBlock")
	}
	toBlock, err = strconv.ParseInt(tb, 0, 64)
	if err != nil {
		return 0, 0, err
	}

	return fromBlock, toBlock, nil
}

func executeGetLogsSubRequests(ctx context.Context, n common.Network, u common.Upstream, r *common.NormalizedRequest, subRequests []ethGetLogsSubRequest, skipCacheRead bool) (*common.JsonRpcResponse, error) {
	logger := u.Logger().With().Str("method", "eth_getLogs").Interface("id", r.ID()).Logger()

	r.SetIsCompositeRequest(true)
	r.SetCompositeType("logs-split")

	wg := sync.WaitGroup{}
	responses := make([]*common.JsonRpcResponse, 0)
	errs := make([]error, 0)
	mu := sync.Mutex{}

	// TODO should we make this semaphore configurable?
	semaphore := make(chan struct{}, 200)
	for _, sr := range subRequests {
		wg.Add(1)
		// Acquire semaphore token (blocks if at capacity)
		semaphore <- struct{}{}
		go func(req ethGetLogsSubRequest) {
			defer wg.Done()
			defer func() {
				// Release semaphore token when done
				<-semaphore
			}()

			srq, err := BuildGetLogsRequest(req.fromBlock, req.toBlock, req.address, req.topics)
			logger.Debug().
				Object("request", srq).
				Msg("executing eth_getLogs sub-request")

			if err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
				return
			}

			sbnrq := common.NewNormalizedRequestFromJsonRpcRequest(srq)
			dr := r.Directives().Clone()
			dr.SkipCacheRead = skipCacheRead
			// TODO dr.UseUpstream = u.Config().Id should we force this (or opposite of it)?
			sbnrq.SetDirectives(dr)
			sbnrq.SetNetwork(n)
			sbnrq.SetParentRequestId(r.ID())

			rs, re := n.Forward(ctx, sbnrq)
			if re != nil {
				mu.Lock()
				errs = append(errs, re)
				mu.Unlock()
				return
			}

			jrr, err := rs.JsonRpcResponse(ctx)
			if err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
				return
			}

			if jrr == nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("unexpected empty json-rpc response %v", rs))
				mu.Unlock()
				return
			}

			if jrr.Error != nil {
				mu.Lock()
				errs = append(errs, jrr.Error)
				mu.Unlock()
				return
			}

			mu.Lock()
			responses = append(responses, jrr)
			mu.Unlock()
		}(sr)
	}
	wg.Wait()

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	mergedResponse := mergeEthGetLogsResults(responses)
	jrq, _ := r.JsonRpcRequest()
	err := mergedResponse.SetID(jrq.ID)
	if err != nil {
		return nil, err
	}

	return mergedResponse, nil
}

func mergeEthGetLogsResults(responses []*common.JsonRpcResponse) *common.JsonRpcResponse {
	writer := NewGetLogsMultiResponseWriter(responses)
	jrr := &common.JsonRpcResponse{}
	jrr.SetResultWriter(writer)
	return jrr
}
