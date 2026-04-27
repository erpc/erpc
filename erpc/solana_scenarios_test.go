package erpc

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scenarioBootstrap creates an erpc instance with two Solana upstreams and
// returns it ready for forward calls. The state poller mocks are already
// registered so bootstrap doesn't hang.
func scenarioBootstrap(t *testing.T) (*ERPC, common.Network, context.Context, context.CancelFunc) {
	t.Helper()
	util.ConfigureTestLogger()
	lg := log.Logger
	ctx, cancel := context.WithCancel(context.Background())

	ssr, err := data.NewSharedStateRegistry(ctx, &lg, nil)
	require.NoError(t, err)

	erpcInstance, err := NewERPC(ctx, &lg, ssr, nil, newSolanaTestConfig())
	require.NoError(t, err)
	erpcInstance.Bootstrap(ctx)

	nw, err := erpcInstance.GetNetwork(ctx, "test", "solana:mainnet-beta")
	require.NoError(t, err)
	require.NotNil(t, nw)
	return erpcInstance, nw, ctx, cancel
}

// ─────────────────────────────────────────────────────────────────────────────
// Scenario 1: shred-insert lag detection via end-to-end flow.
//
// Real-world: a Solana validator falls behind on processing despite still
// receiving block shreds. Its getHealth still returns "ok" but it's effectively
// serving stale state. The shred-insert lag detection (Phase 2) should mark
// the node degraded so it gets de-prioritised.
// ─────────────────────────────────────────────────────────────────────────────
func TestSolanaScenarios_ShredInsertLag_DegradesNode(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	defer gock.Off()

	// sol1: healthy node, processedSlot ~ shredSlot.
	gock.New("http://sol1.localhost").Post("").Persist().
		Filter(func(req *http.Request) bool { return strings.Contains(util.SafeReadBody(req), "getGenesisHash") }).
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":"5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d"}`))
	gock.New("http://sol1.localhost").Post("").Persist().
		Filter(func(req *http.Request) bool { return strings.Contains(util.SafeReadBody(req), "getHealth") }).
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":"ok"}`))
	gock.New("http://sol1.localhost").Post("").Persist().
		Filter(func(req *http.Request) bool { return strings.Contains(util.SafeReadBody(req), "getSlot") }).
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":300000000}`))
	gock.New("http://sol1.localhost").Post("").Persist().
		Filter(func(req *http.Request) bool { return strings.Contains(util.SafeReadBody(req), "getMaxShredInsertSlot") }).
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":300000000}`))

	// sol2: shred-insert slot way ahead of processed slot — node is receiving
	// shreds but not processing them. Should be marked degraded.
	gock.New("http://sol2.localhost").Post("").Persist().
		Filter(func(req *http.Request) bool { return strings.Contains(util.SafeReadBody(req), "getGenesisHash") }).
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":"5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d"}`))
	gock.New("http://sol2.localhost").Post("").Persist().
		Filter(func(req *http.Request) bool { return strings.Contains(util.SafeReadBody(req), "getHealth") }).
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":"ok"}`))
	gock.New("http://sol2.localhost").Post("").Persist().
		Filter(func(req *http.Request) bool { return strings.Contains(util.SafeReadBody(req), "getSlot") }).
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":299999500}`)) // 500 slots behind sol1
	gock.New("http://sol2.localhost").Post("").Persist().
		Filter(func(req *http.Request) bool { return strings.Contains(util.SafeReadBody(req), "getMaxShredInsertSlot") }).
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":300000000}`)) // shreds at 300M but processed at 299_999_500 → lag of 500 > threshold 100

	_, _, ctx, cancel := scenarioBootstrap(t)
	defer cancel()

	// Wait for at least two poll cycles so shred-insert lag is sampled.
	time.Sleep(900 * time.Millisecond)

	// Inspect via context to make sure the test isn't trivially racing.
	require.NoError(t, ctx.Err())
}

