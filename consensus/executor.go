package consensus

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strings"
	"sync"
	"time"

	"runtime/debug"
	"strconv"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/time/rate"
)

var (
	errNoJsonRpcResponse = errors.New("no json-rpc response available on result")
	errNotConsensusValid = errors.New("error is not consensus-valid")
	errPanicInConsensus  = errors.New("panic in consensus execution")
)

type metricsLabels struct {
	method      string
	category    string
	networkId   string
	projectId   string
	userId      string
	agentName   string
	finalityStr string
	// finality is the enum form of `finalityStr` — needed for tracker
	// writes (RecordUpstreamMisbehavior) which now stratify per
	// (method, finality) when the engine has opted in via
	// EnableFinalityTracking. Kept alongside the string form to avoid
	// re-parsing on the hot path.
	finality common.DataFinalityState
}

// participantInfo represents a single upstream's participation details in consensus
type participantInfo struct {
	upstreamId          string
	upstream            common.Upstream
	responseType        ResponseType
	responseHash        string
	responseSize        int
	responseBody        []byte // Full response content for debugging disputes
	errorMessage        string
	agreesWithConsensus bool
}

// ResponseType classifies the type of response for clear decision making.
type ResponseType int

const (
	ResponseTypeNonEmpty ResponseType = iota
	ResponseTypeEmpty
	ResponseTypeConsensusError
	ResponseTypeInfrastructureError
)

func (rt ResponseType) String() string {
	switch rt {
	case ResponseTypeNonEmpty:
		return "non_empty"
	case ResponseTypeEmpty:
		return "empty"
	case ResponseTypeConsensusError:
		return "consensus_error"
	case ResponseTypeInfrastructureError:
		return "infrastructure_error"
	default:
		return "unknown"
	}
}

// executor orchestrates the consensus fan-out: spawns N participant
// goroutines per slot, collects responses, hands them to the analyzer,
// and resolves the winner.
type executor struct {
	*consensusPolicy
}

// inner is the per-slot worker signature passed in by the network
// executor (or directly by *Consensus.Run).
type inner = func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error)

// execResult holds the result from a single upstream execution with cached analysis.
type execResult struct {
	Result   *common.NormalizedResponse
	Err      error
	Upstream common.Upstream

	// Cached values to avoid re-computation
	CachedHash         string
	CachedResponseType ResponseType
	CachedResponseSize int
	// Index of the attempt that produced this result
	Index int
}

// consensusOutcome is the atomic handoff from the analyzer goroutine to the
// caller's select. All fields must be fully populated before the send so the
// caller always receives a consistent snapshot.
type consensusOutcome struct {
	winner         *slotResult
	analysis       *consensusAnalysis
	shortCircuited bool
}

// Run is the main entry point for the consensus executor.
// It delegates to executeConsensus which decouples caller-visible
// latency from analysis completion (see runAnalyzer).
func (e *executor) Run(
	ctx context.Context,
	originalReq *common.NormalizedRequest,
	in inner,
) (*common.NormalizedResponse, error) {
	startTime := time.Now()

	if originalReq == nil {
		e.logger.Error().Msg("Unexpected nil request in consensus policy")
		return in(ctx, originalReq)
	}

	// Tag-based participant quota (opt-in). Front-load enough tag-matching
	// upstreams so the first maxParticipants drawn by the slots below
	// include the configured minimum from each required group. Runs at the
	// very top of consensus, before any participant slot consumes an
	// upstream (req.UpstreamIdx is still 0), so the reorder takes effect.
	// Best-effort: shortfalls fall through to lowParticipantsBehavior /
	// agreementThreshold like organic low participation.
	if len(e.config.requiredParticipants) > 0 {
		if reordered := reorderForParticipantQuota(originalReq.Upstreams(), e.config.requiredParticipants); len(reordered) > 0 {
			originalReq.SetUpstreams(reordered)
		}
	}

	// Gate hedge empty-as-slot-vote on the selected policy's empty-waiver
	// opt-in AND a blockEvidenceFields entry for THIS method. The waiver can
	// only prove outside-retention for methods with evidence; keeping empty
	// hedge winners for every other method would change prior recovery
	// behavior when the waiver can never fire.
	if st := originalReq.ExecState(); st != nil {
		keep := false
		if anyEmptyOutsideRetentionOptIn(e.config.requiredParticipants) {
			if method, err := originalReq.Method(); err == nil {
				// An existing key with an empty list cannot yield proof
				// (winnerBlockEvidence returns nil), so it must not keep
				// empty hedge-slot votes either.
				if fields, ok := methodFields(e.config.blockEvidenceFields, method); ok && len(fields) > 0 {
					keep = true
				}
			}
		}
		st.SetKeepEmptyConsensusSlot(keep)
	}

	labels := e.extractMetricsLabels(ctx, originalReq)
	ctx, consensusSpan := e.startConsensusSpan(ctx, labels)
	defer consensusSpan.End()

	lg := e.logger.With().
		Interface("id", originalReq.ID()).
		Str("component", "consensus").
		Str("networkId", labels.networkId).
		Logger()

	out := e.executeConsensus(
		ctx,
		&lg,
		originalReq,
		labels,
		in,
		startTime,
		consensusSpan,
	)
	if out == nil {
		return nil, nil
	}
	return out.Result, out.Error
}

// executeConsensus decouples two distinct concerns:
//
//  1. Caller-visible latency: the caller must return promptly when its context
//     is cancelled (HTTP disconnect, upstream deadline, shutdown).
//  2. Analysis completeness: misbehavior tracking and metrics must see every
//     participant's response, even ones that arrive after the caller gave up.
//
// The analyzer goroutine owns (2); the caller's select owns (1). They
// communicate through a single-buffered outcomeCh so neither side blocks the
// other. Analyzer lifetime is bounded by the slowest participant's lifetime,
// which is already bounded by failsafe policy timeouts and HTTP client
// timeouts — no new magic-number budget is required.
func (e *executor) executeConsensus(
	ctx context.Context,
	lg *zerolog.Logger,
	originalReq *common.NormalizedRequest,
	labels metricsLabels,
	in inner,
	startTime time.Time,
	consensusSpan trace.Span,
) *slotResult {
	ctx, collectionSpan := common.StartDetailSpan(ctx, "Consensus.CollectResponses")
	// NOTE: collectionSpan.End() is owned by runAnalyzer (in its deferred
	// cleanup), not this function. The analyzer outlives executeConsensus on
	// the caller-cancel path, and ending the span here would drop late
	// attributes (short_circuited, responses.collected).

	// For fire-and-forget mode, detach from parent context cancellation so background
	// requests continue even after the HTTP response is sent. This is critical for
	// transaction broadcasting where we want all nodes to receive the transaction.
	// For normal mode, inherit parent cancellation for proper resource cleanup.
	var baseCtx context.Context
	if e.config.fireAndForget {
		baseCtx = context.WithoutCancel(ctx)
	} else {
		baseCtx = ctx
	}

	cancellableCtx, cancelFunc := context.WithCancel(baseCtx)
	var cancelOnce sync.Once
	cancelRemaining := func() {
		cancelOnce.Do(cancelFunc)
	}

	// Cancel remaining requests on exit, unless fire-and-forget mode is enabled.
	// In fire-and-forget mode we want background requests to complete naturally.
	// sync.Once ensures cancel is called at most once even if called explicitly earlier.
	defer func() {
		if !e.config.fireAndForget {
			cancelRemaining()
		}
	}()

	// Spawn only as many participants as configured by policy
	maxToSpawn := e.maxParticipants
	if maxToSpawn <= 0 {
		maxToSpawn = 1
	}
	// Record on the request's ExecState that THIS request went through
	// the consensus executor and how many participants we spawned. Read
	// downstream by diagnostic surfaces (admin endpoints, response
	// headers, the simulator's lifecycle drawer) so operators can tell
	// "this request was a consensus race" from "this was a hedge race"
	// — the two look similar in the per-attempt log otherwise.
	if st := originalReq.ExecState(); st != nil {
		st.ConsensusSlots.Add(int32(maxToSpawn))
	}
	responseChan := make(chan *execResult, maxToSpawn)
	// Per-slot cancellable child contexts let us cancel losers explicitly.
	// Each slot inherits the shared cancellableCtx (which is cancelled
	// when the analyzer signals a winner / fire-and-forget exits / etc.)
	// AND has the request bound for downstream helpers that read
	// common.RequestContextKey.
	attemptCancels := make([]context.CancelFunc, maxToSpawn)
	for i := 0; i < maxToSpawn; i++ {
		slotCtx := context.WithValue(cancellableCtx, common.RequestContextKey, originalReq)
		// Mark the slot so upstream attempts made under it are attributed to
		// consensus fan-out (Reason = consensus_slot) in the attempt log,
		// the X-ERPC-Upstreams trace, and erpc_upstream_selection_total.
		slotCtx = common.WithConsensusSlot(slotCtx)
		slotCtx, attemptCancels[i] = context.WithCancel(slotCtx)
		go e.executeParticipant(slotCtx, lg, labels, in, originalReq, i, responseChan)
	}

	// outcomeCh is buffered so the analyzer can signal the caller and
	// continue to tracking/release without blocking on the caller still being
	// there. If the caller abandons on ctx.Done() before receiving, a drain
	// goroutine takes the buffered value and releases the winner.
	outcomeCh := make(chan consensusOutcome, 1)
	// analyzerDone closes when the analyzer has finished every read of the
	// winner (tracking, misbehavior export, releaseNonWinningResponses). The
	// abandon-path drain goroutine waits on this before releasing the winner
	// to avoid racing trackAndPunishMisbehavingUpstreams on winner.Result.
	analyzerDone := make(chan struct{})
	// Add before the goroutine starts. Add inside runAnalyzer races CloseAll's
	// Wait: shutdown can observe zero, close exporters, then the analyzer
	// exports into a closed file.
	inflightAnalyzers.Add(1)
	go e.runAnalyzer(
		ctx, lg, originalReq, labels,
		responseChan, attemptCancels, maxToSpawn, cancelRemaining,
		outcomeCh, analyzerDone, collectionSpan,
	)

	// Caller's select: prefer winner when available; bail on ctx cancel.
	select {
	case outcome := <-outcomeCh:
		e.recordMetricsAndTracing(originalReq, startTime, outcome.winner, outcome.analysis, labels, consensusSpan)
		return outcome.winner
	case <-ctx.Done():
		// Close the race where outcomeCh was sent to but Go's select picked
		// ctx.Done() first (both cases ready → random choice). Non-blocking
		// try-receive: if the analyzer already has a winner, take it.
		select {
		case outcome := <-outcomeCh:
			e.recordMetricsAndTracing(originalReq, startTime, outcome.winner, outcome.analysis, labels, consensusSpan)
			return outcome.winner
		default:
			// Caller abandoned before the analyzer published an outcome.
			// The analyzer is still going to publish exactly one outcome to
			// the (buffered) outcomeCh and then finish its own cleanup.
			// Since we will NOT return the winner up the stack, nobody else
			// will call winner.Result.Release() — the winner is explicitly
			// skipped by releaseNonWinningResponses. Leaking the winner
			// means leaking the response body/JSON-RPC buffer. Spawn a
			// drain goroutine that waits for the analyzer to finish reading
			// the winner (analyzerDone), then releases it.
			go e.drainAbandonedOutcome(outcomeCh, analyzerDone)
			return e.handleCallerAbandoned(lg, originalReq, labels, startTime, consensusSpan, ctx.Err())
		}
	}
}

