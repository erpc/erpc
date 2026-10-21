package erpc

import (
	"context"
	"time"

	"github.com/erpc/erpc/common"
	"go.opentelemetry.io/otel/trace"
)

// trackerAttemptSlack is added to an upstream's even share of what remains
// of a sweep, so an upstream that answers near its share is not cut off.
var trackerAttemptSlack = time.Second

// SweepUpstreams makes one chain tracker fetch (spec §7): one pass over the
// upstreams the selection policy orders, or over the one upstream named. No
// failsafe policy, project or network rate limit applies, and upstreams
// whose breaker is open are skipped. Each attempt gets an even share of what
// remains of ctx plus trackerAttemptSlack, so one slow upstream cannot end
// the sweep. The cache is read only when req's directives allow it; the
// answer reaches it only through StoreTrackerAnswer, once the tracker's
// checks passed. A network-wide sweep leads the in-flight multiplexer, so
// identical client requests join it. A failed pass returns
// ErrUpstreamsExhausted carrying each tried upstream's error.
func (n *Network) SweepUpstreams(ctx context.Context, req *common.NormalizedRequest, upstreamId string) (resp *common.NormalizedResponse, err error) {
	startTime := time.Now()
	req.SetNetwork(n)
	req.SetCacheDal(n.cacheDal)
	method, err := req.Method()
	if err != nil {
		return nil, err
	}
	ctx = context.WithValue(ctx, common.RequestContextKey, req)

	if n.cacheDal != nil && !req.ShouldSkipCacheRead("") {
		if cached, err := n.cacheDal.Get(ctx, req); err == nil && cached != nil && !cached.IsObjectNull(ctx) {
			return cached, nil
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

	if d := req.Directives(); upstreamId == "" && (d == nil || d.UseUpstream == "") {
		if mlx := n.leadMultiplexer(ctx, req); mlx != nil {
			// Followers get only a usable answer; on anything else they
			// forward on their own, with their own failsafe policies.
			defer func() {
				var shared *common.NormalizedResponse
				if err == nil && !resp.IsResultEmptyish(ctx) {
					shared = resp
				}
				mlx.Close(ctx, shared, nil)
				n.cleanupMultiplexer(mlx)
			}()
		}
	}

	return n.sweep(ctx, req, method, ups, startTime)
}

// StoreTrackerAnswer writes a tracker fetch's answer to the cache, as
// Forward writes one; the scheduler calls it only after the answer passed
// the tracker's answer and bloom checks.
func (n *Network) StoreTrackerAnswer(ctx context.Context, req *common.NormalizedRequest, resp *common.NormalizedResponse) {
	if n.cacheDal == nil || resp == nil || resp.FromCache() {
		return
	}
	method, err := req.Method()
	if err != nil {
		return
	}
	lg := n.logger.With().Str("method", method).Logger()
	n.storeInCache(ctx, trace.SpanContextFromContext(ctx), &lg, method, req, resp)
}

// sweep tries ups in order and returns the first non-empty answer, else the
// last empty one.
func (n *Network) sweep(ctx context.Context, req *common.NormalizedRequest, method string, ups []common.Upstream, startTime time.Time) (*common.NormalizedResponse, error) {
	finality := req.Finality(ctx)
	deadline, bounded := ctx.Deadline()
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
		actx, cancel := ctx, context.CancelFunc(func() {})
		if bounded {
			remaining := time.Until(deadline)
			actx, cancel = context.WithTimeout(ctx, min(remaining, remaining/time.Duration(len(ups))+trackerAttemptSlack))
		}
		r, err := n.doForward(actx, u, req, req.ShouldSkipCacheRead(""), false)
		if err == nil {
			err = n.normalizeResponse(actx, req, r)
		}
		cancel()
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