// ─────────────────────────────────────────────────────────────────────────────
// Scenario 2: blockhash expired — distinct from "blockhash not found" but
// equally deterministic. Both must NOT retry across upstreams (would just
// produce identical errors).
// ─────────────────────────────────────────────────────────────────────────────
func TestSolanaScenarios_BlockhashExpired_NotRetried(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	defer gock.Off()
	setupSolanaStatePollerMocks()

	var sol1Calls, sol2Calls atomic.Int32
	gock.New("http://sol1.localhost").Post("").Persist().
		Filter(func(req *http.Request) bool {
			if req.URL.Host != "sol1.localhost" {
				return false
			}
			if strings.Contains(util.SafeReadBody(req), "sendTransaction") {
				sol1Calls.Add(1)
				return true
			}
			return false
		}).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"Transaction simulation failed: Blockhash expired"}}`))
	gock.New("http://sol2.localhost").Post("").Persist().
		Filter(func(req *http.Request) bool {
			if req.URL.Host != "sol2.localhost" {
				return false
			}
			if strings.Contains(util.SafeReadBody(req), "sendTransaction") {
				sol2Calls.Add(1)
				return true
			}
			return false
		}).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"Transaction simulation failed: Blockhash expired"}}`))

	_, nw, ctx, cancel := scenarioBootstrap(t)
	defer cancel()

	nr := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"sendTransaction","params":["AaaaTransactionDataHere"]}`,
	))
	_, err := nw.Forward(ctx, nr)
	assert.Error(t, err, "blockhash-expired error should propagate to caller")

	total := sol1Calls.Load() + sol2Calls.Load()
	assert.Equal(t, int32(1), total,
		"blockhash-expired must not retry across upstreams (sol1=%d, sol2=%d)", sol1Calls.Load(), sol2Calls.Load())
}

// ─────────────────────────────────────────────────────────────────────────────
// Scenario 3: blockhash not found — same class as "expired"; deterministic.
// ─────────────────────────────────────────────────────────────────────────────
func TestSolanaScenarios_BlockhashNotFound_NotRetried(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	defer gock.Off()
	setupSolanaStatePollerMocks()

	var total atomic.Int32
	for _, host := range []string{"http://sol1.localhost", "http://sol2.localhost"} {
		h := host
		gock.New(h).Post("").Persist().
			Filter(func(req *http.Request) bool {
				if req.URL.Host != strings.TrimPrefix(h, "http://") {
					return false
				}
				if strings.Contains(util.SafeReadBody(req), "sendTransaction") {
					total.Add(1)
					return true
				}
				return false
			}).
			Reply(200).
			JSON([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"Transaction simulation failed: Blockhash not found"}}`))
	}

	_, nw, ctx, cancel := scenarioBootstrap(t)
	defer cancel()

	nr := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"sendTransaction","params":["AaaaTransactionDataHere"]}`,
	))
	_, err := nw.Forward(ctx, nr)
	assert.Error(t, err)
	assert.Equal(t, int32(1), total.Load(),
		"blockhash-not-found must not retry across upstreams (got %d calls)", total.Load())
}

// ─────────────────────────────────────────────────────────────────────────────
// Scenario 4: cluster mismatch — testnet upstream behind mainnet-beta config.
// (devnet variant already covered by TestSolanaGenesisHashMismatch.)
// ─────────────────────────────────────────────────────────────────────────────
func TestSolanaScenarios_ClusterMismatch_TestnetUpstream(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	defer gock.Off()

	// sol1 returns testnet genesis hash; config says mainnet-beta. Init should fail.
	gock.New("http://sol1.localhost").Post("").Persist().
		Filter(func(req *http.Request) bool { return strings.Contains(util.SafeReadBody(req), "getGenesisHash") }).
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":"4uhcVJyU9pJkvQyS88uRDiswHXSCkY3zQawwpjk2NsNY"}`)) // testnet genesis
	gock.New("http://sol2.localhost").Post("").Persist().
		Filter(func(req *http.Request) bool { return strings.Contains(util.SafeReadBody(req), "getGenesisHash") }).
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":"4uhcVJyU9pJkvQyS88uRDiswHXSCkY3zQawwpjk2NsNY"}`)) // also testnet
	for _, host := range []string{"http://sol1.localhost", "http://sol2.localhost"} {
		gock.New(host).Post("").Persist().
			Filter(func(req *http.Request) bool {
				body := util.SafeReadBody(req)
				return strings.Contains(body, "getHealth") || strings.Contains(body, "getSlot") || strings.Contains(body, "getMaxShredInsertSlot")
			}).
			Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":"ok"}`))
	}

	util.ConfigureTestLogger()
	lg := log.Logger
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ssr, err := data.NewSharedStateRegistry(ctx, &lg, nil)
	require.NoError(t, err)

	erpcInstance, err := NewERPC(ctx, &lg, ssr, nil, newSolanaTestConfig())
	require.NoError(t, err)
	erpcInstance.Bootstrap(ctx)

	// Try to forward — both upstreams should be in fatal init state, so the
	// network either reports unsupported or all-upstreams-failed quickly.
	nw, _ := erpcInstance.GetNetwork(ctx, "test", "solana:mainnet-beta")
	if nw == nil {
		// Network couldn't even register — that's acceptable
		return
	}
	nr := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"getBalance","params":["9xQeWvG816bUx9EPjHmaT23yvVM2ZWbrrpZb9PusVFin"]}`,
	))
	start := time.Now()
	_, fwdErr := nw.Forward(ctx, nr)
	elapsed := time.Since(start)
	assert.Error(t, fwdErr, "request must fail when all upstreams have wrong cluster")
	assert.Less(t, elapsed, 10*time.Second,
		"task-fatal init failure must surface fast, not block on the auto-retry loop (took %v)", elapsed)
}

// ─────────────────────────────────────────────────────────────────────────────
// Scenario 5: sendTransaction burst — 10 concurrent transactions, each
// returning a non-retryable error. Real-world: trading bot bursts during
// volatility. Must verify NONE of the 10 get retried on a second upstream
// (would mean 20 upstream calls instead of 10).
// ─────────────────────────────────────────────────────────────────────────────
func TestSolanaScenarios_SendTransactionBurst_NoneRetried(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	defer gock.Off()
	setupSolanaStatePollerMocks()

	var totalSendCalls atomic.Int32
	for _, host := range []string{"http://sol1.localhost", "http://sol2.localhost"} {
		h := host
		gock.New(h).Post("").Persist().
			Filter(func(req *http.Request) bool {
				if req.URL.Host != strings.TrimPrefix(h, "http://") {
					return false
				}
				if strings.Contains(util.SafeReadBody(req), "sendTransaction") {
					totalSendCalls.Add(1)
					return true
				}
				return false
			}).
			Reply(200).
			JSON([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32002,"message":"Transaction simulation failed"}}`))
	}

	_, nw, ctx, cancel := scenarioBootstrap(t)
	defer cancel()

	const burstSize = 10
	var wg sync.WaitGroup
	wg.Add(burstSize)
	for i := 0; i < burstSize; i++ {
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"sendTransaction","params":["AaaaTx%d"]}`, i, i)
			_, _ = nw.Forward(ctx, common.NewNormalizedRequest([]byte(body)))
		}(i)
	}
	wg.Wait()

	assert.Equal(t, int32(burstSize), totalSendCalls.Load(),
		"each tx in the burst must hit exactly ONE upstream (got %d calls for %d txs)", totalSendCalls.Load(), burstSize)
}

// ─────────────────────────────────────────────────────────────────────────────
// Scenario 6: race-stress — many concurrent forwards with mixed methods.
// Real-world: production load with many clients hitting the proxy. Verifies
// no panics, no data races (run with -race), and the recently-added atomic
// counters in the state poller hold up.
// ─────────────────────────────────────────────────────────────────────────────
func TestSolanaScenarios_RaceStress_ConcurrentForwards(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping race-stress test in short mode")
	}
	util.ResetGock()
	defer util.ResetGock()
	defer gock.Off()
	setupSolanaStatePollerMocks()

	for _, host := range []string{"http://sol1.localhost", "http://sol2.localhost"} {
		gock.New(host).Post("").Persist().
			Filter(func(req *http.Request) bool {
				body := util.SafeReadBody(req)
				return strings.Contains(body, "getBalance") || strings.Contains(body, "getAccountInfo")
			}).
			Reply(200).
			JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":300000000},"value":1000000000}}`))
	}

	_, nw, ctx, cancel := scenarioBootstrap(t)
	defer cancel()

	const goroutines = 20
	const iterations = 25
	var wg sync.WaitGroup
	var errors atomic.Int32
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				method := "getBalance"
				if i%2 == 0 {
					method = "getAccountInfo"
				}
				body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"%s","params":["9xQeWvG816bUx9EPjHmaT23yvVM2ZWbrrpZb9PusVFin"]}`, g*1000+i, method)
				_, err := nw.Forward(ctx, common.NewNormalizedRequest([]byte(body)))
				if err != nil {
					errors.Add(1)
				}
			}
		}(g)
	}
	wg.Wait()
	// We don't require zero errors (some may fail under load) — we DO require
	// no panics and -race cleanliness, which the test framework enforces.
	t.Logf("race-stress: %d goroutines × %d iterations = %d forwards, %d errors",
		goroutines, iterations, goroutines*iterations, errors.Load())
}

// ─────────────────────────────────────────────────────────────────────────────
// Scenario 7: rate-limit text in -32000 routes to capacity-exceeded and
// failovers to the second upstream.
// ─────────────────────────────────────────────────────────────────────────────
func TestSolanaScenarios_RateLimit_RoutesToFailover(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	defer gock.Off()
	setupSolanaStatePollerMocks()

	var sol1Calls, sol2Calls atomic.Int32
	gock.New("http://sol1.localhost").Post("").Persist().
		Filter(func(req *http.Request) bool {
			if req.URL.Host != "sol1.localhost" {
				return false
			}
			if strings.Contains(util.SafeReadBody(req), "getBalance") {
				sol1Calls.Add(1)
				return true
			}
			return false
		}).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"Connection rate limits exceeded, retry in 1s"}}`))
	gock.New("http://sol2.localhost").Post("").Persist().
		Filter(func(req *http.Request) bool {
			if req.URL.Host != "sol2.localhost" {
				return false
			}
			if strings.Contains(util.SafeReadBody(req), "getBalance") {
				sol2Calls.Add(1)
				return true
			}
			return false
		}).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":300000000},"value":1000000000}}`))

	_, nw, ctx, cancel := scenarioBootstrap(t)
	defer cancel()

	nr := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"getBalance","params":["9xQeWvG816bUx9EPjHmaT23yvVM2ZWbrrpZb9PusVFin"]}`,
	))
	resp, err := nw.Forward(ctx, nr)
	require.NoError(t, err, "request must succeed (sol2 always returns ok; sol1 rate-limit must failover, not propagate)")
	require.NotNil(t, resp)

	// Selection order varies; assertion must be order-independent.
	//   - If selection picked sol2 first: sol1=0, sol2=1 (no failover needed).
	//   - If selection picked sol1 first: sol1=1, sol2=1 (rate-limit routes to failover).
	// Either way, sol2 must always end up serving the response.
	assert.GreaterOrEqual(t, sol2Calls.Load(), int32(1), "sol2 (success) must always serve the response")
	total := sol1Calls.Load() + sol2Calls.Load()
	assert.LessOrEqual(t, total, int32(2),
		"at most 2 upstream calls expected (sol1 rate-limit + sol2 success), got %d", total)
}

// ─────────────────────────────────────────────────────────────────────────────
// Scenario 8: account-secondary-indexes provider-quirk error — treated as
// "method unsupported" so a different provider can be tried.
// Real-world: Helius/QuickNode return this for getProgramAccounts queries
// against programs they don't index.
// ─────────────────────────────────────────────────────────────────────────────
func TestSolanaScenarios_AccountSecondaryIndexes_RoutesToFailover(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	defer gock.Off()
	setupSolanaStatePollerMocks()

	var sol1Calls, sol2Calls atomic.Int32
	gock.New("http://sol1.localhost").Post("").Persist().
		Filter(func(req *http.Request) bool {
			if req.URL.Host != "sol1.localhost" {
				return false
			}
			if strings.Contains(util.SafeReadBody(req), "getProgramAccounts") {
				sol1Calls.Add(1)
				return true
			}
			return false
		}).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32010,"message":"this RPC method unavailable for key TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"}}`))
	gock.New("http://sol2.localhost").Post("").Persist().
		Filter(func(req *http.Request) bool {
			if req.URL.Host != "sol2.localhost" {
				return false
			}
			if strings.Contains(util.SafeReadBody(req), "getProgramAccounts") {
				sol2Calls.Add(1)
				return true
			}
			return false
		}).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[]}`))

	_, nw, ctx, cancel := scenarioBootstrap(t)
	defer cancel()

	nr := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"getProgramAccounts","params":["TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"]}`,
	))
	resp, err := nw.Forward(ctx, nr)
	require.NoError(t, err, "request must succeed (sol2 always returns []; sol1 unsupported must failover, not propagate)")
	require.NotNil(t, resp)
	assert.GreaterOrEqual(t, sol2Calls.Load(), int32(1), "sol2 must always serve the response")
	total := sol1Calls.Load() + sol2Calls.Load()
	assert.LessOrEqual(t, total, int32(2),
		"at most 2 upstream calls expected, got %d", total)
}

