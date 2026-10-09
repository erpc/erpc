package erpc

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	bdscommon "github.com/blockchain-data-standards/manifesto/common"
	"github.com/blockchain-data-standards/manifesto/evm"
	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// EvmQueryExecutor answers the MIP-16 eth_query* methods for one network, for
// both transports: gRPC QueryService streams and JSON-RPC eth_query* calls
// (one page per call). It resolves the block range once, checks the
// availability window, then tries the native QueryService upstreams in policy
// order and falls back to the query shim, which builds pages from standard
// JSON-RPC sub-requests sent through Network.Forward.
type EvmQueryExecutor struct {
	network         *Network
	logger          *zerolog.Logger
	parentRequestId interface{}
	// projectShimPages clears unselected fields on shim pages, for gRPC
	// clients; the JSON renderer applies the selection by itself.
	projectShimPages bool
}

func NewEvmQueryExecutor(network *Network, logger *zerolog.Logger) *EvmQueryExecutor {
	return &EvmQueryExecutor{network: network, logger: logger}
}

// errQueryPageDone is returned by an onPage callback that wants no further
// page. Execute then ends without an error.
var errQueryPageDone = errors.New("query page consumed")

// Shim budget defaults: a page scans at most this many blocks, for at most
// this long. Configurable per upstream with evm.queryShim.
const (
	defaultQueryShimMaxBlocksPerPage = 1000
	defaultQueryShimMaxPageDuration  = 10 * time.Second
)

// queryPlan is the resolved form of one eth_query* request.
type queryPlan struct {
	method string
	desc   bool
	// from and to are the resolved fromBlock and toBlock, in request terms:
	// in desc order from is the upper bound.
	from, to uint64
	// target is the target number of primary objects per page; 0 is none.
	target uint32
	budget queryShimBudget
}

type queryShimBudget struct {
	maxBlocks   int64
	maxDuration time.Duration
}

// next returns the block after n in traversal order.
func (p *queryPlan) next(n uint64) uint64 {
	if p.desc {
		return n - 1
	}
	return n + 1
}

// Execute runs req (one of the evm.Query*Request messages) and calls onPage
// for each page in order. onPage may return errQueryPageDone to stop after a
// page.
func (qe *EvmQueryExecutor) Execute(ctx context.Context, req proto.Message, onPage func(proto.Message) error) error {
	method := queryMethodFromProto(req)
	ctx, span := common.StartDetailSpan(ctx, "Query.Execute",
		trace.WithAttributes(attribute.String("query.method", method)),
	)
	defer span.End()

	err := qe.execute(ctx, method, req, onPage)
	if errors.Is(err, errQueryPageDone) {
		err = nil
	}
	if err != nil {
		common.SetTraceSpanError(span, err)
	}
	return err
}

func (qe *EvmQueryExecutor) execute(ctx context.Context, method string, req proto.Message, onPage func(proto.Message) error) error {
	if method == "" {
		return queryInvalidParams("unknown query request type %T", req)
	}
	natives := qe.nativeUpstreams(ctx, method)
	budget, shimEnabled := qe.shimBudget(ctx, method)
	if len(natives) == 0 && !shimEnabled {
		return queryMethodNotServed("no upstream serves %s: none has a native QueryService and none enables evm.queryShim for it", method)
	}

	plan, err := qe.newPlan(ctx, method, req, budget)
	if err != nil {
		return err
	}
	if err := qe.checkAvailability(ctx, plan, natives, shimEnabled); err != nil {
		return err
	}

	var nativeErr error
	for _, ups := range natives {
		err := qe.pipeThrough(ctx, ups, req, onPage)
		if err == nil {
			trace.SpanFromContext(ctx).SetAttributes(attribute.String("query.path", "native"), attribute.String("query.upstream", ups.Id()))
			return nil
		}
		var streamErr *StreamError
		if errors.As(err, &streamErr) {
			if streamErr.PageEmitted {
				// The client already has pages from this upstream: another
				// upstream cannot continue the same stream.
				return streamErr.Err
			}
			// The upstream's own error (a gRPC status with its MIP-16 code),
			// so both transports map it without looking through the wrapper.
			err = streamErr.Err
		}
		qe.logger.Debug().Err(err).Str("upstreamId", ups.Id()).Str("method", method).Msg("native query upstream failed before its first page, trying the next one")
		nativeErr = err
	}
	if !shimEnabled {
		return nativeErr
	}
	trace.SpanFromContext(ctx).SetAttributes(attribute.String("query.path", "shim"))
	return qe.runShim(ctx, plan, req, onPage)
}

