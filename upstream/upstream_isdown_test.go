package upstream

import (
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/failsafe"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// An open breaker only takes the upstream down for the methods its policy
// serves.
func TestUpstream_IsDown_ScopedToTheMethodsPolicy(t *testing.T) {
	lg := zerolog.Nop()
	breaker := func(method string) *upstreamExecutor {
		ex, err := NewUpstreamExecutor(&common.FailsafeConfig{
			MatchMethod: method,
			CircuitBreaker: &common.CircuitBreakerPolicyConfig{
				FailureThresholdCount:    1,
				FailureThresholdCapacity: 1,
				HalfOpenAfter:            common.Duration(time.Hour),
				SuccessThresholdCount:    1,
				SuccessThresholdCapacity: 1,
			},
		}, &lg)
		require.NoError(t, err)
		return ex
	}
	traces, catchAll := breaker("trace_*"), breaker("*")
	u := &Upstream{failsafeExecutors: []*upstreamExecutor{traces, catchAll}}

	traces.Breaker().Record(failsafe.OutcomeFailure)
	require.Equal(t, failsafe.StateOpen, traces.Breaker().State())

	require.True(t, u.IsDown("trace_block", common.DataFinalityStateUnknown))
	require.False(t, u.IsDown("eth_subscribe", common.DataFinalityStateUnknown), "a trace_* breaker must not take eth_subscribe down")

	catchAll.Breaker().Record(failsafe.OutcomeFailure)
	require.True(t, u.IsDown("eth_subscribe", common.DataFinalityStateUnknown))
}

// An open breaker scoped to finalized must take finalized traffic down
// without affecting realtime. Unknown must not pick the finalized-scoped
// breaker when a catch-all also exists (the old synthetic-request path).
func TestUpstream_IsDown_RespectsFinality(t *testing.T) {
	lg := zerolog.Nop()
	mk := func(method string, finalities []common.DataFinalityState) *upstreamExecutor {
		ex, err := NewUpstreamExecutor(&common.FailsafeConfig{
			MatchMethod:   method,
			MatchFinality: finalities,
			CircuitBreaker: &common.CircuitBreakerPolicyConfig{
				FailureThresholdCount:    1,
				FailureThresholdCapacity: 1,
				HalfOpenAfter:            common.Duration(time.Hour),
				SuccessThresholdCount:    1,
				SuccessThresholdCapacity: 1,
			},
		}, &lg)
		require.NoError(t, err)
		return ex
	}
	finalized := mk("eth_call", []common.DataFinalityState{common.DataFinalityStateFinalized})
	catchAll := mk("*", nil)
	u := &Upstream{failsafeExecutors: []*upstreamExecutor{finalized, catchAll}}

	finalized.Breaker().Record(failsafe.OutcomeFailure)
	require.Equal(t, failsafe.StateOpen, finalized.Breaker().State())

	require.True(t, u.IsDown("eth_call", common.DataFinalityStateFinalized),
		"finalized eth_call must see the open finalized breaker")
	require.False(t, u.IsDown("eth_call", common.DataFinalityStateRealtime),
		"realtime eth_call falls through to the closed catch-all")
	require.False(t, u.IsDown("eth_call", common.DataFinalityStateUnknown),
		"unknown finality must not pick the finalized-scoped breaker")
}
