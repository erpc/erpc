package erpc

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/internal/policy"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This suite reproduces the "ineffective integrity mode enforceHighestBlock on
// eth_blockNumber" incident (customer report, 2026-05-13): with a realtime
// cache policy on eth_blockNumber and at least one lagging upstream, clients
// observe a sawtooth of backwards-moving block numbers (thousands of blocks),
// with the largest swings on `x-erpc-cache: HIT` responses.
//
// Topology in every sub-test (full HTTP server, real cache, real pollers,
// only upstream HTTP responses are mocked):
//
//	rpc1 (lagging): head 0x800
//	rpc2 (healthy): head 0x1000  →  network tip (default max mode) = 0x1000
//
// The selection order is pinned to [rpc1, rpc2] so user traffic lands on the
// lagging upstream first, mirroring the customer's "a small amount of volume
// is handled by lagging nodes" condition.
//
// Bug inventory exercised here (hook: architecture/evm/eth_blockNumber.go):
//
//	B1 write-side poisoning: network.Forward async-caches the ORIGINAL stale
//	   response (erpc/networks.go cache-set goroutine) while the hook's
//	   corrected response is only returned to the client — the corrected
//	   value never reaches the cache.
//	B2 read-side bypass: responses with FromCache()==true skip enforcement
//	   entirely, so a poisoned (or otherwise stale) cache entry is served
//	   verbatim for a full TTL window. The realtime age guard cannot save
//	   eth_blockNumber either: its responses carry no block timestamp and
//	   non-head-aware connectors (memory/redis) fail open.
//	B3 selector scope: the hook resolves the tip with a context that does not
//	   carry the request (common.RequestContextKey is only set inside
//	   network.Forward), so X-ERPC-Use-Upstream-pinned requests are corrected
//	   against the NETWORK-WIDE tip instead of the pinned subset's tip,
//	   contradicting the selector-scoped served-tip semantics (#912).
//	B4 directive gating: the hook is gated on the network config
//	   (evm.integrity.enforceHighestBlock, deprecated but auto-defaulted to
//	   true) instead of the request directives that eth_getBlockByNumber
//	   honors — so per-request overrides (enforce-highest-block=false) are
//	   silently ignored for eth_blockNumber.
const (
	bniLaggingHead = int64(0x800)  // rpc1's head
	bniHealthyHead = int64(0x1000) // rpc2's head == expected network tip
)

// setupBniPollerMocks registers persistent state-poller bootstrap mocks for
// both upstreams (chainId / syncing / latest / finalized), giving rpc1 a
// lagging head and rpc2 a healthy head. The state pollers fetch heads via
// eth_getBlockByNumber, so these never collide with user eth_blockNumber
// traffic.
func setupBniPollerMocks() {
	for _, h := range []struct {
		host      string
		latest    string
		finalized string
	}{
		{"http://rpc1.localhost", "0x800", "0x700"},
		{"http://rpc2.localhost", "0x1000", "0x900"},
	} {
		gock.New(h.host).Post("").Persist().
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_chainId")
			}).
			Reply(200).
			JSON([]byte(`{"result":"0x7b"}`))
		gock.New(h.host).Post("").Persist().
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_syncing")
			}).
			Reply(200).
			JSON([]byte(`{"result":false}`))
		gock.New(h.host).Post("").Persist().
			Filter(func(r *http.Request) bool {
				b := util.SafeReadBody(r)
				return strings.Contains(b, "eth_getBlockByNumber") && strings.Contains(b, "latest")
			}).
			Reply(200).
			JSON([]byte(fmt.Sprintf(`{"result":{"number":"%s","timestamp":"0x6702a8f0"}}`, h.latest)))
		gock.New(h.host).Post("").Persist().
			Filter(func(r *http.Request) bool {
				b := util.SafeReadBody(r)
				return strings.Contains(b, "eth_getBlockByNumber") && strings.Contains(b, "finalized")
			}).
			Reply(200).
			JSON([]byte(fmt.Sprintf(`{"result":{"number":"%s","timestamp":"0x6702a8e0"}}`, h.finalized)))
	}
}

// setupBniBlockNumberMocks registers persistent user-facing eth_blockNumber
// mocks: the lagging upstream answers with its stale head, the healthy one
// with the tip. Persist (rather than Times(1)) keeps every sub-test
// deterministic regardless of how many forwards the implementation performs.
func setupBniBlockNumberMocks() {
	gock.New("http://rpc1.localhost").Post("").Persist().
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "eth_blockNumber")
		}).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x800"}`))
	gock.New("http://rpc2.localhost").Post("").Persist().
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "eth_blockNumber")
		}).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x1000"}`))
}

