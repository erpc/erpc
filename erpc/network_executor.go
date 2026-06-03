package erpc

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/failsafe"
	"github.com/erpc/erpc/telemetry"
	"github.com/rs/zerolog"
)

// networkExecutor owns retry / hedge / timeout / consensus
// orchestration for one (method-pattern, finality) match at the
// network scope.
//
// Consensus is referenced opaquely (via the consensusRunner interface)
// so this file does not import consensus/ directly — that avoids a
// circular dependency between erpc/ and consensus/.
type networkExecutor struct {
	cfg     *common.NetworkFailsafeConfig
	logger  *zerolog.Logger
	timeout common.TimeoutFunc

	// consensus is optional. When non-nil, the executor branches into
	// consensus(retry(hedge(slotInner))) per spec §11.2.
	consensus consensusRunner

	method     string
	finalities []common.DataFinalityState

	emptyResultAccept []string

	dynamicBlockUnavailableDelay func() time.Duration
	// networkBlockTime returns the network's EMA-estimated block time (0 until
	// warmup); used for block-time-relative empty-result delays.
	networkBlockTime func() time.Duration
}

// consensusRunner is the minimal interface this package needs from
// consensus.*Consensus. consensus/ will implement Run via its existing
// executor machinery; this file does not import consensus/.
type consensusRunner interface {
	Run(
		ctx context.Context,
		req *common.NormalizedRequest,
		inner func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error),
	) (*common.NormalizedResponse, error)
}

// NewNetworkExecutor builds a per-(method, finality) network executor.
func NewNetworkExecutor(
	cfg *common.NetworkFailsafeConfig,
	logger *zerolog.Logger,
	consensus consensusRunner,
	dynamicBlockUnavailableDelay func() time.Duration,
	networkBlockTime func() time.Duration,
) (*networkExecutor, error) {
	if cfg == nil {
		return &networkExecutor{
			method:                       "*",
			logger:                       logger,
			emptyResultAccept:            common.DefaultEmptyResultAccept(),
			dynamicBlockUnavailableDelay: dynamicBlockUnavailableDelay,
			networkBlockTime:             networkBlockTime,
		}, nil
	}

	if cfg.CircuitBreaker != nil {
		return nil, common.NewErrFailsafeConfiguration(
			errors.New("circuit breaker does not make sense for network-level requests"),
			map[string]interface{}{"policy": cfg.CircuitBreaker},
		)
	}

	e := &networkExecutor{
		cfg:                          cfg,
		logger:                       logger,
		method:                       cfg.MatchMethod,
		finalities:                   cfg.MatchFinality,
		consensus:                    consensus,
		dynamicBlockUnavailableDelay: dynamicBlockUnavailableDelay,
		networkBlockTime:             networkBlockTime,
	}
	if e.method == "" {
		e.method = "*"
	}
	if cfg.Timeout != nil {
		e.timeout = common.NewTimeoutFunc(logger, cfg.Timeout)
	}
	if cfg.Retry != nil && cfg.Retry.EmptyResultAccept != nil {
		e.emptyResultAccept = cfg.Retry.EmptyResultAccept
	} else {
		e.emptyResultAccept = common.DefaultEmptyResultAccept()
	}
	return e, nil
}

// MatchMethod returns the configured method pattern.
func (e *networkExecutor) MatchMethod() string { return e.method }

// MatchFinality returns the configured finality filter.
func (e *networkExecutor) MatchFinality() []common.DataFinalityState { return e.finalities }

// Timeout exposes the configured TimeoutFunc (nil when no timeout).
func (e *networkExecutor) Timeout() common.TimeoutFunc { return e.timeout }

// HasTimeout returns whether a timeout policy is configured.
func (e *networkExecutor) HasTimeout() bool { return e != nil && e.timeout != nil }

// HasConsensus returns whether consensus is configured.
func (e *networkExecutor) HasConsensus() bool {
	if e == nil || e.cfg == nil {
		return false
	}
	return e.cfg.Consensus != nil
}

// EmptyResultAccept returns the configured empty-result accept list.
func (e *networkExecutor) EmptyResultAccept() []string {
	if e == nil {
		return common.DefaultEmptyResultAccept()
	}
	return e.emptyResultAccept
}

// HasHedge returns whether hedge is configured.
func (e *networkExecutor) HasHedge() bool {
	if e == nil || e.cfg == nil {
		return false
	}
	return e.cfg.Hedge != nil && e.cfg.Hedge.MaxCount > 0
}

