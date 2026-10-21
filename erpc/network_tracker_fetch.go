package erpc

import (
	"context"
	"time"

	"github.com/erpc/erpc/common"
)

// SweepUpstreams makes one chain tracker fetch (spec §7): one pass over the
// upstreams the selection policy orders, or over the one upstream named. No
// failsafe policy, multiplexer, project or network rate limit applies, and
// upstreams whose breaker is open are skipped. The cache is read only when
// req's directives allow it. A failed pass returns ErrUpstreamsExhausted
// carrying each tried upstream's error.
func (n *Network) SweepUpstreams(ctx context.Context, req *common.NormalizedRequest, upstreamId string) (*common.NormalizedResponse, error) {
	startTime := time.Now()
	req.SetNetwork(n)
	req.SetCacheDal(n.cacheDal)
	method, err := req.Method()
	if err != nil {
		return nil, err
	}
	ctx = context.WithValue(ctx, common.RequestContextKey, req)

	if n.cacheDal != nil && !req.ShouldSkipCacheRead("") {
		if resp, err := n.cacheDal.Get(ctx, req); err == nil && resp != nil && !resp.IsObjectNull(ctx) {
			return resp, nil
		}
	}

	ups := n.trackerUpstreams(ctx, req, method, upstreamId)
	if len(ups) == 0 {
		return nil, common.NewErrNoUpstreamsFound(n.projectId, n.networkId)
	}
	req.SetUpstreams(ups)
	if err := n.prepareRequest(ctx, req); err != nil {
		return nil, err
	}

	finality := req.Finality(ctx)
	var best *common.NormalizedResponse
	for _, u := range ups {
		if ctx.Err() != nil {
			break
		}
		if d, ok := u.(interface {
			IsDown(string, common.DataFinalityState) bool
		}); ok && d.IsDown(method, finality) {
			continue
		}
		if skipErr, _ := n.checkUpstreamBlockAvailability(ctx, u, req, method); skipErr != nil {
			continue
		}
		r, err := n.doForward(ctx, u, req, req.ShouldSkipCacheRead(""), false)
		if err == nil {
			err = n.normalizeResponse(ctx, req, r)
		}
		req.MarkUpstreamCompleted(ctx, u, r, err)
		if err != nil {
			if common.IsClientError(err) || common.HasErrorCode(err, common.ErrCodeEndpointExecutionException) {
				return nil, err
			}
			continue
		}
		if r == nil || r.IsObjectNull(ctx) {
			continue
		}
		r.SetUpstream(u)
		r.WithRequest(req)
		if !r.IsResultEmptyish(ctx) {
			if best != nil {
				best.Release()
			}
			return r, nil
		}
		// Another upstream may hold the block this one answers null for.
		if best != nil {
			best.Release()
		}
		best = r
	}
	if best != nil {
		return best, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, context.Cause(ctx)
	}
	s := req.ExecState().Snapshot()
	return nil, common.NewErrUpstreamsExhausted(req, &req.ErrorsByUpstream, n.projectId, n.networkId, method,
		time.Since(startTime), s.Attempts, s.Retries, s.Hedges, len(ups))
}

// trackerUpstreams is the named upstream, or the selection order narrowed to
// the request's use-upstream selector.
func (n *Network) trackerUpstreams(ctx context.Context, req *common.NormalizedRequest, method, upstreamId string) []common.Upstream {
	if upstreamId != "" {
		for _, u := range n.AllUpstreams() {
			if u.Id() == upstreamId {
				return []common.Upstream{u}
			}
		}
		return nil
	}
	ups := n.preferUpstreams(ctx, req, method, n.selectionOrder(ctx, req, method))
	sel := ""
	if d := req.Directives(); d != nil {
		sel = d.UseUpstream
	}
	if sel == "" {
		return ups
	}
	out := make([]common.Upstream, 0, len(ups))
	for _, u := range ups {
		if ok, _ := common.UpstreamMatchesSelector(sel, u); ok {
			out = append(out, u)
		}
	}
	return out
}