// candidateUpstreams lists the network's upstreams in selection-policy order
// for method, or in registration order before the policy engine has run.
func (qe *EvmQueryExecutor) candidateUpstreams(ctx context.Context, method string) []common.Upstream {
	var ups []common.Upstream
	if qe.network.policyEngine != nil {
		// A query range spans finalized and unfinalized blocks, so it routes
		// through the wildcard finality slot.
		ups = qe.network.policyEngine.GetOrdered(qe.network.Id(), method, "*")
	}
	if len(ups) == 0 {
		for _, u := range qe.network.upstreamsRegistry.GetNetworkUpstreams(ctx, qe.network.Id()) {
			ups = append(ups, u)
		}
	}
	return ups
}

// nativeUpstreams returns the upstreams that answer bds.evm.QueryService
// themselves and accept method per their allow/ignore config.
func (qe *EvmQueryExecutor) nativeUpstreams(ctx context.Context, method string) []common.Upstream {
	var out []common.Upstream
	for _, u := range qe.candidateUpstreams(ctx, method) {
		if !servesNativeQuery(u) {
			continue
		}
		if ok, err := u.ShouldHandleMethod(method); err == nil && !ok {
			continue
		}
		out = append(out, u)
	}
	return out
}

// shimBudget reports whether any upstream enables the query shim for method,
// and the page budget: the smallest configured bound across those upstreams,
// else the defaults.
func (qe *EvmQueryExecutor) shimBudget(ctx context.Context, method string) (queryShimBudget, bool) {
	budget := queryShimBudget{maxBlocks: math.MaxInt64, maxDuration: time.Duration(math.MaxInt64)}
	enabled := false
	for _, u := range qe.network.upstreamsRegistry.GetNetworkUpstreams(ctx, qe.network.Id()) {
		cfg := u.Config()
		if cfg == nil || cfg.Evm == nil || !cfg.Evm.QueryShim.AllowsMethod(method) {
			continue
		}
		enabled = true
		if v := cfg.Evm.QueryShim.MaxBlocksPerPage; v > 0 && v < budget.maxBlocks {
			budget.maxBlocks = v
		}
		if v := cfg.Evm.QueryShim.MaxPageDuration.Duration(); v > 0 && v < budget.maxDuration {
			budget.maxDuration = v
		}
	}
	if budget.maxBlocks == math.MaxInt64 {
		budget.maxBlocks = defaultQueryShimMaxBlocksPerPage
	}
	if budget.maxDuration == time.Duration(math.MaxInt64) {
		budget.maxDuration = defaultQueryShimMaxPageDuration
	}
	return budget, enabled
}

// queryParams are the request fields every eth_query* method shares.
type queryParams struct {
	fromBlock, toBlock *string
	order              evm.SortOrder
	target             *uint32
}

func queryParamsOf(req proto.Message) queryParams {
	switch r := req.(type) {
	case *evm.QueryBlocksRequest:
		return queryParams{r.FromBlock, r.ToBlock, r.GetOrder(), r.Target}
	case *evm.QueryTransactionsRequest:
		return queryParams{r.FromBlock, r.ToBlock, r.GetOrder(), r.Target}
	case *evm.QueryLogsRequest:
		return queryParams{r.FromBlock, r.ToBlock, r.GetOrder(), r.Target}
	case *evm.QueryTracesRequest:
		return queryParams{r.FromBlock, r.ToBlock, r.GetOrder(), r.Target}
	case *evm.QueryTransfersRequest:
		return queryParams{r.FromBlock, r.ToBlock, r.GetOrder(), r.Target}
	}
	return queryParams{}
}