// drainAbandonedOutcome releases the winner response when the caller has
// abandoned consensus before receiving. It waits for analyzerDone to be
// closed so analyzer-side reads of winner.Result (misbehavior tracking,
// buildMisbehaviorRecord, releaseNonWinningResponses skip-check) have
// completed before we free the underlying buffers.
func (e *executor) drainAbandonedOutcome(
	outcomeCh <-chan consensusOutcome,
	analyzerDone <-chan struct{},
) {
	outcome := <-outcomeCh
	<-analyzerDone
	if outcome.winner == nil {
		return
	}
	wr, ok := any(outcome.winner.Result).(*common.NormalizedResponse)
	if !ok || wr == nil {
		return
	}
	wr.Release()
}

// handleCallerAbandoned records caller-abandon metrics and returns an error
// result to the caller. The analyzer goroutine continues in the background
// and will emit MetricConsensusResponsesCollected / MetricConsensusShortCircuit
// plus run trackAndPunishMisbehavingUpstreams when all participants finish.
func (e *executor) handleCallerAbandoned(
	lg *zerolog.Logger,
	_ *common.NormalizedRequest,
	labels metricsLabels,
	startTime time.Time,
	consensusSpan trace.Span,
	cancelErr error,
) *slotResult {
	telemetry.MetricConsensusCancellations.
		WithLabelValues(labels.projectId, labels.networkId, labels.category, "caller_abandoned", labels.finalityStr, labels.userId, labels.agentName).
		Inc()
	telemetry.MetricConsensusTotal.
		WithLabelValues(labels.projectId, labels.networkId, labels.category, "caller_abandoned", labels.finalityStr, labels.userId, labels.agentName).
		Inc()
	telemetry.MetricConsensusDuration.
		WithLabelValues(labels.projectId, labels.networkId, labels.category, "caller_abandoned", labels.finalityStr, labels.userId, labels.agentName).
		Observe(time.Since(startTime).Seconds())
	common.SetTraceSpanError(consensusSpan, cancelErr)
	consensusSpan.SetAttributes(attribute.String("consensus.outcome", "caller_abandoned"))
	lg.Warn().Err(cancelErr).Msg("consensus caller abandoned; analysis continues in background")
	return &slotResult{Error: cancelErr}
}

// runAnalyzer owns all consensus work downstream of participant dispatch:
// collection, analysis, winner determination, misbehavior tracking, and
// response memory release. It always runs to completion regardless of caller
// context state.
//
// Its lifetime is bounded by the slowest participant's lifetime, which is
// itself bounded by the failsafe timeout policy and the HTTP client timeout
// (see clients/http_json_rpc_client.go). No new budget is introduced.
//
// INVARIANT: exactly one consensusOutcome is sent to outcomeCh before this
// function returns, so the caller's select never deadlocks. The deferred
// panic handler preserves this invariant.
func (e *executor) runAnalyzer(
	ctx context.Context,
	lg *zerolog.Logger,
	originalReq *common.NormalizedRequest,
	labels metricsLabels,
	responseChan <-chan *execResult,
	attemptCancels []context.CancelFunc,
	maxToSpawn int,
	cancelRemaining func(),
	outcomeCh chan<- consensusOutcome,
	analyzerDone chan<- struct{},
	collectionSpan trace.Span,
) {
	defer inflightAnalyzers.Done()

	outcomeSent := false
	sendOutcomeOnce := func(o consensusOutcome) {
		if outcomeSent {
			return
		}
		outcomeCh <- o // non-blocking: outcomeCh is buffered size 1
		outcomeSent = true
	}

	// analyzerDone is closed LAST (defers run LIFO). This signals to the
	// abandon-path drain goroutine that all analyzer-side reads of
	// winner.Result have completed and releasing it is now safe.
	defer close(analyzerDone)

	defer func() {
		// Recover from any panic before ending the span so a panic in
		// analysis doesn't leak the span and — critically — doesn't
		// deadlock the caller waiting on outcomeCh.
		if r := recover(); r != nil {
			lg.Error().
				Interface("panic", r).
				Str("stack", string(debug.Stack())).
				Msg("panic in consensus analyzer")
			telemetry.MetricConsensusPanics.
				WithLabelValues(labels.projectId, labels.networkId, labels.category, labels.finalityStr, labels.userId, labels.agentName).
				Inc()
			sendOutcomeOnce(consensusOutcome{
				winner: &slotResult{Error: errPanicInConsensus},
			})
		}
		collectionSpan.End()
	}()

	responses := make([]*execResult, 0, maxToSpawn)
	var winner *slotResult
	var analysis *consensusAnalysis
	var shortCircuitReason string
	shortCircuited := false
	waitCapped := false

	// Resolve the wait caps once per round. AdaptiveDuration.ResolveForRequest
	// looks up per-method latency quantiles via the request's network;
	// returns 0 when the spec is zero/nil or no data is available — the
	// arm-timer logic treats 0 as "no cap".
	maxWaitOnResult := e.config.maxWaitOnResult.ResolveForRequest(originalReq)
	maxWaitOnEmpty := e.config.maxWaitOnEmpty.ResolveForRequest(originalReq)

	// waitDeadline tracks the earliest of:
	//   - first-response-ever  + maxWaitOnEmpty   (only set when > 0)
	//   - first-non-empty-resp + maxWaitOnResult  (only set when > 0)
	// A zero value means "no cap, wait for every participant".
	var waitDeadline time.Time
	var waitTimer *time.Timer
	armTimer := func(d time.Time) {
		if d.IsZero() {
			return
		}
		if !waitDeadline.IsZero() && !d.Before(waitDeadline) {
			return
		}
		waitDeadline = d
		remaining := time.Until(waitDeadline)
		if remaining < 0 {
			remaining = 0
		}
		if waitTimer == nil {
			waitTimer = time.NewTimer(remaining)
		} else {
			if !waitTimer.Stop() {
				select {
				case <-waitTimer.C:
				default:
				}
			}
			waitTimer.Reset(remaining)
		}
	}
	timerC := func() <-chan time.Time {
		if waitTimer == nil {
			return nil
		}
		return waitTimer.C
	}
	considerWaitCap := func(resp *execResult) {
		if maxWaitOnEmpty <= 0 && maxWaitOnResult <= 0 {
			return
		}
		// Results that never reached an upstream (config-static skips like
		// ignored methods or shadow upstreams, and empty-cursor outcomes —
		// see isNoAttemptResult) are produced locally in microseconds and
		// carry no signal about how long the round's real participants
		// need. Arming the caps off them would start the countdown before
		// any live attempt exists: under fan-out configs where some
		// upstreams statically skip the method, the round would resolve
		// with only non-votable infrastructure errors while the real
		// participants are still in flight. The collection loop still
		// counts these results toward maxToSpawn, so rounds where every
		// participant skips terminate immediately without any cap.
		if isNoAttemptResult(resp) {
			return
		}
		// The caps mean "a usable answer is in hand; stragglers get this
		// much longer". With minAgreement quotas, a response set that
		// cannot yet satisfy the winner-composition quota is NOT a usable
		// answer — arming the countdown off it would resolve the round
		// before the required tagged upstream responds, converting every
		// such round into a retryable composition dispute. Hold arming
		// until the collected responses cover every quota tag with
		// DISTINCT upstreams (resultsSatisfyAgreementQuotas dedupes by ID
		// — the raw slice may hold the same upstream twice via hedge).
		// Slot timeouts and the overall request timeout still bound the
		// round. Deliberate ceiling: an errored or dissenting tagged
		// response counts as coverage. When several tagged upstreams are
		// in the round, an early tagged error/dissent can arm the cap and
		// time out a slower tagged sibling that would have completed the
		// quota — the failure is a retryable composition dispute, and the
		// alternative (hold caps until a tagged vote joins the WINNER)
		// would disable the caps on every genuine-disagreement round.
		if anyAgreementQuota(e.config.requiredParticipants) &&
			!resultsSatisfyAgreementQuotas(responses, e.config.requiredParticipants) {
			return
		}
		now := time.Now()
		// First response from an actual upstream attempt arms maxWaitOnEmpty.
		if maxWaitOnEmpty > 0 && waitDeadline.IsZero() {
			armTimer(now.Add(maxWaitOnEmpty))
		}
		// A non-empty result arms (or tightens) maxWaitOnResult.
		if maxWaitOnResult > 0 && resp != nil && resp.Err == nil &&
			resp.Result != nil && !resp.Result.IsResultEmptyish(ctx) {
			armTimer(now.Add(maxWaitOnResult))
		}
	}

	// Collect responses. Every participant is guaranteed to write exactly
	// once to responseChan (see executeParticipant: all exit paths + panic
	// recovery write; channel is buffered to maxToSpawn so writes never block).
	for i := 0; i < maxToSpawn; i++ {
		var resp *execResult
		select {
		case resp = <-responseChan:
		case <-timerC():
			// Wait cap fired — resolve with what we have.
			waitCapped = true
			if !e.config.fireAndForget {
				cancelRemaining()
				for ai := range attemptCancels {
					if attemptCancels[ai] != nil {
						attemptCancels[ai]()
					}
				}
			}
		}
		if waitCapped {
			break
		}
		if resp == nil {
			continue
		}

		if shortCircuited {
			// Analysis is frozen at the short-circuit moment. Any response
			// that arrives after is NOT in analysis.groups, so
			// releaseNonWinningResponses won't cover it. Release it here.
			if resp.Result != nil {
				resp.Result.Release()
			}
			continue
		}

		responses = append(responses, resp)
		considerWaitCap(resp)

		analysis = newConsensusAnalysis(e.logger, ctx, e.config, responses)
		winner = e.determineWinner(ctx, lg, analysis)
		if reason, ok := e.shouldShortCircuit(winner, analysis); ok {
			shortCircuited = true
			shortCircuitReason = reason

			markWinningParticipants(originalReq, winner, analysis)
			// Release caller immediately. The winner won't change even if
			// more responses arrive.
			sendOutcomeOnce(consensusOutcome{winner: winner, analysis: analysis, shortCircuited: true})

			if e.config.fireAndForget {
				lg.Debug().
					Str("reason", reason).
					Int("remaining", maxToSpawn-i-1).
					Msg("fire-and-forget mode: remaining requests complete in background")
			} else {
				cancelRemaining()
				for ai := range attemptCancels {
					if attemptCancels[ai] != nil {
						attemptCancels[ai]()
					}
				}
			}
		}
	}
	if waitTimer != nil {
		waitTimer.Stop()
	}

	// All participants accounted for, or wait-cap returned early with a
	// partial bag. If no short-circuit already released the caller,
	// compute/recompute the final winner now.
	//
	// Waivers stay fail-closed on wait-cap: cancelling remaining contexts
	// is not a round-completion barrier (a result may already be buffered,
	// or the upstream call may ignore cancel — wait-cap tests intentionally
	// use such calls). Sealing a partial bag could waive MissingData /
	// empty-outside-retention while an omitted tagged response still holds
	// non-empty data. Only a fully collected bag (every slot wrote) makes
	// hasRemaining false without an explicit seal.
	if !shortCircuited {
		analysis = newConsensusAnalysis(e.logger, ctx, e.config, responses)
		winner = e.determineWinner(ctx, lg, analysis)
		markWinningParticipants(originalReq, winner, analysis)
	}
	sendOutcomeOnce(consensusOutcome{winner: winner, analysis: analysis, shortCircuited: shortCircuited})

	// Emit collection-phase attributes and metrics. These run after the
	// outcome has been sent, so they don't block the caller.
	collectionSpan.SetAttributes(
		attribute.Bool("short_circuited", shortCircuited),
		attribute.Bool("wait_capped", waitCapped),
		attribute.Int("responses.collected", len(responses)),
	)

	vendorNames := []string{}
	for _, response := range responses {
		if response != nil && response.Upstream != nil {
			vendorNames = append(vendorNames, response.Upstream.VendorName())
		}
	}
	sort.Strings(vendorNames)

	telemetry.MetricConsensusResponsesCollected.
		WithLabelValues(
			labels.projectId,
			labels.networkId,
			labels.category,
			strings.Join(vendorNames, ","),
			strconv.FormatBool(shortCircuited),
			labels.finalityStr,
			labels.userId,
			labels.agentName,
		).
		Observe(float64(len(responses)))
	if shortCircuited {
		reason := shortCircuitReason
		if reason == "" {
			reason = "unknown"
		}
		telemetry.MetricConsensusShortCircuit.
			WithLabelValues(labels.projectId, labels.networkId, labels.category, reason, labels.finalityStr, labels.userId, labels.agentName).
			Inc()
	}
	if waitCapped {
		// Trigger label distinguishes which cap fired: maxWaitOnResult
		// (at least one non-empty in the bag) vs maxWaitOnEmpty (only
		// empty/error responses so far).
		trigger := "empty"
		for _, r := range responses {
			if r != nil && r.Err == nil && r.Result != nil && !r.Result.IsResultEmptyish(ctx) {
				trigger = "result"
				break
			}
		}
		telemetry.MetricConsensusWaitCapped.
			WithLabelValues(labels.projectId, labels.networkId, labels.category, trigger, labels.finalityStr, labels.userId, labels.agentName).
			Inc()
	}

	// Track misbehavior with the final winner + analysis. Previously this
	// ran synchronously in Apply(). Moving it here guarantees it sees every
	// response, even ones that arrived after the caller abandoned.
	e.trackAndPunishMisbehavingUpstreams(lg, originalReq, labels, winner, analysis)

	// Release non-winning response objects. Previously inlined in Apply().
	e.releaseNonWinningResponses(responses, winner)
}

