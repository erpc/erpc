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

// projectPreForward_eth_getLogs records requested block-range size distribution
// at the project level before cache and upstream selection.
// It does not modify the request or short-circuit; always returns (false, nil, nil).
func projectPreForward_eth_getLogs(ctx context.Context, n common.Network, nq *common.NormalizedRequest) (handled bool, resp *common.NormalizedResponse, err error) {
	if nq == nil || n == nil {
		return false, nil, nil
	}
	jrq, err := nq.JsonRpcRequest(ctx)
	if err != nil || jrq == nil {
		return false, nil, nil
	}
	jrq.RLockWithTrace(ctx)
	if len(jrq.Params) < 1 {
		jrq.RUnlock()
		return false, nil, nil
	}
	filter, ok := jrq.Params[0].(map[string]interface{})
	if !ok {
		jrq.RUnlock()
		return false, nil, nil
	}
	// EIP-234 short-circuit (blockHash): no range size applicable
	if _, ok := filter["blockHash"].(string); ok {
		jrq.RUnlock()
		return false, nil, nil
	}
	// Extract numeric range if hex numbers
	var fromBlock, toBlock int64
	if v, ok := filter["fromBlock"].(string); ok && strings.HasPrefix(v, "0x") {
		if bn, e := strconv.ParseInt(v, 0, 64); e == nil {
			fromBlock = bn
		}
	}
	if v, ok := filter["toBlock"].(string); ok && strings.HasPrefix(v, "0x") {
		if bn, e := strconv.ParseInt(v, 0, 64); e == nil {
			toBlock = bn
		}
	}
	jrq.RUnlock()

	if fromBlock > 0 && toBlock >= fromBlock {
		rangeSize := float64(toBlock - fromBlock + 1)
		finalityStr := nq.Finality(ctx).String()
		telemetry.MetricNetworkEvmGetLogsRangeRequested.
			WithLabelValues(
				n.ProjectId(),
				n.Label(),
				"eth_getLogs",
				nq.UserId(),
				finalityStr,
			).
			Observe(rangeSize)
	}
	return false, nil, nil
}

