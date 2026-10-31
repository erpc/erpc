package erpc

import (
	"context"
	"slices"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/common"
	"go.opentelemetry.io/otel/trace"
)

// trackerAttemptSlack is added to an upstream's even share of what remains
// of a sweep, so an upstream that answers near its share is not cut off.
var trackerAttemptSlack = time.Second

// SweepUpstreams makes one chain tracker fetch (spec §7): one pass over the
// upstreams the selection policy orders, or over the one upstream named. No
// failsafe policy, project or network rate limit applies, and upstreams
// whose breaker is open or that do not serve the method are skipped. Each
// attempt gets an even share of what remains of ctx plus
// trackerAttemptSlack, so one slow upstream cannot end the sweep. The cache
// is read only when req's directives allow it. check is the tracker's
// answer check: an answer it fails is never cached and never shared. A
// content-validation failure (check found the answer itself wrong) fails
// only that upstream: the pass goes on, to upstreams of another vendor
// first (spec §11). Any other failure ends the pass with that answer and
// its error. A network-wide sweep leads the in-flight
// multiplexer, so identical client requests join it. A failed pass returns
// ErrUpstreamsExhausted carrying each tried upstream's error.
func (n *Network) SweepUpstreams(ctx context.Context, req *common.NormalizedRequest, upstreamId string, check func(*common.NormalizedResponse) error) (resp *common.NormalizedResponse, err error) {
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
			return cached, check(cached)
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

	// checked: check passed on resp; a panic in check leaves it false.
	checked := false
	if d := req.Directives(); upstreamId == "" && (d == nil || d.UseUpstream == "") {
		if mlx := n.leadMultiplexer(ctx, req); mlx != nil {
			// Followers get only an answer that passed check; on anything
			// else, a panic included, they forward on their own, with
			// their own failsafe policies.
			defer func() {
				var shared *common.NormalizedResponse
				if checked && !resp.IsResultEmptyish(ctx) {
					shared = resp
				}
				mlx.Close(ctx, shared, nil)
				n.cleanupMultiplexer(mlx)
			}()
		}
	}

	if resp, err = n.sweep(ctx, req, method, ups, startTime, check); err != nil {
		return resp, err
	}
	checked = true
	if n.cacheDal != nil && !resp.FromCache() {
		lg := n.logger.With().Str("method", method).Logger()
		n.storeInCache(ctx, trace.SpanContextFromContext(ctx), &lg, method, req, resp)
	}
	return resp, nil
}

// sweep tries ups in order and returns the first non-empty answer check
// passes, else the last empty one with its check's outcome. An answer
// whose check fails for its content fails only that upstream, empty or not.
func (n *Network) sweep(ctx context.Context, req *common.NormalizedRequest, method string, ups []common.Upstream, startTime time.Time, check func(*common.NormalizedResponse) error) (*common.NormalizedResponse, error) {
	finality := req.Finality(ctx)
	deadline, bounded := ctx.Deadline()
	var (
		best    *common.NormalizedResponse
		bestErr error
	)
	for i := range ups {
		u := ups[i]
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
		err = check(r)
		if common.HasErrorCode(err, common.ErrCodeEndpointContentValidation) {
			req.ErrorsByUpstream.Store(u, err)
			otherVendorsFirst(ups[i+1:], evm.VendorKey(u))
			continue
		}
		if !r.IsResultEmptyish(ctx) {
			if best != nil {
				best.Release()
			}
			return r, err
		}
		// Another upstream may hold the block this one answers null for.
		if best != nil {
			best.Release()
		}
		best, bestErr = r, err
	}
	if best != nil {
		return best, bestErr
	}
	if err := ctx.Err(); err != nil {
		return nil, context.Cause(ctx)
	}
	s := req.ExecState().Snapshot()
	return nil, common.NewErrUpstreamsExhausted(req, &req.ErrorsByUpstream, n.projectId, n.networkId, method,
		time.Since(startTime), s.Attempts, s.Retries, s.Hedges, len(ups))
}

// otherVendorsFirst moves the upstreams of vendor behind every other, in
// place, each side keeping its order.
func otherVendorsFirst(ups []common.Upstream, vendor string) {
	slices.SortStableFunc(ups, func(a, b common.Upstream) int {
		sa, sb := evm.VendorKey(a) == vendor, evm.VendorKey(b) == vendor
		switch {
		case sa == sb:
			return 0
		case sa:
			return 1
		}
		return -1
	})
}

// trackerUpstreams is the named upstream, or the selection order narrowed to
// the request's use-upstream selector; only upstreams that serve method.
func (n *Network) trackerUpstreams(ctx context.Context, req *common.NormalizedRequest, method, upstreamId string) []common.Upstream {
	var ups []common.Upstream
	if upstreamId != "" {
		for _, u := range n.AllUpstreams() {
			if u.Id() == upstreamId {
				ups = []common.Upstream{u}
				break
			}
		}
	} else {
		ups = n.preferUpstreams(ctx, req, method, n.selectionOrder(ctx, req, method))
	}
	sel := ""
	if d := req.Directives(); d != nil && upstreamId == "" {
		sel = d.UseUpstream
	}
	out := make([]common.Upstream, 0, len(ups))
	for _, u := range ups {
		if ok, _ := u.ShouldHandleMethod(method); !ok {
			continue
		}
		if sel != "" {
			if ok, _ := common.UpstreamMatchesSelector(sel, u); !ok {
				continue
			}
		}
		out = append(out, u)
	}
	return out
}