// bniConfig builds a 2-upstream EVM project. integrity==nil exercises the
// modern config path (only DirectiveDefaults, which default enforcement to
// true). withCache adds a realtime cache policy on eth_blockNumber backed by
// a real in-memory connector — the customer's setup (theirs had a 2s TTL; we
// use 10s so entries cannot expire mid-test).
func bniConfig(withCache bool, integrity *common.EvmIntegrityConfig) *common.Config {
	cfg := &common.Config{
		Server: &common.ServerConfig{
			MaxTimeout: common.Duration(10 * time.Second).Ptr(),
		},
		Projects: []*common.ProjectConfig{
			{
				Id: "test_project",
				Networks: []*common.NetworkConfig{
					{
						Architecture: "evm",
						Evm: &common.EvmNetworkConfig{
							ChainId:   123,
							Integrity: integrity,
						},
					},
				},
				Upstreams: []*common.UpstreamConfig{
					{
						Id:       "rpc1",
						Endpoint: "http://rpc1.localhost",
						Type:     common.UpstreamTypeEvm,
						Evm: &common.EvmUpstreamConfig{
							ChainId:             123,
							StatePollerInterval: common.Duration(10 * time.Second),
						},
					},
					{
						Id:       "rpc2",
						Endpoint: "http://rpc2.localhost",
						Type:     common.UpstreamTypeEvm,
						Evm: &common.EvmUpstreamConfig{
							ChainId:             123,
							StatePollerInterval: common.Duration(10 * time.Second),
						},
					},
				},
			},
		},
	}
	if withCache {
		cfg.Database = &common.DatabaseConfig{
			EvmJsonRpcCache: &common.CacheConfig{
				Connectors: []*common.ConnectorConfig{
					{
						Id:     "memory-cache",
						Driver: common.DriverMemory,
						Memory: &common.MemoryConnectorConfig{
							MaxItems: 100_000, MaxTotalSize: "1GB",
						},
					},
				},
				Policies: []*common.CachePolicyConfig{
					{
						Network:   "*",
						Method:    "eth_blockNumber",
						Finality:  common.DataFinalityStateRealtime,
						Connector: "memory-cache",
						TTL:       common.FixedDuration(10 * time.Second),
					},
				},
			},
		}
	}
	return cfg
}

// bniBoot spins up the server, pins upstream order to [rpc1(lagging),
// rpc2(healthy)] and waits until the pollers have learned the healthy head so
// the network tip is deterministic before any assertion runs.
func bniBoot(t *testing.T, cfg *common.Config) (
	func(body string, headers map[string]string, queryParams map[string]string) (int, map[string]string, string),
	*Network,
	func(),
) {
	t.Helper()
	sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)

	prj, err := erpcInstance.GetProject("test_project")
	require.NoError(t, err)
	policy.OverrideAllForTest(prj.policyEngine, "rpc1", "rpc2")

	ntw, err := prj.GetNetwork(context.Background(), util.EvmNetworkId(123))
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return ntw.EvmHighestLatestBlockNumber(context.Background()) == bniHealthyHead
	}, 5*time.Second, 50*time.Millisecond,
		"pollers must learn the healthy head (tip=0x1000) before the scenario starts")

	return sendRequest, ntw, shutdown
}

// bniSend performs one eth_blockNumber request and returns the decoded block
// number plus response headers.
func bniSend(
	t *testing.T,
	send func(body string, headers map[string]string, queryParams map[string]string) (int, map[string]string, string),
	headers map[string]string,
	queryParams map[string]string,
) (int64, map[string]string) {
	t.Helper()
	status, respHeaders, body := send(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`, headers, queryParams)
	require.Equal(t, http.StatusOK, status, "body: %s", body)
	var parsed map[string]interface{}
	require.NoError(t, sonic.UnmarshalString(body, &parsed), "body: %s", body)
	resStr, ok := parsed["result"].(string)
	require.True(t, ok, "expected hex string result, body: %s", body)
	n, err := strconv.ParseInt(strings.TrimPrefix(resStr, "0x"), 16, 64)
	require.NoError(t, err, "body: %s", body)
	return n, respHeaders
}

// bniProbeCache reads the eth_blockNumber cache entry (if any) through the
// network's real cache DAL — the same key the HTTP path reads/writes.
func bniProbeCache(ntw *Network) (int64, bool) {
	probe := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":99,"method":"eth_blockNumber","params":[]}`))
	probe.SetNetwork(ntw)
	resp, err := ntw.cacheDal.Get(context.Background(), probe)
	if err != nil || resp == nil || resp.IsObjectNull() {
		return 0, false
	}
	jrr, err := resp.JsonRpcResponse()
	if err != nil || jrr == nil {
		return 0, false
	}
	s := strings.Trim(strings.TrimSpace(jrr.GetResultString()), `"`)
	if s == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimPrefix(s, "0x"), 16, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// bniWaitForCachedValue polls the cache until an eth_blockNumber entry
