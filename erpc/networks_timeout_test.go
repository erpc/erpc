package erpc

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() { util.ConfigureTestLogger() }

func TestNetwork_TimeoutPolicy(t *testing.T) {
	t.Run("FixedTimeout_BackwardCompatible", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`)

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getBalance")
			}).
			Reply(200).
			Delay(50 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x1111",
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Fixed timeout with no quantile — backward compatible behavior
		network := setupTestNetworkWithTimeoutPolicy(t, ctx, &common.TimeoutPolicyConfig{
			Duration: common.Duration(5 * time.Second),
		})

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)

		require.NoError(t, err)
		require.NotNil(t, resp)

		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Contains(t, jrr.GetResultString(), "0x1111")
	})

	t.Run("FixedTimeout_RequestExceedsTimeout", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`)

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getBalance")
			}).
			Reply(200).
			Delay(2 * time.Second).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x1111",
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Short timeout that the request should exceed
		network := setupTestNetworkWithTimeoutPolicy(t, ctx, &common.TimeoutPolicyConfig{
			Duration: common.Duration(200 * time.Millisecond),
		})

		req := common.NewNormalizedRequest(requestBytes)
		_, err := network.Forward(ctx, req)

		require.Error(t, err)
		assert.True(t,
			common.HasErrorCode(err, common.ErrCodeFailsafeTimeoutExceeded),
			"expected ErrFailsafeTimeoutExceeded, got: %v", err,
		)
	})

	t.Run("ParentContextDeadline_NotMisclassifiedAsTimeoutPolicy", func(t *testing.T) {
		// When the caller's context deadline fires (e.g. HTTP server timeout),
		// the error must NOT be wrapped as ErrFailsafeTimeoutExceeded — it must
		// propagate as a plain context.DeadlineExceeded so the HTTP layer can
		// surface it correctly. This locks in the sentinel-cause design.
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`)

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getBalance")
			}).
			Reply(200).
			Delay(2 * time.Second).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x1111",
			})

		// Policy timeout is generous — setup uses background so chain-id detection
		// completes; the Forward uses a short caller-deadline that should fire first.
		setupCtx, cancelSetup := context.WithCancel(context.Background())
		defer cancelSetup()
		network := setupTestNetworkWithTimeoutPolicy(t, setupCtx, &common.TimeoutPolicyConfig{
			Duration: common.Duration(10 * time.Second),
		})

		parentCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()

		req := common.NewNormalizedRequest(requestBytes)
		_, err := network.Forward(parentCtx, req)

		require.Error(t, err)
		assert.False(t,
			common.HasErrorCode(err, common.ErrCodeFailsafeTimeoutExceeded),
			"parent context deadline must not be misclassified as failsafe timeout policy; got: %v", err,
		)
	})

	t.Run("QuantileTimeout_DynamicComputation", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		// Set up mocks for metric building phase (10 requests with varying latencies)
		for i := 0; i < 10; i++ {
			gock.New("http://rpc1.localhost").
				Post("").
				Filter(func(r *http.Request) bool {
					body := util.SafeReadBody(r)
					return strings.Contains(body, "eth_getBalance")
				}).
				Times(1).
				Reply(200).
				Delay(time.Duration(20+i*5) * time.Millisecond). // 20-65ms
				JSON(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x1111",
				})
		}

		// Then a fast request that should succeed within the dynamic timeout
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getBalance")
			}).
			Reply(200).
			Delay(20 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x2222",
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Quantile-based timeout: p90 of latencies (~60ms), clamped to [200ms, 5s]
		// minDuration ensures timeout is at least 200ms even though p90 is ~60ms
		network := setupTestNetworkWithTimeoutPolicy(t, ctx, &common.TimeoutPolicyConfig{
			Duration:    common.Duration(1 * time.Second), // fallback
			Quantile:    0.9,
			MinDuration: common.Duration(200 * time.Millisecond),
			MaxDuration: common.Duration(5 * time.Second),
		})

		// Build up metrics
		for i := 0; i < 10; i++ {
			req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`))
			resp, err := network.Forward(ctx, req)
			require.NoError(t, err)
			resp.Release()
		}

		// Now test with built-up metrics — should succeed since 20ms < dynamic timeout (min 200ms)
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`))
		resp, err := network.Forward(ctx, req)
		require.NoError(t, err)
		require.NotNil(t, resp)

		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Contains(t, jrr.GetResultString(), "0x2222")
	})

	t.Run("QuantileTimeout_MinDurationBoundary", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		// Build metrics with very fast responses
		for i := 0; i < 5; i++ {
			gock.New("http://rpc1.localhost").
				Post("").
				Filter(func(r *http.Request) bool {
					body := util.SafeReadBody(r)
					return strings.Contains(body, "eth_getBalance")
				}).
				Reply(200).
				Delay(5 * time.Millisecond). // Very fast
				JSON(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x1111",
				})
		}

		// Request that takes longer than the raw quantile but less than minDuration
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getBalance")
			}).
			Reply(200).
			Delay(80 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x2222",
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// p10 of very fast responses would be ~5ms, but minDuration is 200ms
		network := setupTestNetworkWithTimeoutPolicy(t, ctx, &common.TimeoutPolicyConfig{
			Quantile:    0.1,
			MinDuration: common.Duration(200 * time.Millisecond),
			MaxDuration: common.Duration(5 * time.Second),
		})

		// Build metrics
		for i := 0; i < 5; i++ {
			req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`))
			resp, err := network.Forward(ctx, req)
			require.NoError(t, err)
			resp.Release()
		}

		// Should succeed because minDuration (200ms) > actual latency (80ms)
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`))
		resp, err := network.Forward(ctx, req)
		require.NoError(t, err)
		require.NotNil(t, resp)

		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Contains(t, jrr.GetResultString(), "0x2222")
	})

	t.Run("QuantileTimeout_ColdStartFallback", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`)

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getBalance")
			}).
			Reply(200).
			Delay(50 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x1111",
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Quantile-based timeout with Duration as cold start fallback
		network := setupTestNetworkWithTimeoutPolicy(t, ctx, &common.TimeoutPolicyConfig{
			Duration:    common.Duration(5 * time.Second), // fallback during cold start
			Quantile:    0.9,
			MinDuration: common.Duration(100 * time.Millisecond),
			MaxDuration: common.Duration(10 * time.Second),
		})

		// First request — no metrics yet, should use Duration fallback (5s)
		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)
		require.NoError(t, err)
		require.NotNil(t, resp)

		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Contains(t, jrr.GetResultString(), "0x1111")
	})

	t.Run("QuantileTimeout_ColdStartFallbackToMaxDuration", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`)

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getBalance")
			}).
			Reply(200).
			Delay(50 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x1111",
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// No Duration set — should fall back to MaxDuration during cold start
		network := setupTestNetworkWithTimeoutPolicy(t, ctx, &common.TimeoutPolicyConfig{
			Quantile:    0.9,
			MinDuration: common.Duration(100 * time.Millisecond),
			MaxDuration: common.Duration(10 * time.Second),
		})

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)
		require.NoError(t, err)
		require.NotNil(t, resp)

		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Contains(t, jrr.GetResultString(), "0x1111")
	})

	t.Run("NetworkTimeoutIsLifecycleScoped_BoundsRetryBudget", func(t *testing.T) {
		// Regression lock: a 500ms network timeout with 3 retries must bound
		// total wall-clock to ~500ms, not 500ms × 3 attempts. Applying the
		// timeout inside the per-attempt callback would silently blow this
		// budget; this test fails unless the timeout wraps the entire executor.
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		// Each upstream attempt delays 2s. With per-attempt timeout semantics,
		// 3 retries would take up to 6s. With lifecycle semantics, the 500ms
		// network timeout fires once and stops everything.
		for i := 0; i < 5; i++ {
			gock.New("http://rpc1.localhost").
				Post("").
				Filter(func(r *http.Request) bool {
					body := util.SafeReadBody(r)
					return strings.Contains(body, "eth_getBalance")
				}).
				Reply(200).
				Delay(2 * time.Second).
				JSON(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x1111",
				})
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupTestNetworkWithTimeoutAndRetry(t, ctx,
			&common.TimeoutPolicyConfig{Duration: common.Duration(500 * time.Millisecond)},
			&common.RetryPolicyConfig{MaxAttempts: 3, Delay: common.Duration(10 * time.Millisecond)},
		)

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`))
		start := time.Now()
		_, err := network.Forward(ctx, req)
		elapsed := time.Since(start)

		require.Error(t, err)
		assert.True(t,
			common.HasErrorCode(err, common.ErrCodeFailsafeTimeoutExceeded),
			"expected lifecycle timeout to wrap as ErrFailsafeTimeoutExceeded, got: %v", err,
		)
		assert.Less(t, elapsed, 1500*time.Millisecond,
			"lifecycle timeout should bound total wall-clock near 500ms, not 500ms*retries; got %s", elapsed,
		)
	})

	t.Run("RetryExhaustedWithTimeoutOnLastAttempt_NotMisclassifiedAsTimeout", func(t *testing.T) {
		// Regression lock for pc-001: if retry policy exhausts and the last
		// attempt hit a timeout, the classifier must NOT surface it as
		// ErrFailsafeTimeoutExceeded — the retry-exhausted branch owns the
		// final classification. Without the isRetryExceeded guard in
		// TranslateFailsafeError, errors.Is would walk through the Unwrap
		// chain of retrypolicy.ExceededError and incorrectly match the
		// dynamic-timeout sentinel branch first.
		//
		// We put retry+timeout both at the upstream level so the failsafe
		// exhaustion surfaces as a raw retrypolicy.ExceededError without the
		// ErrUpstreamsExhausted short-circuit that kicks in at network scope.
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		// 3 attempts × always-slow mock → retry exhausts with timeout on last.
		for i := 0; i < 6; i++ {
			gock.New("http://rpc1.localhost").
				Post("").
				Filter(func(r *http.Request) bool {
					body := util.SafeReadBody(r)
					return strings.Contains(body, "eth_getBalance")
				}).
				Reply(200).
				Delay(2 * time.Second).
				JSON(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x1111",
				})
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupTestNetworkWithUpstreamTimeoutAndRetry(t, ctx,
			&common.TimeoutPolicyConfig{Duration: common.Duration(50 * time.Millisecond)},
			&common.RetryPolicyConfig{MaxAttempts: 3, Delay: common.Duration(10 * time.Millisecond)},
		)

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`))
		_, err := network.Forward(ctx, req)

		require.Error(t, err)
		// The key invariant: retry-exhausted must NOT be misclassified as
		// FailsafeTimeoutExceeded at the top level, even though the last
		// attempt's underlying cause is a timeout.
		assert.False(t,
			topLevelErrorCode(err) == string(common.ErrCodeFailsafeTimeoutExceeded),
			"retry exhaustion with timeout-on-last-attempt must not surface as FailsafeTimeoutExceeded; got top-level code %s in chain: %v",
			topLevelErrorCode(err), err,
		)
	})

	t.Run("UpstreamTimeout_DoesNotDoubleCountNetworkFiredCounter", func(t *testing.T) {
		// Regression lock: when an upstream-scope timeout bubbles up as
		// ErrFailsafeTimeoutExceeded, the network-scope counter check must
		// see HasErrorCode(ErrCodeFailsafeTimeoutExceeded) and skip the
		// increment. Otherwise every upstream timeout produces two fires
		// (scope=upstream at upstream.go + scope=network at networks.go).
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getBalance")
			}).
			Reply(200).
			Delay(2 * time.Second).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x1111",
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupTestNetworkWithUpstreamTimeoutPolicy(t, ctx, &common.TimeoutPolicyConfig{
			Duration: common.Duration(100 * time.Millisecond),
		})

		upstreamBefore := timeoutFiredCounterValue(t, "upstream")
		networkBefore := timeoutFiredCounterValue(t, "network")
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`))
		_, err := network.Forward(ctx, req)
		require.Error(t, err)

		// Split assertions: each invariant checked independently so a regression
		// in one direction can't pass by compensating in the other.
		assert.Equal(t, upstreamBefore+1, timeoutFiredCounterValue(t, "upstream"),
			"upstream-scope counter must fire exactly once for an upstream-configured timeout")
		assert.Equal(t, networkBefore, timeoutFiredCounterValue(t, "network"),
			"network-scope counter must not increment when the timeout was already classified at upstream scope")
	})

	t.Run("NetworkOnlyTimeout_FiresAtNetworkScopeNotUpstream", func(t *testing.T) {
		// Regression lock: when only the network scope has a timeout policy
		// and the upstream has none, the lifecycle sentinel propagates via
		// ctx inheritance into the upstream's error chain. Both the counter
		// and error classification at upstream scope must be guarded on
		// `failsafeExecutor.timeout != nil` so the scope attribution stays
		// truthful — the timeout was the NETWORK's policy firing, not the
		// upstream's.
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getBalance")
			}).
			Reply(200).
			Delay(2 * time.Second).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x1111",
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Timeout at NETWORK only; upstream has no timeout.
		network := setupTestNetworkWithTimeoutPolicy(t, ctx, &common.TimeoutPolicyConfig{
			Duration: common.Duration(100 * time.Millisecond),
		})

		upstreamBefore := timeoutFiredCounterValue(t, "upstream")
		networkBefore := timeoutFiredCounterValue(t, "network")
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`))
		_, err := network.Forward(ctx, req)
		require.Error(t, err)

		assert.Equal(t, upstreamBefore, timeoutFiredCounterValue(t, "upstream"),
			"upstream counter must not fire when upstream has no timeout policy (sentinel is inherited from network scope)")
		assert.Equal(t, networkBefore+1, timeoutFiredCounterValue(t, "network"),
			"network-scope counter must fire exactly once for a network-configured timeout")
		assert.True(t, common.HasErrorCode(err, common.ErrCodeFailsafeTimeoutExceeded),
			"error should classify as ErrFailsafeTimeoutExceeded, got: %v", err)
	})
}

func TestUpstream_TimeoutPolicy(t *testing.T) {
	t.Run("UpstreamLevelFixedTimeout_ExceededWrapsAsFailsafeTimeout", func(t *testing.T) {
		// Verifies the unified context.WithTimeoutCause path also fires at the
		// upstream scope (not just network scope).
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`)

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getBalance")
			}).
			Reply(200).
			Delay(2 * time.Second).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x1111",
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupTestNetworkWithUpstreamTimeoutPolicy(t, ctx, &common.TimeoutPolicyConfig{
			Duration: common.Duration(150 * time.Millisecond),
		})

		req := common.NewNormalizedRequest(requestBytes)
		_, err := network.Forward(ctx, req)

		require.Error(t, err)
		assert.True(t,
			common.HasErrorCode(err, common.ErrCodeFailsafeTimeoutExceeded),
			"expected upstream-level timeout to wrap as ErrFailsafeTimeoutExceeded, got: %v", err,
		)
	})

	t.Run("UpstreamLevelQuantileTimeout_HistogramEmits", func(t *testing.T) {
		// Verifies that NewTimeoutFunc emits erpc_network_timeout_duration_seconds
		// when used at the upstream scope (not only network scope).
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		for i := 0; i < 10; i++ {
			gock.New("http://rpc1.localhost").
				Post("").
				Filter(func(r *http.Request) bool {
					body := util.SafeReadBody(r)
					return strings.Contains(body, "eth_getBalance")
				}).
				Times(1).
				Reply(200).
				Delay(time.Duration(30+i*5) * time.Millisecond).
				JSON(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x1111",
				})
		}
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getBalance")
			}).
			Reply(200).
			Delay(20 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x2222",
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupTestNetworkWithUpstreamTimeoutPolicy(t, ctx, &common.TimeoutPolicyConfig{
			Duration:    common.Duration(1 * time.Second),
			Quantile:    0.9,
			MinDuration: common.Duration(200 * time.Millisecond),
			MaxDuration: common.Duration(5 * time.Second),
		})

		for i := 0; i < 10; i++ {
			req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`))
			resp, err := network.Forward(ctx, req)
			require.NoError(t, err)
			resp.Release()
		}

		before := testutilCounterValue(t)
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`))
		resp, err := network.Forward(ctx, req)
		require.NoError(t, err)
		require.NotNil(t, resp)
		after := testutilCounterValue(t)

		assert.Greater(t, after, before,
			"histogram should have observed at least one sample after the 11th upstream-scoped dynamic-timeout request")
	})
}

