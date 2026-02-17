package evm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// resolveBlockTagForGetLogs attempts to resolve a block tag (like "latest", "finalized")
// to a hex block number string and its int64 value. If the value is already a hex number,
// it parses and returns it. If the tag cannot be resolved (e.g., "safe", "pending", or
// network state not available), it returns empty string and 0.
// This allows eth_getLogs hooks to validate block ranges even when tags are used.
func resolveBlockTagForGetLogs(ctx context.Context, network common.Network, blockValue string) (hexStr string, blockNum int64) {
	if blockValue == "" {
		return "", 0
	}

	// If already a hex number, parse and return it
	if strings.HasPrefix(blockValue, "0x") {
		bn, err := strconv.ParseInt(blockValue, 0, 64)
		if err != nil {
			return "", 0
		}
		return blockValue, bn
	}

	// Try to resolve block tags using the network's state poller
	if resolved, ok := resolveBlockTagToHex(ctx, network, blockValue); ok {
		bn, err := common.HexToInt64(resolved)
		if err != nil {
			return "", 0
		}
		return resolved, bn
	}

	// Tag could not be resolved (e.g., "safe", "pending", "earliest", or no state available)
	return "", 0
}

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
	// Extract block values (may be hex or tags like "latest")
	fbStr, _ := filter["fromBlock"].(string)
	tbStr, _ := filter["toBlock"].(string)
	jrq.RUnlock()

	// Resolve block tags to numbers for metrics (hex or tags like "latest", "finalized")
	fbHex, fromBlock := resolveBlockTagForGetLogs(ctx, n, fbStr)
	tbHex, toBlock := resolveBlockTagForGetLogs(ctx, n, tbStr)

	// Only observe when both ends are numeric/resolved (including block 0).
	if fbHex != "" && tbHex != "" && toBlock >= fromBlock {
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

	fbStr, _ := filter["fromBlock"].(string)
	tbStr, _ := filter["toBlock"].(string)

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

	// Resolve block tags (like "latest", "finalized") to hex numbers for validation.
	// If tags cannot be resolved (e.g., "safe", "pending", or no state available),
	// pass through to upstream without block range validation.
	fbHex, fromBlock := resolveBlockTagForGetLogs(ctx, n, fbStr)
	tbHex, toBlock := resolveBlockTagForGetLogs(ctx, n, tbStr)

	// If either block couldn't be resolved to a number, skip validation and pass to upstream
	if fbHex == "" || tbHex == "" {
		return false, nil, nil
	}

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

	skipCacheRead := false
	if dirs := nrq.Directives(); dirs != nil {
		skipCacheRead = dirs.SkipCacheRead
	}

	// Cache-aware chunking: split into deterministic ranges to maximize cache hits.
	//
	// Note: we apply chunking even when SkipCacheRead=true. That directive only means
	// "don't read from cache" (force upstream). Chunking still provides deterministic
	// keys and caps per-upstream response sizes / latencies.
	chunkSize := int64(0)
	if ncfg.Evm.GetLogsCacheChunkSize != nil {
		chunkSize = *ncfg.Evm.GetLogsCacheChunkSize
	}
	if requestRange > 0 && chunkSize > 0 && requestRange > chunkSize {
		subRequests := make([]ethGetLogsSubRequest, 0)
		// Align first chunk start to chunkSize boundary for deterministic cache keys
		// e.g. with chunkSize=1000, fromBlock=1500 -> first chunk starts at 1500, ends at 1999
		sb := fromBlock
		for sb <= toBlock {
			// Compute aligned chunk end: next boundary - 1, clamped to toBlock
			alignedEnd := sb - (sb % chunkSize) + chunkSize - 1
			eb := min(alignedEnd, toBlock)
			subRequests = append(subRequests, ethGetLogsSubRequest{
				fromBlock: sb,
				toBlock:   eb,
				address:   filter["address"],
				topics:    filter["topics"],
			})
			sb = eb + 1
		}

		nrq.SetCompositeType(common.CompositeTypeLogsCacheChunk)
		chunkConc := 0
		if ncfg.Evm != nil {
			chunkConc = ncfg.Evm.GetLogsCacheChunkConcurrency
		}
		mergedResponse, meta, err := executeGetLogsSubRequests(ctx, n, nrq, subRequests, skipCacheRead, chunkConc)
		if err != nil {
			return true, nil, err
		}

		nrs := common.NewNormalizedResponse().WithRequest(nrq).WithJsonRpcResponse(mergedResponse)
		if meta != nil && meta.allFromCache {
			nrs.SetFromCache(true)
			if meta.oldestCacheAt > 0 {
				nrs.SetCacheStoredAtUnix(meta.oldestCacheAt)
			}
		}
		nrq.SetLastValidResponse(ctx, nrs)
		return true, nrs, nil
	}

	// Compute effective auto-splitting threshold (min across upstreams)
	effectiveThreshold := int64(0)
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
		splitConc := 0
		if ncfg.Evm != nil {
			splitConc = ncfg.Evm.GetLogsSplitConcurrency
		}
		mergedResponse, meta, err := executeGetLogsSubRequests(ctx, n, nrq, subRequests, skipCacheRead, splitConc)
		if err != nil {
			return true, nil, err
		}

		nrs := common.NewNormalizedResponse().WithRequest(nrq).WithJsonRpcResponse(mergedResponse)
		if meta != nil && meta.allFromCache {
			nrs.SetFromCache(true)
			if meta.oldestCacheAt > 0 {
				nrs.SetCacheStoredAtUnix(meta.oldestCacheAt)
			}
		}
		nrq.SetLastValidResponse(ctx, nrs)
		return true, nrs, nil
	}

	return false, nil, nil
}

