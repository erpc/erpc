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

	require.True(t, u.IsDown("trace_block"))
	require.False(t, u.IsDown("eth_subscribe"), "a trace_* breaker must not take eth_subscribe down")

	catchAll.Breaker().Record(failsafe.OutcomeFailure)
	require.True(t, u.IsDown("eth_subscribe"))
}