// appears (the forward path writes it asynchronously) or the timeout lapses.
func bniWaitForCachedValue(ntw *Network, timeout time.Duration) (int64, bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if v, ok := bniProbeCache(ntw); ok {
			return v, true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return 0, false
}

// bniSeedCache plants an eth_blockNumber entry directly through the cache
// DAL, simulating a value written by another pod sharing the same cache (or
// by this pod moments earlier).
func bniSeedCache(t *testing.T, ntw *Network, hexValue string) {
	t.Helper()
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`))
	req.SetNetwork(ntw)
	jrr, err := common.NewJsonRpcResponse(1, hexValue, nil)
	require.NoError(t, err)
	// eth_blockNumber is a realtime method; the cache policy targets Realtime
	// finality. Synthetic responses (no upstream) would otherwise resolve to
	// Unfinalized (block is above the lowest-finalized head), so we pin the
	// finality explicitly to match the policy and allow the write to succeed.
	resp := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr).WithFinality(common.DataFinalityStateRealtime)
	require.NoError(t, ntw.cacheDal.Set(context.Background(), req, resp))
	// Ristretto Set is asynchronous; poll until the entry is visible.
	v, ok := bniWaitForCachedValue(ntw, 2*time.Second)
	require.True(t, ok, "seeded cache entry must be readable back")
	require.Equal(t, hexValue, fmt.Sprintf("0x%x", v))
}

func TestHttpServer_EthBlockNumberIntegrity(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	integrityOn := func() *common.EvmIntegrityConfig {
		return &common.EvmIntegrityConfig{EnforceHighestBlock: util.BoolPtr(true)}
	}
	integrityOff := func() *common.EvmIntegrityConfig {
		return &common.EvmIntegrityConfig{EnforceHighestBlock: util.BoolPtr(false)}
	}

	// ── Baselines: pin the behavior that already works ──────────────────────

	t.Run("Baseline_FreshStaleResponseIsCorrectedForClient", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		setupBniPollerMocks()
		setupBniBlockNumberMocks()

		send, _, shutdown := bniBoot(t, bniConfig(false, integrityOn()))
		defer shutdown()

		got, _ := bniSend(t, send, nil, nil)
		assert.Equal(t, bniHealthyHead, got,
			"a fresh (non-cached) stale response from the lagging upstream must be corrected to the network tip")
	})

	t.Run("Baseline_EnforcementExplicitlyDisabledServesRawValue", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		setupBniPollerMocks()
		setupBniBlockNumberMocks()

		send, _, shutdown := bniBoot(t, bniConfig(false, integrityOff()))
		defer shutdown()

		got, _ := bniSend(t, send, nil, nil)
		assert.Equal(t, bniLaggingHead, got,
			"with enforcement explicitly disabled the raw upstream value must pass through untouched")
	})

	// ── B1+B2: the customer-visible sawtooth ────────────────────────────────

	t.Run("Bug_CacheHitMustNotServeBlockBelowAlreadyServedTip", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		setupBniPollerMocks()
		setupBniBlockNumberMocks()

		send, ntw, shutdown := bniBoot(t, bniConfig(true, integrityOn()))
		defer shutdown()

		// Request #1: cache miss → lagging upstream answers 0x800 → the hook
		// corrects the client-bound response to the tip (0x1000)...
		first, _ := bniSend(t, send, nil, nil)
		require.Equal(t, bniHealthyHead, first,
			"precondition: fresh-path correction must work (otherwise this test is proving a different bug)")

		// ...meanwhile the forward path persisted SOMETHING for this method
		// (today: the stale 0x800 — bug B1). Wait for the async write so
		// request #2 deterministically exercises the cache-hit path.
		if v, ok := bniWaitForCachedValue(ntw, 2*time.Second); ok {
			t.Logf("cache now holds eth_blockNumber=0x%x", v)
		}

		// Request #2: the client already saw 0x1000; whatever source serves
		// this response (cache hit today — bug B2), the served block number
		// must never fall below the tip already served.
		second, hdrs := bniSend(t, send, nil, nil)
		t.Logf("second response: block=0x%x cacheHeader=%q", second, hdrs["X-Erpc-Cache"])
		assert.GreaterOrEqual(t, second, first,
			"sawtooth: served block number went BACKWARDS by %d blocks vs the tip already served to this client "+
				"(stale upstream value was cached by the forward path and the cache-hit response skipped enforcement)",
			first-second)
	})

	t.Run("Bug_StaleUpstreamValueMustNotPoisonCache", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		setupBniPollerMocks()
		setupBniBlockNumberMocks()

		send, ntw, shutdown := bniBoot(t, bniConfig(true, integrityOn()))
		defer shutdown()

		first, _ := bniSend(t, send, nil, nil)
		require.Equal(t, bniHealthyHead, first, "precondition: fresh-path correction must work")

		// The fix may either write the corrected value back (entry >= tip) or
		// skip caching the stale original (no entry). Both satisfy the
		// invariant; today the stale 0x800 lands in the cache (bug B1).
		v, ok := bniWaitForCachedValue(ntw, 2*time.Second)
		if !ok {
			t.Log("no eth_blockNumber cache entry written — acceptable (stale value skipped)")
			return
		}
		assert.GreaterOrEqual(t, v, bniHealthyHead,
			"cache poisoned: the forward path persisted the stale upstream value (0x%x) instead of the corrected value it served to the client (0x%x)",
			v, first)
	})

	// ── B2 isolated: a stale entry already in the (possibly shared) cache ───

	t.Run("Bug_StaleCacheEntryMustBeEnforcedOnRead", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		setupBniPollerMocks()
		setupBniBlockNumberMocks()

		send, ntw, shutdown := bniBoot(t, bniConfig(true, integrityOn()))
		defer shutdown()

		// Simulate a stale value present in the cache regardless of how it
		// got there (another pod, an earlier window, a race). Note the
		// realtime age guard cannot reject it: eth_blockNumber responses
		// carry no block timestamp and the memory connector is not
		// head-aware, so the guard fails open.
		bniSeedCache(t, ntw, "0x800")

		got, hdrs := bniSend(t, send, nil, nil)
		t.Logf("response: block=0x%x cacheHeader=%q", got, hdrs["X-Erpc-Cache"])
		assert.GreaterOrEqual(t, got, bniHealthyHead,
			"a cache-hit response below the known tip must be corrected just like a fresh response (FromCache responses currently skip enforcement entirely)")
	})

	// ── B3: use-upstream pinned requests must use the subset's tip ──────────

	t.Run("Bug_UseUpstreamPinnedRequestMustUseSelectorScopedTip", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		setupBniPollerMocks()
		setupBniBlockNumberMocks()

		send, _, shutdown := bniBoot(t, bniConfig(false, integrityOn()))
		defer shutdown()

		// The request is explicitly pinned to the lagging upstream. Per the
		// selector-scoped served-tip semantics (#912, honored by max-mode
		// tipCandidateUpstreams when the ctx carries the request), "latest"
		// must be decided AMONG the pinned subset — advertising the
		// network-wide tip promises a block rpc1 cannot serve.
		got, _ := bniSend(t, send, map[string]string{"X-ERPC-Use-Upstream": "rpc1"}, nil)
		assert.Equal(t, bniLaggingHead, got,
			"request pinned to the lagging upstream must be corrected against the pinned subset's tip (0x%x), not the network-wide tip (0x%x)",
			bniLaggingHead, bniHealthyHead)
	})

	// ── B4: directive gating parity with eth_getBlockByNumber ───────────────

	t.Run("Baseline_EnforcementEnabledByDefaultOnModernConfig", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		setupBniPollerMocks()
		setupBniBlockNumberMocks()

		// No integrity block in config: both the deprecated
		// evm.integrity.enforceHighestBlock (which the eth_blockNumber hook
		// reads) and DirectiveDefaults.EnforceHighestBlock (which
		// eth_getBlockByNumber reads) are auto-defaulted to true, so
		// enforcement must run out of the box.
		send, _, shutdown := bniBoot(t, bniConfig(false, nil))
		defer shutdown()

		got, _ := bniSend(t, send, nil, nil)
		assert.Equal(t, bniHealthyHead, got,
			"enforcement must be active by default (no explicit integrity config)")
	})

	t.Run("Bug_PerRequestDirectiveOverrideMustBeHonored", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		setupBniPollerMocks()
		setupBniBlockNumberMocks()

		send, _, shutdown := bniBoot(t, bniConfig(false, integrityOn()))
		defer shutdown()

		// eth_getBlockByNumber honors ?enforce-highest-block=false per
		// request (see HonorsEnforceHighestBlockFalseQueryOverride); the
		// eth_blockNumber hook must behave the same instead of reading only
		// the network config.
		got, _ := bniSend(t, send, nil, map[string]string{"enforce-highest-block": "false"})
		assert.Equal(t, bniLaggingHead, got,
			"per-request enforce-highest-block=false must disable correction for eth_blockNumber (it already does for eth_getBlockByNumber)")
	})
}