func upstreamPreForward_eth_getLogs(ctx context.Context, n common.Network, u common.Upstream, nrq *common.NormalizedRequest) (handled bool, resp *common.NormalizedResponse, err error) {
	up, ok := u.(common.EvmUpstream)
	if !ok {
		n.Logger().Warn().Interface("upstream", u).Object("request", nrq).Msg("passed upstream is not a common.EvmUpstream")
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

	fb, _ := filter["fromBlock"].(string)
	tb, _ := filter["toBlock"].(string)
	jrq.RUnlock()

	// Resolve block tags (like "latest", "finalized") to hex numbers for validation.
	// If tags cannot be resolved (e.g., "safe", "pending", or no state available),
	// pass through to upstream without block range validation.
	_, fromBlock := resolveBlockTagForGetLogs(ctx, n, fb)
	_, toBlock := resolveBlockTagForGetLogs(ctx, n, tb)

	// If either block couldn't be resolved to a number, skip validation and pass to upstream
	if fromBlock == 0 || toBlock == 0 {
		return false, nil, nil
	}

	if fromBlock > toBlock {
		return true, nil, common.NewErrInvalidRequest(
			errors.New("fromBlock (" + strconv.FormatInt(fromBlock, 10) + ") must be less than or equal to toBlock (" + strconv.FormatInt(toBlock, 10) + ")"),
		)
	}

	if err := CheckBlockRangeAvailability(ctx, up, "eth_getLogs", fromBlock, toBlock); err != nil {
		return true, nil, err
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
	// Allow recursive splitting for derived sub-requests (ParentRequestId != nil).
	// This is required when an initial split (by range) still returns responses that are too large or time out.
	// Only block re-entrancy on already-composite requests.
	if rq.IsCompositeRequest() {
		return rs, re
	}

	// Only if upstream complained about large requests, split
	// e.g. if our own eth_getLogs hook complained about large range, do NOT try to split
	splitWorthy := false
	// "Too large" (413-like).
	if common.HasErrorCode(re, common.ErrCodeEndpointRequestTooLarge) {
		splitWorthy = true
	} else {
		// Also accept JsonRpcExceptionInternal with normalized code EvmLargeRange (-32012)
		var jre *common.ErrJsonRpcExceptionInternal
		if errors.As(re, &jre) && jre.NormalizedCode() == common.JsonRpcErrorEvmLargeRange {
			splitWorthy = true
		}
	}
	// Timeout: observed as client-side viem timeouts, or upstream endpoint timeouts.
	// Splitting helps reduce per-call latency and response sizes.
	if !splitWorthy && common.HasErrorCode(re, common.ErrCodeEndpointRequestTimeout, common.ErrCodeNetworkRequestTimeout, common.ErrCodeFailsafeTimeoutExceeded) {
		splitWorthy = true
	}
	if !splitWorthy && errors.Is(re, context.DeadlineExceeded) {
		splitWorthy = true
	}
	if !splitWorthy {
		return rs, re
	}

	// Split by range/addresses/topics
	subs, err := splitEthGetLogsRequest(rq)
	if err != nil || len(subs) == 0 {
		return rs, re
	}

	rq.SetCompositeType(common.CompositeTypeLogsSplitOnError)
	skipCacheRead := false
	if dirs := rq.Directives(); dirs != nil {
		skipCacheRead = dirs.SkipCacheRead
	}
	splitConc := 0
	if ncfg != nil && ncfg.Evm != nil {
		splitConc = ncfg.Evm.GetLogsSplitConcurrency
	}
	merged, meta, err := executeGetLogsSubRequests(ctx, n, rq, subs, skipCacheRead, splitConc)
	if err != nil {
		return rs, re
	}
	if rs != nil {
		rs.Release()
	}
	nrs := common.NewNormalizedResponse().WithRequest(rq).WithJsonRpcResponse(merged)
	if meta != nil && meta.allFromCache {
		nrs.SetFromCache(true)
		if meta.oldestCacheAt > 0 {
			nrs.SetCacheStoredAtUnix(meta.oldestCacheAt)
		}
	}
	return nrs, nil
}

type GetLogsMultiResponseWriter struct {
	mu        sync.RWMutex
	responses []*common.JsonRpcResponse
	holders   []*common.NormalizedResponse
	released  bool
}

func NewGetLogsMultiResponseWriter(responses []*common.JsonRpcResponse, holders []*common.NormalizedResponse) *GetLogsMultiResponseWriter {
	return &GetLogsMultiResponseWriter{
		responses: responses,
		holders:   holders,
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
	for _, h := range g.holders {
		if h != nil {
			h.Release()
		}
	}
	g.responses = nil
	g.holders = nil
	g.released = true
}

type ethGetLogsSubRequest struct {
	fromBlock int64
	toBlock   int64
	address   interface{}
	topics    interface{}
}

func shouldSplitEthGetLogsOnError(err error) bool {
	if err == nil {
		return false
	}

	// "Too large" (413-like).
	if common.HasErrorCode(err, common.ErrCodeEndpointRequestTooLarge) {
		return true
	}

	// Normalized JSON-RPC large-range.
	var jre *common.ErrJsonRpcExceptionInternal
	if errors.As(err, &jre) && jre.NormalizedCode() == common.JsonRpcErrorEvmLargeRange {
		return true
	}

	// Timeouts: upstream endpoint, network, failsafe.
	if common.HasErrorCode(err, common.ErrCodeEndpointRequestTimeout, common.ErrCodeNetworkRequestTimeout, common.ErrCodeFailsafeTimeoutExceeded) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	return false
}

func splitEthGetLogsSubRequest(sr ethGetLogsSubRequest) []ethGetLogsSubRequest {
	// First try splitting by block range
	blockRange := sr.toBlock - sr.fromBlock + 1
	if blockRange > 1 {
		mid := sr.fromBlock + (blockRange / 2)
		return []ethGetLogsSubRequest{
			{fromBlock: sr.fromBlock, toBlock: mid - 1, address: sr.address, topics: sr.topics},
			{fromBlock: mid, toBlock: sr.toBlock, address: sr.address, topics: sr.topics},
		}
	}

	// If single block, try splitting by address
	if addresses, ok := sr.address.([]interface{}); ok && len(addresses) > 1 {
		mid := len(addresses) / 2
		return []ethGetLogsSubRequest{
			{fromBlock: sr.fromBlock, toBlock: sr.toBlock, address: addresses[:mid], topics: sr.topics},
			{fromBlock: sr.fromBlock, toBlock: sr.toBlock, address: addresses[mid:], topics: sr.topics},
		}
	}

	// If single block and single address, try splitting topics[0] if it's an array
	if topics, ok := sr.topics.([]interface{}); ok && len(topics) > 0 {
		if t0arr, ok := topics[0].([]interface{}); ok && len(t0arr) > 1 {
			mid := len(t0arr) / 2
			leftTopics := make([]interface{}, len(topics))
			rightTopics := make([]interface{}, len(topics))
			copy(leftTopics, topics)
			copy(rightTopics, topics)
			leftTopics[0] = append([]interface{}(nil), t0arr[:mid]...)
			rightTopics[0] = append([]interface{}(nil), t0arr[mid:]...)
			return []ethGetLogsSubRequest{
				{fromBlock: sr.fromBlock, toBlock: sr.toBlock, address: sr.address, topics: leftTopics},
				{fromBlock: sr.fromBlock, toBlock: sr.toBlock, address: sr.address, topics: rightTopics},
			}
		}
	}

	return nil
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

// getLogsMergeMeta captures cache metadata for merged eth_getLogs results.
type getLogsMergeMeta struct {
	allFromCache  bool
	oldestCacheAt int64
}

func pickGetLogsSubRequestError(errs []error) error {
	for _, err := range errs {
		if err == nil {
			continue
		}
		jre := &common.ErrJsonRpcExceptionExternal{}
		if errors.As(err, &jre) {
			return jre
		}
	}
	for _, err := range errs {
		if err == nil {
			continue
		}
		var se common.StandardError
		if errors.As(err, &se) {
			return se
		}
	}
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return fmt.Errorf("unknown getLogs sub-request error")
}

func executeGetLogsSubRequests(ctx context.Context, n common.Network, r *common.NormalizedRequest, subRequests []ethGetLogsSubRequest, skipCacheRead bool, concurrency int) (*common.JsonRpcResponse, *getLogsMergeMeta, error) {
	return executeGetLogsSubRequestsInternal(ctx, n, r, subRequests, skipCacheRead, concurrency, 0)
}

func executeGetLogsSubRequestsInternal(ctx context.Context, n common.Network, r *common.NormalizedRequest, subRequests []ethGetLogsSubRequest, skipCacheRead bool, concurrency int, depth int) (*common.JsonRpcResponse, *getLogsMergeMeta, error) {
	logger := n.Logger().With().Str("method", "eth_getLogs").Interface("id", r.ID()).Logger()

	wg := sync.WaitGroup{}
	// Preserve sub-request order for deterministic merged output
	responses := make([]*common.JsonRpcResponse, len(subRequests))
	holders := make([]*common.NormalizedResponse, len(subRequests))
	fromCacheFlags := make([]bool, len(subRequests))
	cacheAts := make([]int64, len(subRequests))
	errs := make([]error, 0)
	mu := sync.Mutex{}

	// Concurrency is passed by caller (cache chunking vs split-on-error differ).
	if concurrency <= 0 {
		concurrency = 10
	}
	if concurrency > len(subRequests) {
		concurrency = len(subRequests)
	}
	if concurrency < 1 {
		concurrency = 1
	}
	semaphore := make(chan struct{}, concurrency)
	loopCtxErr := error(nil)
	const maxSplitDepth = 16
loop:
	for idx, sr := range subRequests {
		wg.Add(1)
		// Acquire semaphore token (blocks if at capacity)
		select {
		case semaphore <- struct{}{}:
		case <-ctx.Done():
			wg.Done()
			loopCtxErr = ctx.Err()
			break loop
		}

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

			// Bound per-sub-request wall time so slow upstream getLogs calls trigger split-and-retry
			// rather than letting the top-level client timeout.
			subTimeout := 10 * time.Second
			if dl, ok := ctx.Deadline(); ok {
				rem := time.Until(dl)
				if rem > 0 && rem < subTimeout {
					subTimeout = rem
				}
			}
			subCtx, cancel := context.WithTimeout(ctx, subTimeout)
			rs, re := n.Forward(subCtx, sbnrq)
			cancel()
			if re != nil {
				// If a sub-request timed out or was too large, try splitting it further.
				// This prevents a single slow/huge chunk from failing the entire merge.
				if depth < maxSplitDepth && shouldSplitEthGetLogsOnError(re) {
					subSubs := splitEthGetLogsSubRequest(req)
					if len(subSubs) > 0 {
						subJrr, subMeta, subErr := executeGetLogsSubRequestsInternal(ctx, n, r, subSubs, skipCacheRead, concurrency, depth+1)
						if subErr == nil {
							responses[i] = subJrr
							holders[i] = nil
							if subMeta != nil {
								fromCacheFlags[i] = subMeta.allFromCache
								cacheAts[i] = subMeta.oldestCacheAt
							} else {
								fromCacheFlags[i] = false
								cacheAts[i] = 0
							}
							return
						}
						re = subErr
					}
				}
				mu.Lock()
				telemetry.CounterHandle(telemetry.MetricNetworkEvmGetLogsSplitFailure,
					n.ProjectId(),
					n.Label(),
					r.UserId(),
					r.AgentName(),
				).Inc()
				errs = append(errs, fmt.Errorf("sub-request [%d-%d] forward failed: %w", req.fromBlock, req.toBlock, re))
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
				// If the upstream returned a split-worthy JSON-RPC error, try splitting and retrying.
				if depth < maxSplitDepth && shouldSplitEthGetLogsOnError(jrr.Error) {
					subSubs := splitEthGetLogsSubRequest(req)
					if len(subSubs) > 0 {
						rs.Release() // no longer needed; we'll return a merged sub-response
						subJrr, subMeta, subErr := executeGetLogsSubRequestsInternal(ctx, n, r, subSubs, skipCacheRead, concurrency, depth+1)
						if subErr == nil {
							responses[i] = subJrr
							holders[i] = nil
							if subMeta != nil {
								fromCacheFlags[i] = subMeta.allFromCache
								cacheAts[i] = subMeta.oldestCacheAt
							} else {
								fromCacheFlags[i] = false
								cacheAts[i] = 0
							}
							return
						}
						// If split execution failed, record the split failure and stop.
						mu.Lock()
						telemetry.CounterHandle(telemetry.MetricNetworkEvmGetLogsSplitFailure,
							n.ProjectId(),
							n.Label(),
							r.UserId(),
							r.AgentName(),
						).Inc()
						errs = append(errs, subErr)
						mu.Unlock()
						return
					}
				}
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

			telemetry.CounterHandle(telemetry.MetricNetworkEvmGetLogsSplitSuccess,
				n.ProjectId(),
				n.Label(),
				r.UserId(),
				r.AgentName(),
			).Inc()
			// Avoid deep clone amplification: JsonRpcResponse already copies parsed fields
			// out of the pooled parse buffer. Keep the sub-response alive until the merged
			// response is released, then free it via GetLogsMultiResponseWriter.Release.
			responses[i] = jrr
			holders[i] = rs
			fromCacheFlags[i] = rs.FromCache()
			cacheAts[i] = rs.CacheStoredAtUnix()
		}(sr, idx)
	}
	wg.Wait()

	if loopCtxErr != nil {
		mu.Lock()
		errs = append(errs, loopCtxErr)
		mu.Unlock()
	}
	if len(errs) > 0 {
		// Ensure we don't leak retained sub-responses if we fail mid-merge.
		for i := range holders {
			if holders[i] != nil {
				holders[i].Release()
				holders[i] = nil
			}
			if responses[i] != nil {
				responses[i].Free()
				responses[i] = nil
			}
		}
		return nil, nil, pickGetLogsSubRequestError(errs)
	}

	mergedResponse := mergeEthGetLogsResults(responses, holders)
	jrq, _ := r.JsonRpcRequest()
	err := mergedResponse.SetID(jrq.ID)
	if err != nil {
		return nil, nil, err
	}

	meta := &getLogsMergeMeta{allFromCache: true, oldestCacheAt: 0}
	for i := range fromCacheFlags {
		if !fromCacheFlags[i] {
			meta.allFromCache = false
		}
		if cacheAts[i] > 0 && (meta.oldestCacheAt == 0 || cacheAts[i] < meta.oldestCacheAt) {
			meta.oldestCacheAt = cacheAts[i]
		}
	}

	return mergedResponse, meta, nil
}

func mergeEthGetLogsResults(responses []*common.JsonRpcResponse, holders []*common.NormalizedResponse) *common.JsonRpcResponse {
	writer := NewGetLogsMultiResponseWriter(responses, holders)
	jrr := &common.JsonRpcResponse{}
	jrr.SetResultWriter(writer)
	return jrr
}