// networkPreForward_eth_getLogs performs network-level validation and proactive splitting
// It must be called after upstreams have been selected for the request.
// It returns (handled=true) when it produced a merged response without contacting an upstream
// for the top-level request. Sub-requests will flow through normal Network.Forward.
func networkPreForward_eth_getLogs(ctx context.Context, n common.Network, ups []common.Upstream, nrq *common.NormalizedRequest) (handled bool, resp *common.NormalizedResponse, err error) {
	if nrq == nil || n == nil {
		return false, nil, nil
	}

	// Avoid re-entrancy for derived sub-requests
	if nrq.ParentRequestId() != nil || nrq.IsCompositeRequest() {
		return false, nil, nil
	}

	jrq, err := nrq.JsonRpcRequest(ctx)
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

	// EIP-234 short-circuit
	if bh, ok := filter["blockHash"].(string); ok && bh != "" {
		jrq.RUnlock()
		return false, nil, nil
	}

	fbStr, ok := filter["fromBlock"].(string)
	if !ok || !strings.HasPrefix(fbStr, "0x") {
		jrq.RUnlock()
		return false, nil, nil
	}
	tbStr, ok := filter["toBlock"].(string)
	if !ok || !strings.HasPrefix(tbStr, "0x") {
		jrq.RUnlock()
		return false, nil, nil
	}
	fromBlock, err := strconv.ParseInt(fbStr, 0, 64)
	if err != nil {
		jrq.RUnlock()
		return true, nil, err
	}
	toBlock, err := strconv.ParseInt(tbStr, 0, 64)
	if err != nil {
		jrq.RUnlock()
		return true, nil, err
	}

	// Capture address/topics counts while under read lock
	var addrCount, topicCount int64
	if addrs, ok := filter["address"].([]interface{}); ok {
		addrCount = int64(len(addrs))
	}
	// Only count topic0: if topics[0] is an array, count its length; if topics[0] is non-nil value, count as 1
	if tps, ok := filter["topics"].([]interface{}); ok && len(tps) > 0 {
		if t0arr, ok := tps[0].([]interface{}); ok {
			topicCount = int64(len(t0arr))
		} else if tps[0] != nil {
			topicCount = 1
		}
	}
	jrq.RUnlock()

	if fromBlock > toBlock {
		return true, nil, common.NewErrInvalidRequest(
			errors.New("fromBlock (" + strconv.FormatInt(fromBlock, 10) + ") must be less than or equal to toBlock (" + strconv.FormatInt(toBlock, 10) + ")"),
		)
	}

	ncfg := n.Config()
	if ncfg == nil || ncfg.Evm == nil {
		return false, nil, nil
	}

	// Enforce network-level hard limits first
	requestRange := toBlock - fromBlock + 1
	if maxRange := ncfg.Evm.GetLogsMaxAllowedRange; maxRange > 0 && requestRange > maxRange {
		return true, nil, common.NewErrGetLogsExceededMaxAllowedRange(requestRange, maxRange)
	}
	if maxAddrs := ncfg.Evm.GetLogsMaxAllowedAddresses; maxAddrs > 0 && addrCount > maxAddrs {
		return true, nil, common.NewErrGetLogsExceededMaxAllowedAddresses(addrCount, maxAddrs)
	}
	if maxTopics := ncfg.Evm.GetLogsMaxAllowedTopics; maxTopics > 0 && topicCount > maxTopics {
		return true, nil, common.NewErrGetLogsExceededMaxAllowedTopics(topicCount, maxTopics)
	}

	// Compute effective auto-splitting threshold (min across upstreams)
	effectiveThreshold := int64(5000)
	foundPositive := false
	for _, cu := range ups {
		if cu == nil || cu.Config() == nil || cu.Config().Evm == nil {
			continue
		}
		th := cu.Config().Evm.GetLogsAutoSplittingRangeThreshold
		if th > 0 {
			if !foundPositive || th < effectiveThreshold || effectiveThreshold == 0 {
				effectiveThreshold = th
			}
			foundPositive = true
		}
	}
	// If none configured positively, keep default (disabled). Then cap by network max allowed range.
	if maxRange := ncfg.Evm.GetLogsMaxAllowedRange; maxRange > 0 && effectiveThreshold > maxRange {
		effectiveThreshold = maxRange
	}

	// Proactive split if needed
	if requestRange > 0 && effectiveThreshold > 0 && requestRange > effectiveThreshold {
		// Build contiguous block sub-requests
		subRequests := make([]ethGetLogsSubRequest, 0)
		sb := fromBlock
		for sb <= toBlock {
			eb := min(sb+effectiveThreshold-1, toBlock)
			subRequests = append(subRequests, ethGetLogsSubRequest{
				fromBlock: sb,
				toBlock:   eb,
				address:   filter["address"],
				topics:    filter["topics"],
			})
			sb = eb + 1
		}

		nrq.SetCompositeType(common.CompositeTypeLogsSplitProactive)
		mergedResponse, err := executeGetLogsSubRequests(ctx, n, nrq, subRequests, nrq.Directives().SkipCacheRead)
		if err != nil {
			return true, nil, err
		}

		nrs := common.NewNormalizedResponse().WithRequest(nrq).WithJsonRpcResponse(mergedResponse)
		nrq.SetLastValidResponse(ctx, nrs)
		return true, nrs, nil
	}

	return false, nil, nil
}