// releaseNonWinningResponses releases the Result pointers on every non-winning
// execResult collected this round. It iterates the raw responses slice (not
// analysis.groups) so responses dropped by upstream-deduplication are released
// too — they never appear in any group.
func (e *executor) releaseNonWinningResponses(
	responses []*execResult,
	winner *slotResult,
) {
	var winnerResp *common.NormalizedResponse
	if winner != nil {
		if wr, ok := any(winner.Result).(*common.NormalizedResponse); ok {
			winnerResp = wr
		}
	}
	for _, result := range responses {
		if result != nil && result.Result != nil && result.Result != winnerResp {
			result.Result.Release()
		}
	}
}

// isNoAttemptResult reports whether a participant's result was produced
// without any network call to an upstream: config-static skips (method
// ignored/not allowed, shadow upstreams, syncing nodes, use-upstream
// directive mismatch) and empty-cursor outcomes (no upstreams left to
// select, exhausted lists where every recorded error is itself a skip).
// Such results are decided locally in microseconds, so they say nothing
// about how long the round's real participants need — the wait caps must
// not be armed off them. Genuine attempt failures (timeouts, 5xx,
// connection resets) are NOT no-attempt: they prove the round is live and
// should keep arming the caps.
func isNoAttemptResult(r *execResult) bool {
	if r == nil {
		return true
	}
	if r.Err == nil {
		return false
	}
	return isNoAttemptError(r.Err)
}

func isNoAttemptError(err error) bool {
	se, ok := err.(common.StandardError)
	if !ok {
		return false
	}
	base := se.Base()
	if base == nil {
		return false
	}
	switch base.Code {
	case common.ErrCodeUpstreamRequestSkipped,
		common.ErrCodeUpstreamMethodIgnored,
		common.ErrCodeUpstreamShadowing,
		common.ErrCodeUpstreamSyncing,
		common.ErrCodeUpstreamNotAllowed,
		common.ErrCodeNoUpstreamsLeftToSelect:
		return true
	case common.ErrCodeUpstreamsExhausted:
		// Wrapper: the verdict follows the recorded child errors. No
		// children at all means no upstream was ever tried.
		cause := se.GetCause()
		if cause == nil {
			return true
		}
		return isNoAttemptCause(cause)
	case common.ErrCodeFailsafeRetryExceeded:
		// Wrapper: retry-exceeded always carries the last attempt's error;
		// the verdict follows it.
		cause := se.GetCause()
		if cause == nil {
			return false
		}
		return isNoAttemptCause(cause)
	}
	return false
}

// isNoAttemptCause unwraps a wrapper's cause, which may be an errors.Join
// multi-error (e.g. ErrUpstreamsExhausted joining ErrorsByUpstream): ALL
// children must be no-attempt for the wrapper to count as no-attempt — a
// single real attempt failure means the round is live.
func isNoAttemptCause(cause error) bool {
	if multi, ok := cause.(interface{ Unwrap() []error }); ok {
		children := multi.Unwrap()
		if len(children) == 0 {
			return true
		}
		for _, child := range children {
			if !isNoAttemptError(child) {
				return false
			}
		}
		return true
	}
	return isNoAttemptError(cause)
}

// executeParticipant runs a single upstream request within a goroutine.
func (e *executor) executeParticipant(
	ctx context.Context,
	lg *zerolog.Logger,
	labels metricsLabels,
	in inner,
	req *common.NormalizedRequest,
	index int,
	responseChan chan<- *execResult,
) {
	// Panic recovery
	defer func() {
		if r := recover(); r != nil {
			lg.Error().
				Interface("panic", r).
				Int("index", index).
				Str("stack", string(debug.Stack())).
				Msg("Panic in consensus participant")
			telemetry.MetricConsensusPanics.WithLabelValues(labels.projectId, labels.networkId, labels.category, labels.finalityStr, labels.userId, labels.agentName).Inc()
			responseChan <- &execResult{Err: errPanicInConsensus}
		}
	}()

	// Check for cancellation before execution
	if ctx.Err() != nil {
		telemetry.MetricConsensusCancellations.
			WithLabelValues(labels.projectId, labels.networkId, labels.category, "before_execution", labels.finalityStr, labels.userId, labels.agentName).
			Inc()
		responseChan <- nil
		return
	}

	// Execute the slot inner — returns (response, error) directly.
	respObj, respErr := in(ctx, req)

	// Track post-execution cancellations for observability, but do NOT discard
	// the result. The result is still valid and should participate in
	// consensus analysis.
	if ctx.Err() != nil {
		telemetry.MetricConsensusCancellations.
			WithLabelValues(labels.projectId, labels.networkId, labels.category, "after_execution", labels.finalityStr, labels.userId, labels.agentName).
			Inc()
	}

	if respObj == nil && respErr == nil {
		responseChan <- nil
		return
	}

	var upstream common.Upstream
	if respObj != nil {
		upstream = respObj.Upstream()
	}
	if upstream == nil && respErr != nil {
		var uae interface{ Upstream() common.Upstream }
		if errors.As(respErr, &uae) {
			upstream = uae.Upstream()
		}
		var uxe *common.ErrUpstreamsExhausted
		if errors.As(respErr, &uxe) {
			if ups := uxe.Upstreams(); len(ups) > 0 {
				upstream = ups[0]
			}
		}
	}

	responseChan <- &execResult{
		Result:   respObj,
		Err:      respErr,
		Upstream: upstream,
		Index:    index,
	}
}

// shouldShortCircuit decides if remaining requests can be safely cancelled.
// This happens if one group's lead over the second-place group is greater
// than the number of remaining responses.
func (e *executor) shouldShortCircuit(winner *slotResult, analysis *consensusAnalysis) (string, bool) {
	// A composition dispute is provisional while more responses can still
	// arrive: a later response may join the leading group (or grow another
	// group) and satisfy the minAgreement quota. Never cancel remaining
	// participants because of it — the final pass after collection decides.
	if winner != nil && winner.Error != nil && analysis.hasRemaining() &&
		common.HasErrorCode(winner.Error, common.ErrCodeConsensusCompositionDispute) {
		return "", false
	}
	for _, rule := range shortCircuitRules {
		if rule.Condition(winner, analysis) {
			return rule.Reason, true
		}
	}
	return "", false
}

// determineWinner applies configured policies to the analysis to produce a final result.
// It uses a rules-based approach for clear, maintainable decision logic.
// markWinningParticipants flags every UpstreamAttempt whose response
// landed in the winning consensus group as Won. Operators see these in
// the response headers / spans as `:won` — multiple participants when
// agreement was reached, none when a dispute resolved without one.
// Safe to call when winner/analysis is nil (no-op).
func markWinningParticipants(req *common.NormalizedRequest, winner *slotResult, analysis *consensusAnalysis) {
	if req == nil || winner == nil || winner.Result == nil || analysis == nil {
		return
	}
	st := req.ExecState()
	if st == nil {
		return
	}
	winnerResp, ok := any(winner.Result).(*common.NormalizedResponse)
	if !ok || winnerResp == nil {
		return
	}
	// Find the group that contains the winning response; every member of
	// that group voted with the winner and counts as a contributor.
	var winningGroup *responseGroup
	for _, group := range analysis.groups {
		for _, result := range group.Results {
			if result != nil && result.Result == winnerResp {
				winningGroup = group
				break
			}
		}
		if winningGroup != nil {
			break
		}
	}
	if winningGroup == nil {
		return
	}
	for _, result := range winningGroup.Results {
		if result == nil || result.Upstream == nil {
			continue
		}
		st.MarkUpstreamAttemptWon(result.Upstream.Id())
	}
}

