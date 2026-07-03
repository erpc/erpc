package erpc

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/architecture/svm"
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
) (*networkExecutor, error) {
	if cfg == nil {
		return &networkExecutor{
			method:                       "*",
			logger:                       logger,
			emptyResultAccept:            common.DefaultEmptyResultAccept(),
			dynamicBlockUnavailableDelay: dynamicBlockUnavailableDelay,
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

		d := e.computeDelay(req, resp, err, attempt)
		if d > 0 {
			// Attribute deliberate catch-up waits (data-not-yet-available
			// retries) so operators can see how much retry latency is chain
			// catch-up vs genuine-error failover. The count side is
			// network_retry_attempt_total{reason}; this is the duration side.
			if isDataUnavailableReason(retryReason) && req != nil && req.Network() != nil {
				method, _ := req.Method()
				telemetry.MetricNetworkDataUnavailableWaitSeconds.WithLabelValues(
					req.Network().ProjectId(),
					req.NetworkLabel(),
					method,
					retryReason,
					req.Finality(ctx).String(),
				).Observe(d.Seconds())
			}
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

// dataUnavailableCapReached reports whether the EmptyResultMaxAttempts cap — the
// single bound on retries when the requested data simply isn't on the upstream yet
// (empty/missing-data point-lookups, pending tx-lookups, and
// ErrUpstreamBlockUnavailable) — has been reached. It is intentionally separate
// from MaxAttempts, which governs genuine-error failover.
func (e *networkExecutor) dataUnavailableCapReached(attempt int) bool {
	if e.cfg == nil || e.cfg.Retry == nil {
		return false
	}
	limit := e.cfg.Retry.EmptyResultMaxAttempts
	return limit > 0 && attempt+1 >= limit
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
			if e.dataUnavailableCapReached(attempt) {
				return ""
			}
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
			if e.dataUnavailableCapReached(attempt) {
				return ""
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
			// Respect the shared "data not available yet" cap.
			if e.dataUnavailableCapReached(attempt) {
				return ""
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
			if e.dataUnavailableCapReached(attempt) {
				return ""
			}
			return "pending_tx"
		}
	}

	return ""
}

// isDataUnavailableReason reports whether a retry reason is a "block not on the
// upstream yet" catch-up wait rather than genuine-error failover. These are
// exactly the reasons that take computeDelay's block-time-relative delay path
// (isBlockUnavailable || isEmptyResult); their wall-clock cost is attributed to
// chain catch-up via network_data_unavailable_wait_seconds. pending_tx is
// deliberately excluded — it retries on exponential backoff, not the block-time
// catch-up delay, so recording it here would mislabel backoff as catch-up.
func isDataUnavailableReason(reason string) bool {
	switch reason {
	case "block_unavailable", "empty_result", "missing_data":
		return true
	default:
		return false
	}
}

func (e *networkExecutor) computeDelay(req *common.NormalizedRequest, resp *common.NormalizedResponse, err error, attempt int) time.Duration {
	if e.cfg == nil || e.cfg.Retry == nil {
		return 0
	}
	cfg := e.cfg.Retry
	// "Data not yet available" retries — a block/tx the upstream hasn't indexed
	// (ErrUpstreamBlockUnavailable), a point-lookup marked empty-as-missing
	// (ErrEndpointMissingData), or a plain emptyish result — all want the same
	// thing: wait about one block before retrying, since that's when the data
	// usually appears. Use the EMA-block-time-relative delay
	// (blockTime × BlockUnavailableDelayMultiplier) once it's warmed up, else the
	// relevant fixed fallback. One mechanism covers both cases; there is no
	// separate per-policy empty-result multiplier.
	isBlockUnavailable := err != nil && common.HasErrorCode(err, common.ErrCodeUpstreamBlockUnavailable)
	isEmptyResult := (resp != nil && !resp.IsObjectNull() && resp.IsResultEmptyish()) ||
		(err != nil && common.HasErrorCode(err, common.ErrCodeEndpointMissingData))
	if isBlockUnavailable || isEmptyResult {
		if e.dynamicBlockUnavailableDelay != nil {
			if d := e.dynamicBlockUnavailableDelay(); d > 0 {
				return d
			}
		}
		// Single fixed fallback before the block-time estimate warms up — covers
		// both empty/missing-data and block-unavailable (same root cause).
		if ed := cfg.EmptyResultDelay.Duration(); ed > 0 {
			return ed
		}
	}
	// Default: exponential backoff for genuine retryable errors, using the real
	// 0-based attempt index (attempt 0 = first retry). Previously hardcoded to 0,
	// which silently disabled backoffFactor / backoffMaxDelay on this path.
	_ = req
	return failsafe.ComputeBackoff(cfg, attempt)
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
	// fan-out elsewhere. SVM names are bare (sendTransaction, requestAirdrop),
	// so both architecture sets are checked.
	if req != nil {
		if m, _ := req.Method(); m != "" && (evm.IsNonRetryableWriteMethod(m) || svm.IsNonRetryableWriteMethod(m)) {
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
