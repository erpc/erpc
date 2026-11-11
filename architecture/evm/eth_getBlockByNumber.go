package evm

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/indexer"
	"github.com/erpc/erpc/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// blockByTagFamily is spec §7 family 2, lower-cased: the methods that read
// one block by number or tag.
var blockByTagFamily = map[string]struct{}{
	"eth_getblockbynumber":                    {},
	"eth_getblockreceipts":                    {},
	"eth_getblocktransactioncountbynumber":    {},
	"eth_gettransactionbyblocknumberandindex": {},
	"eth_getunclebyblocknumberandindex":       {},
	"eth_getunclecountbyblocknumber":          {},
	"trace_block":                             {},
	"debug_traceblockbynumber":                {},
	"trace_replayblocktransactions":           {},
	"debug_getrawblock":                       {},
	"debug_getrawheader":                      {},
	"debug_getrawreceipts":                    {},
	"erigon_getheaderbynumber":                {},
	"arbtrace_block":                          {},
}

// ReadsBlock reports whether method reads one block by number or tag
// (spec §7 family 2).
func ReadsBlock(method string) bool {
	_, ok := blockByTagFamily[strings.ToLower(method)]
	return ok
}

// OffViewBlock reports whether resp, an upstream's eth_getBlockByNumber
// answer to req, is the block at head's Latest on another branch than the
// view's (spec §6.5, §7 family 2), and that block: one of another branch
// is never served under the view's number unless the view moved onto it.
// head is the request's one Head; nil, unknown or hashless, nothing is off
// the view.
func OffViewBlock(ctx context.Context, req *common.NormalizedRequest, resp *common.NormalizedResponse, head *common.EvmHead) (indexer.Cursor, bool) {
	if head == nil || head.Latest <= 0 || head.LatestHash == ([32]byte{}) || resp == nil || resp.IsObjectNull(ctx) {
		return indexer.Cursor{}, false
	}
	if method, _ := req.Method(); method != "eth_getBlockByNumber" {
		return indexer.Cursor{}, false
	}
	jrr, err := resp.JsonRpcResponse(ctx)
	if err != nil || jrr == nil {
		return indexer.Cursor{}, false
	}
	nh, err := jrr.PeekStringByPath(ctx, "number")
	if err != nil {
		return indexer.Cursor{}, false
	}
	if n, err := common.HexToInt64(nh); err != nil || n != head.Latest {
		return indexer.Cursor{}, false
	}
	hh, err := jrr.PeekStringByPath(ctx, "hash")
	if err != nil {
		return indexer.Cursor{}, false
	}
	c := indexer.Cursor{Number: uint64(head.Latest)}
	return c, decodeHex32(hh, &c.Hash) && c.Hash != head.LatestHash
}

// LatestBlockRead reports whether req is a client's eth_getBlockByNumber
// whose "latest" tag was interpolated to its Head's number, a Head with a
// hash (spec §6.4 client read): the read that may reach upstreams as the
// tag. A request a use-upstream directive pins, or one eRPC sent itself,
// is not; full: it asks for the block's transactions.
func LatestBlockRead(ctx context.Context, req *common.NormalizedRequest) (read, full bool) {
	if req.UpstreamPolicies() != common.UpstreamPoliciesConfigured {
		return false, false
	}
	if d := req.Directives(); d != nil && d.UseUpstream != "" {
		return false, false
	}
	if method, _ := req.Method(); method != "eth_getBlockByNumber" {
		return false, false
	}
	if ref, _ := req.EvmBlockRef().(string); ref != "latest" {
		return false, false
	}
	head := req.EvmHead()
	if head == nil || head.Latest <= 0 || head.LatestHash == ([32]byte{}) || TagBlockNumber(ctx, req) != head.Latest {
		return false, false
	}
	jrq, err := req.JsonRpcRequest(ctx)
	if err != nil || jrq == nil {
		return false, false
	}
	jrq.RLock()
	defer jrq.RUnlock()
	if len(jrq.Params) == 0 {
		return false, false
	}
	hx, err := common.NormalizeHex(head.Latest)
	if s, ok := jrq.Params[0].(string); err != nil || !ok || s != hx {
		return false, false
	}
	if len(jrq.Params) > 1 {
		full, _ = jrq.Params[1].(bool)
	}
	return true, full
}

// LatestTagParams is a copy of a LatestBlockRead's params with the block
// as the "latest" tag: what its upstream is sent.
func LatestTagParams(params []interface{}) []interface{} {
	out := slices.Clone(params)
	out[0] = "latest"
	return out
}

