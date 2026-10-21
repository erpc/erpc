package upstream

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/failsafe"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// Subscription calls (the wsupstream adapter's subscription RPCs) must not
// feed the upstream breaker; the same failure from a client request opens it.
func TestUpstreamExecutor_InternalRequestsSkipBreaker(t *testing.T) {
	lg := zerolog.Nop()
	serverErr := common.NewErrEndpointServerSideException(errors.New("boom"), nil, 500)
	inner := func(context.Context, bool) (*common.NormalizedResponse, error) { return nil, serverErr }

	run := func(internal bool) failsafe.State {
		ex, err := NewUpstreamExecutor(&common.FailsafeConfig{
			CircuitBreaker: &common.CircuitBreakerPolicyConfig{
				FailureThresholdCount:    1,
				FailureThresholdCapacity: 1,
				HalfOpenAfter:            common.Duration(time.Hour),
				SuccessThresholdCount:    1,
				SuccessThresholdCapacity: 1,
			},
		}, &lg)
		require.NoError(t, err)
		body := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["newHeads"]}`)
		req := common.NewNormalizedRequest(body)
		if internal {
			req = common.NewProbeRequest(body)
		}
		_, err = ex.Run(context.Background(), req, inner)
		require.Error(t, err)
		return ex.Breaker().State()
	}

	require.Equal(t, failsafe.StateClosed, run(true))
	require.Equal(t, failsafe.StateOpen, run(false))
}
