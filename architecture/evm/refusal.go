package evm

import (
	"context"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
)

// headRefuser is the network's side of the refusal rule (spec §6.4, §6.8):
// its block time, the Ask of the reports older than 1 bt, and the band's
// Ask of the upstreams below a block.
type headRefuser interface {
	EvmBlockTime() time.Duration
	EvmAskStale(ctx context.Context, selector string) (ceiling int64, oldestReportAt time.Time)
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
	return rq.TakeHeadRefusal(DecideHeadRefusal(ctx, nw, common.EvmHeadOf(nw, ctx, rq), sel, bn))
}

// DecideHeadRefusal applies the refusal rule to block n on head h of
// selector sel's lane: above the ceiling with a report 1 bt old or older,
// those reports are Asked first and n decided again; still stale after
// the Ask, n is in the band, the cheaper way to be wrong. Until the first
// block time sample, every report counts as older than 1 bt (spec §6.4).
// A band block Asks the upstreams below it, in the background.
func DecideHeadRefusal(ctx context.Context, nw common.Network, h common.EvmHead, sel string, n int64) *common.HeadRefusal {
	net, _ := nw.(headRefuser)
	bt, cold := health.ColdBlockTime, true
	if net != nil {
		if b := net.EvmBlockTime(); b > 0 {
			bt, cold = b, false
		}
	}
	if cold {
		h.OldestReportAt = time.Time{}
	}
	v := &common.HeadRefusal{Block: n, Rule: h.Rule(n, bt, time.Now())}
	if v.Rule == common.HeadStale && net != nil {
		h.Ceiling, h.OldestReportAt = net.EvmAskStale(ctx, sel)
		if cold {
			h.OldestReportAt = time.Time{}
		}
		v.Rule, v.Refreshed = h.Rule(n, bt, time.Now()), true
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
