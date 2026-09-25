package erpc

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/trace"
)

type cacheWriteBypassKey struct{}

// withCacheWriteBypass marks a context whose Forward must not write the
// response into the ordinary JSON-RPC cache. Head cache hydration uses it:
// hydration reads unfinalized head data that may be orphaned by a reorg the
// head cache detects and discards, and must not leave copies behind in the
// general cache that only expire by TTL.
func withCacheWriteBypass(ctx context.Context) context.Context {
	return context.WithValue(ctx, cacheWriteBypassKey{}, true)
}

func cacheWriteBypassed(ctx context.Context) bool {
	v, _ := ctx.Value(cacheWriteBypassKey{}).(bool)
	return v
}

// cacheStoreReporter is implemented by cache DALs that can report whether a
// Set actually persisted the entry in at least one connector (a Set with no
// matching policy returns nil without storing anything).
type cacheStoreReporter interface {
	SetReport(ctx context.Context, req *common.NormalizedRequest, res *common.NormalizedResponse) (stored bool, err error)
}

// storeInCache is the single cache-write body shared by the async path and
// the synchronous cache-fill leader path: panic recovery, a 10s app-scoped
// timeout, and the response reference release. The caller must have called
// resp.AddRef() and materialized the JSON-RPC response.
func (n *Network) storeInCache(lg *zerolog.Logger, method string, req *common.NormalizedRequest, resp *common.NormalizedResponse, spanCtx trace.SpanContext) (stored bool, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			telemetry.MetricUnexpectedPanicTotal.WithLabelValues(
				"cache-set",
				fmt.Sprintf("network:%s method:%s", n.networkId, method),
				common.ErrorFingerprint(rec),
			).Inc()
			lg.Error().
				Interface("panic", rec).
				Str("stack", string(debug.Stack())).
				Msgf("unexpected panic on cache-set")
			stored, err = false, fmt.Errorf("panic on cache-set: %v", rec)
		}
	}()
	defer resp.DoneRef()

	timeoutCtx, cancel := context.WithTimeoutCause(n.appCtx, 10*time.Second, errors.New("cache driver timeout during set"))
	defer cancel()
	tracedCtx := trace.ContextWithSpanContext(timeoutCtx, spanCtx)
	if r, ok := n.cacheDal.(cacheStoreReporter); ok {
		stored, err = r.SetReport(tracedCtx, req, resp)
	} else {
		err = n.cacheDal.Set(tracedCtx, req, resp)
		// Unknown DAL: success cannot be distinguished from "no policy".
		stored = false
	}
	if err != nil {
		lg.Warn().Err(err).Msgf("could not store response in cache")
	}
	return stored, err
}