// newPlan resolves the block range of req. Omitted bounds take the MIP-16
// defaults for the order, and each tag resolves once, so two equal tags name
// the same block.
func (qe *EvmQueryExecutor) newPlan(ctx context.Context, method string, req proto.Message, budget queryShimBudget) (*queryPlan, error) {
	params := queryParamsOf(req)
	desc := params.order == evm.SortOrder_DESC
	if params.order != evm.SortOrder_ASC && !desc {
		return nil, queryInvalidParams("order must be asc or desc")
	}
	fromRef, toRef := "earliest", "latest"
	if desc {
		fromRef, toRef = "latest", "earliest"
	}
	if params.fromBlock != nil {
		fromRef = *params.fromBlock
	}
	if params.toBlock != nil {
		toRef = *params.toBlock
	}

	resolved := make(map[string]uint64, 2)
	resolve := func(key, ref string) (uint64, error) {
		if n, ok := resolved[ref]; ok {
			return n, nil
		}
		var head int64
		switch ref {
		case "earliest":
			return 0, nil
		case "latest":
			head = common.EvmHighestLatestBlockNumber(qe.network, ctx)
		case "finalized":
			head = common.EvmHighestFinalizedBlockNumber(qe.network, ctx)
		case "safe":
			if head = common.EvmHighestFinalizedBlockNumber(qe.network, ctx); head <= 0 {
				head = common.EvmHighestLatestBlockNumber(qe.network, ctx)
			}
		default:
			n, ok := parseQueryQuantity(ref)
			if !ok {
				return 0, queryInvalidParams(`%s must be a QUANTITY or one of "latest", "earliest", "safe", "finalized"`, key)
			}
			return n, nil
		}
		if head <= 0 {
			return 0, queryRangeUnavailable("the network has no known %q block yet", ref)
		}
		resolved[ref] = uint64(head)
		return uint64(head), nil
	}
	from, err := resolve("fromBlock", fromRef)
	if err != nil {
		return nil, err
	}
	to, err := resolve("toBlock", toRef)
	if err != nil {
		return nil, err
	}
	if err := evm.ValidateQueryRange(params.order, from, to); err != nil {
		return nil, err
	}

	plan := &queryPlan{
		method: method,
		desc:   desc,
		from:   from,
		to:     to,
		budget: budget,
	}
	if params.target != nil {
		if *params.target == 0 {
			return nil, queryInvalidParams("target must be a QUANTITY of at least 0x1")
		}
		plan.target = *params.target
	}
	trace.SpanFromContext(ctx).SetAttributes(
		attribute.String("query.fromBlock", fmt.Sprintf("%#x", from)),
		attribute.String("query.toBlock", fmt.Sprintf("%#x", to)),
		attribute.Bool("query.desc", desc),
	)
	return plan, nil
}

// parseQueryQuantity parses a canonical 64-bit QUANTITY.
func parseQueryQuantity(s string) (uint64, bool) {
	if len(s) < 3 || len(s) > 18 || s[0] != '0' || s[1] != 'x' || (s[2] == '0' && len(s) > 3) {
		return 0, false
	}
	n, err := strconv.ParseUint(s[2:], 16, 64)
	return n, err == nil
}

// queryShimSubMethods are the sub-requests the shim needs to build the
// primary objects of method; an upstream serves the shim for method when it
// accepts any of them.
func queryShimSubMethods(method string) []string {
	switch method {
	case "eth_queryLogs":
		return []string{"eth_getLogs"}
	case "eth_queryTraces", "eth_queryTransfers":
		return []string{"trace_block", "debug_traceBlockByNumber"}
	default:
		return []string{"eth_getBlockByNumber"}
	}
}

// checkAvailability fails with -32001 when a block of the resolved range is
// above the network's highest known block, or outside the union of the
// availability windows of the upstreams that can serve the method (natively
// or through the shim sub-requests).
func (qe *EvmQueryExecutor) checkAvailability(ctx context.Context, p *queryPlan, natives []common.Upstream, shimEnabled bool) error {
	lo, hi := p.from, p.to
	if p.desc {
		lo, hi = p.to, p.from
	}
	if latest := common.EvmHighestLatestBlockNumber(qe.network, ctx); latest > 0 && hi > uint64(latest) {
		return queryRangeUnavailable("block %#x is above the highest known block %#x", hi, latest)
	}

	type window struct{ lo, hi int64 }
	var windows []window
	add := func(u common.Upstream) {
		eu, ok := u.(common.EvmUpstream)
		if !ok {
			windows = append(windows, window{math.MinInt64, math.MaxInt64})
			return
		}
		minBound, maxBound := eu.EvmBlockAvailabilityBounds()
		windows = append(windows, window{minBound, maxBound})
	}
	for _, u := range natives {
		add(u)
	}
	if shimEnabled {
		subMethods := queryShimSubMethods(p.method)
		for _, u := range qe.network.upstreamsRegistry.GetNetworkUpstreams(ctx, qe.network.Id()) {
			for _, m := range subMethods {
				if ok, err := u.ShouldHandleMethod(m); err == nil && ok {
					add(u)
					break
				}
			}
		}
	}
	if len(windows) == 0 {
		return queryMethodNotServed("no upstream serves the sub-requests of %s", p.method)
	}

	// Walk the union of the windows upward from lo; a gap before hi is a block
	// no upstream can serve.
	sort.Slice(windows, func(i, j int) bool { return windows[i].lo < windows[j].lo })
	if hi > math.MaxInt64 {
		return queryRangeUnavailable("block %#x is outside the availability window", hi)
	}
	// lo <= hi <= MaxInt64, so both convert without overflow.
	covered, top := int64(lo&math.MaxInt64), int64(hi&math.MaxInt64) // first block not yet known to be covered; last block
	for _, w := range windows {
		if w.lo > covered {
			break
		}
		if w.hi >= covered {
			if w.hi >= top {
				return nil
			}
			covered = w.hi + 1
		}
	}
	return queryRangeUnavailable("block %#x is outside the availability window of every upstream for %s", covered, p.method)
}