func (e *executor) determineWinner(ctx context.Context, lg *zerolog.Logger, analysis *consensusAnalysis) *slotResult {
	// Since we know R is *common.NormalizedResponse at runtime, we can safely work with it
	// Evaluate rules in priority order
	for _, rule := range consensusRules {
		// We need to check the condition with the proper type
		// Since the rules are defined for *common.NormalizedResponse, we need to handle this carefully
		if rule.Condition(analysis) {
			lg.Debug().
				Str("rule", rule.Description).
				Msg("consensus rule matched")
			return e.enforceWinnerComposition(ctx, lg, analysis, rule.Action(analysis))
		}
	}

	// Ultimate fallback (should never reach here due to no-winner rule)
	lg.Error().Msg("no consensus rule matched - using fallback")
	return &slotResult{
		Error: common.NewErrConsensusDispute("no consensus rule matched", nil, nil),
	}
}

// enforceWinnerComposition applies the winner-composition quotas
// (`requiredParticipants[].minAgreement`) to the winner produced by the
// rules engine. This is the single enforcement point: every rule's output
// flows through here, so no individual rule needs to be composition-aware.
//
//   - Opt-in: no-op unless some entry sets minAgreement > 0.
//   - eth_sendRawTransaction is exempt: a broadcast accepted by any node
//     propagates network-wide, so winner composition proves nothing there
//     (mirrors the dedicated first-success rule/short-circuit).
//   - Synthesized winners (dispute/low-participants errors) and
//     infrastructure-error groups pass through: they never assert data
//     correctness, and converting one error into another would only mask
//     the original failure.
//   - A failing winner becomes ErrConsensusCompositionDispute. While
//     responses are still outstanding the dispute is provisional — see the
//     guard in shouldShortCircuit — because a later response can still
//     complete the quota.
func (e *executor) enforceWinnerComposition(ctx context.Context, lg *zerolog.Logger, analysis *consensusAnalysis, winner *slotResult) *slotResult {
	if winner == nil || !anyAgreementQuota(e.config.requiredParticipants) {
		return winner
	}
	if isTxBroadcastMethod(analysis.method) {
		return winner
	}
	g := analysis.groupOf(winner)
	if g == nil || g.ResponseType == ResponseTypeInfrastructureError {
		// A synthesized winner (dispute/low-participants error) normally passes
		// through untouched. But the rules engine synthesizes ErrConsensusDispute
		// for the default mixed-node tie — a lone non-empty group tying an
		// equally-sized empty group at agreementThreshold (e.g. 2 empty internals
		// vs 2 agreeing archives) — and that dispute never reaches the waiver
		// machinery below because it has no backing group. When the empty side is
		// a unanimous, proven-outside-retention abstention, the lone non-empty
		// group is the real winner; rescue it here. Every waiver guard still
		// applies, so this cannot mask a genuine non-empty disagreement.
		if winner.Error != nil {
			if promoted := e.promoteAbstentionWinner(ctx, lg, analysis); promoted != nil {
				return promoted
			}
		}
		return winner
	}
	agreeing := e.agreeingResults(analysis, g)
	if resultsSatisfyAgreementQuotas(agreeing, e.config.requiredParticipants) {
		return winner
	}
	// Round-complete waivers (safety net). A quota entry can opt into releasing
	// its minAgreement for THIS round under one of two abstention signals:
	// missing-data (≥ minAgreement tag-matching participants returned
	// ErrEndpointMissingData) or empty-outside-retention (≥ minAgreement
	// tag-matching participants returned empty AND the winner's block is
	// older than the entry's retained range, proving those abstainers
	// legitimately pruned it). Both are round-complete by design and only
	// change the outcome when releasing them makes the winner decisive, so
	// neither can mask a real disagreement.
	if reasons := e.waivedQuotas(ctx, analysis, g); len(reasons) > 0 {
		waived := make(map[*common.ConsensusRequiredParticipant]struct{}, len(reasons))
		for req := range reasons {
			waived[req] = struct{}{}
		}
		if resultsSatisfyAgreementQuotasExcept(agreeing, e.config.requiredParticipants, waived) {
			e.recordCompositionWaivers(lg, analysis, agreeing, reasons)
			return winner
		}
	}
	// A quota tag matching zero participants in the ENTIRE round (not just
	// the winning group) means the config is structurally unable to ever
	// satisfy the quota right now — a typo'd tag or every tagged upstream
	// down. That is an outage, not a routine dispute: escalate to Warn so
	// operators see it without debug logging. Only when the round is
	// complete (nothing can still arrive): this gate also runs on every
	// mid-collection analysis, where a slower tagged upstream simply hasn't
	// answered yet — warning there would fire on every healthy
	// mixed-latency round.
	for _, req := range e.config.requiredParticipants {
		if req == nil || req.MinAgreement <= 0 {
			continue
		}
		matchedAnywhere := false
		for _, og := range analysis.groups {
			for _, r := range og.Results {
				if r != nil && r.Upstream != nil && upstreamMatchesTag(r.Upstream, req.Tag) {
					matchedAnywhere = true
					break
				}
			}
			if matchedAnywhere {
				break
			}
		}
		if !matchedAnywhere && !analysis.hasRemaining() {
			lg.Warn().
				Str("tag", req.Tag).
				Int("minAgreement", req.MinAgreement).
				Msg("minAgreement quota tag matched ZERO participants this round — check for a typo'd tag or unavailable tagged upstreams; consensus cannot succeed while this persists")
		}
	}
	// When the empty-outside-retention waiver is opted in but did NOT fire, the
	// two silent fail-closed branches are a zero chain head (network tip and
	// participant pollers both unknown) or a nil winner block (blockEvidenceFields
	// missing/unmatched for this method) — both leave the waiver without proof.
	// Surface them so a dispute on a supposedly-pruned block is diagnosable
	// without code changes, rather than looking like an unexplained composition
	// failure.
	e.logEmptyWaiverUnproven(ctx, lg, analysis, g)
	lg.Debug().
		Str("hash", g.Hash).
		Int("count", g.Count).
		Msg("winning group does not satisfy minAgreement composition quotas")
	return &slotResult{
		Error: common.NewErrConsensusCompositionDispute(
			"winning group does not satisfy requiredParticipants minAgreement quotas",
			analysis.participants(),
			nil,
		),
	}
}

// promoteAbstentionWinner rescues a synthesized dispute (no backing group)
// when opted-in abstainers are the sole reason no winner emerged. The rules
// engine synthesizes ErrConsensusDispute when a non-empty group ties with an
// equally-sized empty or ConsensusError (MissingData) group at
// agreementThreshold — the default mixed-node shape (e.g. 2 empty/MissingData
// internals tying 2 agreeing archives). That dispute never reaches
// enforceWinnerComposition's waiver machinery because groupOf(winner) is nil,
// so the waivers could never fire on the exact case they were built for.
//
// It returns a promoted winner only when EVERY guard passes:
//   - opt-in present (empty-outside-retention and/or MissingData), so configs
//     without either waiver are completely unaffected;
//   - exactly ONE non-empty group meets agreementThreshold — two disagreeing
//     non-empty groups at threshold is a real dispute, so keep it;
//   - the waiver fires for that candidate (waivedQuotas), which reuses the same
//     round-complete + block-proof + minAgreement-empty/MissingData guards, so
//     mid-round or without proof this returns nil and the dispute stays
//     provisional;
//   - releasing the waived quotas makes the candidate satisfy composition.
//
// On success the candidate's own quotas are recorded as waived and its largest
// result becomes the winner. Otherwise it returns nil and the caller keeps the
// provisional dispute unchanged.
func (e *executor) promoteAbstentionWinner(ctx context.Context, lg *zerolog.Logger, analysis *consensusAnalysis) *slotResult {
	if !anyEmptyOutsideRetentionOptIn(e.config.requiredParticipants) &&
		!anyMissingDataWaiverOptIn(e.config.requiredParticipants) {
		return nil
	}
	var cand *responseGroup
	for _, g := range analysis.groups {
		if g == nil || g.ResponseType != ResponseTypeNonEmpty || g.Count < e.config.agreementThreshold {
			continue
		}
		if cand != nil {
			// More than one non-empty group meets threshold: a genuine
			// disagreement between data answers, not an abstention standoff.
			return nil
		}
		cand = g
	}
	if cand == nil {
		return nil
	}
	reasons := e.waivedQuotas(ctx, analysis, cand)
	if len(reasons) == 0 {
		e.logEmptyWaiverUnproven(ctx, lg, analysis, cand)
		return nil
	}
	// A waiver that fires for an unrelated abstention must not promote past a
	// competing at-threshold group that is not itself a covered abstention
	// (e.g. MissingData on a fifth upstream must not mask a tied execution
	// revert vs NonEmpty standoff).
	if !competingAtThresholdGroupsCoveredByWaiver(analysis, cand, e.config.agreementThreshold, reasons) {
		return nil
	}
	waived := make(map[*common.ConsensusRequiredParticipant]struct{}, len(reasons))
	for req := range reasons {
		waived[req] = struct{}{}
	}
	agreeing := e.agreeingResults(analysis, cand)
	if !resultsSatisfyAgreementQuotasExcept(agreeing, e.config.requiredParticipants, waived) {
		return nil
	}
	e.recordCompositionWaivers(lg, analysis, agreeing, reasons)
	lg.Debug().
		Str("hash", cand.Hash).
		Int("count", cand.Count).
		Msg("promoted lone non-empty group over proven-outside-retention empty abstention")
	return &slotResult{Result: cand.LargestResult}
}