// testutilCounterValue returns the total number of observations across all
// label combinations of the MetricNetworkTimeoutDurationSeconds histogram.
// Used to verify the histogram emits regardless of label values.
func testutilCounterValue(t *testing.T) uint64 {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	var total uint64
	for _, mf := range mfs {
		if mf.GetName() != "erpc_network_timeout_duration_seconds" {
			continue
		}
		for _, m := range mf.GetMetric() {
			if h := m.GetHistogram(); h != nil {
				total += h.GetSampleCount()
			}
		}
	}
	return total
}

// Helper to set up network with UPSTREAM-level timeout policy
func setupTestNetworkWithUpstreamTimeoutPolicy(t *testing.T, ctx context.Context, timeoutConfig *common.TimeoutPolicyConfig) *Network {
	t.Helper()

	upstreamConfigs := []*common.UpstreamConfig{
		{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc1",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
			Failsafe: []*common.FailsafeConfig{{
				Timeout: timeoutConfig,
			}},
		},
	}

	networkConfig := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm: &common.EvmNetworkConfig{
			ChainId: 123,
		},
	}

	return setupTestNetwork(t, ctx, upstreamConfigs, networkConfig)
}

// timeoutFiredCounterValue returns the total MetricNetworkTimeoutFiredTotal
// count for the given scope label across all other label combinations.
func timeoutFiredCounterValue(t *testing.T, scope string) uint64 {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	var total uint64
	for _, mf := range mfs {
		if mf.GetName() != "erpc_network_timeout_fired_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			var mScope string
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "scope" {
					mScope = lp.GetValue()
				}
			}
			if mScope != scope {
				continue
			}
			if c := m.GetCounter(); c != nil {
				total += uint64(c.GetValue())
			}
		}
	}
	return total
}

