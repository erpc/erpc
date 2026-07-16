package consensus

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"
)

func init() {
	util.ConfigureTestLogger()
	// Required: MetricConsensusDuration is a histogram that panics on
	// WithLabelValues unless buckets have been initialized.
	_ = telemetry.SetHistogramBuckets("")
}

func newTestRequest() *common.NormalizedRequest {
	return common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{"fromBlock":"0x1","toBlock":"0x1"}]}`,
	))
}

// validResponse returns a non-empty response so the unassailable_lead
// short-circuit rule (which requires ResponseTypeNonEmpty) can fire when
// tests exercise it. The content is arbitrary; only the non-empty shape
// matters for classification.
func validResponse() *common.NormalizedResponse {
	jrpc, _ := common.NewJsonRpcResponse(1, []interface{}{"0x1"}, nil)
	return common.NewNormalizedResponse().WithJsonRpcResponse(jrpc)
}

func validResponseWithValue(value string) *common.NormalizedResponse {
	jrpc, _ := common.NewJsonRpcResponse(1, []interface{}{value}, nil)
	return common.NewNormalizedResponse().WithJsonRpcResponse(jrpc)
}

type trackingReadCloser struct {
	closeCount *atomic.Int32
}

func (t *trackingReadCloser) Read(_ []byte) (int, error) {
	return 0, io.EOF
}

func (t *trackingReadCloser) Close() error {
	if t.closeCount != nil {
		t.closeCount.Add(1)
	}
	return nil
}

func validResponseWithCloser(value string, closeCount *atomic.Int32) *common.NormalizedResponse {
	return validResponseWithValue(value).WithBody(&trackingReadCloser{closeCount: closeCount})
}

// TestConsensus_ContextCancelAfterExecution_DoesNotReturnLowParticipants verifies
// the original bug fix: when the parent context is cancelled after every
// participant has completed innerFn with a valid result, the consensus machinery
// must NOT report ErrConsensusLowParticipants.
//
// Test construction notes:
//   - agreementThreshold=3 (all participants must agree) prevents short-circuit
//     from masking the bug — the analyzer is forced to read every response.
//   - Synchronization uses channels, not time.Sleep, so the test is not flaky
//     under CI scheduling pressure.
//   - The cancel fires only after ALL three participants are inside innerFn,
//     deterministically producing the "post-execution cancel" scenario.
func TestConsensus_ContextCancelAfterExecution_DoesNotReturnLowParticipants(t *testing.T) {
	logger := zerolog.New(zerolog.NewTestWriter(t))

	pol := NewConsensusPolicyBuilder().
		WithMaxParticipants(3).
		WithAgreementThreshold(3).
		WithLogger(&logger).
		Build()

	req := newTestRequest()

	var started atomic.Int32
	allStarted := make(chan struct{})
	completeInnerFn := make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = context.WithValue(ctx, common.RequestContextKey, req)

	type result struct {
		resp *common.NormalizedResponse
		err  error
	}
	resultCh := make(chan result, 1)

	go func() {
		resp, err := pol.Run(ctx, req, func(_ context.Context, _ *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			if started.Add(1) == 3 {
				close(allStarted)
			}
			<-completeInnerFn
			return validResponse(), nil
		})
		resultCh <- result{resp, err}
	}()

	// Wait until all three participants are inside innerFn. Only then cancel.
	<-allStarted
	cancel()

	// Allow participants to finish their work. Their results are valid and
	// should flow to the analyzer even though ctx is cancelled.
	close(completeInnerFn)

	select {
	case r := <-resultCh:
		// Under Option D, two outcomes are valid:
		//   1. The analyzer wins the caller select with a valid winner.
		//   2. The caller wins the select with context.Canceled.
		// What MUST NOT happen is ErrConsensusLowParticipants — that was the bug.
		if r.err != nil {
			require.Falsef(t,
				common.HasErrorCode(r.err, common.ErrCodeConsensusLowParticipants),
				"must not report ErrConsensusLowParticipants when 3 valid responses are available: %v", r.err,
			)
			require.Truef(t,
				errors.Is(r.err, context.Canceled) || errors.Is(r.err, context.DeadlineExceeded),
				"unexpected error after caller abandon: %v", r.err,
			)
		} else {
			require.NotNil(t, r.resp)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("consensus did not return within 2s after context cancel")
	}
}

// TestExecuteParticipant_PostExecutionCancel_PreservesResult locks down the
// exact bug window at the smallest unit boundary: ctx is canceled after the
// participant finishes innerFn but before executeParticipant decides whether
// to forward the result. A naive implementation drops the result as nil and
// creates a false low-participants analysis downstream.
func TestExecuteParticipant_PostExecutionCancel_PreservesResult(t *testing.T) {
	logger := zerolog.New(zerolog.NewTestWriter(t))
	e := &executor{
		consensusPolicy: &consensusPolicy{
			config: &config{},
			logger: &logger,
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var bodyClosed atomic.Int32
	expected := validResponseWithCloser("0xfeed", &bodyClosed)
	responseCh := make(chan *execResult, 1)

	e.executeParticipant(
		ctx,
		&logger,
		metricsLabels{},
		func(_ context.Context, _ *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			cancel()
			return expected, nil
		},
		newTestRequest(),
		0,
		responseCh,
	)

	select {
	case got := <-responseCh:
		require.NotNil(t, got, "post-execution cancellation must still forward the completed result")
		require.Same(t, expected, got.Result)
		require.NoError(t, got.Err)
		require.Equal(t, int32(0), bodyClosed.Load(), "executeParticipant must not release a still-valid result")
	case <-time.After(time.Second):
		t.Fatal("executeParticipant did not forward its result within 1s")
	}
}

// TestConsensus_CallerAbandons_ParticipantsStillComplete verifies that when the
// caller abandons the request (ctx cancelled), the analyzer goroutine keeps
// running and all participants get a chance to execute their innerFn. This is
// the core property of Option D: caller latency is decoupled from analysis
// completeness.
func TestConsensus_CallerAbandons_ParticipantsStillComplete(t *testing.T) {
	logger := zerolog.New(zerolog.NewTestWriter(t))

	pol := NewConsensusPolicyBuilder().
		WithMaxParticipants(3).
		WithAgreementThreshold(3).
		WithLogger(&logger).
		Build()

	req := newTestRequest()

	var innerFnStarts atomic.Int32
	var innerFnCompletes atomic.Int32
	allStarted := make(chan struct{})
	completeInnerFn := make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = context.WithValue(ctx, common.RequestContextKey, req)

	callerReturned := make(chan struct{})
	go func() {
		_, _ = pol.Run(ctx, req, func(_ context.Context, _ *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			if innerFnStarts.Add(1) == 3 {
				close(allStarted)
			}
			<-completeInnerFn
			innerFnCompletes.Add(1)
			return validResponse(), nil
		})
		close(callerReturned)
	}()

	<-allStarted
	cancel()

	// Caller should return promptly without waiting for participants to finish.
	// Participants are still blocked inside innerFn.
	select {
	case <-callerReturned:
		// Caller returned quickly — this is the Option D guarantee.
	case <-time.After(500 * time.Millisecond):
		t.Fatal("caller did not return within 500ms of ctx cancel — blocked on stuck participants")
	}

	// Sanity: participants are still running (have started but not completed).
	require.Equal(t, int32(3), innerFnStarts.Load(), "all participants should have entered innerFn")
	require.Equal(t, int32(0), innerFnCompletes.Load(), "no participant should have completed innerFn yet")

	// Now let participants finish. The analyzer reads their results even
	// though the caller is long gone.
	close(completeInnerFn)

	require.Eventually(t, func() bool {
		return innerFnCompletes.Load() == 3
	}, time.Second, 10*time.Millisecond, "all three participants should complete after unblock")
}

// TestConsensus_TwoParticipants_CancelAfterExecution_DoesNotReturnLowParticipants
// covers the smallest quorum boundary that still requires agreement. This is
// the most failure-prone production shape: only two eligible participants, both
// finish real work, and the caller disconnects before analysis completes.
func TestConsensus_TwoParticipants_CancelAfterExecution_DoesNotReturnLowParticipants(t *testing.T) {
	logger := zerolog.New(zerolog.NewTestWriter(t))

	pol := NewConsensusPolicyBuilder().
		WithMaxParticipants(2).
		WithAgreementThreshold(2).
		WithLogger(&logger).
		Build()

	req := newTestRequest()

	var started atomic.Int32
	allStarted := make(chan struct{})
	completeInnerFn := make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = context.WithValue(ctx, common.RequestContextKey, req)

	type result struct {
		resp *common.NormalizedResponse
		err  error
	}
	resultCh := make(chan result, 1)

	go func() {
		resp, err := pol.Run(ctx, req, func(_ context.Context, _ *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			if started.Add(1) == 2 {
				close(allStarted)
			}
			<-completeInnerFn
			return validResponse(), nil
		})
		resultCh <- result{resp, err}
	}()

	<-allStarted
	cancel()
	close(completeInnerFn)

	select {
	case r := <-resultCh:
		if r.err != nil {
			require.Falsef(t,
				common.HasErrorCode(r.err, common.ErrCodeConsensusLowParticipants),
				"must not report ErrConsensusLowParticipants when both 2/2 responses are valid: %v", r.err,
			)
			require.True(t,
				errors.Is(r.err, context.Canceled) || errors.Is(r.err, context.DeadlineExceeded),
				"unexpected error after caller abandon at 2/2 boundary: %v", r.err,
			)
		} else {
			require.NotNil(t, r.resp)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("consensus did not return within 2s at the 2/2 participant boundary")
	}
}

// TestConsensus_ShortCircuit_CallerGetsWinnerBeforeSlowParticipants verifies
// that short-circuit still works under Option D: once enough matching
// responses arrive to give one group an unassailable lead, the caller
// receives the winner without waiting for the remaining slow participants.
//
// With maxParticipants=3 and agreementThreshold=1, short-circuit fires after
// two matching responses (lead=2, remaining=1). So we configure two fast
// participants and one slow one — after the two fast ones return, the
// analyzer must short-circuit and hand the caller a winner.
func TestConsensus_ShortCircuit_CallerGetsWinnerBeforeSlowParticipants(t *testing.T) {
	logger := zerolog.New(zerolog.NewTestWriter(t))

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

	start := time.Now()
	resp, err := pol.Run(ctx, req, func(_ context.Context, _ *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		n := callCount.Add(1)
		if n <= 2 {
			return validResponse(), nil // two fast participants with matching responses
		}
		<-slowRelease // third participant blocks indefinitely
		return validResponse(), nil
	})
	elapsed := time.Since(start)

	require.NoError(t, err)
	require.NotNil(t, resp)
	require.Less(t, elapsed, 500*time.Millisecond, "short-circuit should fire before the slow participant completes")

	// Unblock the slow participant so the analyzer can finish in the background
	// and the test doesn't leak a goroutine.
	close(slowRelease)
}

// TestConsensus_ShortCircuit_ReleasesLateResponses verifies the cleanup side of
// the refactor: once the caller has been released on short-circuit, a later
// non-winning response still has to be released exactly once. Without this,
// the branch fixes correctness but leaks response buffers in the background.
func TestConsensus_ShortCircuit_ReleasesLateResponses(t *testing.T) {
	logger := zerolog.New(zerolog.NewTestWriter(t))

	pol := NewConsensusPolicyBuilder().
		WithMaxParticipants(3).
		WithAgreementThreshold(1).
		WithLogger(&logger).
		Build()

	req := newTestRequest()

	var callCount atomic.Int32
	var slowBodyClosed atomic.Int32
	slowRelease := make(chan struct{})

	// A start barrier ensures all three participants pass executeParticipant's
	// ctx.Err() pre-check and actually enter innerFn before any of them
	// returns. Without this, slow CI schedulers may let participant 1 finish
	// and short-circuit (cancelling the shared ctx) before participant 3 ever
	// enters innerFn, causing the pre-check to early-return with nil and the
	// trackingReadCloser never being constructed — a false negative for this
	// test's "late response is released" invariant.
	var started sync.WaitGroup
	started.Add(3)
	allStarted := make(chan struct{})
	go func() {
		started.Wait()
		close(allStarted)
	}()

	ctx := context.WithValue(context.Background(), common.RequestContextKey, req)

	resp, err := pol.Run(ctx, req, func(_ context.Context, _ *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		n := callCount.Add(1)
		started.Done()
		<-allStarted
		switch n {
		case 1, 2:
			return validResponseWithValue("0x1"), nil
		default:
			<-slowRelease
			return validResponseWithCloser("0x2", &slowBodyClosed), nil
		}
	})

	require.NoError(t, err)
	require.NotNil(t, resp)
	require.Equal(t, int32(0), slowBodyClosed.Load(), "slow response should not be released before it finishes")

	close(slowRelease)

	require.Eventually(t, func() bool {
		return slowBodyClosed.Load() == 1
	}, 5*time.Second, 10*time.Millisecond, "late post-short-circuit response should be released exactly once")
}

// TestConsensus_HappyPath_NoCancel verifies the refactor does not break the
// normal non-cancelled execution path.
func TestConsensus_HappyPath_NoCancel(t *testing.T) {
	logger := zerolog.New(zerolog.NewTestWriter(t))

	pol := NewConsensusPolicyBuilder().
		WithMaxParticipants(3).
		WithAgreementThreshold(2).
		WithLogger(&logger).
		Build()

	req := newTestRequest()

	ctx := context.WithValue(context.Background(), common.RequestContextKey, req)

	resp, err := pol.Run(ctx, req, func(_ context.Context, _ *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		return validResponse(), nil
	})

	require.NoError(t, err)
	require.NotNil(t, resp)
}

// TestConsensus_CancelBeforeExecution_ReturnsLowParticipants verifies the
// preserved behavior for the case that is NOT the bug being fixed: when every
// participant sees the context as already-cancelled before it enters innerFn,
// there are no results to analyze, and ErrConsensusLowParticipants is the
// correct response.
func TestConsensus_CancelBeforeExecution_ReturnsLowParticipants(t *testing.T) {
	logger := zerolog.New(zerolog.NewTestWriter(t))

	pol := NewConsensusPolicyBuilder().
		WithMaxParticipants(3).
		WithAgreementThreshold(2).
		WithLogger(&logger).
		Build()

	req := newTestRequest()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately, before any participant runs
	ctx = context.WithValue(ctx, common.RequestContextKey, req)

	var callCount atomic.Int32
	_, err := pol.Run(ctx, req, func(_ context.Context, _ *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		callCount.Add(1)
		return validResponse(), nil
	})

	// Caller may see either ErrConsensusLowParticipants (if analyzer ran to
	// completion and saw 0 valid responses) or context.Canceled (if the caller
	// select hit ctx.Done() before the analyzer sent the outcome). Both are
	// correct; what matters is that neither a bogus consensus nor a panic
	// occurs.
	require.Error(t, err)
	assert.True(t,
		common.HasErrorCode(err, common.ErrCodeConsensusLowParticipants) ||
			errors.Is(err, context.Canceled),
		"unexpected error when ctx is cancelled before any participant runs: %v", err,
	)
}

// TestConsensus_FireAndForget_CallerCancelDoesNotStopParticipants verifies
// that fire-and-forget mode continues to run participants to completion even
// after the caller's context is cancelled. This is the critical property for
// transaction broadcasting: the tx must reach every node regardless of
// whether the HTTP client disconnected.
func TestConsensus_FireAndForget_CallerCancelDoesNotStopParticipants(t *testing.T) {
	logger := zerolog.New(zerolog.NewTestWriter(t))

	pol := NewConsensusPolicyBuilder().
		WithMaxParticipants(3).
		WithAgreementThreshold(3).
		WithFireAndForget(true).
		WithLogger(&logger).
		Build()

	req := newTestRequest()

	var innerFnStarts atomic.Int32
	var innerFnCompletes atomic.Int32
	allStarted := make(chan struct{})
	completeInnerFn := make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = context.WithValue(ctx, common.RequestContextKey, req)

	callerReturned := make(chan struct{})
	go func() {
		_, _ = pol.Run(ctx, req, func(_ context.Context, _ *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			if innerFnStarts.Add(1) == 3 {
				close(allStarted)
			}
			<-completeInnerFn
			innerFnCompletes.Add(1)
			return validResponse(), nil
		})
		close(callerReturned)
	}()

	<-allStarted
	cancel()

	// Caller returns quickly.
	select {
	case <-callerReturned:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("fire-and-forget caller did not return promptly after ctx cancel")
	}

	// Participants have not been cancelled (fire-and-forget uses WithoutCancel).
	require.Equal(t, int32(0), innerFnCompletes.Load(), "participants must not complete before we release them")

	// Release them; they should all complete.
	close(completeInnerFn)

	require.Eventually(t, func() bool {
		return innerFnCompletes.Load() == 3
	}, time.Second, 10*time.Millisecond, "all fire-and-forget participants should complete after unblock")
}

// TestConsensus_CallerAbandons_WinnerResponseIsReleased guards against the
// resource leak where a caller cancels its context before the analyzer
// publishes an outcome: the analyzer still completes and publishes a winner
// to the buffered outcomeCh, but nothing up the stack ever calls
// winner.Result.Release() because the caller isn't returning the winner.
// Without the abandon-path drain goroutine, the winner's body (JSON-RPC
// buffer, pooled handles, trackingReadCloser) would never close.
func TestConsensus_CallerAbandons_WinnerResponseIsReleased(t *testing.T) {
	logger := zerolog.New(zerolog.NewTestWriter(t))

	pol := NewConsensusPolicyBuilder().
		WithMaxParticipants(3).
		WithAgreementThreshold(1).
		WithLogger(&logger).
		Build()

	req := newTestRequest()

	var callCount atomic.Int32
	var winnerBodyClosed atomic.Int32
	var started sync.WaitGroup
	started.Add(3)
	allStarted := make(chan struct{})
	go func() {
		started.Wait()
		close(allStarted)
	}()
	completeInnerFn := make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = context.WithValue(ctx, common.RequestContextKey, req)

	callerReturned := make(chan struct{})
	go func() {
		_, _ = pol.Run(ctx, req, func(_ context.Context, _ *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			n := callCount.Add(1)
			started.Done()
			<-allStarted
			<-completeInnerFn
			if n == 1 {
				// First response becomes the winner (threshold=1 →
				// short-circuit on first valid result).
				return validResponseWithCloser("0x1", &winnerBodyClosed), nil
			}
			return validResponseWithValue("0x1"), nil
		})
		close(callerReturned)
	}()

	<-allStarted
	cancel() // Abandon before any participant completes.

	select {
	case <-callerReturned:
	case <-time.After(time.Second):
		t.Fatal("caller did not return promptly after ctx cancel")
	}

	// Caller is gone; nothing up the stack holds a reference to the winner.
	require.Equal(t, int32(0), winnerBodyClosed.Load(), "winner must not be released until analyzer finishes its reads")

	// Unblock participants; analyzer now processes them, publishes the winner
	// on outcomeCh, and finishes its cleanup — at which point the drain
	// goroutine releases the winner.
	close(completeInnerFn)

	require.Eventually(t, func() bool {
		return winnerBodyClosed.Load() == 1
	}, 5*time.Second, 10*time.Millisecond, "abandon-path drain goroutine must release winner exactly once")
}

// TestRecordMetricsAndTracing_NilAnalysis_DoesNotPanic covers the catastrophic
// path where the analyzer goroutine panics before any responses are
// classified and hands the caller an outcome with nil analysis. The caller
// must not trigger a secondary nil-pointer dereference when recording
// metrics — it should emit a minimal outcome and return.
func TestRecordMetricsAndTracing_NilAnalysis_DoesNotPanic(t *testing.T) {
	logger := zerolog.New(zerolog.NewTestWriter(t))
	e := &executor{
		consensusPolicy: &consensusPolicy{
			config: &config{agreementThreshold: 1},
			logger: &logger,
		},
	}

	req := newTestRequest()
	labels := metricsLabels{
		projectId:   "test-proj",
		networkId:   "test-net",
		category:    "eth_getLogs",
		finalityStr: "latest",
		method:      "eth_getLogs",
	}
	span := trace.SpanFromContext(context.Background()) // noop span

	result := &slotResult{
		Error: errors.New("simulated analyzer panic"),
	}

	require.NotPanics(t, func() {
		e.recordMetricsAndTracing(req, time.Now(), result, nil /* analysis */, labels, span)
	})
}

// TestRecordMetricsAndTracing_InfoSeverityNotCountedAsConsensusError verifies
// that rounds settling on a deterministic client/execution error (severity
// info — e.g. all upstreams reject an eth_sendRawTransaction broadcast with
// nonce-too-low) do not increment consensus_errors_total: consensus itself
// succeeded, the error belongs to the caller. Non-info errors on the same
// path must still be counted.
func TestRecordMetricsAndTracing_InfoSeverityNotCountedAsConsensusError(t *testing.T) {
	logger := zerolog.New(zerolog.NewTestWriter(t))
	cfg := &config{agreementThreshold: 2}
	e := &executor{
		consensusPolicy: &consensusPolicy{
			config: cfg,
			logger: &logger,
		},
	}

	revertErr := common.NewErrEndpointExecutionException(
		common.NewErrJsonRpcExceptionInternal(3, 3, "execution reverted", nil, nil),
	)

	// Two participants agreeing on the same execution error → hasConsensus,
	// outcome "consensus_on_error".
	buildAgreedErrorAnalysis := func() *consensusAnalysis {
		responses := []*execResult{
			{Err: revertErr, Index: 0},
			{Err: revertErr, Index: 1},
		}
		analysis := &consensusAnalysis{
			config:            cfg,
			groups:            make(map[string]*responseGroup),
			totalParticipants: len(responses),
			method:            "eth_sendRawTransaction",
		}
		for _, r := range responses {
			classifyAndHashResponse(r, nil, cfg)
			analysis.validParticipants++
			group, exists := analysis.groups[r.CachedHash]
			if !exists {
				group = &responseGroup{
					Hash:         r.CachedHash,
					ResponseType: r.CachedResponseType,
				}
				analysis.groups[r.CachedHash] = group
			}
			group.Count++
			group.Results = append(group.Results, r)
			if group.FirstError == nil {
				group.FirstError = r.Err
			}
		}
		return analysis
	}

	span := trace.SpanFromContext(context.Background()) // noop span

	t.Run("info severity skips consensus_errors_total", func(t *testing.T) {
		labels := metricsLabels{
			projectId:   "test-proj-info-sev",
			networkId:   "test-net",
			category:    "eth_sendRawTransaction",
			finalityStr: "latest",
			method:      "eth_sendRawTransaction",
			userId:      "n/a",
			agentName:   "n/a",
		}
		errCounter := telemetry.MetricConsensusErrors.WithLabelValues(
			labels.projectId, labels.networkId, labels.category, "consensus_on_error", labels.finalityStr, labels.userId, labels.agentName)
		totalCounter := telemetry.MetricConsensusTotal.WithLabelValues(
			labels.projectId, labels.networkId, labels.category, "consensus_on_error", labels.finalityStr, labels.userId, labels.agentName)
		errBefore := testutil.ToFloat64(errCounter)
		totalBefore := testutil.ToFloat64(totalCounter)

		e.recordMetricsAndTracing(newTestRequest(), time.Now(),
			&slotResult{Error: revertErr}, buildAgreedErrorAnalysis(), labels, span)

		assert.Equal(t, totalBefore+1, testutil.ToFloat64(totalCounter),
			"round must still be counted in consensus_total with consensus_on_error outcome")
		assert.Equal(t, errBefore, testutil.ToFloat64(errCounter),
			"info-severity agreed error must not count as a consensus error")
	})

	t.Run("non-info severity still increments consensus_errors_total", func(t *testing.T) {
		labels := metricsLabels{
			projectId:   "test-proj-warn-sev",
			networkId:   "test-net",
			category:    "eth_sendRawTransaction",
			finalityStr: "latest",
			method:      "eth_sendRawTransaction",
			userId:      "n/a",
			agentName:   "n/a",
		}
		serverErr := common.NewErrEndpointServerSideException(errors.New("boom"), nil, 500)
		errCounter := telemetry.MetricConsensusErrors.WithLabelValues(
			labels.projectId, labels.networkId, labels.category, "consensus_on_error", labels.finalityStr, labels.userId, labels.agentName)
		errBefore := testutil.ToFloat64(errCounter)

		e.recordMetricsAndTracing(newTestRequest(), time.Now(),
			&slotResult{Error: serverErr}, buildAgreedErrorAnalysis(), labels, span)

		assert.Equal(t, errBefore+1, testutil.ToFloat64(errCounter),
			"non-info errors must still be counted as consensus errors")
	})
}
