package data

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	promUtil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// finalityNetwork is the smallest common.Network that lets a request resolve
// a fixed finality and carry a project/network identity: Finality delegates to
// network.GetFinality, the executor metric reads ProjectId/Label, and nothing
// else on the interface is touched.
type finalityNetwork struct {
	common.Network
	finality common.DataFinalityState
}

func (n *finalityNetwork) GetFinality(context.Context, *common.NormalizedRequest, *common.NormalizedResponse) common.DataFinalityState {
	return n.finality
}
func (n *finalityNetwork) ProjectId() string { return "test-project" }
func (n *finalityNetwork) Label() string     { return "evm:4663" }
func (n *finalityNetwork) Id() string        { return "evm:4663" }

func requestWithFinality(method string, f common.DataFinalityState) context.Context {
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"` + method + `","params":[],"id":1}`))
	req.SetNetwork(&finalityNetwork{finality: f})
	return context.WithValue(context.Background(), common.RequestContextKey, req)
}

func executorAttempts(connector, direction, method, finality, outcome string) float64 {
	return promUtil.ToFloat64(telemetry.MetricCacheExecutorAttempt.WithLabelValues("test-project", "evm:4663", connector, direction, method, finality, outcome))
}

func executorTransitions(connector, direction, method, finality, transition string) float64 {
	return promUtil.ToFloat64(telemetry.MetricCacheExecutorBreakerStateChange.WithLabelValues(connector, direction, method, finality, transition))
}

// A finalized read must land on the finalized|unknown executor and open ONLY
// that executor's breaker; the unfinalized|realtime executor must keep
// serving, and the metrics must attribute every attempt and the transition
// to the executor that actually handled it. This is the contract the
// per-finality cache failsafe split depends on, and the reason
// cache_executor_* metrics exist: cache_get_* carries the policy's finality,
// which cannot tell these two apart.
func TestCacheFailsafe_FinalitySplit_BreakerIsolatedPerExecutor(t *testing.T) {
	logger := zerolog.New(io.Discard)
	mc := NewMockConnector("prism-test")

	connErr := errors.New("connection refused")
	mc.On("Get", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(nil, connErr)

	fc, err := NewFailsafeConnector(context.Background(), &logger, mc, []*common.FailsafeConfig{
		{
			MatchMethod:   "*",
			MatchFinality: []common.DataFinalityState{common.DataFinalityStateFinalized, common.DataFinalityStateUnknown},
			CircuitBreaker: &common.CircuitBreakerPolicyConfig{
				FailureThresholdCount: 2,
				HalfOpenAfter:         common.Duration(1 * time.Minute),
			},
		},
		{
			MatchMethod:   "*",
			MatchFinality: []common.DataFinalityState{common.DataFinalityStateUnfinalized, common.DataFinalityStateRealtime},
			CircuitBreaker: &common.CircuitBreakerPolicyConfig{
				FailureThresholdCount: 2,
				HalfOpenAfter:         common.Duration(1 * time.Minute),
			},
		},
	}, nil)
	require.NoError(t, err)

	const cold, hot = "finalized|unknown", "realtime|unfinalized"
	coldBefore := executorAttempts("prism-test", "get", "*", cold, "breaker_open")
	hotBefore := executorAttempts("prism-test", "get", "*", hot, "breaker_open")
	coldErrBefore := executorAttempts("prism-test", "get", "*", cold, "transport_error")
	openBefore := executorTransitions("prism-test", "get", "*", cold, "closed_to_open")

	finalized := requestWithFinality("eth_getBlockByNumber", common.DataFinalityStateFinalized)
	for range 2 {
		_, _ = fc.Get(finalized, "", "pk", "rk", nil)
	}
	_, err = fc.Get(finalized, "", "pk", "rk", nil)
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeFailsafeCircuitBreakerOpen), "third finalized read must be rejected by the cold breaker")

	// The hot executor's breaker never saw a failure: an unfinalized read of
	// the same method still reaches the connector.
	unfinalized := requestWithFinality("eth_getBlockByNumber", common.DataFinalityStateUnfinalized)
	_, err = fc.Get(unfinalized, "", "pk", "rk", nil)
	require.Error(t, err)
	assert.False(t, common.HasErrorCode(err, common.ErrCodeFailsafeCircuitBreakerOpen), "unfinalized read must not be rejected by the cold executor's open breaker")
	mc.AssertNumberOfCalls(t, "Get", 3)

	assert.Equal(t, 2.0, executorAttempts("prism-test", "get", "*", cold, "transport_error")-coldErrBefore)
	assert.Equal(t, 1.0, executorAttempts("prism-test", "get", "*", cold, "breaker_open")-coldBefore)
	assert.Equal(t, 0.0, executorAttempts("prism-test", "get", "*", hot, "breaker_open")-hotBefore)
	assert.Equal(t, 1.0, executorTransitions("prism-test", "get", "*", cold, "closed_to_open")-openBefore)
}

func TestFinalityLabel_StableAcrossOrder(t *testing.T) {
	a := finalityLabel([]common.DataFinalityState{common.DataFinalityStateUnfinalized, common.DataFinalityStateRealtime})
	b := finalityLabel([]common.DataFinalityState{common.DataFinalityStateRealtime, common.DataFinalityStateUnfinalized})
	assert.Equal(t, a, b)
	assert.Equal(t, "realtime|unfinalized", a)
	assert.Equal(t, "*", finalityLabel(nil))
}
