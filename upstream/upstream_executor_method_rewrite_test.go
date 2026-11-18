package upstream

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The upstream executor judges writes by the method the upstream runs: a
// client method renamed to a write eRPC never sends twice
// (eth_sendTransaction) is neither retried nor hedged on that upstream.
func TestUpstreamExecutor_RenamedWriteIsNotRetried(t *testing.T) {
	lg := zerolog.Nop()
	serverErr := common.NewErrEndpointServerSideException(errors.New("boom"), nil, 500)

	run := func(wireMethod func(string) string) int64 {
		cfg := &common.FailsafeConfig{
			MatchMethod: "*",
			Retry:       &common.RetryPolicyConfig{MaxAttempts: 3},
			Hedge:       &common.HedgePolicyConfig{Delay: common.NewStaticDuration(5 * time.Millisecond), MaxCount: 2},
		}
		require.NoError(t, cfg.SetDefaults(nil))
		ex, err := NewUpstreamExecutor(cfg, &lg)
		require.NoError(t, err)
		ex.wireMethod = wireMethod

		var calls atomic.Int64
		inner := func(context.Context, bool) (*common.NormalizedResponse, error) {
			calls.Add(1)
			time.Sleep(20 * time.Millisecond)
			return nil, serverErr
		}
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"custom_send","params":[]}`))
		_, err = ex.Run(context.Background(), req, inner)
		require.Error(t, err)
		return calls.Load()
	}

	rewrite := (&common.UpstreamConfig{MethodRewrite: map[string]string{"custom_send": "eth_sendTransaction"}}).WireMethod
	assert.Equal(t, int64(1), run(rewrite), "a renamed write is sent once")
	assert.Greater(t, run(nil), int64(1), "the same client method without the rename is retried/hedged")
}