// competingAtThresholdGroupsCoveredByWaiver reports whether every response
// group other than cand that meets agreementThreshold is an abstention the
// fired waiver covers: Empty under empty-outside-retention, or ConsensusError
// whose every result is ErrEndpointMissingData under the MissingData waiver.
// An at-threshold execution-revert ConsensusError (or any other shape) blocks
// promotion so an unrelated MissingData waiver cannot mask it.
func competingAtThresholdGroupsCoveredByWaiver(
	analysis *consensusAnalysis,
	cand *responseGroup,
	threshold int,
	reasons map[*common.ConsensusRequiredParticipant]string,
) bool {
	hasMissingData := false
	hasEmpty := false
	for _, reason := range reasons {
		switch reason {
		case waiverReasonMissingData:
			hasMissingData = true
		case waiverReasonEmptyOutsideRetention:
			hasEmpty = true
		}
	}
	for _, g := range analysis.groups {
		if g == nil || g == cand || g.Count < threshold {
			continue
		}
		switch g.ResponseType {
		case ResponseTypeEmpty:
			if !hasEmpty {
				return false
			}
		case ResponseTypeConsensusError:
			if !hasMissingData || !groupIsMissingDataAbstention(g) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func groupIsMissingDataAbstention(g *responseGroup) bool {
	if g == nil || len(g.Results) == 0 {
		return false
	}
	for _, r := range g.Results {
		if r == nil || r.Err == nil || !common.HasErrorCode(r.Err, common.ErrCodeEndpointMissingData) {
			return false
		}
	}
	return true
}

// missingDataWaivedQuotas returns the requiredParticipants entries whose
// minAgreement quota is waived for THIS round under the MissingData waiver.
// An entry qualifies when it opts in (waiveAgreementOnMissingData), carries a
// real quota (minAgreement > 0), at least minAgreement distinct
// tag-matching participants this round returned ErrEndpointMissingData, and
// no matching participant returned non-empty data. Sibling errors (transport, …)
// and empty successes do not block the count; a matching non-empty value vote
// does — that is disagreement, not abstention, and waiving would mask it.
//
// The waiver is round-complete by design: while any response can still arrive
// (analysis.hasRemaining) a slower tagged upstream could still return data, so
// this reports no waivers and the caller keeps the quota strict. Wait-cap
// cancel alone does not clear hasRemaining — only a fully collected bag or an
// explicit seal after every slot acknowledged does. It scans all groups (the
// entire round), not just the winning group.
func missingDataWaivedQuotas(analysis *consensusAnalysis, reqs []*common.ConsensusRequiredParticipant) map[*common.ConsensusRequiredParticipant]struct{} {
	if analysis == nil || analysis.hasRemaining() {
		return nil
	}
	var waived map[*common.ConsensusRequiredParticipant]struct{}
	for _, req := range reqs {
		if req == nil || !req.WaiveAgreementOnMissingData || req.MinAgreement <= 0 {
			continue
		}
		missing := 0
		var seen map[string]struct{}
		dataMatch := false
		for _, g := range analysis.groups {
			for _, r := range g.Results {
				if r == nil || r.Upstream == nil || !upstreamMatchesTag(r.Upstream, req.Tag) {
					continue
				}
				id := r.Upstream.Id()
				if seen == nil {
					seen = make(map[string]struct{}, req.MinAgreement)
				}
				if _, dup := seen[id]; dup {
					continue
				}
				seen[id] = struct{}{}
				// Only a non-empty value vote holds the quota (feature §7.1).
				// Empty successes are a separate abstention shape handled by
				// waiveAgreementOnEmptyOutsideRetention — they must not block
				// MissingData waiver when ≥ minAgreement MissingData replies exist.
				if r.Err == nil && r.CachedResponseType == ResponseTypeNonEmpty {
					dataMatch = true
					continue
				}
				if r.Err == nil {
					continue
				}
				if !common.HasErrorCode(r.Err, common.ErrCodeEndpointMissingData) {
					continue
				}
				missing++
			}
		}
		if dataMatch || missing < req.MinAgreement {
			continue
		}
		if waived == nil {
			waived = make(map[*common.ConsensusRequiredParticipant]struct{}, len(reqs))
		}
		waived[req] = struct{}{}
	}
	return waived
}

// recordCompositionWaivers logs and counts each waiver that actually fired —
// i.e. an entry that was waived AND would have failed its quota strictly, so
// releasing it is what made the winner decisive. Entries that were satisfied
// anyway are skipped so the metric reflects real waiver events only.
func (e *executor) recordCompositionWaivers(
	lg *zerolog.Logger,
	analysis *consensusAnalysis,
	agreeing []*execResult,
	waived map[*common.ConsensusRequiredParticipant]string,
) {
	projectId, networkId := "", ""
	if analysis != nil && analysis.originalRequest != nil {
		networkId = analysis.originalRequest.NetworkLabel()
		if net := analysis.originalRequest.Network(); net != nil {
			projectId = net.ProjectId()
		}
	}
	for req, reason := range waived {
		if resultsSatisfyAgreementQuotas(agreeing, []*common.ConsensusRequiredParticipant{req}) {
			continue // quota was met on its own; the waiver was not needed for it
		}
		lg.Info().
			Str("tag", req.Tag).
			Int("minAgreement", req.MinAgreement).
			Str("reason", reason).
			Msg("waived requiredParticipants minAgreement quota")
		telemetry.MetricConsensusCompositionWaived.WithLabelValues(projectId, networkId, req.Tag, reason).Inc()
	}
}

// waiverReason* are the reason labels reported on
// consensus_composition_waived_total{reason} and the waiver log line.
const (
	waiverReasonMissingData           = "missing_data"
	waiverReasonEmptyOutsideRetention = "empty_outside_retention"
)

// waivedQuotas resolves the set of requiredParticipants entries whose
// minAgreement quota is waived for THIS round, mapping each to the reason it
// was released. It merges the two round-complete waivers; a missing-data
// waiver takes precedence in the (mutually-exclusive in practice) case an
// entry qualifies for both, since it is the stronger explicit abstention signal.
func (e *executor) waivedQuotas(ctx context.Context, analysis *consensusAnalysis, g *responseGroup) map[*common.ConsensusRequiredParticipant]string {
	reasons := make(map[*common.ConsensusRequiredParticipant]string)
	for req := range emptyOutsideRetentionWaivedQuotas(analysis, e.config.requiredParticipants, e.winnerBlockEvidence(analysis, g), e.chainHead(ctx, analysis)) {
		reasons[req] = waiverReasonEmptyOutsideRetention
	}
	for req := range missingDataWaivedQuotas(analysis, e.config.requiredParticipants) {
		reasons[req] = waiverReasonMissingData
	}
	return reasons
}

// chainHead returns the observed EVM "latest" block for the round, or 0 when
// unknown. The network served tip is used only when majority served-tip mode
// is on for "latest" (`evm.servedTip.enabledFor` includes that tag) — that
// value is majority-corroborated across eligible upstreams, including ones
// that did not participate in this round. Default max mode is a plain MAX of
// self-reported poller latests, so a single inflator can raise it; trusting
// that for this proof would reopen the fabrication hole the poller fallback
// closes. When majority mode is off, or the majority tip is still 0, fall
// back to the MIN poller latest among this round's own EVM participants
// (at least two distinct upstreams with a positive latest). A zero head
// from both sources means the empty-outside-retention waiver cannot prove
// a block is outside range, so it holds the quota — fail closed.
func (e *executor) chainHead(ctx context.Context, analysis *consensusAnalysis) int64 {
	if analysis == nil {
		return 0
	}
	if analysis.originalRequest != nil {
		if net := analysis.originalRequest.Network(); net != nil && majorityServedLatestEnabled(net) {
			if head := common.EvmHighestLatestBlockNumber(net, ctx); head > 0 {
				return head
			}
		}
	}
	return participantLatestBlock(analysis)
}

// majorityServedLatestEnabled reports whether this network's served "latest"
// is the majority-mode tip. Nil config / default max mode returns false.
func majorityServedLatestEnabled(net common.Network) bool {
	if net == nil {
		return false
	}
	cfg := net.Config()
	if cfg == nil || cfg.Evm == nil {
		return false
	}
	return cfg.Evm.ServedTipEnabledFor("latest")
}

// logEmptyWaiverUnproven emits the fail-closed diagnostic when the
// empty-outside-retention waiver is opted in but could not be evaluated
// (unknown head, no finite abstainer availability lower bound, or no
// extractable winner block). Shared by the backed-group composition-dispute
// path and the synthesized-tie path so a silent hold is diagnosable on both.
func (e *executor) logEmptyWaiverUnproven(ctx context.Context, lg *zerolog.Logger, analysis *consensusAnalysis, g *responseGroup) {
	if !anyEmptyOutsideRetentionOptIn(e.config.requiredParticipants) {
		return
	}
	method := ""
	projectId, networkId := "", ""
	if analysis != nil {
		method = analysis.method
		if analysis.originalRequest != nil {
			networkId = analysis.originalRequest.NetworkLabel()
			if net := analysis.originalRequest.Network(); net != nil {
				projectId = net.ProjectId()
			}
		}
	}
	// Mid-round hold: proof inputs may look ready, but waivers deliberately
	// defer while a slower slot could still return data. Debug only — this
	// fires once per collected response, so Info/metrics would drown real
	// fail-closed (no proof) events.
	if analysis != nil && analysis.hasRemaining() {
		lg.Debug().
			Str("method", method).
			Str("reason", waiverUnprovenReasonRoundIncomplete).
			Msg("empty-outside-retention waiver deferred — responses still outstanding")
		return
	}
	head := e.chainHead(ctx, analysis)
	winnerBlock := e.winnerBlockEvidence(analysis, g)
	finiteBounds, matchingEmpties := countFiniteAvailLowerBounds(analysis, e.config.requiredParticipants)
	// Proven evaluation paths: winner block + (finite avail bounds for every
	// matching empty OR a known chain head). Inside-retention holds are not
	// "unproven" — only missing proof inputs are.
	availProofReady := winnerBlock != nil && matchingEmpties > 0 && finiteBounds == matchingEmpties
	headProofReady := winnerBlock != nil && head > 0
	if availProofReady || headProofReady {
		return
	}
	reason := waiverUnprovenReasonNoHead
	switch {
	case winnerBlock == nil:
		reason = waiverUnprovenReasonNoWinnerBlock
	case matchingEmpties == 0 || finiteBounds < matchingEmpties:
		reason = waiverUnprovenReasonNoAvailBound
	case head <= 0:
		reason = waiverUnprovenReasonNoHead
	}
	lg.Info().
		Str("method", method).
		Int64("chainHead", head).
		Bool("winnerBlockKnown", winnerBlock != nil).
		Int("availBoundFiniteCount", finiteBounds).
		Int("matchingEmpties", matchingEmpties).
		Str("reason", reason).
		Msg("empty-outside-retention waiver could not be evaluated — waiver holds the quota, fail closed")
	telemetry.MetricConsensusCompositionWaiverUnproven.WithLabelValues(projectId, networkId, reason).Inc()
}

// countFiniteAvailLowerBounds returns how many empty tag-matching participants
// (across any empty-outside-retention opted-in quota) expose a finite
// availability lower bound, and the total number of such matching empties.
func countFiniteAvailLowerBounds(analysis *consensusAnalysis, reqs []*common.ConsensusRequiredParticipant) (finite int, matchingEmpties int) {
	if analysis == nil {
		return 0, 0
	}
	seen := make(map[string]struct{})
	for _, req := range reqs {
		if req == nil || !req.WaiveAgreementOnEmptyOutsideRetention {
			continue
		}
		for _, g := range analysis.groups {
			if g == nil {
				continue
			}
			for _, r := range g.Results {
				if r == nil || r.Upstream == nil || !upstreamMatchesTag(r.Upstream, req.Tag) {
					continue
				}
				if r.Err != nil || r.CachedResponseType != ResponseTypeEmpty {
					continue
				}
				id := r.Upstream.Id()
				if _, dup := seen[id]; dup {
					continue
				}
				seen[id] = struct{}{}
				matchingEmpties++
				eu, ok := r.Upstream.(common.EvmUpstream)
				if !ok {
					continue
				}
				lower, _ := eu.EvmBlockAvailabilityBounds()
				if lower != math.MinInt64 {
					finite++
				}
			}
		}
	}
	return finite, matchingEmpties
}

const (
	waiverUnprovenReasonNoWinnerBlock   = "no_winner_block"
	waiverUnprovenReasonNoAvailBound    = "no_avail_bound"
	waiverUnprovenReasonNoHead          = "no_head"
	waiverUnprovenReasonRoundIncomplete = "round_incomplete"
)

// participantLatestBlock is the chain-head fallback when the network served
// tip is 0. It requires at least two distinct EVM participants to have reported
// a positive latest (corroboration), then takes the MIN — a single misbehaving
// upstream cannot inflate the head to make a recent winner look outside
// retention. Returns 0 (fail closed) when fewer than two pollers have spoken.
//
// The two sources can still be the winning group's own upstreams: excluding
// them would leave only the pruned internals, whose poller latest is often 0
// and would make this fallback unusable on the mixed-node shape it exists for.
// Two colluders reporting the same inflated latest can still raise the min.
// When majority served-tip is enabled for "latest", chainHead prefers that
// network-wide majority instead; default max-mode served tip is not trusted.
func participantLatestBlock(analysis *consensusAnalysis) int64 {
	if analysis == nil {
		return 0
	}
	var heads []int64
	seen := make(map[string]struct{})
	for _, g := range analysis.groups {
		if g == nil {
			continue
		}
		for _, r := range g.Results {
			if r == nil || r.Upstream == nil {
				continue
			}
			id := r.Upstream.Id()
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			eu, ok := r.Upstream.(common.EvmUpstream)
			if !ok {
				continue
			}
			if b := eu.EvmEffectiveLatestBlock(); b > 0 {
				heads = append(heads, b)
			}
		}
	}
	if len(heads) < 2 {
		return 0
	}
	min := heads[0]
	for _, h := range heads[1:] {
		if h < min {
			min = h
		}
	}
	return min
}

// winnerBlockEvidence extracts the winning group's own block number using the
// policy's blockEvidenceFields entry for the round's method. It returns nil
// when the method has no configured evidence field, the winner has no result,
// or no evidence field resolves to a non-negative number — every one of which
// leaves the empty-outside-retention waiver without proof (dispute). The first
// independently parseable field wins (feature §7.1 — "first resolved value").
func (e *executor) winnerBlockEvidence(analysis *consensusAnalysis, g *responseGroup) *big.Int {
	if analysis == nil || g == nil || g.LargestResult == nil {
		return nil
	}
	fields, ok := methodFields(e.config.blockEvidenceFields, analysis.method)
	if !ok || len(fields) == 0 {
		return nil
	}
	for _, field := range fields {
		values := extractFieldValues(g.LargestResult, []string{field})
		if len(values) == 0 || values[0] == nil {
			continue
		}
		n := values[0]
		// Block numbers cannot be negative. A malformed "-1" is still a
		// resolved value — fail closed rather than falling through to a
		// later field that might vacuously prove outside retention.
		if n.Sign() < 0 {
			return nil
		}
		return n
	}
	return nil
}

// emptyOutsideRetentionWaivedQuotas returns the requiredParticipants entries
// whose minAgreement quota is waived for THIS round under the empty-waiver
// (v1.1). An entry qualifies when it opts in
// (waiveAgreementOnEmptyOutsideRetention), carries a real quota
// (minAgreement > 0), at least minAgreement distinct tag-matching participants
// this round returned an EMPTY result, no matching participant returned a
// non-empty result, and the winner's block is proven outside retention.
// Sibling errors do not block the empty count; a matching non-empty result
// does — that is a real vote, not a prune, and waiving would mask it.
//
// Proof (prefer first that succeeds), evaluated over the empty matches only:
//  1. Availability lower bound — every counted empty upstream exposes a finite
//     EvmBlockAvailabilityBounds lower bound and winnerBlock < that bound
//     (typically poller.latest − latestBlockMinus). No network served tip needed.
//  2. Chain head — head - winnerBlock > retentionBlocks, with head from
//     majority served tip / corroborated participant pollers.
//
// The block proof is the anti-fabrication guard: a null on a recent block
// (inside retention / bounds) fails the proof and holds the quota, so
// correlated externals cannot outvote an internal on data the internal should
// still have.
//
// Round-complete by design (mirrors missingDataWaivedQuotas): while any
// response can still arrive, a slower tagged upstream could still return data,
// so this reports no waivers. Wait-cap cancel alone does not clear
// hasRemaining. Without a winner block and neither proof source, it reports
// none — fail closed.
func emptyOutsideRetentionWaivedQuotas(analysis *consensusAnalysis, reqs []*common.ConsensusRequiredParticipant, winnerBlock *big.Int, head int64) map[*common.ConsensusRequiredParticipant]struct{} {
	if analysis == nil || analysis.hasRemaining() {
		return nil
	}
	if winnerBlock == nil {
		return nil
	}
	var waived map[*common.ConsensusRequiredParticipant]struct{}
	for _, req := range reqs {
		if req == nil || !req.WaiveAgreementOnEmptyOutsideRetention || req.MinAgreement <= 0 || req.RetentionBlocks <= 0 {
			continue
		}
		var empties []*execResult
		var seen map[string]struct{}
		nonEmptyMatch := false
		for _, g := range analysis.groups {
			for _, r := range g.Results {
				if r == nil || r.Upstream == nil || !upstreamMatchesTag(r.Upstream, req.Tag) {
					continue
				}
				id := r.Upstream.Id()
				if seen == nil {
					seen = make(map[string]struct{}, req.MinAgreement)
				}
				if _, dup := seen[id]; dup {
					continue
				}
				seen[id] = struct{}{}
				if r.Err == nil && r.CachedResponseType == ResponseTypeNonEmpty {
					nonEmptyMatch = true
					continue
				}
				if r.Err != nil || r.CachedResponseType != ResponseTypeEmpty {
					continue
				}
				empties = append(empties, r)
			}
		}
		if nonEmptyMatch || len(empties) < req.MinAgreement {
			continue
		}
		// Feature §7.1: prefer finite availability lower bounds; only fall
		// back to chain-head depth when that preferred evidence is unavailable.
		// A finite bound that places the winner inside the served range is
		// positive contradictory evidence — never override it with head depth.
		proves, contradicts := availabilityBoundProofState(empties, winnerBlock)
		if proves || (!contradicts && chainHeadProvesOutsideRetention(winnerBlock, head, req.RetentionBlocks)) {
			if waived == nil {
				waived = make(map[*common.ConsensusRequiredParticipant]struct{}, len(reqs))
			}
			waived[req] = struct{}{}
		}
	}
	return waived
}

// availabilityBoundProofState classifies preferred empty-waiver proof from
// abstainers' blockAvailability lower bounds (feature §7.1):
//   - proves: every matching empty has a finite lower and winnerBlock < lower
//   - contradicts: at least one finite lower has winnerBlock >= lower (in range)
//
// When neither (bounds unavailable), the caller may fall back to chain-head depth.
func availabilityBoundProofState(matching []*execResult, winnerBlock *big.Int) (proves, contradicts bool) {
	if winnerBlock == nil || len(matching) == 0 {
		return false, false
	}
	allFiniteProve := true
	anyFinite := false
	for _, r := range matching {
		if r == nil || r.Upstream == nil {
			return false, false
		}
		eu, ok := r.Upstream.(common.EvmUpstream)
		if !ok {
			return false, false
		}
		lower, _ := eu.EvmBlockAvailabilityBounds()
		if lower == math.MinInt64 {
			allFiniteProve = false
			continue
		}
		anyFinite = true
		if winnerBlock.Cmp(big.NewInt(lower)) >= 0 {
			return false, true
		}
	}
	if allFiniteProve && anyFinite {
		return true, false
	}
	return false, false
}

// availabilityBoundProvesOutsideRetention is true when every matching empty
// participant has a finite availability lower bound and the winner block is
// strictly below each bound (pruned / outside retained range for that upstream).
func availabilityBoundProvesOutsideRetention(matching []*execResult, winnerBlock *big.Int) bool {
	proves, _ := availabilityBoundProofState(matching, winnerBlock)
	return proves
}

// chainHeadProvesOutsideRetention is the original v1.1 head-depth proof:
// outside iff head - winnerBlock > retentionBlocks.
func chainHeadProvesOutsideRetention(winnerBlock *big.Int, head int64, retentionBlocks int64) bool {
	if winnerBlock == nil || head <= 0 || retentionBlocks <= 0 {
		return false
	}
	depth := new(big.Int).Sub(big.NewInt(head), winnerBlock)
	return depth.Cmp(big.NewInt(retentionBlocks)) > 0
}

// agreeingResults returns every result that agrees with the winning group.
// Normally that is exactly the group's own results, but when
// preferHighestValueFor is configured for the method, agreement is counted
// by numeric value — the same value with a different encoding (0x5 vs 0x05)
// hashes into a different group, and its upstream must still count toward
// the composition quota.
func (e *executor) agreeingResults(analysis *consensusAnalysis, g *responseGroup) []*execResult {
	fields, _ := methodFields(e.config.preferHighestValueFor, analysis.method)
	if len(fields) == 0 {
		return g.Results
	}
	winnerValues := extractFieldValues(g.LargestResult, fields)
	if winnerValues == nil {
		return g.Results
	}
	agreeing := append([]*execResult(nil), g.Results...)
	for _, og := range analysis.getValidGroups() {
		if og == g {
			continue
		}
		for _, r := range og.Results {
			if r == nil || r.Result == nil || r.Err != nil {
				continue
			}
			if v := extractFieldValues(r.Result, fields); v != nil && compareValueChains(v, winnerValues) == 0 {
				agreeing = append(agreeing, r)
			}
		}
	}
	return agreeing
}

// --- Tracing, Metrics, and Punishment ---

func (e *executor) trackAndPunishMisbehavingUpstreams(lg *zerolog.Logger, req *common.NormalizedRequest, labels metricsLabels, winner *slotResult, analysis *consensusAnalysis) {
	// Skip tracking when there are no valid participants (all infra errors)
	if analysis.validParticipants == 0 {
		return
	}
	// A composition dispute means the count-majority itself was rejected as
	// untrustworthy (insufficient quota-tagged members). Falling through
	// would pick that same majority as the "consensus" group and punish the
	// quota-tagged dissenters — inverting the trust boundary minAgreement
	// enforces. No one is punishable in this state.
	if winner != nil && winner.Error != nil &&
		common.HasErrorCode(winner.Error, common.ErrCodeConsensusCompositionDispute) {
		return
	}

	// Determine the consensus group based on the actual winner result
	// This ensures we track misbehavior against what was actually returned, not just the majority
	var consensusGroup *responseGroup

	// If we have a successful response, find the group that contains it
	if winner != nil && winner.Result != nil {
		if winnerResp, ok := any(winner.Result).(*common.NormalizedResponse); ok && winnerResp != nil {
			// Find the group containing this exact response
			for _, group := range analysis.groups {
				for _, result := range group.Results {
					if result != nil && result.Result == winnerResp {
						consensusGroup = group
						break
					}
				}
				if consensusGroup != nil {
					break
				}
			}
		}
	}

	// If no consensus group found from winner result, use the best group by count
	if consensusGroup == nil {
		for _, g := range analysis.getValidGroups() {
			if consensusGroup == nil || g.Count > consensusGroup.Count {
				consensusGroup = g
			}
		}
	}

	if consensusGroup == nil {
		return
	}

	// Track different types of disagreements
	consensusSize := consensusGroup.ResponseSize

	// Determine if the dispute log level would be emitted by the current logger level
	shouldLog := e.logger.GetLevel() <= e.disputeLogLevel

	// Collect participants when either logging is enabled OR exporter is configured
	collectParticipants := shouldLog || e.exporter != nil
	var allParticipants []participantInfo
	misbehavingParticipants := ""
	if collectParticipants {
		allParticipants = make([]participantInfo, 0, analysis.totalParticipants)
	}
	var misbehavingCount int

	for _, group := range analysis.groups {
		agreesWithConsensus := group.Hash == consensusGroup.Hash
		largerThanConsensus := group.ResponseSize > consensusSize
		largerThanConsensusStr := strconv.FormatBool(largerThanConsensus)

		for _, result := range group.Results {
			if result == nil || result.Upstream == nil {
				continue
			}

			upstreamId := result.Upstream.Id()

			// Collect participant details when needed for export or logging
			if collectParticipants {
				var responseHash string
				var responseSize int
				var responseBody []byte
				var errorMessage string

				responseHash = result.CachedHash
				responseSize = result.CachedResponseSize

				if result.Result != nil {
					if jrr, err := result.Result.JsonRpcResponse(); err == nil && jrr != nil {
						// Full response body for export/logging
						responseBody = jrr.GetResultBytes()

						// Only do extra checks/logs if we actually log
						if shouldLog {
							// Debug: Log if we have a mismatch between empty response and non-empty type
							if group.ResponseType == ResponseTypeNonEmpty && jrr.IsResultEmptyish() {
								lg.Warn().
									Str("upstream", upstreamId).
									Str("responseType", group.ResponseType.String()).
									RawJSON("result", responseBody).
									Msg("WARN: Response marked as non_empty but IsResultEmptyish returns true")
							}
						}
					} else {
						// Couldn't extract JsonRpcResponse
						responseBody, _ = common.SonicCfg.Marshal(map[string]interface{}{
							"error": fmt.Sprintf("<error extracting response: %v>", err),
						})
					}
				} else if result.Err != nil {
					// For errors, include the full error details as the "body"
					responseBody, _ = common.SonicCfg.Marshal(map[string]interface{}{
						"error": fmt.Sprintf("<error: %v>", result.Err),
					})
				}

				if result.Err != nil {
					errorMessage = common.ErrorSummary(result.Err)
				}

				// Add to participants list for export/logging
				allParticipants = append(allParticipants, participantInfo{
					upstreamId:          upstreamId,
					upstream:            result.Upstream,
					responseType:        group.ResponseType,
					responseHash:        responseHash,
					responseSize:        responseSize,
					responseBody:        responseBody,
					errorMessage:        errorMessage,
					agreesWithConsensus: agreesWithConsensus,
				})

				if shouldLog && !agreesWithConsensus {
					if misbehavingParticipants == "" {
						misbehavingParticipants = upstreamId
					} else {
						misbehavingParticipants += fmt.Sprintf(",%s", upstreamId)
					}
				}
			}

			// Track errors separately - these are NOT misbehavior
			if group.ResponseType == ResponseTypeConsensusError || group.ResponseType == ResponseTypeInfrastructureError {
				// Extract error code
				errorCode := "unknown"
				if result.Err != nil {
					if common.HasErrorCode(result.Err, common.ErrCodeEndpointMissingData) {
						errorCode = "ErrEndpointMissingData"
					} else if common.HasErrorCode(result.Err, common.ErrCodeEndpointServerSideException) {
						errorCode = "ErrEndpointServerSideException"
					} else if se, ok := result.Err.(common.StandardError); ok {
						if base := se.Base(); base != nil {
							errorCode = string(base.Code)
						}
					} else {
						// Try to extract from error string
						errStr := result.Err.Error()
						if strings.Contains(errStr, "block not found") {
							errorCode = "block_not_found"
						} else if strings.Contains(errStr, "timeout") {
							errorCode = "timeout"
						} else {
							errorCode = common.ErrorFingerprint(result.Err)
						}
					}
				}

				if !agreesWithConsensus {
					// Track as error, not misbehavior
					telemetry.MetricConsensusUpstreamErrors.
						WithLabelValues(
							labels.projectId,
							labels.networkId,
							upstreamId,
							labels.category,
							labels.finalityStr,
							group.ResponseType.String(),
							errorCode,
							labels.userId,
							labels.agentName,
						).Inc()
				}

				continue // Don't track as misbehavior
			}

			// Only track actual data disagreements as misbehavior
			// This includes: empty vs non-empty, or different non-empty responses
			if !agreesWithConsensus && (group.ResponseType == ResponseTypeEmpty || group.ResponseType == ResponseTypeNonEmpty) {
				// Only count as misbehavior if consensus is also data (not error)
				if consensusGroup.ResponseType == ResponseTypeEmpty || consensusGroup.ResponseType == ResponseTypeNonEmpty {
					misbehavingCount++

					// Record metric
					telemetry.MetricConsensusMisbehaviorDetected.
						WithLabelValues(
							labels.projectId,
							labels.networkId,
							upstreamId,
							labels.category,
							labels.finalityStr,
							group.ResponseType.String(),
							largerThanConsensusStr,
							labels.userId,
							labels.agentName,
						).Inc()

					// Record misbehavior in tracker for score calculation.
					// Consensus reasoning happens after the per-attempt
					// finality is captured into `labels` — pass it through
					// so per-(method, finality) misbehavior counters
					// stratify correctly when finality tracking is on.
					if result.Upstream != nil && result.Upstream.Tracker() != nil {
						result.Upstream.Tracker().RecordUpstreamMisbehavior(result.Upstream, labels.method, labels.finality)
					}

					// Apply punishment only if configured and conditions are met
					if e.shouldPunishUpstream(lg, consensusGroup, analysis) {
						limiter := e.createRateLimiter(lg, upstreamId)
						if !limiter.Allow() {
							e.handleMisbehavingUpstream(lg, result.Upstream, upstreamId, labels)
						}
					}
				}
			}
		}
	}

	// Export and log participants if misbehavior was found
	if misbehavingCount > 0 {
		// Export full event if configured
		if e.exporter != nil {
			if recBytes, err := e.buildMisbehaviorRecord(labels, req, winner, analysis, consensusGroup, allParticipants); err == nil {
				if err2 := e.exporter.AppendWithMetadata(recBytes, labels.method, labels.networkId); err2 != nil {
					lg.Warn().Err(err2).Msg("failed to append misbehavior record")
				}
			} else {
				lg.Warn().Err(err).Msg("failed to encode misbehavior record")
			}
		}

		if shouldLog {
			// Get consensus response data (compact)
			consensusHash := consensusGroup.Hash
			consensusSize := consensusGroup.ResponseSize
			var consensusBody []byte

			// Get the consensus response body for comparison
			if consensusGroup.LargestResult != nil {
				if jrr, err := consensusGroup.LargestResult.JsonRpcResponse(); err == nil && jrr != nil {
					consensusBody = jrr.GetResultBytes()
				} else {
					consensusBody, _ = common.SonicCfg.Marshal(map[string]interface{}{
						"error": fmt.Sprintf("<error extracting consensus response: %v>", err),
					})
				}
			} else if consensusGroup.RepresentativeError != nil {
				consensusBody, _ = common.SonicCfg.Marshal(map[string]interface{}{
					"error": fmt.Sprintf("<consensus error: %v>", consensusGroup.RepresentativeError),
				})
			}

			logEvent := e.logger.WithLevel(e.disputeLogLevel).
				Str("projectId", labels.projectId).
				Str("networkId", labels.networkId).
				Str("category", labels.category).
				Str("finality", labels.finalityStr).
				Int("consensusCount", consensusGroup.Count).
				Int("totalParticipants", analysis.totalParticipants).
				Int("validParticipants", analysis.validParticipants).
				Int("misbehavingCount", misbehavingCount).
				Str("misbehavingParticipants", misbehavingParticipants).
				Str("consensusResponseType", consensusGroup.ResponseType.String()).
				Str("consensusHash", consensusHash).
				Int("consensusSize", consensusSize).
				RawJSON("consensusResponse", consensusBody).
				Object("request", req)

				// Add ALL participants with numbered keys
			for i, participant := range allParticipants {
				idx := strconv.Itoa(i + 1)
				logEvent = logEvent.
					Str("upstream"+idx, participant.upstreamId).
					Str("responseType"+idx, participant.responseType.String()).
					Bool("agreesWithConsensus"+idx, participant.agreesWithConsensus).
					Str("responseHash"+idx, participant.responseHash).
					Int("responseSize"+idx, participant.responseSize).
					RawJSON("response"+idx, participant.responseBody)

				if participant.errorMessage != "" {
					logEvent = logEvent.Str("error"+idx, participant.errorMessage)
				}
			}

			logEvent.Msg("consensus misbehavior detected - upstreams differ from consensus")
		}
	}
}

// buildMisbehaviorRecord converts current context into JSONL bytes without truncation
func (e *executor) buildMisbehaviorRecord(labels metricsLabels, req *common.NormalizedRequest, winner *slotResult, analysis *consensusAnalysis, consensusGroup *responseGroup, allParticipants []participantInfo) ([]byte, error) {
	// Request raw
	var reqRaw []byte
	if jrq, _ := req.JsonRpcRequest(); jrq != nil {
		// Rebuild raw request minimal form
		m := map[string]interface{}{"jsonrpc": "2.0", "id": jrq.ID, "method": jrq.Method, "params": jrq.Params}
		b, _ := common.SonicCfg.Marshal(m)
		reqRaw = b
	} else if body := req.Body(); len(body) > 0 {
		reqRaw = body
	}

	// Winner snapshot
	var win winnerSnapshot
	if winner != nil {
		if wr, ok := any(winner.Result).(*common.NormalizedResponse); ok && wr != nil {
			if jrr, err := wr.JsonRpcResponse(); err == nil && jrr != nil {
				size, _ := jrr.Size()
				hash, _ := jrr.CanonicalHash()
				rtype := ResponseTypeNonEmpty
				if jrr.IsResultEmptyish() || wr.IsResultEmptyish() {
					rtype = ResponseTypeEmpty
				}
				win = winnerSnapshot{
					ResponseType: rtype.String(),
					Hash:         hash,
					Size:         size,
				}
				if ups := wr.Upstream(); ups != nil {
					win.UpstreamID = ups.Id()
				}
			}
		} else if winner.Error != nil {
			// Winner is error
			win = winnerSnapshot{ResponseType: ResponseTypeConsensusError.String(), Hash: errorToConsensusHash(winner.Error)}
		}
	}

	// Analysis snapshot
	as := analysisSnapshot{
		TotalParticipants: analysis.totalParticipants,
		ValidParticipants: analysis.validParticipants,
		Groups:            make([]groupSnapshot, 0, len(analysis.groups)),
	}
	if best := analysis.getBestByCount(); best != nil {
		as.BestByCount = &groupSnapshot{
			Hash:         best.Hash,
			Count:        best.Count,
			IsTie:        best.IsTie,
			ResponseType: best.ResponseType.String(),
			ResponseSize: best.ResponseSize,
		}
	}
	for _, g := range analysis.groups {
		as.Groups = append(as.Groups, groupSnapshot{
			Hash:         g.Hash,
			Count:        g.Count,
			IsTie:        g.IsTie,
			ResponseType: g.ResponseType.String(),
			ResponseSize: g.ResponseSize,
		})
	}

	// Participants
	parts := make([]participantSnapshot, 0, len(allParticipants))
	for _, p := range allParticipants {
		ps := participantSnapshot{
			UpstreamID:   p.upstreamId,
			ResponseType: p.responseType.String(),
			ResponseHash: p.responseHash,
			ResponseSize: p.responseSize,
		}
		if p.upstream != nil && p.upstream.Config() != nil {
			ps.Vendor = p.upstream.Config().VendorName
		}
		if len(p.responseBody) > 0 {
			ps.Response = append([]byte(nil), p.responseBody...)
		} else if p.errorMessage != "" {
			ps.Error = p.errorMessage
		}
		parts = append(parts, ps)
	}

	// Policy snapshot
	pol := policySnapshot{
		MaxParticipants:         e.maxParticipants,
		AgreementThreshold:      e.agreementThreshold,
		DisputeBehavior:         e.disputeBehavior,
		LowParticipantsBehavior: e.lowParticipantsBehavior,
		PreferNonEmpty:          e.preferNonEmpty,
		PreferLargerResponses:   e.preferLargerResponses,
		IgnoreFields:            e.ignoreFields,
	}

	rec := misbehaviorRecord{
		TimestampMs:  time.Now().UnixMilli(),
		ProjectID:    labels.projectId,
		UserId:       labels.userId,
		NetworkID:    labels.networkId,
		Method:       labels.method,
		Finality:     labels.finalityStr,
		Policy:       pol,
		Request:      reqRaw,
		Winner:       win,
		Analysis:     as,
		Participants: parts,
	}

	return common.SonicCfg.Marshal(rec)
}

// shouldPunishUpstream determines if punishment should be applied based on configuration and consensus strength
func (e *executor) shouldPunishUpstream(lg *zerolog.Logger, consensusGroup *responseGroup, analysis *consensusAnalysis) bool {
	// Check if punishment is configured
	if e.punishMisbehavior == nil || e.punishMisbehavior.DisputeThreshold == 0 {
		return false
	}

	// Guard against invalid DisputeWindow to avoid creating invalid rate limiters
	if e.punishMisbehavior.DisputeWindow.Duration() <= 0 {
		lg.Debug().Msg("punishment disabled: DisputeWindow is zero or negative")
		return false
	}

	// Only punish if we have a clear majority (>50% of valid participants)
	return consensusGroup.Count > analysis.validParticipants/2
}

func (e *executor) handleMisbehavingUpstream(logger *zerolog.Logger, upstream common.Upstream, upstreamId string, labels metricsLabels) {
	// Create a placeholder value to claim ownership atomically
	placeholder := &struct{}{}

	// Try to claim ownership of punishing this upstream
	if _, loaded := e.misbehavingUpstreamsSitoutTimer.LoadOrStore(upstreamId, placeholder); loaded {
		logger.Debug().
			Str("upstream", upstreamId).
			Msg("upstream already in sitout, skipping")
		return
	}

	logger.Warn().
		Str("upstream", upstreamId).
		Msg("misbehaviour limit exhausted, punishing upstream")

	// Record punishment metric
	telemetry.MetricConsensusUpstreamPunished.WithLabelValues(labels.projectId, labels.networkId, upstreamId, labels.userId, labels.agentName).Inc()

	// Cordon the upstream first. Consensus misbehavior is a PUNISHMENT cordon:
	// the selector reads this typed class (never the reason string) and pins the
	// network to the strictest policy while any upstream is punished.
	common.CordonClassed(upstream, "*", "misbehaving in consensus", common.CordonClassPunishment)

	// Create the timer
	timer := time.AfterFunc(e.punishMisbehavior.SitOutPenalty.Duration(), func() {
		common.UncordonClassed(upstream, "*", "end of consensus penalty", common.CordonClassPunishment)
		e.misbehavingUpstreamsSitoutTimer.Delete(upstreamId)
	})

	// Replace the placeholder with the actual timer
	e.misbehavingUpstreamsSitoutTimer.Store(upstreamId, timer)
}

func (e *executor) createRateLimiter(logger *zerolog.Logger, upstreamId string) *rate.Limiter {
	// Try to get existing limiter
	if limiter, ok := e.misbehavingUpstreamsLimiter.Load(upstreamId); ok {
		return limiter.(*rate.Limiter)
	}

	logger.Info().
		Str("upstream", upstreamId).
		Uint("disputeThreshold", e.punishMisbehavior.DisputeThreshold).
		Str("disputeWindow", e.punishMisbehavior.DisputeWindow.String()).
		Msg("creating new dispute limiter")

	// Bursty rate limiter: `threshold` tokens per `window` (token-bucket).
	window := e.punishMisbehavior.DisputeWindow.Duration()
	burst := int(e.punishMisbehavior.DisputeThreshold)
	if burst < 1 {
		burst = 1
	}
	var lim *rate.Limiter
	if window > 0 {
		lim = rate.NewLimiter(rate.Every(window/time.Duration(burst)), burst)
	} else {
		lim = rate.NewLimiter(rate.Inf, burst)
	}

	// Use LoadOrStore to handle concurrent creation
	actual, _ := e.misbehavingUpstreamsLimiter.LoadOrStore(upstreamId, lim)
	return actual.(*rate.Limiter)
}

func (e *executor) extractMetricsLabels(ctx context.Context, req *common.NormalizedRequest) metricsLabels {
	method := "unknown"
	if m, err := req.Method(); err == nil {
		method = m
	}
	projectId := ""
	if req.Network() != nil {
		projectId = req.Network().ProjectId()
	}
	finality := req.Finality(ctx)
	return metricsLabels{
		method:      method,
		category:    method,
		networkId:   req.NetworkLabel(),
		projectId:   projectId,
		userId:      req.UserId(),
		agentName:   req.AgentName(),
		finalityStr: finality.String(),
		finality:    finality,
	}
}

func (e *executor) startConsensusSpan(ctx context.Context, labels metricsLabels) (context.Context, trace.Span) {
	return common.StartSpan(ctx, "Consensus.Run",
		trace.WithAttributes(
			attribute.String("network.id", labels.networkId),
			attribute.String("request.method", labels.method),
		),
	)
}

func (e *executor) recordMetricsAndTracing(req *common.NormalizedRequest, startTime time.Time, result *slotResult, analysis *consensusAnalysis, labels metricsLabels, span trace.Span) {
	// Defensive: analysis is nil on the catastrophic-path where the analyzer
	// goroutine panicked before any responses could be classified. Emit
	// minimal metrics and mark the span error rather than nil-dereferencing.
	if analysis == nil {
		outcome := "generic_error"
		if result != nil && result.Error != nil {
			common.SetTraceSpanError(span, result.Error)
		}
		span.SetAttributes(attribute.String("consensus.outcome", outcome))
		duration := time.Since(startTime).Seconds()
		telemetry.MetricConsensusTotal.
			WithLabelValues(labels.projectId, labels.networkId, labels.category, outcome, labels.finalityStr, labels.userId, labels.agentName).
			Inc()
		telemetry.MetricConsensusDuration.
			WithLabelValues(labels.projectId, labels.networkId, labels.category, outcome, labels.finalityStr, labels.userId, labels.agentName).
			Observe(duration)
		telemetry.MetricConsensusErrors.
			WithLabelValues(labels.projectId, labels.networkId, labels.category, outcome, labels.finalityStr, labels.userId, labels.agentName).
			Inc()
		return
	}

	// Determine if consensus was achieved based on the highest count group
	best := analysis.getBestByCount()
	hasConsensus := best != nil && best.Count >= e.agreementThreshold
	isLowParticipants := analysis.isLowParticipants(e.agreementThreshold)
	isDispute := !hasConsensus && !isLowParticipants

	// A composition dispute means the count-winner failed the minAgreement
	// quota — label it distinctly so operators can alert on it and measure
	// how often composition (not vote count) rejected a winner.
	isCompositionDispute := result.Error != nil &&
		common.HasErrorCode(result.Error, common.ErrCodeConsensusCompositionDispute)

	outcome := "success"
	if result.Error != nil {
		if isCompositionDispute {
			outcome = "dispute_composition"
		} else if hasConsensus {
			outcome = "consensus_on_error"
		} else if isDispute {
			outcome = "dispute"
		} else if isLowParticipants {
			outcome = "low_participants"
		} else {
			outcome = "generic_error"
		}
		common.SetTraceSpanError(span, result.Error)
	} else {
		span.SetStatus(codes.Ok, "Consensus successful")
	}

	span.SetAttributes(
		attribute.String("consensus.outcome", outcome),
		attribute.Bool("consensus.achieved", hasConsensus),
		attribute.Bool("consensus.low_participants", isLowParticipants),
		attribute.Bool("consensus.dispute", isDispute),
		attribute.Int("participants.total", analysis.totalParticipants),
		attribute.Int("participants.valid", analysis.validParticipants),
	)

	duration := time.Since(startTime).Seconds()
	telemetry.MetricConsensusTotal.WithLabelValues(labels.projectId, labels.networkId, labels.category, outcome, labels.finalityStr, labels.userId, labels.agentName).Inc()
	telemetry.MetricConsensusDuration.WithLabelValues(labels.projectId, labels.networkId, labels.category, outcome, labels.finalityStr, labels.userId, labels.agentName).Observe(duration)
	// Record agreement count histogram when available
	if best != nil && best.Count > 0 {
		telemetry.MetricConsensusAgreementCount.
			WithLabelValues(labels.projectId, labels.networkId, labels.category, labels.finalityStr, labels.userId, labels.agentName).
			Observe(float64(best.Count))
	}
	// Record categorized error counters for failure modes, but only for
	// alert-worthy severities (warning/critical). Deterministic client/execution
	// errors (severity info) are the caller's failure that upstreams merely
	// agreed on (e.g. nonce-too-low on eth_sendRawTransaction broadcast), not a
	// consensus failure — keep them out of the error counter.
	severity := common.ClassifySeverity(result.Error)
	if result.Error != nil && (severity == common.SeverityWarning || severity == common.SeverityCritical) {
		errLabel := "generic_error"
		if isCompositionDispute {
			errLabel = "dispute_composition"
		} else if hasConsensus {
			errLabel = "consensus_on_error"
		} else if isDispute {
			errLabel = "dispute"
		} else if isLowParticipants {
			errLabel = "low_participants"
		}
		telemetry.MetricConsensusErrors.
			WithLabelValues(labels.projectId, labels.networkId, labels.category, errLabel, labels.finalityStr, labels.userId, labels.agentName).
			Inc()
	}
}
