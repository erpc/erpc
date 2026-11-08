package erpc

import (
	"context"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
)

// latestReadWait bounds how long a client read of latest sent as the tag
// waits for the view to take its answer (spec §6.4 client read): the
// tracker's mailbox, never a fetch, and never another upstream.
const latestReadWait = 50 * time.Millisecond

// tagsLatestRead reports whether req's attempts reach their upstreams as
// the "latest" tag (spec §6.4 client read): a client's
// eth_getBlockByNumber(latest) (evm.LatestBlockRead) on a network with a
// tracker, without a consensus policy, whose slots would compare answers
// of different blocks, and without the cache anchor, whose Latest may
// trail the view's.
func (n *Network) tagsLatestRead(ctx context.Context, req *common.NormalizedRequest, consensus bool) bool {
	if consensus || n.chainTracker == nil || n.followsCacheHead() {
		return false
	}
	read, _ := evm.LatestBlockRead(ctx, req)
	return read
}

// latestRead is one attempt of a client read of latest sent as the tag:
// the upstream's poll until it ends (EvmStatePoller.ReadLatest).
type latestRead struct {
	end  func(block []byte, full bool) common.EvmHeader
	full bool
}

// startLatestRead returns ctx whose attempt sends u req's params with the
// block as the tag, and the read, sent at sent; nil, and ctx as is, when
// u has no poller to take the answer.
func (n *Network) startLatestRead(ctx context.Context, req *common.NormalizedRequest, u common.Upstream, sent time.Time) (context.Context, *latestRead) {
	eu, ok := u.(common.EvmUpstream)
	if !ok {
		return ctx, nil
	}
	sp := eu.EvmStatePoller()
	if sp == nil || sp.IsObjectNull() {
		return ctx, nil
	}
	jrq, err := req.JsonRpcRequest(ctx)
	if err != nil || jrq == nil {
		return ctx, nil
	}
	_, full := evm.LatestBlockRead(ctx, req)
	jrq.RLock()
	params := evm.LatestTagParams(jrq.Params)
	jrq.RUnlock()
	return common.WithWireParams(ctx, req, params), &latestRead{end: sp.ReadLatest(sent), full: full}
}

// finishLatestRead takes rd's answer r (err its error) as its upstream's
// poll of latest, through intake, and reports whether r is served: its
// block is at or above req's Head and the lane's view pins it, at or
// below the view's Head, now or once the tracker's mailbox took the vote
// (latestReadWait). Otherwise the attempt serves the view's Head block
// instead: an answer below the Head (a lagging upstream), on another
// branch, whose header intake refused or the view cannot take without a
// walk or more votes. Each attempt counts its outcome.
func (n *Network) finishLatestRead(ctx context.Context, req *common.NormalizedRequest, rd *latestRead, r *common.NormalizedResponse, err error) (served bool) {
	var block []byte
	if err == nil && r != nil && !r.IsObjectNull(ctx) && !r.IsResultEmptyish(ctx) {
		if jrr, jerr := r.JsonRpcResponse(ctx); jerr == nil && jrr != nil && jrr.Error == nil {
			block = jrr.GetResultBytes()
		}
	}
	sh, ok := rd.end(block, rd.full).(evm.SourceHeader)
	switch {
	case block == nil:
		n.countLatestRead("failed")
		return false
	case ok && int64(sh.Cursor().Number) >= req.EvmHead().Latest && n.viewTakes(ctx, sh):
		n.countLatestRead("served")
		return true
	}
	n.countLatestRead("head")
	return false
}

// viewTakes reports whether the network-wide view pins sh at or below its
// Head, now or after the tracker handled the messages sent before, at
// most latestReadWait.
func (n *Network) viewTakes(ctx context.Context, sh evm.SourceHeader) bool {
	t := n.laneTracker("")
	if t == nil {
		return false
	}
	c := sh.Cursor()
	in := func() bool {
		v := t.View()
		head, ok := v.Head()
		return ok && c.Number <= head.Number && v.Pinned(c)
	}
	if in() {
		return true
	}
	ctx, cancel := context.WithTimeout(ctx, latestReadWait)
	defer cancel()
	t.Sync(ctx)
	return in()
}

// cachedHeadBlock is the cache's entry of req's own block, the Head's,
// for a read of latest whose answer was not served (finishLatestRead):
// a head fetch's seed (seedHeadBlock); nil when there is none, or it is
// another branch's.
func (n *Network) cachedHeadBlock(ctx context.Context, req *common.NormalizedRequest) *common.NormalizedResponse {
	if n.cacheDal == nil || req.ShouldSkipCacheRead("") {
		return nil
	}
	resp, err := n.cacheDal.Get(ctx, req)
	if err != nil || resp == nil {
		return nil
	}
	if _, off := evm.OffViewBlock(ctx, req, resp, req.EvmHead()); off || resp.IsObjectNull(ctx) || resp.IsResultEmptyish(ctx) {
		resp.Release()
		return nil
	}
	return resp
}

// servesAnotherBlock reports whether resp, the answer to a read of latest
// sent as the tag, is a block other than req's Head's: the view took a
// newer one (finishLatestRead). Its cache key, req's params, names the
// Head's block, so it is never written there.
func servesAnotherBlock(ctx context.Context, req *common.NormalizedRequest, resp *common.NormalizedResponse) bool {
	jrr, err := resp.JsonRpcResponse(ctx)
	if err != nil || jrr == nil {
		return true
	}
	s, err := jrr.PeekStringByPath(ctx, "number")
	if err != nil {
		return true
	}
	bn, err := common.HexToInt64(s)
	return err != nil || bn != req.EvmHead().Latest
}

func (n *Network) countLatestRead(outcome string) {
	telemetry.CounterHandle(telemetry.MetricChainTrackerLatestReadTotal, n.projectId, n.Label(), outcome).Inc()
}
