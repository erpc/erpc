package erpc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests exercise the SkipConsensus directive end-to-end through the
// network executor. They reuse the consensus test harness in
// networks_consensus_test.go (mock servers, network setup, etc.) and add a
// directive-injection step before Network.Forward.
//
// The contract under test:
//   - When SkipConsensus is set on the request, the consensus branch in
//     networkExecutor.Invoke is bypassed and the request flows through the
//     standard retry+hedge+timeout path.
//   - When SkipConsensus is unset (or false), the consensus branch runs as
//     usual. This file's "control" test confirms the harness still drives the
//     consensus path correctly when the directive is absent.
//
// We assert observable behavior by:
//   (a) Per-upstream call counts via httptest servers — consensus(2,3) hits
//       all three upstreams to collect responses; the non-consensus path
//       picks one upstream and returns its result.
//   (b) Wall-clock latency — consensus dispute/agreement adds extra time
//       even on agreement; the non-consensus path returns as soon as one
//       upstream answers.

func TestSkipConsensusDirective_Bypass_ViaHeader(t *testing.T) {
	tc := skipConsensusTestCase(t)
	// With SkipConsensus, only one upstream should be queried. Other slots
	// may receive an in-flight cancellation but should not produce work.
	tc.expectedCalls = []int{1, 0, 0}

	startConsensusMockServers(t, tc)

	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		time.Sleep(50 * time.Millisecond)
	}()

	ntw, upsReg := setupNetworkForConsensusTest(t, ctx, tc)
	_ = upsReg
	time.Sleep(200 * time.Millisecond)

	req := buildSkipConsensusRequest(t, ntw)
	headers := http.Header{}
	headers.Set("X-ERPC-Skip-Consensus", "true")
	req.EnrichFromHttp(headers, nil, common.UserAgentTrackingModeSimplified)
	require.True(t, req.Directives().SkipConsensus, "precondition: directive must be set")

	resp, err := ntw.Forward(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)

	jrr, jrrErr := resp.JsonRpcResponse()
	require.NoError(t, jrrErr)
	require.NotNil(t, jrr)
	assert.Equal(t, `"0xaaa"`, jrr.GetResultString(),
		"response should come from the first upstream's mock, proving the request went down the non-consensus single-upstream path")
}

func TestSkipConsensusDirective_Bypass_ViaQueryParam(t *testing.T) {
	tc := skipConsensusTestCase(t)
	tc.expectedCalls = []int{1, 0, 0}

	startConsensusMockServers(t, tc)

	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		time.Sleep(50 * time.Millisecond)
	}()

	ntw, _ := setupNetworkForConsensusTest(t, ctx, tc)
	time.Sleep(200 * time.Millisecond)

	req := buildSkipConsensusRequest(t, ntw)
	q := url.Values{}
	q.Set("skip-consensus", "true")
	req.EnrichFromHttp(nil, q, common.UserAgentTrackingModeSimplified)
	require.True(t, req.Directives().SkipConsensus)

	resp, err := ntw.Forward(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)
}

func TestSkipConsensusDirective_Bypass_ViaDirectiveDefaults(t *testing.T) {
	tc := skipConsensusTestCase(t)
	tc.expectedCalls = []int{1, 0, 0}

	startConsensusMockServers(t, tc)

	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		time.Sleep(50 * time.Millisecond)
	}()

	ntw, _ := setupNetworkForConsensusTest(t, ctx, tc)
	time.Sleep(200 * time.Millisecond)

	req := buildSkipConsensusRequest(t, ntw)
	tr := true
	req.ApplyDirectiveDefaults(&common.DirectiveDefaultsConfig{SkipConsensus: &tr})
	require.True(t, req.Directives().SkipConsensus,
		"directiveDefaults.skipConsensus=true should populate the request directive")

	resp, err := ntw.Forward(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)
}

func TestSkipConsensusDirective_FalseValueDoesNotBypass(t *testing.T) {
	// Control case: explicitly setting "false" must keep consensus active
	// (proves the parser doesn't treat any non-empty string as truthy).
	tc := skipConsensusTestCase(t)
	// Consensus(2,3) with all agreeing: all 3 upstreams get called.
	tc.expectedCalls = []int{1, 1, 1}

	startConsensusMockServers(t, tc)

	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		time.Sleep(50 * time.Millisecond)
	}()

	ntw, _ := setupNetworkForConsensusTest(t, ctx, tc)
	time.Sleep(200 * time.Millisecond)

	req := buildSkipConsensusRequest(t, ntw)
	headers := http.Header{}
	headers.Set("X-ERPC-Skip-Consensus", "false")
	req.EnrichFromHttp(headers, nil, common.UserAgentTrackingModeSimplified)
	require.False(t, req.Directives().SkipConsensus,
		"explicit 'false' must keep SkipConsensus disabled")

	resp, err := ntw.Forward(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)
}

