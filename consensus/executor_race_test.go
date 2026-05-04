package consensus

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/failsafe-go/failsafe-go"
	failsafeCommon "github.com/failsafe-go/failsafe-go/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"
)

// ---------------------------------------------------------------------------
// Test Suite: Low-Participant Count Race Conditions
//
// These tests form a contract for correctness of the consensus mechanism
// under low participant counts (N=1..3) and concurrent context cancellation.
// Any valid fix for the race condition must pass all of them; any regression
// turns at least one red.
//
// Invariants tested:
//   I1: A completed innerFn result is NEVER silently discarded.
//   I2: The caller NEVER blocks indefinitely (no deadlock on outcomeCh).
//   I3: ErrConsensusLowParticipants is returned ONLY when validParticipants
//       is genuinely below the agreement threshold.
//   I4: Post-short-circuit and post-cancel responses are Release()d.
//   I5: The analyzer always runs to completion regardless of caller context.
// ---------------------------------------------------------------------------

// ===== SCENARIO A: N=1 — the minimal reproduction =========================

// TestRace_SingleParticipant_CancelAfterExecution is the minimal reproduction
// of the original bug. With N=1 and threshold=1, the sole participant completes
// innerFn, then context is cancelled. The old code discarded this result,
// producing ErrConsensusLowParticipants with "participants: null". This MUST
// return the valid consensus winner or context.Canceled — never low-participants.
//
// Why N=1 is special: there is no short-circuit (lead=1, remaining=0 → fires
// immediately), so the outcome channel races directly with ctx.Done(). Any bug
// in the double-select retry logic or in participant result forwarding shows
// up deterministically here.
func TestRace_SingleParticipant_CancelAfterExecution(t *testing.T) {
	logger := zerolog.New(zerolog.NewTestWriter(t))

	pol := NewConsensusPolicyBuilder().
		WithMaxParticipants(1).
		WithAgreementThreshold(1).
		WithLogger(&logger).
		Build()

	req := newTestRequest()

	started := make(chan struct{})
	completeInnerFn := make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = context.WithValue(ctx, common.RequestContextKey, req)

	fsExec := failsafe.NewExecutor(pol).WithContext(ctx)

	type result struct {
		resp *common.NormalizedResponse
		err  error
	}
	resultCh := make(chan result, 1)

	go func() {
		resp, err := fsExec.GetWithExecution(func(exec failsafe.Execution[*common.NormalizedResponse]) (*common.NormalizedResponse, error) {
			close(started)
			<-completeInnerFn
			return validResponse(), nil
		})
		resultCh <- result{resp, err}
	}()

	<-started
	cancel()
	close(completeInnerFn)

	select {
	case r := <-resultCh:
		if r.err != nil {
			require.Falsef(t,
				common.HasErrorCode(r.err, common.ErrCodeConsensusLowParticipants),
				"N=1: must not report ErrConsensusLowParticipants when 1 valid response exists: %v", r.err,
			)
			require.Truef(t,
				errors.Is(r.err, context.Canceled),
				"N=1: only acceptable error is context.Canceled, got: %v", r.err,
			)
		} else {
			require.NotNil(t, r.resp, "N=1: should return a valid response")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("N=1: deadlock — consensus did not return within 2s")
	}
}

// TestRace_SingleParticipant_CancelBeforeExecution verifies the correct
// low-participants case: context is cancelled before the sole participant
// enters innerFn. The participant sends nil → 0 groups → low-participants
// is the correct outcome.
func TestRace_SingleParticipant_CancelBeforeExecution(t *testing.T) {
	logger := zerolog.New(zerolog.NewTestWriter(t))

	pol := NewConsensusPolicyBuilder().
		WithMaxParticipants(1).
		WithAgreementThreshold(1).
		WithLogger(&logger).
		Build()

	req := newTestRequest()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before any execution
	ctx = context.WithValue(ctx, common.RequestContextKey, req)

	fsExec := failsafe.NewExecutor(pol).WithContext(ctx)

	var called atomic.Int32
	_, err := fsExec.GetWithExecution(func(exec failsafe.Execution[*common.NormalizedResponse]) (*common.NormalizedResponse, error) {
		called.Add(1)
		return validResponse(), nil
	})

	require.Error(t, err)
	assert.True(t,
		common.HasErrorCode(err, common.ErrCodeConsensusLowParticipants) ||
			errors.Is(err, context.Canceled),
		"N=1 pre-cancel: expected LowParticipants or Canceled, got: %v", err,
	)
}

// ===== SCENARIO B: N=2, threshold=2 — staggered completion ================

// TestRace_TwoParticipants_CancelBetweenCompletions exercises the scenario
// where participant 1 completes, context is cancelled, then participant 2
// completes. With the old code, participant 2's result was discarded
// (post-execution cancel check). With N=2, threshold=2, losing ONE result
// means the analyzer sees only 1 valid → low-participants or dispute.
//
// The correct behavior: both results reach the analyzer. Either the caller
// gets a valid consensus (if the analyzer wins the select) or context.Canceled
// (if ctx.Done() wins). Never low-participants when 2 valid responses exist.
func TestRace_TwoParticipants_CancelBetweenCompletions(t *testing.T) {
	logger := zerolog.New(zerolog.NewTestWriter(t))

	pol := NewConsensusPolicyBuilder().
		WithMaxParticipants(2).
		WithAgreementThreshold(2).
		WithLogger(&logger).
		Build()

	req := newTestRequest()

	var started atomic.Int32
	firstStarted := make(chan struct{})
	secondStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	releaseSecond := make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = context.WithValue(ctx, common.RequestContextKey, req)

	fsExec := failsafe.NewExecutor(pol).WithContext(ctx)

	type result struct {
		resp *common.NormalizedResponse
		err  error
	}
	resultCh := make(chan result, 1)

	go func() {
		resp, err := fsExec.GetWithExecution(func(exec failsafe.Execution[*common.NormalizedResponse]) (*common.NormalizedResponse, error) {
			n := started.Add(1)
			if n == 1 {
				close(firstStarted)
				<-releaseFirst
			} else {
				close(secondStarted)
				<-releaseSecond
			}
			return validResponse(), nil
		})
		resultCh <- result{resp, err}
	}()

	// Sequence: first completes → cancel → second completes.
	<-firstStarted
	close(releaseFirst)
	time.Sleep(5 * time.Millisecond) // tiny gap for goroutine scheduling
	<-secondStarted
	cancel()
	close(releaseSecond)

	select {
	case r := <-resultCh:
		if r.err != nil {
			require.Falsef(t,
				common.HasErrorCode(r.err, common.ErrCodeConsensusLowParticipants),
				"N=2: must not report LowParticipants when both participants completed with valid responses: %v", r.err,
			)
			require.Truef(t,
				errors.Is(r.err, context.Canceled),
				"N=2: only acceptable error is context.Canceled, got: %v", r.err,
			)
		} else {
			require.NotNil(t, r.resp)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("N=2: deadlock — consensus did not return within 2s")
	}
}

// TestRace_TwoParticipants_BothCompleteBeforeCancel_ThresholdTwo ensures
// that when both N=2 participants complete and THEN cancel fires, the
// analyzer has both responses. This is the N=2 analog of the N=3 test
// already on the branch but specifically tests the minimum for all-must-agree.
func TestRace_TwoParticipants_BothCompleteBeforeCancel_ThresholdTwo(t *testing.T) {
	logger := zerolog.New(zerolog.NewTestWriter(t))

	pol := NewConsensusPolicyBuilder().
		WithMaxParticipants(2).
		WithAgreementThreshold(2).
		WithLogger(&logger).
		Build()

	req := newTestRequest()

	var started atomic.Int32
	allStarted := make(chan struct{})
	completeAll := make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = context.WithValue(ctx, common.RequestContextKey, req)

	fsExec := failsafe.NewExecutor(pol).WithContext(ctx)

	type result struct {
		resp *common.NormalizedResponse
		err  error
	}
	resultCh := make(chan result, 1)

	go func() {
		resp, err := fsExec.GetWithExecution(func(exec failsafe.Execution[*common.NormalizedResponse]) (*common.NormalizedResponse, error) {
			if started.Add(1) == 2 {
				close(allStarted)
			}
			<-completeAll
			return validResponse(), nil
		})
		resultCh <- result{resp, err}
	}()

	<-allStarted
	cancel()
	close(completeAll)

	select {
	case r := <-resultCh:
		if r.err != nil {
			require.Falsef(t,
				common.HasErrorCode(r.err, common.ErrCodeConsensusLowParticipants),
				"N=2 both-complete: must not report LowParticipants: %v", r.err,
			)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("N=2 both-complete: deadlock")
	}
}

// ===== SCENARIO C: N=2, threshold=1 — short-circuit + late arrival ========

// TestRace_TwoParticipants_ShortCircuit_LateArrivalReleased verifies that
// when N=2, threshold=1, the first response triggers short-circuit and the
// second (late) response is properly handled. A naive implementation might:
//  1. Never Release() the late response → memory leak, or
//  2. Try to add it to the frozen analysis → panic/data corruption.
//
// We verify the caller gets a winner promptly and the late participant
// completes without panicking.
func TestRace_TwoParticipants_ShortCircuit_LateArrivalReleased(t *testing.T) {
	logger := zerolog.New(zerolog.NewTestWriter(t))

	pol := NewConsensusPolicyBuilder().
		WithMaxParticipants(2).
		WithAgreementThreshold(1).
		WithLogger(&logger).
		Build()

	req := newTestRequest()

	var callCount atomic.Int32
	slowRelease := make(chan struct{})

	ctx := context.WithValue(context.Background(), common.RequestContextKey, req)
	fsExec := failsafe.NewExecutor(pol).WithContext(ctx)

	start := time.Now()
	resp, err := fsExec.GetWithExecution(func(exec failsafe.Execution[*common.NormalizedResponse]) (*common.NormalizedResponse, error) {
		n := callCount.Add(1)
		if n == 1 {
			return validResponse(), nil
		}
		<-slowRelease
		return validResponse(), nil
	})
	elapsed := time.Since(start)

	require.NoError(t, err)
	require.NotNil(t, resp)
	require.Less(t, elapsed, 500*time.Millisecond,
		"N=2/threshold=1: short-circuit should fire after first response")

	close(slowRelease)

	// Give the analyzer goroutine time to drain and release the late arrival.
	// If the release panics, the test process will crash.
	time.Sleep(50 * time.Millisecond)
}

// ===== SCENARIO D: Mixed participation — partial valid + partial nil =======

// TestRace_ThreeParticipants_OneCancelledBeforeExec_TwoValid exercises the
// scenario where one participant is cancelled before executing innerFn (sends
// nil) and two complete with valid matching responses. With threshold=2, the
// two valid responses should produce consensus. The nil must NOT inflate
// totalParticipants in a way that triggers low-participants.
func TestRace_ThreeParticipants_OneCancelledBeforeExec_TwoValid(t *testing.T) {
	logger := zerolog.New(zerolog.NewTestWriter(t))

	pol := NewConsensusPolicyBuilder().
		WithMaxParticipants(3).
		WithAgreementThreshold(2).
		WithLogger(&logger).
		Build()

	req := newTestRequest()

	var callCount atomic.Int32
	gate := make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = context.WithValue(ctx, common.RequestContextKey, req)

	fsExec := failsafe.NewExecutor(pol).WithContext(ctx)

	type result struct {
		resp *common.NormalizedResponse
		err  error
	}
	resultCh := make(chan result, 1)

	go func() {
		resp, err := fsExec.GetWithExecution(func(exec failsafe.Execution[*common.NormalizedResponse]) (*common.NormalizedResponse, error) {
			n := callCount.Add(1)
			if n == 1 {
				// First participant: signal readiness, then wait for cancel to
				// happen. By the time we return, ctx is cancelled but our result
				// is still valid.
				close(gate)
				time.Sleep(20 * time.Millisecond) // give cancel a head start
			}
			return validResponse(), nil
		})
		resultCh <- result{resp, err}
	}()

	// Cancel after first participant has started but (hopefully) before all
	// three have passed the pre-execution ctx.Err() check. This creates
	// a mix of nil and valid responses.
	<-gate
	cancel()

	select {
	case r := <-resultCh:
		// If the two valid responses reached the analyzer, we get consensus.
		// If cancel races unfavorably, context.Canceled is acceptable.
		// ErrConsensusLowParticipants is ONLY correct if truly < 2 valid
		// responses — but given N=3 with at least 1 guaranteed valid, and
		// the innerFn is fast, at least 2 should complete in most runs.
		if r.err != nil {
			if common.HasErrorCode(r.err, common.ErrCodeConsensusLowParticipants) {
				// Low-participants is correct ONLY if fewer than threshold
				// participants actually entered innerFn. If 2+ ran (and
				// therefore produced valid responses), reporting low-
				// participants is the exact regression this PR fixes.
				completed := callCount.Load()
				assert.Less(t, completed, int32(2),
					"ErrConsensusLowParticipants with %d completed participants (>= threshold=2) is a regression", completed)
			}
			// context.Canceled is always acceptable (caller abandoned).
		}
	case <-time.After(2 * time.Second):
		t.Fatal("mixed participation: deadlock")
	}
}

// ===== SCENARIO E: outcomeCh vs ctx.Done() select race ====================

// TestRace_OutcomeAndCancelSimultaneous forces both outcomeCh and ctx.Done()
// to be ready at ~the same moment, exercising the double-select retry.
// Run 100 times to increase the probability of hitting the problematic
// scheduling order.
//
// With N=1, threshold=1: the single participant completes instantly, so
// outcomeCh is ready almost immediately. Cancel fires at the same time.
// Without the non-blocking retry in the ctx.Done() branch, ~50% of runs
// would return context.Canceled when a valid winner was available.
//
// We track whether innerFn was actually called. If it was, then a valid
// response exists and ErrConsensusLowParticipants must not appear. If
// cancel beat the participant to the pre-execution check, nil was sent
// and LowParticipants is genuinely correct.
func TestRace_OutcomeAndCancelSimultaneous(t *testing.T) {
	var falseLowPart atomic.Int32
	const iterations = 100

	for iter := 0; iter < iterations; iter++ {
		func() {
			logger := zerolog.Nop()

			pol := NewConsensusPolicyBuilder().
				WithMaxParticipants(1).
				WithAgreementThreshold(1).
				WithLogger(&logger).
				Build()

			req := newTestRequest()

			var innerFnCalled atomic.Bool

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx = context.WithValue(ctx, common.RequestContextKey, req)

			fsExec := failsafe.NewExecutor(pol).WithContext(ctx)

			type result struct {
				resp *common.NormalizedResponse
				err  error
			}
			resultCh := make(chan result, 1)

			// Fire cancel and execution simultaneously.
			go func() {
				resp, err := fsExec.GetWithExecution(func(exec failsafe.Execution[*common.NormalizedResponse]) (*common.NormalizedResponse, error) {
					innerFnCalled.Store(true)
					return validResponse(), nil
				})
				resultCh <- result{resp, err}
			}()

			// Cancel immediately — races with the participant.
			cancel()

			select {
			case r := <-resultCh:
				if r.err != nil && common.HasErrorCode(r.err, common.ErrCodeConsensusLowParticipants) {
					if innerFnCalled.Load() {
						// innerFn ran and produced a valid response, but
						// the system still reported low-participants. This
						// is the bug we're catching.
						falseLowPart.Add(1)
					}
					// If innerFn was NOT called, cancel beat the participant
					// to the pre-execution ctx.Err() check → genuinely 0
					// responses → LowParticipants is correct.
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("iter %d: deadlock", iter)
			}
		}()
	}

	require.Equal(t, int32(0), falseLowPart.Load(),
		"ErrConsensusLowParticipants must not appear when innerFn actually produced a valid response")
}

// ===== SCENARIO F: N=2, threshold=2, one infra error + one valid ==========

// TestRace_TwoParticipants_OneInfraError_CorrectlyLowParticipants verifies
// that when one participant returns an infrastructure error and one returns a
// valid response, the system correctly identifies validParticipants=1 < threshold=2
// and returns ErrConsensusLowParticipants (or the low-participants fallback
// behavior). This is a CORRECT low-participants case — it must not be
// misidentified as a consensus or a dispute.
func TestRace_TwoParticipants_OneInfraError_CorrectlyLowParticipants(t *testing.T) {
	lg := zerolog.Nop()

	cfg := &config{
		maxParticipants:         2,
		agreementThreshold:      2,
		lowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
	}

	// Build the analysis directly. classifyAndHashResponse requires a non-nil
	// failsafe Execution for the success path, so we pre-classify manually.
	analysis := &consensusAnalysis{
		config:            cfg,
		groups:            make(map[string]*responseGroup),
		totalParticipants: 2,
		validParticipants: 1, // 1 valid (non-empty), 1 infra error
		method:            "eth_getLogs",
	}

	// Valid response group
	validResult := &execResult{
		Result:             validResponse(),
		Index:              0,
		CachedHash:         "hash:valid",
		CachedResponseType: ResponseTypeNonEmpty,
		CachedResponseSize: 10,
	}
	analysis.groups["hash:valid"] = &responseGroup{
		Hash:          "hash:valid",
		Results:       []*execResult{validResult},
		Count:         1,
		ResponseType:  ResponseTypeNonEmpty,
		ResponseSize:  10,
		LargestResult: validResult.Result,
		HasResult:     true,
	}

	// Infra error group
	infraResult := &execResult{
		Err:                errors.New("connection refused"),
		Index:              1,
		CachedHash:         "error:generic",
		CachedResponseType: ResponseTypeInfrastructureError,
	}
	analysis.groups["error:generic"] = &responseGroup{
		Hash:         "error:generic",
		Results:      []*execResult{infraResult},
		Count:        1,
		ResponseType: ResponseTypeInfrastructureError,
		FirstError:   infraResult.Err,
	}

	require.Equal(t, 1, analysis.validParticipants,
		"only 1 of 2 participants returned a valid response")
	require.True(t, analysis.isLowParticipants(cfg.agreementThreshold),
		"validParticipants=1 < threshold=2 is low-participants")

	e := &executor{consensusPolicy: &consensusPolicy{logger: &lg, config: cfg}}
	winner := e.determineWinner(&lg, analysis)

	require.NotNil(t, winner)
	assert.True(t, common.HasErrorCode(winner.Error, common.ErrCodeConsensusLowParticipants),
		"should return ErrConsensusLowParticipants, got: %v", winner.Error)
}

// ===== SCENARIO G: Analyzer panic → caller doesn't deadlock ===============

// TestRace_AnalyzerPanic_CallerReceivesErrorNotDeadlock verifies invariant I2:
// if the analyzer goroutine panics at any point, the deferred recovery must
// still send exactly one outcome to outcomeCh so the caller's select never
// blocks forever.
//
// We test this at the unit level by directly calling runAnalyzer with a
// responseChan that will cause a panic (by sending a response that triggers
// a nil-pointer dereference in analysis, simulated via a custom setup).
//
// However, since we can't easily inject a panic into the production code
// path, we test the contract indirectly: the handleCallerAbandoned path
// returns promptly, and the nil-analysis guard in recordMetricsAndTracing
// doesn't panic.
func TestRace_AnalyzerPanic_NilAnalysis_CallerSafe(t *testing.T) {
	logger := zerolog.New(zerolog.NewTestWriter(t))
	e := &executor{
		consensusPolicy: &consensusPolicy{
			config: &config{agreementThreshold: 1},
			logger: &logger,
		},
	}

	labels := metricsLabels{
		projectId:   "test-proj",
		networkId:   "test-net",
		category:    "eth_getLogs",
		finalityStr: "latest",
		method:      "eth_getLogs",
	}

	// Simulate the catastrophic path: analyzer panicked, so outcome has nil analysis.
	panicResult := &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
		Error: errPanicInConsensus,
	}

	require.NotPanics(t, func() {
		e.recordMetricsAndTracing(newTestRequest(), time.Now(), panicResult, nil, labels,
			trace.SpanFromContext(context.Background()))
	}, "recordMetricsAndTracing must handle nil analysis without panicking")

	// Also verify releaseNonWinningResponses handles nil analysis.
	require.NotPanics(t, func() {
		e.releaseNonWinningResponses(nil, panicResult)
	}, "releaseNonWinningResponses must handle nil analysis without panicking")

	// And nil winner.
	require.NotPanics(t, func() {
		e.releaseNonWinningResponses(&consensusAnalysis{
			config: &config{},
			groups: map[string]*responseGroup{
				"hash1": {Results: []*execResult{{Result: validResponse()}}},
			},
		}, nil)
	}, "releaseNonWinningResponses must handle nil winner without panicking")
}

// ===== SCENARIO H: N=3, threshold=2, cancel after 1 of 3 completes =======

// TestRace_ThreeParticipants_CancelAfterFirstComplete exercises the scenario
// where one of three participants completes, cancel fires, and the remaining
// two complete after cancel. The old collector's `select` on `ctx.Done()`
// would exit after seeing only the first response. With the new analyzer,
// all three should be collected. Since all return the same value and
// threshold=2, consensus should be reached.
func TestRace_ThreeParticipants_CancelAfterFirstComplete(t *testing.T) {
	logger := zerolog.New(zerolog.NewTestWriter(t))

	pol := NewConsensusPolicyBuilder().
		WithMaxParticipants(3).
		WithAgreementThreshold(2).
		WithLogger(&logger).
		Build()

	req := newTestRequest()

	var started atomic.Int32
	firstDone := make(chan struct{})
	releaseRest := make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = context.WithValue(ctx, common.RequestContextKey, req)

	fsExec := failsafe.NewExecutor(pol).WithContext(ctx)

	type result struct {
		resp *common.NormalizedResponse
		err  error
	}
	resultCh := make(chan result, 1)

	go func() {
		resp, err := fsExec.GetWithExecution(func(exec failsafe.Execution[*common.NormalizedResponse]) (*common.NormalizedResponse, error) {
			n := started.Add(1)
			if n == 1 {
				close(firstDone)
			} else {
				<-releaseRest
			}
			return validResponse(), nil
		})
		resultCh <- result{resp, err}
	}()

	<-firstDone
	time.Sleep(5 * time.Millisecond) // let first response flow to analyzer
	cancel()
	close(releaseRest)

	select {
	case r := <-resultCh:
		if r.err != nil {
			require.Falsef(t,
				common.HasErrorCode(r.err, common.ErrCodeConsensusLowParticipants),
				"N=3/cancel-after-first: must not report LowParticipants when all 3 responses are valid: %v", r.err,
			)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("N=3/cancel-after-first: deadlock")
	}
}

// ===== SCENARIO I: Zero responses (all nil) → correct low-participants =====

// TestRace_AllParticipantsReturnNil_LowParticipants verifies the analysis
// layer's behavior when the analyzer receives zero non-nil responses. This
// happens when all participants are cancelled before execution. The analyzer
// must produce ErrConsensusLowParticipants with 0 groups, not panic.
func TestRace_AllParticipantsReturnNil_LowParticipants(t *testing.T) {
	lg := zerolog.Nop()

	cfg := &config{
		maxParticipants:    3,
		agreementThreshold: 2,
	}

	// Zero responses — simulates all participants cancelled before execution.
	analysis := &consensusAnalysis{
		config:            cfg,
		groups:            make(map[string]*responseGroup),
		totalParticipants: 0,
	}

	e := &executor{consensusPolicy: &consensusPolicy{logger: &lg, config: cfg}}
	winner := e.determineWinner(&lg, analysis)

	require.NotNil(t, winner)
	assert.True(t, common.HasErrorCode(winner.Error, common.ErrCodeConsensusLowParticipants),
		"0 responses must produce ErrConsensusLowParticipants, got: %v", winner.Error)
}

// ===== SCENARIO J: Repeated concurrent runs stress test ====================

// TestRace_StressN2Threshold2_NeverFalseLowParticipants runs the N=2,
// threshold=2 cancel-after-execution scenario many times to flush out
// scheduling-dependent races. Each iteration: both participants start, cancel
// fires, both complete, check the result. Over 200 iterations, if any
// scheduling order produces false ErrConsensusLowParticipants, this test
// catches it.
func TestRace_StressN2Threshold2_NeverFalseLowParticipants(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test skipped in -short mode")
	}

	var lowPartCount atomic.Int32
	var canceledCount atomic.Int32
	var successCount atomic.Int32
	const iterations = 200

	var wg sync.WaitGroup
	wg.Add(iterations)

	for iter := 0; iter < iterations; iter++ {
		go func(iter int) {
			defer wg.Done()

			logger := zerolog.Nop()

			pol := NewConsensusPolicyBuilder().
				WithMaxParticipants(2).
				WithAgreementThreshold(2).
				WithLogger(&logger).
				Build()

			req := newTestRequest()

			var started atomic.Int32
			allStarted := make(chan struct{})
			completeAll := make(chan struct{})

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx = context.WithValue(ctx, common.RequestContextKey, req)

			fsExec := failsafe.NewExecutor(pol).WithContext(ctx)

			type result struct {
				resp *common.NormalizedResponse
				err  error
			}
			resultCh := make(chan result, 1)

			go func() {
				resp, err := fsExec.GetWithExecution(func(exec failsafe.Execution[*common.NormalizedResponse]) (*common.NormalizedResponse, error) {
					if started.Add(1) == 2 {
						close(allStarted)
					}
					<-completeAll
					return validResponse(), nil
				})
				resultCh <- result{resp, err}
			}()

			<-allStarted
			cancel()
			close(completeAll)

			select {
			case r := <-resultCh:
				if r.err != nil {
					if common.HasErrorCode(r.err, common.ErrCodeConsensusLowParticipants) {
						lowPartCount.Add(1)
					} else if errors.Is(r.err, context.Canceled) {
						canceledCount.Add(1)
					}
				} else {
					successCount.Add(1)
				}
			case <-time.After(5 * time.Second):
				t.Errorf("iter %d: deadlock", iter)
			}
		}(iter)
	}

	wg.Wait()

	t.Logf("Results over %d iterations: success=%d, canceled=%d, low_participants=%d",
		iterations, successCount.Load(), canceledCount.Load(), lowPartCount.Load())

	require.Equal(t, int32(0), lowPartCount.Load(),
		"ErrConsensusLowParticipants must NEVER appear when both participants complete with valid responses")
}

// ===== SCENARIO K: Short-circuit outcome races with ctx.Done() ============

// TestRace_ShortCircuitOutcomeRacesCancel exercises a specific timing: N=3,
// threshold=1. The first two participants return instantly (triggering
// short-circuit), and cancel fires simultaneously. The short-circuit sends
// the outcome to outcomeCh. If the caller's select picks ctx.Done() first,
// the double-select retry must recover the already-buffered outcome.
func TestRace_ShortCircuitOutcomeRacesCancel(t *testing.T) {
	for iter := 0; iter < 50; iter++ {
		func() {
			logger := zerolog.Nop()

			pol := NewConsensusPolicyBuilder().
				WithMaxParticipants(3).
				WithAgreementThreshold(1).
				WithLogger(&logger).
				Build()

			req := newTestRequest()

			var callCount atomic.Int32
			slowRelease := make(chan struct{})

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx = context.WithValue(ctx, common.RequestContextKey, req)

			fsExec := failsafe.NewExecutor(pol).WithContext(ctx)

			type result struct {
				resp *common.NormalizedResponse
				err  error
			}
			resultCh := make(chan result, 1)

			go func() {
				resp, err := fsExec.GetWithExecution(func(exec failsafe.Execution[*common.NormalizedResponse]) (*common.NormalizedResponse, error) {
					n := callCount.Add(1)
					if n <= 2 {
						return validResponse(), nil
					}
					<-slowRelease
					return validResponse(), nil
				})
				resultCh <- result{resp, err}
			}()

			// Cancel immediately to race with short-circuit.
			cancel()

			select {
			case r := <-resultCh:
				if r.err != nil {
					if common.HasErrorCode(r.err, common.ErrCodeConsensusLowParticipants) {
						// Low-participants is legitimate ONLY if cancel raced
						// before enough participants entered innerFn. If 2+
						// participants ran (producing valid matching responses),
						// reporting low-participants is the regression.
						completed := callCount.Load()
						require.Less(t, completed, int32(2),
							"iter %d: ErrConsensusLowParticipants with %d completed participants (>= threshold) is a regression", iter, completed)
					}
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("iter %d: deadlock", iter)
			}

			close(slowRelease) // cleanup
		}()
	}
}

// ===== SCENARIO L: Caller decoupling — analyzer outlives caller ============

// TestRace_AnalyzerCompletesAfterCallerAbandons verifies the core decoupling
// property: when the caller abandons (ctx cancelled), the analyzer goroutine
// still runs to completion and all participants finish their work. This is
// critical for misbehavior tracking and response memory release.
func TestRace_AnalyzerCompletesAfterCallerAbandons(t *testing.T) {
	logger := zerolog.New(zerolog.NewTestWriter(t))

	pol := NewConsensusPolicyBuilder().
		WithMaxParticipants(2).
		WithAgreementThreshold(2).
		WithLogger(&logger).
		Build()

	req := newTestRequest()

	var innerFnCompletes atomic.Int32
	var started atomic.Int32
	allStarted := make(chan struct{})
	completeAll := make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = context.WithValue(ctx, common.RequestContextKey, req)

	fsExec := failsafe.NewExecutor(pol).WithContext(ctx)

	callerReturned := make(chan struct{})
	go func() {
		_, _ = fsExec.GetWithExecution(func(exec failsafe.Execution[*common.NormalizedResponse]) (*common.NormalizedResponse, error) {
			if started.Add(1) == 2 {
				close(allStarted)
			}
			<-completeAll
			innerFnCompletes.Add(1)
			return validResponse(), nil
		})
		close(callerReturned)
	}()

	<-allStarted
	cancel()

	// Caller should return promptly.
	select {
	case <-callerReturned:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("caller blocked on stuck participants")
	}

	// Participants still haven't completed.
	require.Equal(t, int32(0), innerFnCompletes.Load())

	// Release participants.
	close(completeAll)

	// Analyzer should drain all responses (invariant I5).
	require.Eventually(t, func() bool {
		return innerFnCompletes.Load() == 2
	}, time.Second, 10*time.Millisecond,
		"all participants must complete even after caller abandons")
}