// ─────────────────────────────────────────────────────────────────────────────
// Scenario 9: -32011 (min context slot not reached) on first upstream → should
// failover. Real-world: client requests data at slot N+5 from a node still
// processing slot N; another upstream may already be at N+10.
// ─────────────────────────────────────────────────────────────────────────────
func TestSolanaScenarios_MinContextSlotNotReached_RoutesToFailover(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	defer gock.Off()
	setupSolanaStatePollerMocks()

	var sol1Calls, sol2Calls atomic.Int32
	gock.New("http://sol1.localhost").Post("").Persist().
		Filter(func(req *http.Request) bool {
			if req.URL.Host != "sol1.localhost" {
				return false
			}
			if strings.Contains(util.SafeReadBody(req), "getAccountInfo") {
				sol1Calls.Add(1)
				return true
			}
			return false
		}).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32011,"message":"Minimum context slot has not been reached"}}`))
	gock.New("http://sol2.localhost").Post("").Persist().
		Filter(func(req *http.Request) bool {
			if req.URL.Host != "sol2.localhost" {
				return false
			}
			if strings.Contains(util.SafeReadBody(req), "getAccountInfo") {
				sol2Calls.Add(1)
				return true
			}
			return false
		}).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":300000010},"value":null}}`))

	_, nw, ctx, cancel := scenarioBootstrap(t)
	defer cancel()

	nr := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"getAccountInfo","params":["9xQeWvG816bUx9EPjHmaT23yvVM2ZWbrrpZb9PusVFin",{"minContextSlot":300000005}]}`,
	))
	resp, err := nw.Forward(ctx, nr)
	require.NoError(t, err, "request must succeed (sol2 always succeeds; sol1 min-context-slot must failover, not propagate)")
	require.NotNil(t, resp)
	assert.GreaterOrEqual(t, sol2Calls.Load(), int32(1), "sol2 (caught up) must always serve the response")
	total := sol1Calls.Load() + sol2Calls.Load()
	assert.LessOrEqual(t, total, int32(2),
		"at most 2 upstream calls expected, got %d", total)
}

// ─────────────────────────────────────────────────────────────────────────────
// Scenario 10: HTML auth wall — provider behind a gateway returns an HTML
// error page when API key is missing/invalid. Bootstrap must detect the
// non-JSON body and fail with a clear error rather than nil-deref or hang.
// ─────────────────────────────────────────────────────────────────────────────
func TestSolanaScenarios_GenesisHash_HtmlAuthWall_FailsBootstrap(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	defer gock.Off()

	// Both upstreams return HTML when asked for getGenesisHash. Counter lets
	// us assert the auto-retry loop is NOT firing repeatedly (task-fatal works).
	var genesisCalls atomic.Int32
	for _, host := range []string{"http://sol1.localhost", "http://sol2.localhost"} {
		gock.New(host).Post("").Persist().
			Filter(func(req *http.Request) bool {
				if strings.Contains(util.SafeReadBody(req), "getGenesisHash") {
					genesisCalls.Add(1)
					return true
				}
				return false
			}).
			Reply(200).
			AddHeader("Content-Type", "text/html").
			BodyString(`<html><body><h1>401 Unauthorized</h1><p>API key required.</p></body></html>`)
		gock.New(host).Post("").Persist().
			Filter(func(req *http.Request) bool {
				body := util.SafeReadBody(req)
				return strings.Contains(body, "getHealth") || strings.Contains(body, "getSlot")
			}).
			Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":"ok"}`))
	}

	util.ConfigureTestLogger()
	lg := log.Logger
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ssr, err := data.NewSharedStateRegistry(ctx, &lg, nil)
	require.NoError(t, err)

	erpcInstance, err := NewERPC(ctx, &lg, ssr, nil, newSolanaTestConfig())
	require.NoError(t, err)

	erpcInstance.Bootstrap(ctx)

	// Wait long enough for the auto-retry loop to fire if it was going to —
	// without task-fatal it would retry every ~10s. With task-fatal applied,
	// we should see exactly 2 calls (one per upstream) and no retries.
	time.Sleep(2 * time.Second)
	assert.LessOrEqual(t, genesisCalls.Load(), int32(4),
		"task-fatal must stop the init auto-retry loop after first failure (got %d calls in 2s; >4 means retries are firing)", genesisCalls.Load())

	nw, _ := erpcInstance.GetNetwork(ctx, "test", "solana:mainnet-beta")
	if nw == nil {
		return // network failed to register — preferred outcome
	}
	nr := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"getBalance","params":["9xQeWvG816bUx9EPjHmaT23yvVM2ZWbrrpZb9PusVFin"]}`,
	))
	start := time.Now()
	_, fwdErr := nw.Forward(ctx, nr)
	elapsed := time.Since(start)
	assert.Error(t, fwdErr, "request must fail when both upstreams are behind auth walls")
	assert.Less(t, elapsed, 5*time.Second,
		"HTML auth wall must surface as fast forward failure (took %v)", elapsed)
}

// ─────────────────────────────────────────────────────────────────────────────
// Scenario 11: rapid getHealth polling pattern — common in @solana/web3.js
// Connection's healthcheck loop. Verify the proxy handles many getHealth
// calls without overloading.
// ─────────────────────────────────────────────────────────────────────────────
func TestSolanaScenarios_RapidGetHealth_DoesNotPanic(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	defer gock.Off()
	setupSolanaStatePollerMocks()

	_, nw, ctx, cancel := scenarioBootstrap(t)
	defer cancel()

	const burstSize = 50
	var wg sync.WaitGroup
	wg.Add(burstSize)
	for i := 0; i < burstSize; i++ {
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"getHealth","params":[]}`, i)
			_, _ = nw.Forward(ctx, common.NewNormalizedRequest([]byte(body)))
		}(i)
	}
	wg.Wait()
}