// HasRetry returns whether retry is configured.
func (e *networkExecutor) HasRetry() bool {
	if e == nil || e.cfg == nil {
		return false
	}
	return e.cfg.Retry != nil && e.cfg.Retry.MaxAttempts > 0
}

// Run applies consensus + retry + hedge + timeout for one network-scope
// request. The caller supplies `tryUpstream` — a function that picks one
// upstream and forwards the request (preflight + Upstream.Forward +
// postflight).
//
// Composition per spec §11.2:
//   - When consensus is configured: consensus(retry(hedge(tryOneUpstream)))
//     where tryOneUpstream picks ONE upstream (via NextUpstream) per slot.
//   - When consensus is NOT configured: retry(hedge(runUpstreamSweep))
//     where runUpstreamSweep tries all upstreams within one execution.
//
// The caller is responsible for providing tryOneUpstream / runUpstreamSweep
// closures that match the integration shape; the executor only orchestrates.
func (e *networkExecutor) Run(
	ctx context.Context,
	req *common.NormalizedRequest,
	tryOneUpstream func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error),
	runUpstreamSweep func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error),
) (*common.NormalizedResponse, error) {
	if e == nil {
		return runUpstreamSweep(ctx, req)
	}

	// Apply lifecycle timeout that wraps the entire executor invocation.
	if e.timeout != nil {
		if td := e.timeout(ctx, req); td != nil {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeoutCause(ctx, *td, common.ErrDynamicTimeoutExceeded)
			defer cancel()
		}
	}

	// Consensus branch: each slot is retry(hedge(tryOneUpstream)).
	// Skipped when the request carries the SkipConsensus directive (header
	// `X-ERPC-Skip-Consensus: true`, query `?skip-consensus=true`, or
	// `directiveDefaults.skipConsensus: true` in the network/project config).
	// Falls through to the standard non-consensus retry+hedge path; all
	// other policies (retry, hedge, breaker, timeout) still apply.
	skipConsensus := false
	if rds := req.Directives(); rds != nil {
		skipConsensus = rds.SkipConsensus
	}
	if e.HasConsensus() && e.consensus != nil && !skipConsensus {
		slotInner := func(slotCtx context.Context, slotReq *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return e.runRetryHedge(slotCtx, slotReq, tryOneUpstream)
		}
		return e.consensus.Run(ctx, req, slotInner)
	}

	// Non-consensus branch: retry(hedge(runUpstreamSweep)).
	return e.runRetryHedge(ctx, req, runUpstreamSweep)
}

func (e *networkExecutor) runRetryHedge(
	ctx context.Context,
	req *common.NormalizedRequest,
	inner func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error),
) (*common.NormalizedResponse, error) {
	hedgeWrapped := func(ctx context.Context) (*common.NormalizedResponse, error) {
		return e.runHedge(ctx, req, inner)
	}
	return e.runRetry(ctx, req, hedgeWrapped)
}

