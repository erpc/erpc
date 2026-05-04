package evm

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// TraceFilterMethods lists the JSON-RPC method names this file handles.
// trace_filter is the OpenEthereum/Erigon/Reth/Nethermind spelling.
// arbtrace_filter is the Arbitrum Nova spelling with identical semantics.
var TraceFilterMethods = []string{"trace_filter", "arbtrace_filter"}

// isTraceFilterMethod returns true if the given method is a trace_filter variant.
func isTraceFilterMethod(method string) bool {
	m := strings.ToLower(method)
	for _, tfm := range TraceFilterMethods {
		if m == tfm {
			return true
		}
	}
	return false
}

// BuildTraceFilterRequest builds a trace_filter or arbtrace_filter JSON-RPC request.
// method must be one of the values in TraceFilterMethods.
func BuildTraceFilterRequest(method string, fromBlock, toBlock int64, fromAddress, toAddress interface{}) (*common.JsonRpcRequest, error) {
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
	if fromAddress != nil {
		filter["fromAddress"] = fromAddress
	}
	if toAddress != nil {
		filter["toAddress"] = toAddress
	}
	jrq := common.NewJsonRpcRequest(method, []interface{}{filter})
	if err := jrq.SetID(util.RandomID()); err != nil {
		return nil, err
	}
	return jrq, nil
}