func networkPostForward_eth_getBlockByNumber(ctx context.Context, network common.Network, nq *common.NormalizedRequest, nr *common.NormalizedResponse, re error) (*common.NormalizedResponse, error) {
	ctx, span := common.StartDetailSpan(ctx, "Network.PostForward.eth_getBlockByNumber", trace.WithAttributes(
		attribute.String("request.id", fmt.Sprintf("%v", nq.ID())),
		attribute.String("network.id", network.Id()),
	))
	defer span.End()

	if re != nil {
		if out, ok := unservedNull(ctx, nq, re); ok {
			return out, nil
		}
		common.SetTraceSpanError(span, re)
		return nr, re
	}

	// Track timestamp distance for "latest" blocks and add lag metrics to detailed spans
	if nr != nil && RequestBlockTag(ctx, nq) == "latest" {
		_, respBlockNumber, bnErr := ExtractBlockReferenceFromResponse(ctx, nr)
		blockTimestamp, tsErr := ExtractBlockTimestampFromResponse(ctx, nr)

		if common.IsTracingDetailed() && bnErr == nil && respBlockNumber > 0 {
			highestBlock := common.EvmHeadOf(network, ctx, nq).Latest
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

		if tsErr == nil && blockTimestamp > 0 {
			currentTime := time.Now().Unix()
			timestampLag := currentTime - blockTimestamp
			if common.IsTracingDetailed() {
				span.SetAttributes(
					attribute.Int64("block.timestamp", blockTimestamp),
					attribute.Int64("current.timestamp", currentTime),
					attribute.Int64("block.timestamp_lag", timestampLag),
				)
			}
			telemetry.MetricNetworkLatestBlockTimestampDistance.WithLabelValues(
				network.ProjectId(),
				network.Label(),
				"network_response",
			).Set(float64(timestampLag))
		}
	}

	return enforceNonNullBlock(ctx, nq, nr)
}

// unservedNull is the null answer of nq, a numbered eth_getBlockByNumber
// of a block above its Latest, when re says its upstreams lacked the block
// or were skipped for it: a node lacking a block answers null, and we
// never announced it (spec §6.4 band sweep). That null is its answer
// (Refuse), never cached.
func unservedNull(ctx context.Context, nq *common.NormalizedRequest, re error) (*common.NormalizedResponse, bool) {
	if RequestBlockTag(ctx, nq) != "" || !common.HasErrorCode(re, common.ErrCodeEndpointMissingData, common.ErrCodeUpstreamBlockUnavailable) {
		return nil, false
	}
	v := HeadRefusalOf(ctx, nq)
	if v == nil || v.Latest <= 0 || v.Block <= v.Latest {
		return nil, false
	}
	out, ok := NullAnswer(nq, v.Block)
	if ok {
		v.Refuse()
	}
	return out, ok
}

// NullAnswer is req's null answer for block, a block no upstream serves
// it yet (spec §6.4): what a node answers.
func NullAnswer(req *common.NormalizedRequest, block int64) (*common.NormalizedResponse, bool) {
	jrr, err := common.NewJsonRpcResponse(req.ID(), nil, nil)
	if err != nil {
		return nil, false
	}
	resp := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
	resp.SetEvmBlockNumber(block)
	return resp, true
}

// enforceNonNullBlock checks if the block result is null/empty and returns an appropriate error.
// A request for a tag, interpolated or not (the tag stays in EvmBlockRef,
// spec §7, #477), follows the EnforceNonNullTaggedBlocks directive.
func enforceNonNullBlock(ctx context.Context, nq *common.NormalizedRequest, nr *common.NormalizedResponse) (*common.NormalizedResponse, error) {
	if nr != nil && !nr.IsObjectNull() && !nr.IsResultEmptyish() {
		return nr, nil
	}

	var bnp string
	if rqj, _ := nq.JsonRpcRequest(ctx); rqj != nil {
		rqj.RLock()
		if len(rqj.Params) > 0 {
			bnp = fmt.Sprintf("%v", rqj.Params[0])
		}
		rqj.RUnlock()
	}
	isTag := RequestBlockTag(ctx, nq) != ""

	// For tagged blocks, check directive
	if isTag {
		dirs := nq.Directives()
		if dirs == nil || !dirs.EnforceNonNullTaggedBlocks {
			// Directive not set or disabled - allow null tagged blocks
			return nr, nil
		}
	}

	// A null the refusal rule calls final (a future block, or a band block
	// after its first miss) is the truthful answer, not missing data, and
	// so is a numbered block's above Latest, which we never announced
	// (spec §6.4 band sweep): a multiplexed follower's request missed
	// nothing itself. Mirrors the upstream-level markUnexpectedEmpty guard
	// so the two layers agree.
	if EmptyResultBeyondConfidence(ctx, nq) {
		return nr, nil
	}
	if v := HeadRefusalOf(ctx, nq); !isTag && v != nil && v.Latest > 0 && v.Block > v.Latest {
		v.Refuse()
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