// setupTestNetworkWithTimeoutAndRetry attaches both a timeout and retry policy
// at the network level — used to verify lifecycle semantics.
func setupTestNetworkWithTimeoutAndRetry(t *testing.T, ctx context.Context, timeoutConfig *common.TimeoutPolicyConfig, retryConfig *common.RetryPolicyConfig) *Network {
	t.Helper()
	upstreamConfigs := []*common.UpstreamConfig{{
		Type:     common.UpstreamTypeEvm,
		Id:       "rpc1",
		Endpoint: "http://rpc1.localhost",
		Evm:      &common.EvmUpstreamConfig{ChainId: 123},
	}}
	networkConfig := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
		Failsafe: []*common.FailsafeConfig{{
			Timeout: timeoutConfig,
			Retry:   retryConfig,
		}},
	}
	return setupTestNetwork(t, ctx, upstreamConfigs, networkConfig)
}

// setupTestNetworkWithUpstreamTimeoutAndRetry attaches both timeout and retry
// at the upstream level. Used to verify retry-exhaust classification when the
// last attempt is a timeout: failsafe exhaustion surfaces here as a raw
// retrypolicy.ExceededError without the ErrUpstreamsExhausted wrapping that
// network scope adds around upstream iteration.
func setupTestNetworkWithUpstreamTimeoutAndRetry(t *testing.T, ctx context.Context, timeoutConfig *common.TimeoutPolicyConfig, retryConfig *common.RetryPolicyConfig) *Network {
	t.Helper()
	upstreamConfigs := []*common.UpstreamConfig{{
		Type:     common.UpstreamTypeEvm,
		Id:       "rpc1",
		Endpoint: "http://rpc1.localhost",
		Evm:      &common.EvmUpstreamConfig{ChainId: 123},
		Failsafe: []*common.FailsafeConfig{{
			Timeout: timeoutConfig,
			Retry:   retryConfig,
		}},
	}}
	networkConfig := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
	}
	return setupTestNetwork(t, ctx, upstreamConfigs, networkConfig)
}

// topLevelErrorCode returns the ErrorCode of the outermost StandardError in
// the chain, or "" if err is not a StandardError.
func topLevelErrorCode(err error) string {
	if se, ok := err.(common.StandardError); ok {
		return string(se.Base().Code)
	}
	return ""
}

// Helper to set up network with timeout policy
func setupTestNetworkWithTimeoutPolicy(t *testing.T, ctx context.Context, timeoutConfig *common.TimeoutPolicyConfig) *Network {
	t.Helper()

	upstreamConfigs := []*common.UpstreamConfig{
		{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc1",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		},
	}

	networkConfig := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm: &common.EvmNetworkConfig{
			ChainId: 123,
		},
		Failsafe: []*common.FailsafeConfig{{
			Timeout: timeoutConfig,
		}},
	}

	return setupTestNetwork(t, ctx, upstreamConfigs, networkConfig)
}