// Errors. Each carries the MIP-16 code evm.QueryErrorCode reads, and the gRPC
// status code BaseError.ToGRPCStatus maps it to.

func queryInvalidParams(format string, args ...interface{}) error {
	return &evm.QueryParamsError{Message: fmt.Sprintf(format, args...)}
}

// queryRangeUnavailable is -32001 / OUT_OF_RANGE.
func queryRangeUnavailable(format string, args ...interface{}) error {
	return bdscommon.NewError(bdscommon.ErrorCode_RANGE_OUTSIDE_AVAILABLE, fmt.Sprintf(format, args...))
}

// queryMethodNotServed is -32004 / UNIMPLEMENTED.
func queryMethodNotServed(format string, args ...interface{}) error {
	return bdscommon.NewError(bdscommon.ErrorCode_UNSUPPORTED_METHOD, fmt.Sprintf(format, args...))
}

// queryBudgetExceeded is -32005 / RESOURCE_EXHAUSTED.
func queryBudgetExceeded(format string, args ...interface{}) error {
	return bdscommon.NewError(bdscommon.ErrorCode_RANGE_TOO_LARGE, fmt.Sprintf(format, args...))
}

// queryJsonRpcError converts an executor failure to the JSON-RPC error the
// HTTP server renders, with the exact MIP-16 code. Errors that carry no
// MIP-16 meaning (upstream failures, timeouts of sub-requests, auth) pass
// through unchanged for the usual erpc normalization.
func queryJsonRpcError(err error) error {
	if err == nil {
		return nil
	}
	code, message := 0, err.Error()
	var paramsErr *evm.QueryParamsError
	var baseErr *bdscommon.BaseError
	switch {
	case errors.As(err, &paramsErr):
		code = evm.JsonRpcCodeInvalidParams
	case errors.As(err, &baseErr):
		code = evm.QueryErrorCode(baseErr)
		message = baseErr.Message
	default:
		st, ok := status.FromError(err)
		if !ok {
			return err
		}
		message = st.Message()
		if code = evm.QueryErrorCode(err); code == evm.JsonRpcCodeInternalError {
			code = queryCodeOfGrpc(st.Code())
		}
	}
	return common.NewErrJsonRpcExceptionInternal(code, common.JsonRpcErrorNumber(code), message, err, nil)
}

// queryGrpcError converts an executor failure to the gRPC status of the
// MIP-16 condition, or returns nil when err carries none.
func queryGrpcError(err error) error {
	var paramsErr *evm.QueryParamsError
	var baseErr *bdscommon.BaseError
	switch {
	case errors.As(err, &paramsErr):
		return status.Error(codes.InvalidArgument, paramsErr.Error())
	case errors.As(err, &baseErr):
		return baseErr.ToGRPCStatus().Err()
	}
	if st, ok := status.FromError(err); ok {
		return st.Err()
	}
	return nil
}

func queryCodeOfGrpc(code codes.Code) int {
	switch code {
	case codes.InvalidArgument:
		return evm.JsonRpcCodeInvalidParams
	case codes.OutOfRange, codes.NotFound:
		return evm.JsonRpcCodeResourceNotFound
	case codes.Unimplemented:
		return evm.JsonRpcCodeMethodNotSupported
	case codes.ResourceExhausted, codes.DeadlineExceeded:
		return evm.JsonRpcCodeLimitExceeded
	}
	return evm.JsonRpcCodeInternalError
}