func upstreamPreForward_eth_getLogs(ctx context.Context, n common.Network, u common.Upstream, nrq *common.NormalizedRequest) (handled bool, resp *common.NormalizedResponse, err error) {
	up, ok := u.(common.EvmUpstream)
	if !ok {
		log.Warn().Interface("upstream", u).Object("request", nrq).Msg("passed upstream is not a common.EvmUpstream")
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
		attribute.String("request.id", fmt.Sprintf("%v", nrq.ID())),
		attribute.String("network.id", n.Id()),
		attribute.String("upstream.id", up.Id()),
	))
	defer span.End()

	jrq, err := nrq.JsonRpcRequest(ctx)
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
		return true, nil, common.NewErrInvalidRequest(
			errors.New("fromBlock (" + strconv.FormatInt(fromBlock, 10) + ") must be less than or equal to toBlock (" + strconv.FormatInt(toBlock, 10) + ")"),
		)
	}

	statePoller := up.EvmStatePoller()
	if statePoller == nil || statePoller.IsObjectNull() {
		return true, nil, common.NewErrUpstreamInitialization(
			fmt.Errorf("upstream evm state poller is not available"),
			up.Id(),
		)
	}

	// Check if the upstream can handle the requested block range
	available, err := up.EvmAssertBlockAvailability(ctx, "eth_getLogs", common.AvailbilityConfidenceBlockHead, true, toBlock)
	if err != nil {
		return true, nil, err
	}
	if !available {
		latestBlock := statePoller.LatestBlock()
		finalizedBlock := statePoller.FinalizedBlock()

		return true, nil, common.NewErrEndpointMissingData(
			fmt.Errorf("block not found for eth_getLogs, requested toBlock %d is not available on the upstream node (latestBlock: %d, finalizedBlock: %d)", toBlock, latestBlock, finalizedBlock),
			up,
		)
	}
	available, err = up.EvmAssertBlockAvailability(ctx, "eth_getLogs", common.AvailbilityConfidenceBlockHead, false, fromBlock)
	if err != nil {
		return true, nil, err
	}
	if !available {
		latestBlock := statePoller.LatestBlock()
		finalizedBlock := statePoller.FinalizedBlock()

		return true, nil, common.NewErrEndpointMissingData(
			fmt.Errorf("block not found for eth_getLogs, fromBlock %d is not available on the upstream node (latestBlock: %d, finalizedBlock: %d)", fromBlock, latestBlock, finalizedBlock),
			up,
		)
	}

	// Continue with the original forward flow
	return false, nil, nil
}

func upstreamPostForward_eth_getLogs(ctx context.Context, n common.Network, u common.Upstream, rq *common.NormalizedRequest, rs *common.NormalizedResponse, re error) (*common.NormalizedResponse, error) {
	ctx, span := common.StartDetailSpan(ctx, "Upstream.PostForwardHook.eth_getLogs", trace.WithAttributes(
		attribute.String("request.id", fmt.Sprintf("%v", rq.ID())),
		attribute.String("network.id", n.Id()),
		attribute.String("upstream.id", u.Id()),
	))
	defer span.End()

	if re == nil && rs != nil && rs.IsResultEmptyish(ctx) {
		// This is to normalize empty logs responses (e.g. instead of returning "null")
		jrr, err := common.NewJsonRpcResponse(rq.ID(), []interface{}{}, nil)
		if err != nil {
			return nil, err
		}
		nnr := common.NewNormalizedResponse().WithRequest(rq).WithJsonRpcResponse(jrr)
		nnr.SetFromCache(rs.FromCache())
		nnr.SetEvmBlockRef(rs.EvmBlockRef())
		nnr.SetEvmBlockNumber(rs.EvmBlockNumber())
		nnr.SetAttempts(rs.Attempts())
		nnr.SetRetries(rs.Retries())
		nnr.SetHedges(rs.Hedges())
		nnr.SetUpstream(u)
		rq.SetLastValidResponse(ctx, nnr)
		// We replaced the original response with a normalized one; release the old instance
		rs.Release()
		return nnr, nil
	}

	return rs, re
}

