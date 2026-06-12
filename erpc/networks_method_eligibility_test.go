package erpc

import (
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFilterMethodEligible(t *testing.T) {
	full1 := common.NewFakeUpstream("full-1")
	full2 := common.NewFakeUpstream("full-2")
	archive1 := common.NewFakeUpstream("archive-1")
	archive1.Config().IgnoreMethods = []string{"eth_send*"}
	archive2 := common.NewFakeUpstream("archive-2")
	archive2.Config().IgnoreMethods = []string{"*"}

	t.Run("drops ineligible upstreams preserving order", func(t *testing.T) {
		ups := []common.Upstream{archive1, full1, archive2, full2}
		eligible, dropped := filterMethodEligible(ups, "eth_sendRawTransaction")
		require.Equal(t, 2, dropped)
		require.Len(t, eligible, 2)
		assert.Equal(t, "full-1", eligible[0].Id())
		assert.Equal(t, "full-2", eligible[1].Id())
	})

	t.Run("returns input slice untouched when all are eligible", func(t *testing.T) {
		ups := []common.Upstream{full1, archive1, full2}
		eligible, dropped := filterMethodEligible(ups, "eth_getLogs")
		assert.Equal(t, 0, dropped)
		// Same backing array, no copy: the selection-policy cache slice is
		// passed through as-is on the hot path.
		assert.Equal(t, &ups[0], &eligible[0])
		assert.Len(t, eligible, 3)
	})

	t.Run("all ineligible reports full drop", func(t *testing.T) {
		ups := []common.Upstream{archive1, archive2}
		eligible, dropped := filterMethodEligible(ups, "eth_sendRawTransaction")
		assert.Equal(t, 2, dropped)
		assert.Empty(t, eligible)
		// The Forward call-site keeps the original list in this case so the
		// request still fails with the descriptive per-upstream
		// "method ignored" error rather than "no upstreams found".
	})

	t.Run("does not mutate the input slice", func(t *testing.T) {
		ups := []common.Upstream{full1, archive1, full2}
		eligible, dropped := filterMethodEligible(ups, "eth_sendRawTransaction")
		require.Equal(t, 1, dropped)
		require.Len(t, eligible, 2)
		assert.Equal(t, "full-1", ups[0].Id())
		assert.Equal(t, "archive-1", ups[1].Id())
		assert.Equal(t, "full-2", ups[2].Id())
	})
}

// sumCounterFamily returns the sum of every series in a counter family from
// the default prometheus registry. Reading the family sum (rather than exact
// label values) keeps the assertion robust to label changes; callers compare
// before/after deltas around the code under test.
func sumCounterFamily(t *testing.T, name string) float64 {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	var sum float64
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			sum += m.GetCounter().GetValue()
		}
	}
	return sum
}

// TestNetworkConsensus_MethodIgnoredUpstreams_EndToEnd exercises the full
// network path (real Network, real upstream registry, per-upstream HTTP
// servers) for consensus over a mixed pool where some upstreams statically
// ignore the requested method. End goals verified:
//
//  1. method-ignored upstreams receive ZERO traffic and never become
//     participants (strict per-upstream call counts);
//  2. their instant exclusions do NOT arm maxWaitOnResult/maxWaitOnEmpty —
//     the round waits for the real participants even when they are far
//     slower than both caps (duration check + wait-capped metric delta);
//  3. surplus participant slots (maxParticipants > eligible upstreams)
//     resolve as no-attempt and do not arm the caps either;
//  4. when EVERY upstream ignores the method, the request still fails fast
//     with the descriptive per-upstream "method ignored" error and no
//     upstream traffic.
func TestNetworkConsensus_MethodIgnoredUpstreams_EndToEnd(t *testing.T) {
	t.Run("ignored upstreams excluded and wait caps not armed by exclusions", func(t *testing.T) {
		upstreams := createTestUpstreams(4)
		// Archive-style nodes: statically ignore broadcast methods.
		upstreams[0].IgnoreMethods = []string{"eth_send*"}
		upstreams[1].IgnoreMethods = []string{"eth_send*"}

		waitCappedBefore := sumCounterFamily(t, "erpc_consensus_wait_capped_total")

		runConsensusTest(t, consensusTestCase{
			name:          "ignored_upstreams_excluded_wait_caps_not_armed",
			upstreams:     upstreams,
			requestMethod: "eth_sendRawTransaction",
			requestParams: []interface{}{"0x02f870018203e8843b9aca00847735940082520894aabbccddeeff00112233445566778899aabbccdd8080c0"},
			consensusConfig: &common.ConsensusPolicyConfig{
				// More participants than eligible upstreams: the surplus
				// slots resolve instantly with no-upstreams-left and must
				// not arm the wait caps.
				MaxParticipants: 4,
				// Unreachable threshold: the round must rely on response
				// collection (and the method's first-success rule), not on
				// an agreement short-circuit, so premature wait-cap firing
				// would be visible as a failure.
				AgreementThreshold:      4,
				LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
				// Far smaller than the real upstreams' response time: if
				// the instant exclusions armed these, the round would
				// resolve before any tx hash arrives.
				MaxWaitOnResult: common.NewStaticDuration(30 * time.Millisecond),
				MaxWaitOnEmpty:  common.NewStaticDuration(30 * time.Millisecond),
			},
			mockResponses: []mockResponse{
				{}, // ignored upstream: must never be called (placeholder keeps indexes aligned)
				{}, // ignored upstream: must never be called
				{status: 200, body: jsonRpcSuccess("0x1111111111111111111111111111111111111111111111111111111111111111"), delay: 150 * time.Millisecond},
				{status: 200, body: jsonRpcSuccess("0x1111111111111111111111111111111111111111111111111111111111111111"), delay: 150 * time.Millisecond},
			},
			expectedCalls: []int{0, 0, 1, 1},
			expectedResult: &expectedResult{
				contains: "0x1111111111111111111111111111111111111111111111111111111111111111",
				check: func(t *testing.T, resp *common.NormalizedResponse, duration time.Duration) {
					assert.GreaterOrEqual(t, duration, 140*time.Millisecond,
						"round must wait for the real (slow) participants; instant exclusions must not start the wait-cap countdown")
				},
			},
		})

		assert.Equal(t, waitCappedBefore, sumCounterFamily(t, "erpc_consensus_wait_capped_total"),
			"wait caps must not fire when the only instant responses are method-ignored exclusions")
	})

	t.Run("all upstreams ignore the method: descriptive error, zero traffic", func(t *testing.T) {
		upstreams := createTestUpstreams(3)
		for _, u := range upstreams {
			u.IgnoreMethods = []string{"eth_send*"}
		}

		runConsensusTest(t, consensusTestCase{
			name:          "all_upstreams_ignore_method",
			upstreams:     upstreams,
			requestMethod: "eth_sendRawTransaction",
			requestParams: []interface{}{"0x02f870018203e8843b9aca00847735940082520894aabbccddeeff00112233445566778899aabbccdd8080c0"},
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:    3,
				AgreementThreshold: 3,
				MaxWaitOnResult:    common.NewStaticDuration(30 * time.Millisecond),
				MaxWaitOnEmpty:     common.NewStaticDuration(30 * time.Millisecond),
			},
			mockResponses: []mockResponse{},
			expectedCalls: []int{0, 0, 0},
			expectedError: &expectedError{
				code: common.ErrCodeUpstreamMethodIgnored,
			},
		})
	})
}