// JSON-RPC entry

// canonicalQueryMethod returns the canonical name of an eth_query* method,
// matched case-insensitively, or "" for any other method.
func canonicalQueryMethod(method string) string {
	switch strings.ToLower(method) {
	case "eth_queryblocks":
		return "eth_queryBlocks"
	case "eth_querytransactions":
		return "eth_queryTransactions"
	case "eth_querylogs":
		return "eth_queryLogs"
	case "eth_querytraces":
		return "eth_queryTraces"
	case "eth_querytransfers":
		return "eth_queryTransfers"
	}
	return ""
}

func parseQueryJsonRpc(method string, chainID uint64, params []byte) (proto.Message, error) {
	switch method {
	case "eth_queryBlocks":
		return evm.QueryBlocksRequestFromJsonRpc(chainID, params)
	case "eth_queryTransactions":
		return evm.QueryTransactionsRequestFromJsonRpc(chainID, params)
	case "eth_queryLogs":
		return evm.QueryLogsRequestFromJsonRpc(chainID, params)
	case "eth_queryTraces":
		return evm.QueryTracesRequestFromJsonRpc(chainID, params)
	case "eth_queryTransfers":
		return evm.QueryTransfersRequestFromJsonRpc(chainID, params)
	}
	return nil, queryInvalidParams("unknown query method %s", method)
}

func renderQueryJsonRpc(chainID uint64, req, page proto.Message) map[string]interface{} {
	switch r := req.(type) {
	case *evm.QueryBlocksRequest:
		return evm.QueryBlocksResponseToJsonRpc(chainID, r, page.(*evm.QueryBlocksResponse))
	case *evm.QueryTransactionsRequest:
		return evm.QueryTransactionsResponseToJsonRpc(chainID, r, page.(*evm.QueryTransactionsResponse))
	case *evm.QueryLogsRequest:
		return evm.QueryLogsResponseToJsonRpc(chainID, r, page.(*evm.QueryLogsResponse))
	case *evm.QueryTracesRequest:
		return evm.QueryTracesResponseToJsonRpc(chainID, r, page.(*evm.QueryTracesResponse))
	case *evm.QueryTransfersRequest:
		return evm.QueryTransfersResponseToJsonRpc(chainID, r, page.(*evm.QueryTransfersResponse))
	}
	return nil
}

// forwardQuery answers a JSON-RPC eth_query* request with one page.
//
// Network.Forward calls it after the request passed what every JSON request
// passes (project auth, rate limits and allow/ignore checks in the HTTP
// server, static responses) and after upstream selection, in place of the
// per-upstream dispatch: the executor routes by itself, to native
// QueryService upstreams or to shim sub-requests that go through
// Network.Forward on their own. The project then runs HandleNetworkPostForward
// and records metrics on the result like any response.
func (n *Network) forwardQuery(ctx context.Context, req *common.NormalizedRequest, method string) (*common.NormalizedResponse, error) {
	ctx, span := common.StartDetailSpan(ctx, "Network.ForwardQuery")
	defer span.End()

	if err := n.acquireRateLimitPermit(ctx, req); err != nil {
		return nil, err
	}
	jrq, err := req.JsonRpcRequest(ctx)
	if err != nil {
		return nil, err
	}
	jrq.RLock()
	params, err := common.SonicCfg.Marshal(jrq.Params)
	jrq.RUnlock()
	if err != nil {
		return nil, err
	}
	chainID := uint64(n.cfg.Evm.ChainId)
	queryReq, err := parseQueryJsonRpc(method, chainID, params)
	if err != nil {
		return nil, queryJsonRpcError(err)
	}

	lg := n.logger.With().Str("method", method).Interface("id", req.ID()).Logger()
	executor := NewEvmQueryExecutor(n, &lg)
	executor.parentRequestId = req.ID()
	var page proto.Message
	err = executor.Execute(ctx, queryReq, func(p proto.Message) error {
		page = p
		return errQueryPageDone
	})
	if err != nil {
		common.SetTraceSpanError(span, err)
		return nil, queryJsonRpcError(err)
	}
	if page == nil {
		return nil, queryJsonRpcError(fmt.Errorf("%s produced no page", method))
	}
	jrr, err := common.NewJsonRpcResponse(req.ID(), renderQueryJsonRpc(chainID, queryReq, page), nil)
	if err != nil {
		return nil, err
	}
	return common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr), nil
}