// networkPostForward_eth_getLogs performs network-level error-based splitting on 413-like errors
func networkPostForward_eth_getLogs(ctx context.Context, n common.Network, rq *common.NormalizedRequest, rs *common.NormalizedResponse, re error) (*common.NormalizedResponse, error) {
	if re == nil {
		return rs, nil
	}
	ncfg := n.Config()
	if ncfg == nil || ncfg.Evm == nil || ncfg.Evm.GetLogsSplitOnError == nil || !*ncfg.Evm.GetLogsSplitOnError {
		return rs, re
	}
	if rq.ParentRequestId() != nil || rq.IsCompositeRequest() {
		return rs, re
	}

	// Only if upstream complained about large requests, split
	// e.g. if our own eth_getLogs hook complained about large range, do NOT try to split
	isTooLarge := common.HasErrorCode(re, common.ErrCodeEndpointRequestTooLarge)
	if !isTooLarge {
		// Also accept JsonRpcExceptionInternal with normalized code EvmLargeRange (-32012)
		var jre *common.ErrJsonRpcExceptionInternal
		if errors.As(re, &jre) {
			if jre.NormalizedCode() == common.JsonRpcErrorEvmLargeRange {
				isTooLarge = true
			}
		}
	}
	if !isTooLarge {
		return rs, re
	}

	// Split by range/addresses/topics
	subs, err := splitEthGetLogsRequest(rq)
	if err != nil || len(subs) == 0 {
		return rs, re
	}

	rq.SetCompositeType(common.CompositeTypeLogsSplitOnError)
	merged, err := executeGetLogsSubRequests(ctx, n, rq, subs, rq.Directives().SkipCacheRead)
	if err != nil {
		return rs, re
	}
	if rs != nil {
		rs.Release()
	}
	return common.NewNormalizedResponse().WithRequest(rq).WithJsonRpcResponse(merged), nil
}

type GetLogsMultiResponseWriter struct {
	mu        sync.RWMutex
	responses []*common.JsonRpcResponse
	released  bool
}

func NewGetLogsMultiResponseWriter(responses []*common.JsonRpcResponse) *GetLogsMultiResponseWriter {
	return &GetLogsMultiResponseWriter{
		responses: responses,
	}
}

func (g *GetLogsMultiResponseWriter) WriteTo(w io.Writer, trimSides bool) (n int64, err error) {
	g.mu.RLock()
	responses := g.responses
	g.mu.RUnlock()

	// Write opening bracket
	if !trimSides {
		nn, err := w.Write([]byte{'['})
		if err != nil {
			return int64(nn), err
		}
		n += int64(nn)
	}

	first := true
	for _, response := range responses {
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
		if err != nil {
			return n + int64(nn), err
		}
		n += int64(nn)
	}

	return n, nil
}

func (g *GetLogsMultiResponseWriter) IsResultEmptyish() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()

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

func (g *GetLogsMultiResponseWriter) Size(ctx ...context.Context) (int, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	size := 0
	for _, response := range g.responses {
		s, err := response.Size(ctx...)
		if err != nil {
			return 0, err
		}
		size += s
	}
	return size, nil
}

