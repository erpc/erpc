package erpc

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEmptyResultAcceptShortCircuit verifies that the upstream loop returns
// immediately after the first emptyish result when the method is in the
// emptyResultAccept list. Before this fix, the loop tried every upstream for
// emptyish results and only checked emptyResultAccept at the failsafe-retry
// level — wasting time on slow/timing-out upstreams.
func TestEmptyResultAcceptShortCircuit(t *testing.T) {
	t.Run("TraceBlock_EmptyArrayCancelsHedge_InvalidShapeRotates", func(t *testing.T) {
		for _, mode := range []string{"hedge", "retry"} {
			for _, first := range []string{"[]", "[ \t\r\n ]", "{}", `""`, "null", "missing"} {
				t.Run(mode+"/"+first, func(t *testing.T) {
					util.ResetGock()
					defer util.ResetGock()
					util.SetupMocksForEvmStatePoller()
					var calls atomic.Int32
					for i, host := range []string{"http://rpc1.localhost", "http://rpc2.localhost"} {
						result := "[]"
						if i == 0 {
							result = first
						}
						gock.New(host).Post("").Filter(func(r *http.Request) bool {
							return strings.Contains(util.SafeReadBody(r), "debug_traceBlockByNumber")
						}).Persist().Reply(200).Map(func(r *http.Response) *http.Response {
							calls.Add(1)
							return r
						}).BodyString(func() string {
							if result == "missing" {
								return `{"jsonrpc":"2.0","id":1}`
							}
							return `{"jsonrpc":"2.0","id":1,"result":` + result + `}`
						}())
					}
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					var network *Network
					if mode == "hedge" {
						network = setupTestNetworkWithHedgePolicy(t, ctx, &common.HedgePolicyConfig{
							Delay: common.NewStaticDuration(250 * time.Millisecond), MaxCount: 1,
						})
					} else {
						network = setupTestNetworkWithRetryConfig(t, ctx,
							&common.DirectiveDefaultsConfig{RetryEmpty: util.BoolPtr(true)},
							&common.RetryPolicyConfig{MaxAttempts: 3, EmptyResultDelay: common.Duration(250 * time.Millisecond)},
						)
					}
					network.PinUpstreamOrderForTest("rpc1", "rpc2")
					req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"debug_traceBlockByNumber","params":["0x10",{"tracer":"callTracer"}]}`))
					req.ApplyDirectiveDefaults(network.cfg.DirectiveDefaults)
					start := time.Now()
					resp, err := network.Forward(ctx, req)
					require.NoError(t, err)
					require.NotNil(t, resp)
					defer resp.Release()
					jrr, err := resp.JsonRpcResponse()
					require.NoError(t, err)
					assert.Equal(t, "[]", strings.Join(strings.Fields(jrr.GetResultString()), ""))
					wantCalls := int32(2)
					if strings.HasPrefix(first, "[") {
						wantCalls = 1
					}
					assert.Equal(t, wantCalls, calls.Load())
					assert.Less(t, time.Since(start), 250*time.Millisecond)
					t.Logf("mode=%s first=%q calls=%d elapsed=%v", mode, first, calls.Load(), time.Since(start))
				})
			}
		}
	})
	t.Run("EthGetLogs_EmptyArray_OnlyFirstUpstreamCalled", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		var rpc1Calls, rpc2Calls atomic.Int32

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getLogs")
			}).
			Persist().
			Reply(200).
			Map(func(r *http.Response) *http.Response { rpc1Calls.Add(1); return r }).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  []interface{}{},
			})

		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getLogs")
			}).
			Persist().
			Reply(200).
			Map(func(r *http.Response) *http.Response { rpc2Calls.Add(1); return r }).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  []interface{}{},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupTestNetworkWithRetryConfig(t, ctx,
			&common.DirectiveDefaultsConfig{RetryEmpty: util.BoolPtr(true)},
			&common.RetryPolicyConfig{
				MaxAttempts:       3,
				Delay:             common.Duration(50 * time.Millisecond),
				EmptyResultDelay:  common.Duration(200 * time.Millisecond),
				EmptyResultAccept: common.DefaultEmptyResultAccept(),
			},
		)

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{"fromBlock":"0x1","toBlock":"0x100"}]}`)
		req := common.NewNormalizedRequest(requestBytes)
		req.ApplyDirectiveDefaults(network.cfg.DirectiveDefaults)

		start := time.Now()
		resp, err := network.Forward(ctx, req)
		elapsed := time.Since(start)

		require.NoError(t, err, "eth_getLogs empty should be accepted")
		require.NotNil(t, resp)

		jrr, jrrErr := resp.JsonRpcResponse()
		require.NoError(t, jrrErr)
		assert.True(t, jrr.IsResultEmptyish(), "result should be emptyish")

		t.Logf("rpc1=%d  rpc2=%d  elapsed=%v", rpc1Calls.Load(), rpc2Calls.Load(), elapsed)
		assert.Equal(t, int32(1), rpc1Calls.Load(), "rpc1 should be called exactly once")
		assert.Equal(t, int32(0), rpc2Calls.Load(), "rpc2 should never be called — short-circuited")
		assert.Less(t, elapsed, 500*time.Millisecond, "should return immediately without trying rpc2")
	})

	t.Run("EthCall_EmptyHex_OnlyFirstUpstreamCalled", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		var rpc1Calls, rpc2Calls atomic.Int32

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_call")
			}).
			Persist().
			Reply(200).
			Map(func(r *http.Response) *http.Response { rpc1Calls.Add(1); return r }).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x",
			})

		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_call")
			}).
			Persist().
			Reply(200).
			Map(func(r *http.Response) *http.Response { rpc2Calls.Add(1); return r }).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x",
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupTestNetworkWithRetryConfig(t, ctx,
			&common.DirectiveDefaultsConfig{RetryEmpty: util.BoolPtr(true)},
			&common.RetryPolicyConfig{
				MaxAttempts:       3,
				Delay:             common.Duration(50 * time.Millisecond),
				EmptyResultDelay:  common.Duration(200 * time.Millisecond),
				EmptyResultAccept: common.DefaultEmptyResultAccept(),
			},
		)

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0xabc"},"latest"]}`)
		req := common.NewNormalizedRequest(requestBytes)
		req.ApplyDirectiveDefaults(network.cfg.DirectiveDefaults)

		start := time.Now()
		resp, err := network.Forward(ctx, req)
		elapsed := time.Since(start)

		require.NoError(t, err, "eth_call empty should be accepted")
		require.NotNil(t, resp)

		jrr, jrrErr := resp.JsonRpcResponse()
		require.NoError(t, jrrErr)
		assert.True(t, jrr.IsResultEmptyish(), "result should be emptyish")

		t.Logf("rpc1=%d  rpc2=%d  elapsed=%v", rpc1Calls.Load(), rpc2Calls.Load(), elapsed)
		assert.Equal(t, int32(1), rpc1Calls.Load(), "rpc1 should be called exactly once")
		assert.Equal(t, int32(0), rpc2Calls.Load(), "rpc2 should never be called — short-circuited")
		assert.Less(t, elapsed, 500*time.Millisecond, "should return immediately without trying rpc2")
	})

	t.Run("MethodNotInAcceptList_BothUpstreamsTried", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		var rpc1Calls, rpc2Calls atomic.Int32

		// eth_getBlockReceipts returns "[]" which is emptyish, but this
		// method is NOT in the emptyResultAccept list (nor the
		// mark-empty-as-error list) so both upstreams must be tried before
		// returning to the failsafe layer.
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getBlockReceipts")
			}).
			Persist().
			Reply(200).
			Map(func(r *http.Response) *http.Response { rpc1Calls.Add(1); return r }).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  []interface{}{},
			})

		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getBlockReceipts")
			}).
			Persist().
			Reply(200).
			Map(func(r *http.Response) *http.Response { rpc2Calls.Add(1); return r }).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  []interface{}{},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupTestNetworkWithRetryConfig(t, ctx,
			&common.DirectiveDefaultsConfig{RetryEmpty: util.BoolPtr(true)},
			&common.RetryPolicyConfig{
				MaxAttempts:       2,
				Delay:             common.Duration(10 * time.Millisecond),
				EmptyResultDelay:  common.Duration(10 * time.Millisecond),
				EmptyResultAccept: common.DefaultEmptyResultAccept(),
			},
		)

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":["0x1234"]}`)
		req := common.NewNormalizedRequest(requestBytes)
		req.ApplyDirectiveDefaults(network.cfg.DirectiveDefaults)

		resp, err := network.Forward(ctx, req)
		require.NoError(t, err, "should eventually return the emptyish result after trying all upstreams")
		require.NotNil(t, resp)

		t.Logf("rpc1=%d  rpc2=%d", rpc1Calls.Load(), rpc2Calls.Load())
		assert.GreaterOrEqual(t, rpc1Calls.Load(), int32(1), "rpc1 should be called at least once")
		assert.GreaterOrEqual(t, rpc2Calls.Load(), int32(1), "rpc2 should also be called — method not in accept list")
	})

	t.Run("NonEmptyResult_ReturnsImmediately_RegardlessOfAcceptList", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		var rpc1Calls, rpc2Calls atomic.Int32

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
			}).
			Persist().
			Reply(200).
			Map(func(r *http.Response) *http.Response { rpc1Calls.Add(1); return r }).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0xde0b6b3a7640000",
			})

		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
			}).
			Persist().
			Reply(200).
			Map(func(r *http.Response) *http.Response { rpc2Calls.Add(1); return r }).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0xde0b6b3a7640000",
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// eth_getBalance is NOT in emptyResultAccept but the result is
		// non-emptyish so it should return after the first upstream anyway.
		network := setupTestNetworkWithRetryConfig(t, ctx,
			&common.DirectiveDefaultsConfig{RetryEmpty: util.BoolPtr(true)},
			&common.RetryPolicyConfig{
				MaxAttempts:       3,
				Delay:             common.Duration(50 * time.Millisecond),
				EmptyResultDelay:  common.Duration(200 * time.Millisecond),
				EmptyResultAccept: common.DefaultEmptyResultAccept(),
			},
		)

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xabc","latest"]}`)
		req := common.NewNormalizedRequest(requestBytes)
		req.ApplyDirectiveDefaults(network.cfg.DirectiveDefaults)

		start := time.Now()
		resp, err := network.Forward(ctx, req)
		elapsed := time.Since(start)

		require.NoError(t, err)
		require.NotNil(t, resp)

		jrr, jrrErr := resp.JsonRpcResponse()
		require.NoError(t, jrrErr)
		assert.False(t, jrr.IsResultEmptyish(), "result should not be emptyish")

		t.Logf("rpc1=%d  rpc2=%d  elapsed=%v", rpc1Calls.Load(), rpc2Calls.Load(), elapsed)
		assert.Equal(t, int32(1), rpc1Calls.Load(), "rpc1 should be called exactly once")
		assert.Equal(t, int32(0), rpc2Calls.Load(), "rpc2 should never be called — non-empty result returns immediately")
		assert.Less(t, elapsed, 500*time.Millisecond)
	})

	t.Run("DefaultAcceptList_EthGetLogs_ShortCircuitsWithoutExplicitConfig", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		var rpc1Calls, rpc2Calls atomic.Int32

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getLogs")
			}).
			Persist().
			Reply(200).
			Map(func(r *http.Response) *http.Response { rpc1Calls.Add(1); return r }).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  []interface{}{},
			})

		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getLogs")
			}).
			Persist().
			Reply(200).
			Map(func(r *http.Response) *http.Response { rpc2Calls.Add(1); return r }).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  []interface{}{},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// No EmptyResultAccept configured — the default list
		// (eth_getLogs, eth_call) should apply.
		network := setupTestNetworkWithRetryConfig(t, ctx,
			&common.DirectiveDefaultsConfig{RetryEmpty: util.BoolPtr(true)},
			&common.RetryPolicyConfig{
				MaxAttempts:      3,
				Delay:            common.Duration(50 * time.Millisecond),
				EmptyResultDelay: common.Duration(200 * time.Millisecond),
			},
		)

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{"fromBlock":"0x1","toBlock":"0x100"}]}`)
		req := common.NewNormalizedRequest(requestBytes)
		req.ApplyDirectiveDefaults(network.cfg.DirectiveDefaults)

		start := time.Now()
		resp, err := network.Forward(ctx, req)
		elapsed := time.Since(start)

		require.NoError(t, err)
		require.NotNil(t, resp)

		jrr, jrrErr := resp.JsonRpcResponse()
		require.NoError(t, jrrErr)
		assert.True(t, jrr.IsResultEmptyish())

		t.Logf("rpc1=%d  rpc2=%d  elapsed=%v", rpc1Calls.Load(), rpc2Calls.Load(), elapsed)
		assert.Equal(t, int32(1), rpc1Calls.Load(), "rpc1 should be called exactly once")
		assert.Equal(t, int32(0), rpc2Calls.Load(), "rpc2 should never be called — default accept list")
		assert.Less(t, elapsed, 500*time.Millisecond, "should return immediately")
	})

	t.Run("ExplicitEmptyAcceptList_OverridesDefaults_BothUpstreamsTried", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		var rpc1Calls, rpc2Calls atomic.Int32

		// eth_getLogs is in the DEFAULT accept list, but the user explicitly
		// sets EmptyResultAccept to an empty array — meaning "accept nothing".
		// Both upstreams must be tried (no short-circuit).
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getLogs")
			}).
			Persist().
			Reply(200).
			Map(func(r *http.Response) *http.Response { rpc1Calls.Add(1); return r }).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  []interface{}{},
			})

		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getLogs")
			}).
			Persist().
			Reply(200).
			Map(func(r *http.Response) *http.Response { rpc2Calls.Add(1); return r }).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  []interface{}{},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupTestNetworkWithRetryConfig(t, ctx,
			&common.DirectiveDefaultsConfig{RetryEmpty: util.BoolPtr(true)},
			&common.RetryPolicyConfig{
				MaxAttempts:       2,
				Delay:             common.Duration(10 * time.Millisecond),
				EmptyResultDelay:  common.Duration(10 * time.Millisecond),
				EmptyResultAccept: []string{},
			},
		)

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{"fromBlock":"0x1","toBlock":"0x100"}]}`)
		req := common.NewNormalizedRequest(requestBytes)
		req.ApplyDirectiveDefaults(network.cfg.DirectiveDefaults)

		resp, err := network.Forward(ctx, req)
		require.NoError(t, err)
		require.NotNil(t, resp)

		t.Logf("rpc1=%d  rpc2=%d", rpc1Calls.Load(), rpc2Calls.Load())
		assert.GreaterOrEqual(t, rpc1Calls.Load(), int32(1), "rpc1 should be called")
		assert.GreaterOrEqual(t, rpc2Calls.Load(), int32(1), "rpc2 should also be called — empty accept list overrides defaults")
	})
}
