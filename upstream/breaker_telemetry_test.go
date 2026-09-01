package upstream

import (
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/failsafe"
	"github.com/erpc/erpc/telemetry"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestBreakerUpstream builds a bare Upstream carrying the given
// failsafe entries, with breaker telemetry wired the same way
// NewUpstream wires it. No network/client bootstrap is needed — the
// reporters only read identity off the upstream.
func newTestBreakerUpstream(t *testing.T, projectId, upstreamId, network string, fsCfgs ...*common.UpstreamFailsafeConfig) *Upstream {
	t.Helper()
	lg := zerolog.Nop()

	u := &Upstream{
		ProjectId: projectId,
		logger:    &lg,
		config: &common.UpstreamConfig{
			Id:         upstreamId,
			VendorName: "testvendor",
		},
	}
	for _, fsCfg := range fsCfgs {
		ex, err := NewUpstreamExecutor(fsCfg, &lg)
		require.NoError(t, err)
		u.failsafeExecutors = append(u.failsafeExecutors, ex)
	}
	u.networkLabel.Store(network)
	u.networkId.Store(network)
	u.wireBreakerTelemetry()
	return u
}

func cbTestConfig() *common.CircuitBreakerPolicyConfig {
	return &common.CircuitBreakerPolicyConfig{
		FailureThresholdCount:    2,
		FailureThresholdCapacity: 2,
		SuccessThresholdCount:    1,
		SuccessThresholdCapacity: 1,
		HalfOpenAfter:            common.Duration(50 * time.Millisecond),
	}
}

func cbGauge(t *testing.T, projectId, upstreamId, network, category, finality string) float64 {
	t.Helper()
	return testutil.ToFloat64(telemetry.MetricUpstreamCircuitBreakerState.WithLabelValues(
		projectId, "testvendor", network, upstreamId, category, finality,
	))
}

// requireGaugeEventually waits for the gauge to settle: OnTransition
// callbacks are fired on their own goroutine by the breaker.
func requireGaugeEventually(t *testing.T, want float64, projectId, upstreamId, network, category, finality string) {
	t.Helper()
	require.Eventually(t, func() bool {
		return cbGauge(t, projectId, upstreamId, network, category, finality) == want
	}, 2*time.Second, 5*time.Millisecond,
		"gauge never reached %v (last=%v)", want, cbGauge(t, projectId, upstreamId, network, category, finality))
}

// TestCircuitBreakerStateValue pins the exported numeric encoding: it is
// a public contract, independent of the failsafe.State iota order.
func TestCircuitBreakerStateValue(t *testing.T) {
	assert.Equal(t, float64(0), circuitBreakerStateValue(failsafe.StateClosed))
	assert.Equal(t, float64(1), circuitBreakerStateValue(failsafe.StateOpen))
	assert.Equal(t, float64(2), circuitBreakerStateValue(failsafe.StateHalfOpen))
	assert.Equal(t, float64(-1), circuitBreakerStateValue(failsafe.State(99)))
}

func TestBreakerScopeLabels(t *testing.T) {
	lg := zerolog.Nop()

	t.Run("CatchAllExecutor", func(t *testing.T) {
		ex, err := NewUpstreamExecutor(nil, &lg)
		require.NoError(t, err)
		category, finality := breakerScopeLabels(ex)
		assert.Equal(t, "*", category)
		assert.Equal(t, "*", finality)
	})

	t.Run("MethodOnly", func(t *testing.T) {
		ex, err := NewUpstreamExecutor(&common.UpstreamFailsafeConfig{MatchMethod: "eth_call"}, &lg)
		require.NoError(t, err)
		category, finality := breakerScopeLabels(ex)
		assert.Equal(t, "eth_call", category)
		assert.Equal(t, "*", finality)
	})

	t.Run("MethodAndFinality", func(t *testing.T) {
		ex, err := NewUpstreamExecutor(&common.UpstreamFailsafeConfig{
			MatchMethod:   "eth_getLogs",
			MatchFinality: []common.DataFinalityState{common.DataFinalityStateFinalized, common.DataFinalityStateRealtime},
		}, &lg)
		require.NoError(t, err)
		category, finality := breakerScopeLabels(ex)
		assert.Equal(t, "eth_getLogs", category)
		assert.Equal(t, "finalized|realtime", finality)
	})
}

// TestBreakerStateGaugeFullCycle drives a real breaker through every
// transition of the state machine and asserts the gauge follows:
// closed -> open -> half-open -> closed.
func TestBreakerStateGaugeFullCycle(t *testing.T) {
	const (
		project  = "prj_cycle"
		upstream = "ups_cycle"
		network  = "evm:123"
	)
	u := newTestBreakerUpstream(t, project, upstream, network, &common.UpstreamFailsafeConfig{
		CircuitBreaker: cbTestConfig(),
	})
	b := u.failsafeExecutors[0].Breaker()
	require.NotNil(t, b)

	// Initial state is only published once the network is known, which is
	// what SetNetworkConfig does in production.
	u.publishBreakerStates()
	require.Equal(t, float64(0), cbGauge(t, project, upstream, network, "*", "*"))
	require.Equal(t, failsafe.StateClosed, b.State())

	// closed -> open (failure threshold reached).
	require.True(t, b.TryAcquirePermit())
	b.Record(failsafe.OutcomeFailure)
	require.True(t, b.TryAcquirePermit())
	b.Record(failsafe.OutcomeFailure)
	require.Equal(t, failsafe.StateOpen, b.State())
	requireGaugeEventually(t, 1, project, upstream, network, "*", "*")

	// Open refuses permits until halfOpenAfter elapses.
	require.False(t, b.TryAcquirePermit())

	// open -> half-open (delay elapsed, permit granted).
	require.Eventually(t, b.TryAcquirePermit, 2*time.Second, 5*time.Millisecond)
	require.Equal(t, failsafe.StateHalfOpen, b.State())
	requireGaugeEventually(t, 2, project, upstream, network, "*", "*")

	// half-open -> closed (success threshold reached).
	b.Record(failsafe.OutcomeSuccess)
	require.Equal(t, failsafe.StateClosed, b.State())
	requireGaugeEventually(t, 0, project, upstream, network, "*", "*")
}

// TestBreakerStateGaugeHalfOpenReopen covers the remaining transition:
// a failed trial in half-open puts the breaker back to open.
func TestBreakerStateGaugeHalfOpenReopen(t *testing.T) {
	const (
		project  = "prj_reopen"
		upstream = "ups_reopen"
		network  = "evm:123"
	)
	u := newTestBreakerUpstream(t, project, upstream, network, &common.UpstreamFailsafeConfig{
		CircuitBreaker: cbTestConfig(),
	})
	b := u.failsafeExecutors[0].Breaker()

	require.True(t, b.TryAcquirePermit())
	b.Record(failsafe.OutcomeFailure)
	require.True(t, b.TryAcquirePermit())
	b.Record(failsafe.OutcomeFailure)
	requireGaugeEventually(t, 1, project, upstream, network, "*", "*")

	require.Eventually(t, b.TryAcquirePermit, 2*time.Second, 5*time.Millisecond)
	requireGaugeEventually(t, 2, project, upstream, network, "*", "*")

	b.Record(failsafe.OutcomeFailure)
	require.Equal(t, failsafe.StateOpen, b.State())
	requireGaugeEventually(t, 1, project, upstream, network, "*", "*")
}

// TestBreakerStateGaugeKeepsTransitionCounter proves the pre-existing
// transition counter still fires alongside the new gauge, and that
// seeding the gauge does NOT fabricate a transition.
func TestBreakerStateGaugeKeepsTransitionCounter(t *testing.T) {
	const (
		project  = "prj_counter"
		upstream = "ups_counter"
		network  = "evm:123"
	)
	u := newTestBreakerUpstream(t, project, upstream, network, &common.UpstreamFailsafeConfig{
		CircuitBreaker: cbTestConfig(),
	})
	b := u.failsafeExecutors[0].Breaker()

	counter := telemetry.MetricUpstreamBreakerStateChange.WithLabelValues(project, upstream, "closed_to_open")
	before := testutil.ToFloat64(counter)

	// Seeding the initial state must not touch the transition counter.
	u.publishBreakerStates()
	u.publishBreakerStates()
	require.Equal(t, before, testutil.ToFloat64(counter))
	require.Equal(t, float64(0), cbGauge(t, project, upstream, network, "*", "*"))

	require.True(t, b.TryAcquirePermit())
	b.Record(failsafe.OutcomeFailure)
	require.True(t, b.TryAcquirePermit())
	b.Record(failsafe.OutcomeFailure)

	requireGaugeEventually(t, 1, project, upstream, network, "*", "*")
	require.Eventually(t, func() bool {
		return testutil.ToFloat64(counter) == before+1
	}, 2*time.Second, 5*time.Millisecond)
}

// TestBreakerStateGaugePerMethodIndependence proves that two failsafe
// entries on the SAME upstream keep independent series — the reason the
// gauge carries the entry's match scope in its labels.
func TestBreakerStateGaugePerMethodIndependence(t *testing.T) {
	const (
		project  = "prj_permethod"
		upstream = "ups_permethod"
		network  = "evm:123"
	)
	u := newTestBreakerUpstream(t, project, upstream, network,
		&common.UpstreamFailsafeConfig{MatchMethod: "eth_call", CircuitBreaker: cbTestConfig()},
		&common.UpstreamFailsafeConfig{MatchMethod: "eth_getLogs", CircuitBreaker: cbTestConfig()},
	)
	u.publishBreakerStates()

	require.Equal(t, float64(0), cbGauge(t, project, upstream, network, "eth_call", "*"))
	require.Equal(t, float64(0), cbGauge(t, project, upstream, network, "eth_getLogs", "*"))

	// Only the eth_call breaker trips.
	b := u.failsafeExecutors[0].Breaker()
	require.True(t, b.TryAcquirePermit())
	b.Record(failsafe.OutcomeFailure)
	require.True(t, b.TryAcquirePermit())
	b.Record(failsafe.OutcomeFailure)

	requireGaugeEventually(t, 1, project, upstream, network, "eth_call", "*")
	assert.Equal(t, float64(0), cbGauge(t, project, upstream, network, "eth_getLogs", "*"),
		"eth_getLogs breaker must not be affected by the eth_call breaker")
}

// TestBreakerStateGaugeUpstreamIndependence proves two upstreams do not
// overwrite each other's state.
func TestBreakerStateGaugeUpstreamIndependence(t *testing.T) {
	const (
		project = "prj_multiups"
		network = "evm:123"
	)
	upA := newTestBreakerUpstream(t, project, "ups_a", network, &common.UpstreamFailsafeConfig{
		CircuitBreaker: cbTestConfig(),
	})
	upB := newTestBreakerUpstream(t, project, "ups_b", network, &common.UpstreamFailsafeConfig{
		CircuitBreaker: cbTestConfig(),
	})
	upA.publishBreakerStates()
	upB.publishBreakerStates()

	b := upA.failsafeExecutors[0].Breaker()
	require.True(t, b.TryAcquirePermit())
	b.Record(failsafe.OutcomeFailure)
	require.True(t, b.TryAcquirePermit())
	b.Record(failsafe.OutcomeFailure)

	requireGaugeEventually(t, 1, project, "ups_a", network, "*", "*")
	assert.Equal(t, float64(0), cbGauge(t, project, "ups_b", network, "*", "*"))
}

// TestBreakerStateGaugeSeededOnNetworkConfig proves the operator-facing
// promise: the series exists with the real network label as soon as the
// upstream is attached to its network, before any transition happens.
func TestBreakerStateGaugeSeededOnNetworkConfig(t *testing.T) {
	const (
		project  = "prj_seed"
		upstream = "ups_seed"
	)
	u := newTestBreakerUpstream(t, project, upstream, "n/a", &common.UpstreamFailsafeConfig{
		CircuitBreaker: cbTestConfig(),
	})

	u.SetNetworkConfig(&common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 42161},
	})

	assert.Equal(t, "evm:42161", u.NetworkLabel())
	assert.Equal(t, float64(0), cbGauge(t, project, upstream, "evm:42161", "*", "*"))
}

// TestBreakerTelemetryNoBreakerConfigured guards the common case: an
// upstream with no circuit-breaker policy registers no reporter and
// therefore emits no series at all.
func TestBreakerTelemetryNoBreakerConfigured(t *testing.T) {
	u := newTestBreakerUpstream(t, "prj_nobreaker", "ups_nobreaker", "evm:123",
		&common.UpstreamFailsafeConfig{MatchMethod: "eth_call"},
	)
	assert.Empty(t, u.breakerReporters)
	u.publishBreakerStates() // must not panic
}