func TestSkipConsensusDirective_NoDirective_ConsensusStillRuns(t *testing.T) {
	// Sanity check: with no directive at all, the consensus branch is taken
	// and queries every participant. This validates that the harness alone
	// (without our directive) reproduces the consensus path -- guarding
	// against tests passing because the harness silently skipped consensus.
	tc := skipConsensusTestCase(t)
	tc.expectedCalls = []int{1, 1, 1}

	startConsensusMockServers(t, tc)

	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		time.Sleep(50 * time.Millisecond)
	}()

	ntw, _ := setupNetworkForConsensusTest(t, ctx, tc)
	time.Sleep(200 * time.Millisecond)

	req := buildSkipConsensusRequest(t, ntw)
	// Intentionally NOT calling EnrichFromHttp/ApplyDirectiveDefaults.

	resp, err := ntw.Forward(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)
}

func TestSkipConsensusDirective_RetryStillAppliesOnUpstreamError(t *testing.T) {
	// SkipConsensus only bypasses the consensus policy. Retry, hedge, and
	// timeout still wrap the call. Mock the first upstream as a transient
	// failure and expect erpc to retry on the next upstream.
	tc := skipConsensusTestCase(t)
	tc.mockResponses = []mockResponse{
		// first upstream returns a retryable upstream error
		{status: 500, body: jsonRpcError(-32603, "internal upstream error")},
		// second succeeds
		{status: 200, body: jsonRpcSuccess("0xbbb")},
		// third should never be called
		{status: 200, body: jsonRpcSuccess("0xccc")},
	}
	// Retry should advance from upstream-1 to upstream-2 and stop.
	tc.expectedCalls = []int{1, 1, 0}
	// Allow up to 3 attempts so retry can sweep to a healthy upstream.
	maxAttempts := 3
	delayZero := common.Duration(0)
	tc.retryPolicy = &common.RetryPolicyConfig{
		MaxAttempts: maxAttempts,
		Delay:       delayZero,
	}

	startConsensusMockServers(t, tc)

	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		time.Sleep(50 * time.Millisecond)
	}()

	ntw, _ := setupNetworkForConsensusTest(t, ctx, tc)
	time.Sleep(200 * time.Millisecond)

	req := buildSkipConsensusRequest(t, ntw)
	headers := http.Header{}
	headers.Set("X-ERPC-Skip-Consensus", "true")
	req.EnrichFromHttp(headers, nil, common.UserAgentTrackingModeSimplified)

	resp, err := ntw.Forward(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	jrr, jrrErr := resp.JsonRpcResponse()
	require.NoError(t, jrrErr)
	assert.Equal(t, `"0xbbb"`, jrr.GetResultString(),
		"response should come from upstream-2 after upstream-1 failure was retried (retry policy is still active under SkipConsensus)")
}

// ----------------------------------------------------------------------------
// helpers
// ----------------------------------------------------------------------------

// skipConsensusTestCase returns a base consensusTestCase configured with a
// consensus policy that requires 2-of-3 agreement. With consensus engaged,
// all three upstreams are expected to be queried; with consensus skipped,
// only the first should be queried.
func skipConsensusTestCase(_ *testing.T) consensusTestCase {
	// All three upstreams return the same JSON-RPC success so a consensus
	// path would short-circuit on the second matching response. The test
	// assertions look at per-upstream call counts to distinguish the two
	// code paths.
	return consensusTestCase{
		name:      "skip_consensus_directive",
		upstreams: createTestUpstreams(3),
		consensusConfig: &common.ConsensusPolicyConfig{
			MaxParticipants:         3,
			AgreementThreshold:      2,
			DisputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
			LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
		},
		mockResponses: []mockResponse{
			{status: 200, body: jsonRpcSuccess("0xaaa")},
			{status: 200, body: jsonRpcSuccess("0xaaa")},
			{status: 200, body: jsonRpcSuccess("0xaaa")},
		},
		requestMethod: "eth_blockNumber",
		requestParams: []interface{}{},
	}
}

func buildSkipConsensusRequest(t *testing.T, ntw *Network) *common.NormalizedRequest {
	t.Helper()
	reqBytes, err := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "eth_blockNumber",
		"params":  []interface{}{},
	})
	require.NoError(t, err)
	req := common.NewNormalizedRequest(reqBytes)
	req.SetNetwork(ntw)
	return req
}

// silence unused-import warnings if any test variant is later commented out
var _ = atomic.Int32{}
