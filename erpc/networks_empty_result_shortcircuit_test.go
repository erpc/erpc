package erpc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	bdsevm "github.com/blockchain-data-standards/manifesto/evm"
	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	gethcommon "github.com/ethereum/go-ethereum/common"
	gethtypes "github.com/ethereum/go-ethereum/core/types"
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
	t.Run("TraceBlock_UnknownEmptyRotates", func(t *testing.T) {
		for _, mode := range []string{"hedge", "retry"} {
			for _, first := range []string{"[]", "[ \t\r\n ]", "[" + strings.Repeat(" ", 128) + "]", "{}", `""`, "null", "missing"} {
				t.Run(mode+"/"+first, func(t *testing.T) {
					util.ResetGock()
					defer util.ResetGock()
					util.SetupMocksForEvmStatePoller()
					var calls atomic.Int32
					for i, host := range []string{"http://rpc1.localhost", "http://rpc2.localhost"} {
						result := `[{"txHash":"0xabc","result":{"type":"CALL","gasUsed":"0x1"}}]`
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
					assert.Contains(t, jrr.GetResultString(), "0xabc")
					assert.Equal(t, int32(2), calls.Load())
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

func TestEmptyResultAcceptFollowedTraceBlock(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	fillFetches := func() float64 {
		return metricSum(telemetry.MetricChainTrackerFetchTotal, map[string]string{"project": "test", "class": "fill"})
	}
	fillsAtStart := fillFetches()
	// The counters count only fetches made for a Forward under test, which
	// carry the marker in their context: tracker fetches (fill, no-source
	// asks) run on the network's context and cannot change them.
	type forwardUnderTest struct{}
	fctx := func(ctx context.Context) context.Context { return context.WithValue(ctx, forwardUnderTest{}, true) }
	marked := func(r *http.Response) bool { return r.Request.Context().Value(forwardUnderTest{}) != nil }
	// numberedBlockCalls counts block fetches by number.
	var traceCalls, numberedBlockCalls atomic.Int32
	var empty atomic.Bool
	var latestNum atomic.Int64
	latestNum.Store(0x3e8)
	// Headers carry no transactions array, as source headers do: only
	// transactionsRoot and gasUsed prove a block empty, and only in a header
	// whose hash recomputes. Blocks are held as first answered, so each case
	// is its own height: 0x3e8 the empty-trie root, 0x3e9 no root, 0x3ea a
	// root with one transaction, 0x3eb the empty-trie root in a header whose
	// hash does not recompute.
	headers := make(map[int64]string)
	hashes := make(map[int64]gethcommon.Hash)
	var parent gethcommon.Hash
	for n := int64(0); n <= 0x3f0; n++ {
		h := &gethtypes.Header{ParentHash: parent, UncleHash: gethtypes.EmptyUncleHash, TxHash: gethtypes.EmptyTxsHash,
			ReceiptHash: gethtypes.EmptyReceiptsHash, Difficulty: big.NewInt(0), Number: big.NewInt(n), GasLimit: 30_000_000,
			Time: uint64(0x6702a8f0 + n), BaseFee: big.NewInt(7)}
		if n == 0x3ea {
			h.TxHash = gethcommon.HexToHash("0x" + strings.Repeat("ab", 32))
		}
		raw, err := h.MarshalJSON()
		require.NoError(t, err)
		var fields map[string]any
		require.NoError(t, json.Unmarshal(raw, &fields))
		switch n {
		case 0x3e9:
			delete(fields, "transactionsRoot")
		case 0x3eb:
			fields["extraData"] = "0x01"
		}
		raw, err = json.Marshal(fields)
		require.NoError(t, err)
		headers[n] = string(raw)
		parent = h.Hash()
		hashes[n] = parent
	}
	refRe := regexp.MustCompile(`"params":\["(0x[0-9a-fA-F]+)"`)
	blockRef := func(body string) int64 {
		m := refRe.FindStringSubmatch(body)
		if m == nil {
			return -1
		}
		n, _ := common.HexToInt64(m[1])
		return n
	}
	for _, host := range []string{"http://rpc1.localhost", "http://rpc2.localhost"} {
		gock.New(host).Post("").Persist().Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_chainId") }).Reply(200).BodyString(`{"jsonrpc":"2.0","id":1,"result":"0x7b"}`)
		gock.New(host).Post("").Persist().Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_syncing") }).Reply(200).BodyString(`{"jsonrpc":"2.0","id":1,"result":false}`)
		gock.New(host).Post("").Persist().Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockByNumber") }).Reply(200).Map(func(r *http.Response) *http.Response {
			n := blockRef(util.SafeReadBody(r.Request))
			if n >= 0 && marked(r) {
				numberedBlockCalls.Add(1)
			}
			if n < 0 {
				n = latestNum.Load()
			}
			r.Body = io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","id":1,"result":` + headers[n] + `}`))
			return r
		}).BodyString(`{}`)
		gock.New(host).Post("").Persist().Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "debug_traceBlockByNumber") }).Reply(200).Map(func(r *http.Response) *http.Response {
			if marked(r) {
				traceCalls.Add(1)
			}
			result := `[{"result":{"type":"CALL","gasUsed":"0x0"}}]`
			if empty.Load() && (blockRef(util.SafeReadBody(r.Request)) != 0x3ea || host == "http://rpc1.localhost") {
				result = "[" + strings.Repeat(" ", 128) + "]"
			}
			r.Body = io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","id":1,"result":` + result + `}`))
			return r
		}).BodyString(`{}`)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}, Integrity: &common.IntegrityConfig{IntegritySettings: common.IntegritySettings{Checks: map[string]*common.IntegrityCheckConfig{"traceBlockGasReconciliation": {Enabled: util.BoolPtr(true)}}, Follow: &common.IntegrityFollowConfig{Enabled: util.BoolPtr(true)}}}, Failsafe: []*common.FailsafeConfig{{Retry: &common.RetryPolicyConfig{MaxAttempts: 3, EmptyResultDelay: common.Duration(250 * time.Millisecond)}, Hedge: &common.HedgePolicyConfig{Delay: common.NewStaticDuration(250 * time.Millisecond), MaxCount: 1}}}}
	ups := []*common.UpstreamConfig{{Type: common.UpstreamTypeEvm, Id: "rpc1", Endpoint: "http://rpc1.localhost", Evm: &common.EvmUpstreamConfig{ChainId: 123}}, {Type: common.UpstreamTypeEvm, Id: "rpc2", Endpoint: "http://rpc2.localhost", Evm: &common.EvmUpstreamConfig{ChainId: 123}}}
	require.NoError(t, cfg.SetDefaults(ups, nil))
	n := setupTestNetwork(t, ctx, ups, cfg)
	n.PinUpstreamOrderForTest("rpc1", "rpc2")
	// The project calls it for a client request; the follow tap starts there.
	evm.ClientRequested(n)
	request := func(ref string) *common.NormalizedRequest {
		r := common.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"debug_traceBlockByNumber","params":["%s",{"tracer":"callTracer"}]}`, ref)))
		r.SetNetwork(n)
		r.SetDirectives(&common.RequestDirectives{RetryEmpty: true})
		return r
	}
	warm := request("0x3e8")
	resp, err := n.Forward(ctx, warm)
	require.NoError(t, err)
	resp.Release()
	require.Eventually(t, func() bool { return evm.HasVerifiedEmptyTraceBlock(warm) }, 2*time.Second, 10*time.Millisecond)
	// Fill holds the pinned chain reorgWindow blocks below the head and
	// stops there: no resumable tap is open.
	tracker := evm.ChainTrackerOf(ctx, n, "")
	window := int64(tracker.ReorgWindow())
	require.Eventually(t, func() bool {
		from, _, ok := tracker.FollowedRange()
		return ok && from == 0x3e8-window
	}, 10*time.Second, 10*time.Millisecond)
	fills := fillFetches()
	time.Sleep(500 * time.Millisecond)
	from, _, _ := tracker.FollowedRange()
	require.Equal(t, 0x3e8-window, from, "fill stops reorgWindow blocks below the head")
	require.Equal(t, fills, fillFetches(), "no fill fetch below reorgWindow")
	empty.Store(true)
	// 0x3e8 and 0x3e7 are pinned with no transactions; latest is a tag and
	// 0x100 lies below the window, so neither is proven.
	for _, ref := range []string{"0x3e8", "0x3e7", "latest", "0x100"} {
		beforeTrace, beforeBlock := traceCalls.Load(), numberedBlockCalls.Load()
		start := time.Now()
		resp, err = n.Forward(fctx(ctx), request(ref))
		require.NoError(t, err)
		jrr, e := resp.JsonRpcResponse()
		require.NoError(t, e)
		t.Logf("ref=%s result=%q proof=%v", ref, jrr.GetResultString(), evm.HasVerifiedEmptyTraceBlock(resp.Request()))
		require.True(t, strings.HasPrefix(strings.TrimSpace(jrr.GetResultString()), "["))
		resp.Release()
		calls := traceCalls.Load() - beforeTrace
		if ref == "0x3e8" || ref == "0x3e7" {
			require.Equal(t, int32(1), calls)
			require.Equal(t, beforeBlock, numberedBlockCalls.Load(), "the held pin proves the block: no block fetch at all")
			require.Less(t, time.Since(start), 250*time.Millisecond)
		} else {
			require.Greater(t, calls, int32(1))
		}
		t.Logf("ref=%s traceCalls=%d numberedBlockCalls=%d elapsed=%v", ref, calls, numberedBlockCalls.Load()-beforeBlock, time.Since(start))
	}
	// With no source, the follow tap's demand makes the tracker ask for
	// latest each block time: the pins follow the network head.
	latestNum.Store(0x3ea)
	for _, u := range n.upstreamsRegistry.GetNetworkUpstreams(ctx, n.networkId) {
		u.EvmStatePoller().SuggestLatestBlock(0x3ea, time.Now())
	}
	require.Eventually(t, func() bool {
		_, ok := evm.ChainTrackerOf(ctx, n, "").HashAt(0x3ea)
		return ok
	}, 2*time.Second, 10*time.Millisecond)
	noRoot := request("0x3e9")
	require.False(t, evm.HasVerifiedEmptyTraceBlock(noRoot), "a header without transactionsRoot proves nothing")
	beforeMissing := traceCalls.Load()
	resp, err = n.Forward(fctx(ctx), noRoot)
	require.NoError(t, err)
	resp.Release()
	require.Greater(t, traceCalls.Load()-beforeMissing, int32(1))
	t.Logf("no transactionsRoot traceCalls=%d", traceCalls.Load()-beforeMissing)
	beforeNonempty := traceCalls.Load()
	resp, err = n.Forward(fctx(ctx), request("0x3ea"))
	require.False(t, evm.HasVerifiedEmptyTraceBlock(request("0x3ea")), "a transactionsRoot of one transaction proves the block is not empty")
	require.NoError(t, err)
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	require.Contains(t, jrr.GetResultString(), "CALL")
	resp.Release()
	require.Equal(t, int32(2), traceCalls.Load()-beforeNonempty)
	t.Logf("known nonempty traceCalls=%d", traceCalls.Load()-beforeNonempty)
	latestNum.Store(0x3eb)
	for _, u := range n.upstreamsRegistry.GetNetworkUpstreams(ctx, n.networkId) {
		u.EvmStatePoller().SuggestLatestBlock(0x3eb, time.Now())
	}
	require.Eventually(t, func() bool {
		_, ok := evm.ChainTrackerOf(ctx, n, "").HashAt(0x3eb)
		return ok
	}, 15*time.Second, 10*time.Millisecond)
	forged := request("0x3eb")
	require.False(t, evm.HasVerifiedEmptyTraceBlock(forged), "an empty-trie root in a header whose hash does not recompute proves nothing")
	beforeForged := traceCalls.Load()
	resp, err = n.Forward(fctx(ctx), forged)
	require.NoError(t, err)
	resp.Release()
	require.Greater(t, traceCalls.Load()-beforeForged, int32(1), "the empty trace is not trusted on that header's word")
	t.Logf("non-recomputing header traceCalls=%d", traceCalls.Load()-beforeForged)
	for _, tc := range []struct {
		name    string
		list    []string
		inherit bool
		accept  bool
	}{
		{"replacedDefault", []string{}, false, false},
		{"default", nil, false, true},
		{"inheritedDefault", nil, true, true},
		{"explicitEmpty", []string{}, false, false},
		{"inheritedEmpty", []string{}, true, false},
		{"explicitOther", []string{"eth_call"}, false, false},
		{"inheritedOther", []string{"eth_call"}, true, false},
		{"explicitTrace", []string{"debug_traceBlockByNumber"}, false, true},
		{"inheritedTrace", []string{"debug_traceBlockByNumber"}, true, true},
	} {
		policy := &common.RetryPolicyConfig{MaxAttempts: 3, EmptyResultDelay: common.Duration(250 * time.Millisecond), EmptyResultAccept: tc.list}
		require.NoError(t, policy.SetDefaults(nil))
		if tc.name == "replacedDefault" {
			policy = &common.RetryPolicyConfig{MaxAttempts: 3, EmptyResultDelay: common.Duration(250 * time.Millisecond)}
			require.NoError(t, policy.SetDefaults(nil))
			policy.EmptyResultAccept = tc.list
		}
		if tc.inherit {
			child := &common.RetryPolicyConfig{}
			require.NoError(t, child.SetDefaults(policy.Copy()))
			policy = child
		}
		require.NoError(t, policy.SetDefaults(nil))
		for _, executor := range n.failsafeExecutors {
			if executor.cfg == nil || executor.cfg.Retry == nil {
				continue
			}
			executor.cfg.Retry = policy.Copy()
			executor.emptyResultAccept = policy.EmptyResultAccept
			if executor.emptyResultAccept == nil {
				executor.emptyResultAccept = common.DefaultEmptyResultAccept()
			}
		}
		before := traceCalls.Load()
		resp, err = n.Forward(fctx(ctx), request("0x3e8"))
		require.NoError(t, err)
		resp.Release()
		calls := traceCalls.Load() - before
		if tc.accept {
			require.Equal(t, int32(1), calls)
		} else {
			require.Greater(t, calls, int32(1))
		}
		t.Logf("policy=%s traceCalls=%d", tc.name, calls)
	}
	// The heads since moved the window's top only: one fill fetch per
	// block of reorgWindow over the whole flow.
	from, _, _ = tracker.FollowedRange()
	require.Equal(t, 0x3e8-window, from)
	require.Equal(t, float64(window), fillFetches()-fillsAtStart, "fill fetches without a resumable tap")

	// A gRPC resume 2× reorgWindow below the head, inside maxBackfill (5m
	// at one block a second): fill reaches the cursor on demand, and the
	// stream sends every block above it, oldest first. While the stream is
	// open, fill holds the whole window.
	tip, ok := tracker.Tip()
	require.True(t, ok)
	cursor := tip.Number - uint64(2*window)
	conn, mdCtx := grpcConn(t, ctx, n)
	sctx, stopStream := context.WithTimeout(mdCtx, 10*time.Second)
	defer stopStream()
	stream, err := bdsevm.NewStreamServiceClient(conn).StreamBlocks(sctx, &bdsevm.StreamBlocksRequest{FromBlock: &cursor, FromHash: hashes[int64(cursor)].Bytes()})
	require.NoError(t, err)
	// 0x3e9 has no transactionsRoot, so the stream cannot encode it.
	for want := int64(cursor) + 1; want <= 0x3e8; want++ {
		msg, err := stream.Recv()
		require.NoError(t, err, "block %d", want)
		require.Equal(t, uint64(want), msg.GetHeader().GetNumber())
		require.Equal(t, hashes[want].Bytes(), msg.GetHeader().GetHash(), "block %d", want)
	}
	require.Eventually(t, func() bool {
		from, _, ok := tracker.FollowedRange()
		return ok && from == int64(tip.Number)-300
	}, 10*time.Second, 10*time.Millisecond, "an open resumable tap fills down to maxBackfill")
	stopStream()
}