func (e *networkExecutor) runRetry(
	ctx context.Context,
	req *common.NormalizedRequest,
	hedged func(ctx context.Context) (*common.NormalizedResponse, error),
) (*common.NormalizedResponse, error) {
	maxAttempts := 1
	if e.cfg != nil && e.cfg.Retry != nil && e.cfg.Retry.MaxAttempts > 0 {
		maxAttempts = e.cfg.Retry.MaxAttempts
	}
	startTime := time.Now()
	st := req.ExecState()

	var bestResp *common.NormalizedResponse
	var lastErr error
	// firstInformativeErr captures the first attempt's error that
	// contains real upstream details (ErrUpstreamsExhausted with
	// children, ErrExecutionException, etc.) — subsequent retry
	// attempts can degenerate into bare ErrNoUpstreamsLeftToSelect
	// which loses that info; the final wrap uses this instead.
	var firstInformativeErr error
	retriesAttempted := 0

	for attempt := 0; attempt < maxAttempts; attempt++ {
		// Bail out early if the lifecycle context has fired — the timeout
		// owns the classification, not retry-exhausted.
		if ctxErr := ctx.Err(); ctxErr != nil {
			cause := context.Cause(ctx)
			if cause != nil {
				return bestResp, cause
			}
			return bestResp, ctxErr
		}

		if attempt > 0 && st != nil {
			st.NetworkRetries.Add(1)
			retriesAttempted++
		}
		if st != nil {
			st.NetworkAttempts.Add(1)
		}

		resp, err := hedged(ctx)

		// If the lifecycle context fired during the attempt, surface the
		// ctx cause directly instead of wrapping as retry-exhausted.
		if ctxErr := ctx.Err(); ctxErr != nil {
			cause := context.Cause(ctx)
			if cause != nil {
				return bestResp, cause
			}
			return bestResp, ctxErr
		}

		// If this is the last attempt OR shouldRetry says no, return.
		retryReason := ""
		if attempt+1 < maxAttempts {
			retryReason = e.shouldRetryWithReason(req, resp, err, attempt)
		}
		if attempt+1 >= maxAttempts || retryReason == "" {
			if err != nil && retriesAttempted > 0 {
				if bestResp != nil {
					return bestResp, nil
				}
				// Surface the most informative error: if later attempts
				// degenerated into ErrNoUpstreamsLeftToSelect (all
				// previously-tried upstreams were marked consumed),
				// prefer the first attempt's richer error.
				surfaceErr := err
				if common.HasErrorCode(err, common.ErrCodeNoUpstreamsLeftToSelect) && firstInformativeErr != nil {
					surfaceErr = firstInformativeErr
				}
				// eth_sendRawTransaction's execution-reverted is the
				// REAL answer (broadcasted but reverted) — operators
				// want the original error, not a retry-exhausted wrapper.
				method, _ := req.Method()
				if strings.EqualFold(method, "eth_sendRawTransaction") &&
					common.HasErrorCode(surfaceErr, common.ErrCodeEndpointExecutionException) {
					return nil, surfaceErr
				}
				return nil, common.NewErrFailsafeRetryExceeded(common.ScopeNetwork, surfaceErr, &startTime)
			}
			return resp, err
		}
		// Emit retry-reason metric (operators see WHY a retry fired).
		if req != nil && req.Network() != nil {
			method, _ := req.Method()
			finality := req.Finality(ctx)
			telemetry.MetricNetworkRetryAttemptTotal.WithLabelValues(
				req.Network().ProjectId(),
				req.NetworkLabel(),
				method,
				retryReason,
				finality.String(),
			).Inc()
		}
		lastErr = err
		// Capture the first informative error (anything other than the
		// degenerate ErrNoUpstreamsLeftToSelect / empty exhausted) so
		// later retry attempts can recover specific cause info on the
		// final wrap.
		if firstInformativeErr == nil && err != nil &&
			!common.HasErrorCode(err, common.ErrCodeNoUpstreamsLeftToSelect) {
			firstInformativeErr = err
		}
		if resp != nil {
			if bestResp != nil {
				bestResp.Release()
			}
			bestResp = resp
		}

		d := e.computeDelay(req, resp, err)
		if d > 0 {
			if serr := failsafe.SleepCtx(ctx, d); serr != nil {
				// SleepCtx returns ctx.Err(). Get the cause for typed wrapping.
				if cause := context.Cause(ctx); cause != nil {
					return bestResp, cause
				}
				return bestResp, serr
			}
		}
	}

	if lastErr != nil {
		if bestResp != nil {
			return bestResp, nil
		}
		return nil, common.NewErrFailsafeRetryExceeded(common.ScopeNetwork, lastErr, &startTime)
	}
	return bestResp, nil
}

// shouldRetry decides whether a (resp, err) outcome from `inner` should
// trigger another retry attempt. Returning true causes the caller to
// emit a `network_retry_attempt_total{reason}` metric.
func (e *networkExecutor) shouldRetry(req *common.NormalizedRequest, resp *common.NormalizedResponse, err error, attempt int) bool {
	return e.shouldRetryWithReason(req, resp, err, attempt) != ""
}

