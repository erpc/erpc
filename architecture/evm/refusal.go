package evm

import (
	"bytes"
	"context"
	"strings"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
)

// headRefuser is the network's side of the refusal rule (spec §6.4, §6.8):
// its block time, whether its lane's view pins a block, the near-future
// wait for the ceiling, and the band block's read ahead
// (ChainTracker.ReadAhead). None of them Asks an upstream.
type headRefuser interface {
	EvmBlockTime() time.Duration
	EvmPinned(selector string, block int64) bool
	EvmAwaitCeiling(ctx context.Context, selector string, block int64) (ceiling int64, oldestReportAt time.Time)
	EvmAskBand(selector string, block int64)
}

// HeadRefusalOf is the refusal verdict for rq's block, decided once: by the
// network before dispatch, else by its first reader. A request naming no
// concrete block number gets HeadServe for block 0, so a block number its
// answer carries later is never judged as asked.
func HeadRefusalOf(ctx context.Context, rq *common.NormalizedRequest) *common.HeadRefusal {
	if rq == nil {
		return nil
	}
	if v := rq.HeadRefusal(); v != nil {
		return v
	}
	nw := rq.Network()
	if nw == nil {
		return nil
	}
	_, bn, err := ExtractBlockReferenceFromRequest(ctx, rq)
	if err != nil || bn <= 0 {
		return rq.TakeHeadRefusal(&common.HeadRefusal{})
	}
	sel := ""
	if d := rq.Directives(); d != nil {
		sel = d.UseUpstream
	}
	return rq.TakeHeadRefusal(DecideHeadRefusal(ctx, nw, common.EvmHeadOf(nw, ctx, rq), sel, bn, !ShortCircuitsFuture(rq)))
}

// ShortCircuitsFuture reports whether a future block on rq gets null with
// no dispatch and no wait: a numbered eth_getBlockByNumber on a network
// with shortCircuitFutureBlocks (spec §6.4, #1238).
func ShortCircuitsFuture(rq *common.NormalizedRequest) bool {
	nw := rq.Network()
	if nw == nil {
		return false
	}
	if cfg := nw.Config(); cfg == nil || !cfg.Evm.ShortCircuitFutureBlocksEnabled() {
		return false
	}
	method, _ := rq.Method()
	return strings.EqualFold(method, "eth_getBlockByNumber")
}

// RefusalMiss reports whether result, method's answer for a block above
// Firm, is a node's miss of that block (spec §6.4): null or nothing, what
// a node lacking the block answers a lookup by block (the block, its
// receipts, a transaction in it); for a range query (eth_getLogs,
// trace_filter), also an empty array, what a node behind the range
// answers. Any other empty value is the block's answer: eth_call's 0x, a
// zero balance or nonce, an empty block's [] receipts or traces; a node
// that lacks the block errors instead. Counted as misses, arc-mainnet's
// `eth_call`s of 0x at a thin Head took 2-5 attempts each (277
// thin_missing on image 22).
func RefusalMiss(method string, result []byte) bool {
	switch r := bytes.TrimSpace(result); {
	case len(r) == 0 || bytes.Equal(r, []byte("null")):
		return true
	case bytes.Equal(r, []byte("[]")):
		return method == "eth_getLogs" || isTraceFilterMethod(method)
	}
	return false
}

// DecideHeadRefusal applies the refusal rule to block n on head h of
// selector sel's lane; nothing on it Asks an upstream (spec §6.4). A block
// above Firm that the lane's view pins exists: it is thin and Held,
// whatever h's Latest and ceiling, which a method only some upstreams
// serve keeps below it (spec §6.4 held). Above the ceiling, a block beyond
// the near future (nearFuture), whose bound counts the blocks the chain
// can have made since each counted report, is future at once: no counted
// upstream can hold it yet, however stale its report, and none is Asked.
// A report age that is unknown bounds nothing, and until the first block
// time sample none is known (spec §6.4). With wait, an n in the near future
// waits for the ceiling to reach it, at most W, as the lane's read ahead,
// whose one head fetch every such read shares, and is decided again on
// the ceiling the wait saw: still above it, n is future; Latest stays the
// request's own (spec §7). Without wait, a near n above a report 1 bt old
// or older is in the band, the cheaper way to be wrong. A band block is
// the lane's read ahead too, in the background.
func DecideHeadRefusal(ctx context.Context, nw common.Network, h common.EvmHead, sel string, n int64, wait bool) *common.HeadRefusal {
	net, _ := nw.(headRefuser)
	bt, cold := health.ColdBlockTime, true
	if net != nil {
		if b := net.EvmBlockTime(); b > 0 {
			bt, cold = b, false
		}
	}
	if cold {
		h.OldestReportAt, h.ReachAt = time.Time{}, time.Time{}
	}
	now := time.Now()
	v := &common.HeadRefusal{Block: n, Latest: h.Latest, Rule: h.Rule(n, bt, now)}
	if v.Rule != common.HeadServe && net != nil && net.EvmPinned(sel, n) {
		v.Rule, v.Held, v.Ceiling = common.HeadThin, true, h.Ceiling
		return v
	}
	if v.Rule == common.HeadFuture || v.Rule == common.HeadStale {
		switch {
		case !nearFuture(h, bt, n, now):
			v.Rule = common.HeadFuture
		case wait && net != nil:
			h.Ceiling, h.OldestReportAt = net.EvmAwaitCeiling(ctx, sel, n)
			if cold {
				h.OldestReportAt = time.Time{}
			}
			v.Rule, v.Waited = h.Rule(n, bt, time.Now()), true
			if v.Rule == common.HeadStale {
				v.Rule = common.HeadFuture
			}
		}
	}
	if v.Rule == common.HeadStale {
		v.Rule = common.HeadBand
	}
	v.Ceiling = h.Ceiling
	if v.Rule == common.HeadBand && net != nil {
		net.EvmAskBand(sel, n)
	}
	return v
}

// nearFuture reports whether n, above h's ceiling, waits for it (spec §6.4
// near future): n is at most W of chain plus 2 blocks above where the
// counted reports put the chain now, the report furthest along once the
// blocks made since each are counted (Reach). The margin is the block the
// chain may have made right after a report, and one for a reporter that
// trailed it by a block. A report with no time bounds nothing.
func nearFuture(h common.EvmHead, bt time.Duration, n int64, now time.Time) bool {
	if h.ReachAt.IsZero() {
		return true
	}
	reach := h.Reach + int64(max(0, now.Sub(h.ReachAt))/bt)
	return n-reach <= int64(health.FreshnessWindow(bt)/bt)+2
}

// EmptyResultBeyondConfidence reports whether an empty answer for rq is its answer rather
// than missing data, so it is neither retried nor marked: a block the
// refusal rule calls future, or a band block after its one miss (spec
// §6.4). With emptyResultConfidence finalizedBlock, also any block above
// Head.Finalized. false for requests naming no concrete block, and while
// the head is unknown.
func EmptyResultBeyondConfidence(ctx context.Context, rq *common.NormalizedRequest) bool {
	v := HeadRefusalOf(ctx, rq)
	if v == nil {
		return false
	}
	if cfg := rq.Network().Config(); cfg != nil && cfg.Evm != nil && cfg.Evm.EmptyResultConfidence == common.AvailbilityConfidenceFinalized {
		if f := common.EvmHeadOf(rq.Network(), ctx, rq).Finalized; f > 0 && v.Block > f {
			return true
		}
	}
	return v.Final()
}