// projectPreForward_trace_filter records the requested block-range size histogram
// before cache and upstream selection. It does not modify the request or
// short-circuit; always returns (false, nil, nil).
func projectPreForward_trace_filter(ctx context.Context, n common.Network, nq *common.NormalizedRequest) (handled bool, resp *common.NormalizedResponse, err error) {
	if nq == nil || n == nil {
		return false, nil, nil
	}
	method, err := nq.Method()
	if err != nil || !isTraceFilterMethod(method) {
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
	fbStr, _ := filter["fromBlock"].(string)
	tbStr, _ := filter["toBlock"].(string)
	jrq.RUnlock()

	// Reuse the getLogs tag resolver since trace_filter uses the same fromBlock/toBlock semantics.
	_, fromBlock := resolveBlockTagForGetLogs(ctx, n, fbStr)
	_, toBlock := resolveBlockTagForGetLogs(ctx, n, tbStr)

	if fromBlock > 0 && toBlock >= fromBlock {
		rangeSize := float64(toBlock - fromBlock + 1)
		finalityStr := nq.Finality(ctx).String()
		telemetry.MetricNetworkEvmTraceFilterRangeRequested.
			WithLabelValues(
				n.ProjectId(),
				n.Label(),
				strings.ToLower(method),
				nq.UserId(),
				finalityStr,
			).
			Observe(rangeSize)
	}
	return false, nil, nil
}

// networkPreForward_trace_filter performs network-level proactive splitting when the
// requested block range exceeds any upstream's TraceFilterAutoSplittingRangeThreshold.
// It must be called after upstreams have been selected for the request.
// Returns (handled=true) when it produced a merged response without contacting an upstream
// for the top-level request. Sub-requests flow through normal Network.Forward.
func networkPreForward_trace_filter(ctx context.Context, n common.Network, ups []common.Upstream, nrq *common.NormalizedRequest) (handled bool, resp *common.NormalizedResponse, err error) {
	if nrq == nil || n == nil {
		return false, nil, nil
	}

	// Avoid re-entrancy for derived sub-requests.
	if nrq.ParentRequestId() != nil || nrq.IsCompositeRequest() {
		return false, nil, nil
	}

	method, err := nrq.Method()
	if err != nil || !isTraceFilterMethod(method) {
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

	fbStr, _ := filter["fromBlock"].(string)
	tbStr, _ := filter["toBlock"].(string)
	jrq.RUnlock()

	// If either block can't be resolved to a number, pass through to upstream
	// and rely on availability/splitting hooks downstream.
	_, fromBlock := resolveBlockTagForGetLogs(ctx, n, fbStr)
	_, toBlock := resolveBlockTagForGetLogs(ctx, n, tbStr)
	if fromBlock == 0 || toBlock == 0 {
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

	requestRange := toBlock - fromBlock + 1

	// Compute effective auto-splitting threshold (min across upstreams).
	effectiveThreshold := int64(0)
	foundPositive := false
	for _, cu := range ups {
		if cu == nil || cu.Config() == nil || cu.Config().Evm == nil {
			continue
		}
		th := cu.Config().Evm.TraceFilterAutoSplittingRangeThreshold
		if th > 0 {
			if !foundPositive || th < effectiveThreshold || effectiveThreshold == 0 {
				effectiveThreshold = th
			}
			foundPositive = true
		}
	}

	if requestRange > 0 && effectiveThreshold > 0 && requestRange > effectiveThreshold {
		subRequests := make([]traceFilterSubRequest, 0)
		sb := fromBlock
		for sb <= toBlock {
			eb := min(sb+effectiveThreshold-1, toBlock)
			subRequests = append(subRequests, traceFilterSubRequest{
				method:      strings.ToLower(method),
				fromBlock:   sb,
				toBlock:     eb,
				fromAddress: filter["fromAddress"],
				toAddress:   filter["toAddress"],
			})
			sb = eb + 1
		}

		nrq.SetCompositeType(common.CompositeTypeTraceFilterSplitProactive)
		skipCache := ""
		if dirs := nrq.Directives(); dirs != nil {
			skipCache = dirs.SkipCacheRead
		}
		mergedResponse, fromCache, err := executeTraceFilterSubRequests(ctx, n, nrq, subRequests, skipCache)
		if err != nil {
			return true, nil, err
		}

		nrs := common.NewNormalizedResponse().WithRequest(nrq).WithJsonRpcResponse(mergedResponse).SetFromCache(fromCache)
		nrq.SetLastValidResponse(ctx, nrs)
		return true, nrs, nil
	}

	return false, nil, nil
}

// networkPostForward_trace_filter performs reactive splitting when an upstream
// returns a range-too-large error for a trace_filter/arbtrace_filter request.
func networkPostForward_trace_filter(ctx context.Context, n common.Network, rq *common.NormalizedRequest, rs *common.NormalizedResponse, re error) (*common.NormalizedResponse, error) {
	if re == nil {
		return rs, nil
	}
	ncfg := n.Config()
	if ncfg == nil || ncfg.Evm == nil || ncfg.Evm.TraceFilterSplitOnError == nil || !*ncfg.Evm.TraceFilterSplitOnError {
		return rs, re
	}
	if rq.ParentRequestId() != nil || rq.IsCompositeRequest() {
		return rs, re
	}

	method, mErr := rq.Method()
	if mErr != nil || !isTraceFilterMethod(method) {
		return rs, re
	}

	// Only split if the upstream signalled that the request was too large.
	isTooLarge := common.HasErrorCode(re, common.ErrCodeEndpointRequestTooLarge)
	if !isTooLarge {
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

	subs, err := splitTraceFilterRequest(rq)
	if err != nil || len(subs) == 0 {
		return rs, re
	}

	rq.SetCompositeType(common.CompositeTypeTraceFilterSplitOnError)
	skipCacheRead := ""
	if dirs := rq.Directives(); dirs != nil {
		skipCacheRead = dirs.SkipCacheRead
	}
	merged, fromCache, err := executeTraceFilterSubRequests(ctx, n, rq, subs, skipCacheRead)
	if err != nil {
		return rs, re
	}
	if rs != nil {
		rs.Release()
	}
	return common.NewNormalizedResponse().WithRequest(rq).WithJsonRpcResponse(merged).SetFromCache(fromCache), nil
}

// upstreamPreForward_trace_filter performs block range availability checking
// for trace_filter and arbtrace_filter methods.
// These methods have fromBlock/toBlock parameters similar to eth_getLogs and
// need dual-bound checking to ensure both bounds are available on the upstream.
func upstreamPreForward_trace_filter(ctx context.Context, n common.Network, u common.Upstream, nrq *common.NormalizedRequest) (handled bool, resp *common.NormalizedResponse, err error) {
	up, ok := u.(common.EvmUpstream)
	if !ok {
		n.Logger().Warn().Interface("upstream", u).Object("request", nrq).Msg("passed upstream is not a common.EvmUpstream")
		return false, nil, nil
	}

	ncfg := n.Config()
	// Reuse the same config as eth_getLogs since trace_filter has the same block range semantics
	if ncfg == nil ||
		ncfg.Evm == nil ||
		ncfg.Evm.Integrity == nil ||
		ncfg.Evm.Integrity.EnforceGetLogsBlockRange == nil ||
		!*ncfg.Evm.Integrity.EnforceGetLogsBlockRange {
		// If integrity check for block range is disabled, skip this hook.
		return false, nil, nil
	}

	method, _ := nrq.Method()
	ctx, span := common.StartDetailSpan(ctx, "Upstream.PreForwardHook.trace_filter", trace.WithAttributes(
		attribute.String("request.id", fmt.Sprintf("%v", nrq.ID())),
		attribute.String("network.id", n.Id()),
		attribute.String("upstream.id", up.Id()),
		attribute.String("method", method),
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

	if err := CheckBlockRangeAvailability(ctx, up, method, fromBlock, toBlock); err != nil {
		return true, nil, err
	}

	// Continue with the original forward flow
	return false, nil, nil
}

// upstreamPostForward_trace_filter normalizes emptyish results (e.g. `null`)
// into `[]`. Some upstreams return `null` when no traces match, which breaks
// consumers that decode the result as an array.
func upstreamPostForward_trace_filter(ctx context.Context, n common.Network, u common.Upstream, rq *common.NormalizedRequest, rs *common.NormalizedResponse, re error) (*common.NormalizedResponse, error) {
	ctx, span := common.StartDetailSpan(ctx, "Upstream.PostForwardHook.trace_filter", trace.WithAttributes(
		attribute.String("request.id", fmt.Sprintf("%v", rq.ID())),
		attribute.String("network.id", n.Id()),
		attribute.String("upstream.id", u.Id()),
	))
	defer span.End()

	if re == nil && rs != nil && rs.IsResultEmptyish(ctx) {
		return normalizeEmptyArrayResponse(ctx, u, rq, rs)
	}

	return rs, re
}

// traceFilterSubRequest captures the parameters needed to construct a split
// trace_filter/arbtrace_filter sub-request.
type traceFilterSubRequest struct {
	method      string // "trace_filter" or "arbtrace_filter"
	fromBlock   int64
	toBlock     int64
	fromAddress interface{}
	toAddress   interface{}
}

// splitTraceFilterRequest bisects the request along the first viable dimension:
// block range first (bisect in half), then fromAddress list, then toAddress list.
// Returns an error when no further split is possible (e.g. single block + single
// or empty address filter).
func splitTraceFilterRequest(r *common.NormalizedRequest) ([]traceFilterSubRequest, error) {
	method, mErr := r.Method()
	if mErr != nil || !isTraceFilterMethod(method) {
		return nil, fmt.Errorf("unsupported method: %s", method)
	}
	method = strings.ToLower(method)

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

	// Try splitting by block range first.
	blockRange := tb - fb + 1
	if blockRange > 1 {
		if n != nil {
			telemetry.MetricNetworkEvmTraceFilterForcedSplits.WithLabelValues(
				n.ProjectId(),
				n.Label(),
				method,
				"block_range",
				r.UserId(),
				r.AgentName(),
			).Inc()
		}
		mid := fb + (blockRange / 2)
		return []traceFilterSubRequest{
			{method: method, fromBlock: fb, toBlock: mid - 1, fromAddress: filter["fromAddress"], toAddress: filter["toAddress"]},
			{method: method, fromBlock: mid, toBlock: tb, fromAddress: filter["fromAddress"], toAddress: filter["toAddress"]},
		}, nil
	}

	// Single block: try splitting by fromAddress list.
	if addrs, ok := filter["fromAddress"].([]interface{}); ok && len(addrs) > 1 {
		mid := len(addrs) / 2
		if n != nil {
			telemetry.MetricNetworkEvmTraceFilterForcedSplits.WithLabelValues(
				n.ProjectId(),
				n.Label(),
				method,
				"from_address",
				r.UserId(),
				r.AgentName(),
			).Inc()
		}
		return []traceFilterSubRequest{
			{method: method, fromBlock: fb, toBlock: tb, fromAddress: addrs[:mid], toAddress: filter["toAddress"]},
			{method: method, fromBlock: fb, toBlock: tb, fromAddress: addrs[mid:], toAddress: filter["toAddress"]},
		}, nil
	}

	// Fall back to toAddress list.
	if addrs, ok := filter["toAddress"].([]interface{}); ok && len(addrs) > 1 {
		mid := len(addrs) / 2
		if n != nil {
			telemetry.MetricNetworkEvmTraceFilterForcedSplits.WithLabelValues(
				n.ProjectId(),
				n.Label(),
				method,
				"to_address",
				r.UserId(),
				r.AgentName(),
			).Inc()
		}
		return []traceFilterSubRequest{
			{method: method, fromBlock: fb, toBlock: tb, fromAddress: filter["fromAddress"], toAddress: addrs[:mid]},
			{method: method, fromBlock: fb, toBlock: tb, fromAddress: filter["fromAddress"], toAddress: addrs[mid:]},
		}, nil
	}

	return nil, fmt.Errorf("request cannot be split further")
}

// executeTraceFilterSubRequests dispatches split sub-requests concurrently,
// returning a merged JSON-RPC response. Sub-results are concatenated in request
// order; sub-ranges and sub-address-halves are disjoint by construction so no
// deduplication is required.
func executeTraceFilterSubRequests(ctx context.Context, n common.Network, r *common.NormalizedRequest, subRequests []traceFilterSubRequest, skipCacheRead string) (*common.JsonRpcResponse, bool, error) {
	origMethod, _ := r.Method()
	logger := n.Logger().With().Str("method", origMethod).Interface("id", r.ID()).Logger()

	wg := sync.WaitGroup{}
	responses := make([]*common.JsonRpcResponse, len(subRequests))
	fromCacheSr := make([]bool, len(subRequests))
	errs := make([]error, 0)
	mu := sync.Mutex{}

	concurrency := 10
	if cfg := n.Config(); cfg != nil && cfg.Evm != nil && cfg.Evm.TraceFilterSplitConcurrency > 0 {
		concurrency = cfg.Evm.TraceFilterSplitConcurrency
	}
	semaphore := make(chan struct{}, concurrency)

	recordFailure := func(method string, err error) {
		telemetry.CounterHandle(telemetry.MetricNetworkEvmTraceFilterSplitFailure,
			n.ProjectId(),
			n.Label(),
			method,
			r.UserId(),
			r.AgentName(),
		).Inc()
		errs = append(errs, err)
	}

	for idx, sr := range subRequests {
		wg.Add(1)
		semaphore <- struct{}{}
		go func(req traceFilterSubRequest, i int) {
			defer wg.Done()
			defer func() { <-semaphore }()

			srq, err := BuildTraceFilterRequest(req.method, req.fromBlock, req.toBlock, req.fromAddress, req.toAddress)
			logger.Debug().
				Object("request", srq).
				Msg("executing trace_filter sub-request")

			if err != nil {
				mu.Lock()
				recordFailure(req.method, err)
				mu.Unlock()
				return
			}

			sbnrq := common.NewNormalizedRequestFromJsonRpcRequest(srq)
			dr := r.Directives().Clone()
			dr.SkipCacheRead = skipCacheRead
			sbnrq.SetDirectives(dr)
			sbnrq.SetNetwork(n)
			sbnrq.SetParentRequestId(r.ID())
			sbnrq.CopyHttpContextFrom(r)

			rs, re := n.Forward(ctx, sbnrq)
			if re != nil {
				mu.Lock()
				recordFailure(req.method, re)
				mu.Unlock()
				return
			}

			jrr, err := rs.JsonRpcResponse(ctx)
			if err != nil {
				mu.Lock()
				recordFailure(req.method, err)
				mu.Unlock()
				rs.Release()
				return
			}

			if jrr == nil {
				mu.Lock()
				recordFailure(req.method, fmt.Errorf("unexpected empty json-rpc response %v", rs))
				mu.Unlock()
				rs.Release()
				return
			}

			if jrr.Error != nil {
				mu.Lock()
				recordFailure(req.method, jrr.Error)
				mu.Unlock()
				rs.Release()
				return
			}

			mu.Lock()
			telemetry.CounterHandle(telemetry.MetricNetworkEvmTraceFilterSplitSuccess,
				n.ProjectId(),
				n.Label(),
				req.method,
				r.UserId(),
				r.AgentName(),
			).Inc()
			jrrc, err := jrr.Clone()
			if err != nil {
				errs = append(errs, err)
				mu.Unlock()
				rs.Release()
				return
			}
			responses[i] = jrrc
			fromCacheSr[i] = rs.FromCache()
			mu.Unlock()
			rs.Release()
		}(sr, idx)
	}
	wg.Wait()

	if len(errs) > 0 {
		return nil, false, errors.Join(errs...)
	}

	// trace_filter results are disjoint arrays; reuse the concatenating writer.
	writer := NewGetLogsMultiResponseWriter(responses)
	merged := &common.JsonRpcResponse{}
	merged.SetResultWriter(writer)

	jrq, _ := r.JsonRpcRequest()
	if err := merged.SetID(jrq.ID); err != nil {
		return nil, false, err
	}

	return merged, !slices.Contains(fromCacheSr, false), nil
}