// shouldRetryWithReason returns the reason for retrying, or "" if no
// retry should fire. The reason becomes the `reason` label of the
// retry metric so operators can see which retry-path is busy.
func (e *networkExecutor) shouldRetryWithReason(req *common.NormalizedRequest, resp *common.NormalizedResponse, err error, attempt int) string {
	if err == nil && resp == nil {
		return ""
	}
	if req != nil && req.IsCompositeRequest() {
		return ""
	}
	if err != nil {
		if common.HasErrorCode(err, common.ErrCodeEndpointExecutionException) {
			if se, ok := err.(common.StandardError); ok {
				if retryable, ok := se.DeepSearch("retryableTowardNetwork").(bool); ok && retryable {
					return "execution_exception_retryable"
				}
			}
			return ""
		}
		if common.HasErrorCode(err, common.ErrCodeUpstreamBlockUnavailable) {
			return "block_unavailable"
		}
		if common.HasErrorCode(err, common.ErrCodeEndpointMissingData) {
			// MissingData = "the upstream doesn't have this data".
			// Respect the EXPLICIT RetryEmpty=false directive (caller
			// said "don't retry"). When the directive is unset, retry
			// — another upstream may have the data.
			if req != nil {
				if rds := req.Directives(); rds != nil && !rds.RetryEmpty {
					return ""
				}
			}
			return "missing_data"
		}
		if common.IsRetryableTowardNetwork(err) {
			return "retryable_error"
		}
		return ""
	}

	// resp != nil case — directive-based retry on empty results and pending
	// transactions.
	if resp == nil || resp.IsObjectNull() {
		return ""
	}
	if req == nil {
		return ""
	}
	rds := req.Directives()

	// RetryEmpty directive on emptyish responses.
	if rds != nil && rds.RetryEmpty {
		if resp.IsResultEmptyish() {
			// Respect EmptyResultMaxAttempts cap.
			if e.cfg != nil && e.cfg.Retry != nil && e.cfg.Retry.EmptyResultMaxAttempts > 0 {
				if attempt+1 >= e.cfg.Retry.EmptyResultMaxAttempts {
					return ""
				}
			}
			// If the method is in the empty-result-accept list, treat empty as valid.
			method, _ := req.Method()
			for _, m := range e.emptyResultAccept {
				if m == method {
					return ""
				}
			}
			return "empty_result"
		}
	}

	// RetryPending directive on tx-lookup methods retries to fish a
	// fresh upstream that has the tx confirmed (legacy heuristic:
	// retry tx-lookup methods until MaxAttempts).
	if rds != nil && rds.RetryPending {
		method, _ := req.Method()
		switch method {
		case "eth_getTransactionReceipt",
			"eth_getTransactionByHash",
			"eth_getTransactionByBlockHashAndIndex",
			"eth_getTransactionByBlockNumberAndIndex":
			return "pending_tx"
		}
	}

	return ""
}

func (e *networkExecutor) computeDelay(req *common.NormalizedRequest, resp *common.NormalizedResponse, err error) time.Duration {
	if e.cfg == nil || e.cfg.Retry == nil {
		return 0
	}
	cfg := e.cfg.Retry
	// Special-case delays: block-unavailable and empty-result delays
	// override normal backoff.
	if err != nil && common.HasErrorCode(err, common.ErrCodeUpstreamBlockUnavailable) {
		if e.dynamicBlockUnavailableDelay != nil {
			if d := e.dynamicBlockUnavailableDelay(); d > 0 {
				return d
			}
		}
		if fd := cfg.BlockUnavailableDelay.Duration(); fd > 0 {
			return fd
		}
	}
	// Empty-result / missing-data retries: block-time-relative delay (per-policy
	// EmptyResultDelayMultiplier) when configured and the block time is known,
	// else the fixed EmptyResultDelay. A not-yet-visible block/tx typically
	// appears within ~one block, so this waits about that long instead of a
	// hand-tuned constant — reusing the same EMA block-time the block-unavailable
	// delay already relies on.
	isEmptyResult := (resp != nil && !resp.IsObjectNull() && resp.IsResultEmptyish()) ||
		(err != nil && common.HasErrorCode(err, common.ErrCodeEndpointMissingData))
	if isEmptyResult {
		if m := cfg.EmptyResultDelayMultiplier; m != nil && *m > 0 && e.networkBlockTime != nil {
			if bt := e.networkBlockTime(); bt > 0 {
				return time.Duration(float64(bt) * *m)
			}
		}
		if ed := cfg.EmptyResultDelay.Duration(); ed > 0 {
			return ed
		}
	}
	// Default: exponential backoff using ComputeBackoff (caller supplies
	// the attempt index via a closure not exposed here; this function is
	// invoked from inside the retry loop where attempt is implicit).
	_ = req
	return failsafe.ComputeBackoff(cfg, 0)
}