// Release frees memory retained by sub-responses.
// This method should be called when the writer is no longer needed.
func (g *GetLogsMultiResponseWriter) Release() {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.released {
		return
	}

	for _, r := range g.responses {
		if r != nil {
			r.Free()
		}
	}
	g.responses = nil
	g.released = true
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

	// First try splitting by block range
	blockRange := tb - fb + 1
	if blockRange > 1 {
		if n != nil {
			telemetry.MetricNetworkEvmGetLogsForcedSplits.WithLabelValues(
				n.ProjectId(),
				n.Label(),
				"block_range",
				r.UserId(),
				r.AgentName(),
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
		if n != nil {
			telemetry.MetricNetworkEvmGetLogsForcedSplits.WithLabelValues(
				n.ProjectId(),
				n.Label(),
				"addresses",
				r.UserId(),
				r.AgentName(),
			).Inc()
		}
		return []ethGetLogsSubRequest{
			{fromBlock: fb, toBlock: tb, address: addresses[:mid], topics: filter["topics"]},
			{fromBlock: fb, toBlock: tb, address: addresses[mid:], topics: filter["topics"]},
		}, nil
	}

	// If single address or no address, try splitting only topics[0] when it is an OR list
	if topics, ok := filter["topics"].([]interface{}); ok && len(topics) > 0 {
		if t0arr, ok := topics[0].([]interface{}); ok && len(t0arr) > 1 {
			mid := len(t0arr) / 2
			if n != nil {
				telemetry.MetricNetworkEvmGetLogsForcedSplits.WithLabelValues(
					n.ProjectId(),
					n.Label(),
					"topics0",
					r.UserId(),
					r.AgentName(),
				).Inc()
			}
			leftTopics := make([]interface{}, len(topics))
			copy(leftTopics, topics)
			rightTopics := make([]interface{}, len(topics))
			copy(rightTopics, topics)
			leftTopics[0] = append([]interface{}(nil), t0arr[:mid]...)
			rightTopics[0] = append([]interface{}(nil), t0arr[mid:]...)
			return []ethGetLogsSubRequest{
				{fromBlock: fb, toBlock: tb, address: filter["address"], topics: leftTopics},
				{fromBlock: fb, toBlock: tb, address: filter["address"], topics: rightTopics},
			}, nil
		}
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

func executeGetLogsSubRequests(ctx context.Context, n common.Network, r *common.NormalizedRequest, subRequests []ethGetLogsSubRequest, skipCacheRead bool) (*common.JsonRpcResponse, error) {
	logger := n.Logger().With().Str("method", "eth_getLogs").Interface("id", r.ID()).Logger()

	wg := sync.WaitGroup{}
	// Preserve sub-request order for deterministic merged output
	responses := make([]*common.JsonRpcResponse, len(subRequests))
	errs := make([]error, 0)
	mu := sync.Mutex{}

	// Use network-level concurrency configuration for split sub-requests
	concurrency := 10
	if cfg := n.Config(); cfg != nil && cfg.Evm != nil && cfg.Evm.GetLogsSplitConcurrency > 0 {
		concurrency = cfg.Evm.GetLogsSplitConcurrency
	}
	semaphore := make(chan struct{}, concurrency)
	for idx, sr := range subRequests {
		wg.Add(1)
		// Acquire semaphore token (blocks if at capacity)
		semaphore <- struct{}{}
		go func(req ethGetLogsSubRequest, i int) {
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
				telemetry.CounterHandle(telemetry.MetricNetworkEvmGetLogsSplitFailure,
					n.ProjectId(),
					n.Label(),
					r.UserId(),
					r.AgentName(),
				).Inc()
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

			// Copy HTTP context (headers, query parameters, user) for proper metrics tracking
			sbnrq.CopyHttpContextFrom(r)

			rs, re := n.Forward(ctx, sbnrq)
			if re != nil {
				mu.Lock()
				telemetry.CounterHandle(telemetry.MetricNetworkEvmGetLogsSplitFailure,
					n.ProjectId(),
					n.Label(),
					r.UserId(),
					r.AgentName(),
				).Inc()
				errs = append(errs, re)
				mu.Unlock()
				return
			}

			jrr, err := rs.JsonRpcResponse(ctx)
			if err != nil {
				mu.Lock()
				telemetry.CounterHandle(telemetry.MetricNetworkEvmGetLogsSplitFailure,
					n.ProjectId(),
					n.Label(),
					r.UserId(),
					r.AgentName(),
				).Inc()
				errs = append(errs, err)
				mu.Unlock()
				rs.Release()
				return
			}

			if jrr == nil {
				mu.Lock()
				telemetry.CounterHandle(telemetry.MetricNetworkEvmGetLogsSplitFailure,
					n.ProjectId(),
					n.Label(),
					r.UserId(),
					r.AgentName(),
				).Inc()
				errs = append(errs, fmt.Errorf("unexpected empty json-rpc response %v", rs))
				mu.Unlock()
				rs.Release()
				return
			}

			if jrr.Error != nil {
				mu.Lock()
				telemetry.CounterHandle(telemetry.MetricNetworkEvmGetLogsSplitFailure,
					n.ProjectId(),
					n.Label(),
					r.UserId(),
					r.AgentName(),
				).Inc()
				errs = append(errs, jrr.Error)
				mu.Unlock()
				rs.Release()
				return
			}

			mu.Lock()
			telemetry.CounterHandle(telemetry.MetricNetworkEvmGetLogsSplitSuccess,
				n.ProjectId(),
				n.Label(),
				r.UserId(),
				r.AgentName(),
			).Inc()
			jrrc, err := jrr.Clone()
			if err != nil {
				errs = append(errs, err)
				mu.Unlock()
				return
			}
			responses[i] = jrrc
			mu.Unlock()
			rs.Release()
		}(sr, idx)
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