func (e *networkExecutor) runHedge(
	ctx context.Context,
	req *common.NormalizedRequest,
	inner func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error),
) (*common.NormalizedResponse, error) {
	if e.cfg == nil || e.cfg.Hedge == nil || e.cfg.Hedge.MaxCount <= 0 {
		return inner(ctx, req)
	}
	if req != nil && req.IsCompositeRequest() {
		return inner(ctx, req)
	}
	// Write methods are not safe to hedge (non-idempotent broadcasts cause
	// duplicate side-effects). eth_sendRawTransaction has its own consensus
	// fan-out elsewhere.
	if req != nil {
		if m, _ := req.Method(); m != "" && evm.IsNonRetryableWriteMethod(m) {
			return inner(ctx, req)
		}
	}

	// Hedge delay is the unified AdaptiveDuration — scalar Base for fixed
	// delays, Quantile for adaptive timing, Min/Max for floor/ceiling.
	// ResolveForRequest looks up per-method latency via the network's
	// QuantileTracker; returns Base alone when no data is available
	// (cold start, no quantile, or no network on the request).
	spec := e.cfg.Hedge.Delay
	delayFn := func(idx int) time.Duration {
		return spec.ResolveForRequest(req)
	}
	var fireCount atomic.Int32
	wrapInner := func(hctx context.Context) (*common.NormalizedResponse, error) {
		idx := fireCount.Add(1)
		_ = idx // hedge tag could be carried via a typed context value
		return inner(hctx, req)
	}
	keep := func(r *common.NormalizedResponse, err error) bool {
		kept := false
		defer func() {
			if !kept || r == nil || r.Upstream() == nil {
				return
			}
			// Record the hedge-race winner. Operators use this to
			// detect skew: is one upstream consistently winning hedges?
			if req == nil || req.Network() == nil {
				return
			}
			method, _ := req.Method()
			finality := req.Finality(ctx)
			telemetry.MetricNetworkHedgeWinnerTotal.WithLabelValues(
				req.Network().ProjectId(),
				req.NetworkLabel(),
				r.Upstream().Id(),
				method,
				finality.String(),
			).Inc()
		}()
		if err != nil {
			// ErrNoUpstreamsLeftToSelect: this fan-out exhausted its share —
			// terminal for this leg, but the race continues if siblings have
			// not yet returned. Same applies for an empty ErrUpstreamsExhausted
			// (no upstreams ever tried) — treat as "this leg is done, but
			// siblings might still produce a result".
			if common.HasErrorCode(err, common.ErrCodeNoUpstreamsLeftToSelect) {
				return false
			}
			if uxe, ok := err.(*common.ErrUpstreamsExhausted); ok {
				if uxe.Upstreams() == nil || len(uxe.Upstreams()) == 0 {
					return false
				}
			}
			// Underlying-retryable wrapped errors (e.g. ErrUpstreamsExhausted
			// wrapping a 5xx) should continue racing for a healthier sibling.
			kept = !common.IsRetryableTowardNetwork(err)
			return kept
		}
		if r == nil || r.IsObjectNull(ctx) {
			return false
		}
		// Mirror the upstream-sweep empty-result policy so a fast
		// {"result": null} from one hedge leg does not cancel siblings
		// that may still return real data. When the method legitimately
		// returns empty (eth_getLogs, eth_call, point state reads, …)
		// the method is in emptyResultAccept and we keep the fast empty
		// winner — preserving prior behaviour.
		//
		// For methods like eth_getBlockByNumber / eth_getTransactionByHash /
		// eth_getTransactionReceipt, null means "this upstream does not
		// have it yet" (tip lag, reorg, pruned). Letting that null win
		// the hedge cancels the in-flight legs that could have returned
		// the data, then forces the retry layer to redo the whole fan-
		// out — amplifying latency on the cold path. Reject emptyish
		// here so the hedge keeps racing for a non-empty sibling; if all
		// legs finish empty the failsafe hedge falls through to the
		// last response, matching the pre-existing terminal behaviour.
		if r.IsResultEmptyish(ctx) {
			method, _ := req.Method()
			accepted := false
			for _, m := range e.emptyResultAccept {
				if m == method {
					accepted = true
					break
				}
			}
			if !accepted {
				return false
			}
		}
		kept = true
		return true
	}
	release := func(r *common.NormalizedResponse) {
		if r != nil {
			r.Release()
		}
	}
	hooks := failsafe.HedgeHooks{
		OnFire: func(fireIdx int, d time.Duration) {
			if st := req.ExecState(); st != nil {
				// Each hedge fire is an extra inner invocation at the
				// network scope: counts as both an attempt and a hedge.
				// Totals (Snapshot.Attempts / Hedges) sum across scopes.
				st.NetworkAttempts.Add(1)
				st.NetworkHedges.Add(1)
			}
		},
	}
	return failsafe.RunHedged[*common.NormalizedResponse](
		ctx, e.cfg.Hedge.MaxCount, delayFn, wrapInner, keep, release, hooks,
	)
}
