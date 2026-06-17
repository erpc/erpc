package erpc

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/architecture/svm"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/thirdparty"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// svmNet wires up a minimal Network + registry for a single SVM mainnet-beta
// project. All upstreams passed in must already carry type:svm + svm.cluster.
func setupTestSvmNetwork(t *testing.T, ctx context.Context, upstreams []*common.UpstreamConfig) *Network {
	t.Helper()

	rateLimitersRegistry, _ := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{}, &log.Logger)
	metricsTracker := health.NewTracker(&log.Logger, "test", time.Minute)

	vr := thirdparty.NewVendorsRegistry()
	pr, err := thirdparty.NewProvidersRegistry(&log.Logger, vr, []*common.ProviderConfig{}, nil)
	require.NoError(t, err)

	sharedStateCfg := &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"},
		},
		LockMaxWait:     common.Duration(200 * time.Millisecond),
		UpdateMaxWait:   common.Duration(200 * time.Millisecond),
		FallbackTimeout: common.Duration(3 * time.Second),
		LockTtl:         common.Duration(4 * time.Second),
	}
	sharedStateCfg.SetDefaults("test")
	ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, sharedStateCfg)
	require.NoError(t, err)

	upstreamsRegistry := upstream.NewUpstreamsRegistry(
		ctx, &log.Logger, "test",
		upstreams, ssr, rateLimitersRegistry, vr, pr,
		nil, metricsTracker, nil,
	)

	networkConfig := &common.NetworkConfig{
		Architecture: common.ArchitectureSvm,
		Svm: &common.SvmNetworkConfig{
			Cluster:    "mainnet-beta",
			Commitment: "confirmed",
		},
	}
	network, err := NewNetwork(
		ctx, &log.Logger, "test", networkConfig,
		rateLimitersRegistry, upstreamsRegistry, metricsTracker, nil,
	)
	require.NoError(t, err)

	upstreamsRegistry.Bootstrap(ctx)
	time.Sleep(100 * time.Millisecond)
	require.NoError(t, upstreamsRegistry.PrepareUpstreamsForNetwork(ctx, util.SvmNetworkId("", "mainnet-beta")))
	// Allow state poller to run one tick.
	time.Sleep(150 * time.Millisecond)
	return network
}

func svmUpstreamConfig(id, host string) *common.UpstreamConfig {
	return &common.UpstreamConfig{
		Id:       id,
		Type:     common.UpstreamTypeSvm,
		Endpoint: "http://" + host,
		Svm:      &common.SvmUpstreamConfig{Cluster: "mainnet-beta"},
	}
}

// svmProjectForward reproduces the production project-layer path
// (PreparedProject.doForward): it runs HandleProjectPreForward — which is where
// commitment injection and the getGenesisHash short-circuit live — before
// delegating to network.Forward. Tests must use this instead of calling
// net.Forward directly; otherwise project-level pre-forward is skipped and the
// upstream never sees the injected commitment the mocks expect.
func svmProjectForward(ctx context.Context, net *Network, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	if h := net.architectureHandler; h != nil {
		if handled, resp, err := h.HandleProjectPreForward(ctx, net, req); handled {
			return resp, err
		}
	}
	return net.Forward(ctx, req)
}

// TestSvm_GetSlot_BasicProxy verifies that a getSlot request is forwarded to
// the upstream and the result is returned to the caller. This is the smoke
// test for the whole SVM pipeline (handler registration, client creation,
// architectureHandler dispatch).
func TestSvm_GetSlot_BasicProxy(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForSvmStatePoller("svm-basic-rpc1.localhost", 1000, 990)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Primary user-facing mock — getAccountInfo returning a canned account blob.
	gock.New("http://svm-basic-rpc1.localhost").
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			return r.URL.Host == "svm-basic-rpc1.localhost" &&
				strings.Contains(util.SafeReadBody(r), `"method":"getAccountInfo"`)
		}).
		Reply(200).
		BodyString(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":1000},"value":{"lamports":42}}}`)

	net := setupTestSvmNetwork(t, ctx, []*common.UpstreamConfig{svmUpstreamConfig("rpc1", "svm-basic-rpc1.localhost")})
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"getAccountInfo","params":["pubkey"]}`))
	resp, err := svmProjectForward(ctx, net, req)
	require.NoError(t, err)
	require.NotNil(t, resp)

	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	assert.Contains(t, string(jrr.GetResultBytes()), `"lamports":42`)
}

// TestSvm_GetGenesisHash_ShortCircuitForKnownCluster confirms the
// projectPreForward hook answers getGenesisHash locally — no upstream RPC
// call is made. A pending gock mock would leak if the upstream were hit.
func TestSvm_GetGenesisHash_ShortCircuitForKnownCluster(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForSvmStatePoller("svm-genesis-rpc1.localhost", 1000, 990)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	net := setupTestSvmNetwork(t, ctx, []*common.UpstreamConfig{svmUpstreamConfig("rpc1", "svm-genesis-rpc1.localhost")})

	// The SvmArchitectureHandler.HandleProjectPreForward is only invoked from the
	// project-level doForward path. Network-level Forward bypasses it. Invoke it
	// directly to verify the short-circuit behavior.
	h, err := common.GetArchitectureHandler(common.ArchitectureSvm)
	require.NoError(t, err)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"getGenesisHash","params":[]}`))
	handled, resp, err := h.HandleProjectPreForward(ctx, net, req)
	require.NoError(t, err)
	require.True(t, handled, "getGenesisHash should be handled locally")
	require.NotNil(t, resp)

	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	expected, _ := common.KnownGenesisHash("", "mainnet-beta")
	assert.Contains(t, string(jrr.GetResultBytes()), expected)
}

// TestSvm_Failover confirms network-scope retry routes past a failing upstream.
// Primary returns HTTP 500; secondary succeeds. The caller must see the
// secondary's result.
func TestSvm_Failover(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForSvmStatePoller("svm-failover-rpc1.localhost", 1000, 990)
	util.SetupMocksForSvmStatePoller("svm-failover-rpc2.localhost", 1000, 990)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Match user-facing getSlot calls by the injected commitment level. The
	// network config in setupTestSvmNetwork uses Commitment:"confirmed", so the
	// injection hook stamps that onto every outgoing getSlot from a user request.
	// State-poller getSlot calls use "processed" and "finalized", so this filter
	// isolates just the user path.
	gock.New("http://svm-failover-rpc1.localhost").
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			if r.URL.Host != "svm-failover-rpc1.localhost" {
				return false
			}
			body := util.SafeReadBody(r)
			return strings.Contains(body, `"method":"getSlot"`) &&
				strings.Contains(body, `"commitment":"confirmed"`)
		}).
		Reply(500).
		BodyString(`{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"Internal error"}}`)

	gock.New("http://svm-failover-rpc2.localhost").
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			if r.URL.Host != "svm-failover-rpc2.localhost" {
				return false
			}
			body := util.SafeReadBody(r)
			return strings.Contains(body, `"method":"getSlot"`) &&
				strings.Contains(body, `"commitment":"confirmed"`)
		}).
		Reply(200).
		BodyString(`{"jsonrpc":"2.0","id":1,"result":1234}`)

	net := setupTestSvmNetwork(t, ctx, []*common.UpstreamConfig{
		svmUpstreamConfig("rpc1", "svm-failover-rpc1.localhost"),
		svmUpstreamConfig("rpc2", "svm-failover-rpc2.localhost"),
	})
	// Pin the order — without this, score-based selection may pick rpc2 first and
	// the test's "primary fails, secondary succeeds" invariant becomes untestable.
	net.PinUpstreamOrderForTest("rpc1", "rpc2")

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"getSlot","params":[]}`))
	resp, err := svmProjectForward(ctx, net, req)
	require.NoError(t, err)
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	assert.Contains(t, string(jrr.GetResultBytes()), "1234")
}

// TestSvm_SendTransaction_NotRetriedAcrossUpstreams is the double-spend guard.
// Primary upstream returns an error for sendTransaction; the secondary must
// NOT be contacted (would result in duplicate broadcast if the first actually
// propagated).
func TestSvm_SendTransaction_NotRetriedAcrossUpstreams(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForSvmStatePoller("svm-sendtx-rpc1.localhost", 1000, 990)
	util.SetupMocksForSvmStatePoller("svm-sendtx-rpc2.localhost", 1000, 990)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Primary: sendTransaction errors out.
	gock.New("http://svm-sendtx-rpc1.localhost").
		Post("").
		Times(1).
		Filter(func(r *http.Request) bool {
			return r.URL.Host == "svm-sendtx-rpc1.localhost" &&
				strings.Contains(util.SafeReadBody(r), `"method":"sendTransaction"`)
		}).
		Reply(500).
		BodyString(`{"jsonrpc":"2.0","id":1,"error":{"code":-32005,"message":"Node behind"}}`)

	// Secondary: register a sendTransaction mock that would return a
	// different signature. If the retry guard is broken, the caller would
	// see this value; if the guard holds, this mock is never consumed.
	gock.New("http://svm-sendtx-rpc2.localhost").
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			return r.URL.Host == "svm-sendtx-rpc2.localhost" &&
				strings.Contains(util.SafeReadBody(r), `"method":"sendTransaction"`)
		}).
		Reply(200).
		BodyString(`{"jsonrpc":"2.0","id":1,"result":"DuplicateBroadcastSignature"}`)

	net := setupTestSvmNetwork(t, ctx, []*common.UpstreamConfig{
		svmUpstreamConfig("rpc1", "svm-sendtx-rpc1.localhost"),
		svmUpstreamConfig("rpc2", "svm-sendtx-rpc2.localhost"),
	})
	// Pin the order so rpc1 is always tried first. The double-spend guard assertion
	// only holds if we know the "primary" is actually the one receiving the error.
	net.PinUpstreamOrderForTest("rpc1", "rpc2")

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"sendTransaction","params":["base64tx"]}`))
	resp, err := svmProjectForward(ctx, net, req)
	// The guard must surface the primary error OR the primary's error payload,
	// never the secondary's successful response.
	if err != nil {
		assert.NotContains(t, err.Error(), "DuplicateBroadcastSignature",
			"secondary upstream was called despite double-spend guard")
	}
	if resp != nil {
		jrr, _ := resp.JsonRpcResponse()
		if jrr != nil {
			assert.NotContains(t, string(jrr.GetResultBytes()), "DuplicateBroadcastSignature",
				"secondary upstream result was returned to caller; retry guard failed")
		}
	}
}

// TestSvm_GenesisHashMismatch_FailsBootstrap verifies that an upstream
// configured for mainnet-beta but actually returning a different genesis
// hash is rejected during bootstrap. This guards against accidental
// cross-cluster mis-configuration where an RPC endpoint points at devnet
// but is listed under mainnet-beta.
func TestSvm_GenesisHashMismatch_FailsBootstrap(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	// getGenesisHash returns the devnet hash even though the config says mainnet-beta.
	// KnownGenesisHash("mainnet-beta") returns the Solana mainnet genesis; we return
	// devnet's to force a mismatch. Registered BEFORE SetupMocksForSvmStatePoller
	// (which also mocks getGenesisHash, with the mainnet hash) so gock's
	// registration-order match picks this devnet override.
	devnetHash, _ := common.KnownGenesisHash("", "devnet")
	gock.New("http://svm-mismatch-rpc1.localhost").
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			return r.URL.Host == "svm-mismatch-rpc1.localhost" &&
				strings.Contains(util.SafeReadBody(r), `"method":"getGenesisHash"`)
		}).
		Reply(200).
		BodyString(`{"jsonrpc":"2.0","id":1,"result":"` + devnetHash + `"}`)

	util.SetupMocksForSvmStatePoller("svm-mismatch-rpc1.localhost", 1000, 990)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Build registry directly so we can observe the bootstrap error — setupTestSvmNetwork
	// would require.NoError on PrepareUpstreamsForNetwork and fail the test prematurely.
	rateLimiters, _ := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{}, &log.Logger)
	mt := health.NewTracker(&log.Logger, "test", time.Minute)
	vr := thirdparty.NewVendorsRegistry()
	pr, _ := thirdparty.NewProvidersRegistry(&log.Logger, vr, []*common.ProviderConfig{}, nil)
	sharedStateCfg := &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"},
		},
		LockMaxWait: common.Duration(200 * time.Millisecond), UpdateMaxWait: common.Duration(200 * time.Millisecond),
		FallbackTimeout: common.Duration(3 * time.Second), LockTtl: common.Duration(4 * time.Second),
	}
	sharedStateCfg.SetDefaults("test")
	ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, sharedStateCfg)
	require.NoError(t, err)

	upCfg := &common.UpstreamConfig{
		Id:       "rpc1",
		Type:     common.UpstreamTypeSvm,
		Endpoint: "http://svm-mismatch-rpc1.localhost",
		Svm:      &common.SvmUpstreamConfig{Cluster: "mainnet-beta"},
	}
	reg := upstream.NewUpstreamsRegistry(
		ctx, &log.Logger, "test",
		[]*common.UpstreamConfig{upCfg}, ssr, rateLimiters, vr, pr,
		nil, mt, nil,
	)
	reg.Bootstrap(ctx)
	// Give bootstrap a moment to run detectFeatures + state poller + genesis check.
	time.Sleep(300 * time.Millisecond)

	// Short-deadline context: Prepare waits for at least one ready upstream; with
	// bootstrap failing there will never be one, so we want the wait to time out
	// quickly. The assertion is on the initializer status, not the Prepare error.
	shortCtx, shortCancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer shortCancel()
	_ = reg.PrepareUpstreamsForNetwork(shortCtx, util.SvmNetworkId("", "mainnet-beta"))

	// The upstream must not be registered for this network because its bootstrap
	// task failed with a genesis-hash mismatch.
	status := reg.GetInitializer().Status()
	found := false
	for _, task := range status.Tasks {
		if strings.Contains(task.Name, "rpc1") && task.Err != nil {
			if strings.Contains(task.Err.Error(), "genesis hash mismatch") {
				found = true
				break
			}
		}
	}
	assert.True(t, found, "expected a bootstrap task to fail with 'genesis hash mismatch', got tasks: %+v", status.Tasks)
}

// TestSvm_CacheHit_ServedFromCacheOnSecondRequest proves the cache DAL is
// reachable through Network.Forward: the first request hits the upstream and
// writes to the cache; the second identical request is served from cache
// without contacting the upstream. If retry or handler plumbing regresses and
// the cache is bypassed, the second request would re-hit the (exhausted)
// gock mock and either fail or fall through to a real HTTP request.
func TestSvm_CacheHit_ServedFromCacheOnSecondRequest(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForSvmStatePoller("svm-cache-rpc1.localhost", 1000, 990)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Upstream mock responds exactly ONCE. A second call would leave the mock
	// exhausted — gock would either 404 or pass through to the real network.
	// Either behavior would differ from a successful cache hit, so the assertion
	// is simply that both calls return the same cached payload.
	gock.New("http://svm-cache-rpc1.localhost").
		Post("").
		Times(1).
		Filter(func(r *http.Request) bool {
			return r.URL.Host == "svm-cache-rpc1.localhost" &&
				strings.Contains(util.SafeReadBody(r), `"method":"getBlock"`)
		}).
		Reply(200).
		BodyString(`{"jsonrpc":"2.0","id":1,"result":{"blockhash":"cached-block-42","parentSlot":41}}`)

	net := setupTestSvmNetwork(t, ctx, []*common.UpstreamConfig{
		svmUpstreamConfig("rpc1", "svm-cache-rpc1.localhost"),
	})

	// Build a minimal SVM cache backed by memory and attach it to the network.
	// In production networks_registry.prepareNetwork does this wiring; the test
	// helper setupTestSvmNetwork doesn't, so we plug it in directly.
	cache, err := svm.NewSvmJsonRpcCache(ctx, &log.Logger, &common.CacheConfig{
		Connectors: []*common.ConnectorConfig{
			{Id: "mem", Driver: common.DriverMemory,
				Memory: &common.MemoryConnectorConfig{MaxItems: 1000, MaxTotalSize: "1MB"}},
		},
		Policies: []*common.CachePolicyConfig{
			{Connector: "mem", Network: "svm:*", Method: "*",
				Finality: common.DataFinalityStateFinalized},
		},
	})
	require.NoError(t, err)
	net.cacheDal = cache

	// First request — upstream handles it, result lands in cache.
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"getBlock","params":[42,{"commitment":"finalized"}]}`)
	resp1, err := svmProjectForward(ctx, net, common.NewNormalizedRequest(body))
	require.NoError(t, err)
	require.NotNil(t, resp1)
	jrr1, err := resp1.JsonRpcResponse()
	require.NoError(t, err)
	assert.Contains(t, string(jrr1.GetResultBytes()), "cached-block-42")

	// Let Ristretto flush the admission buffer so the Set is visible.
	time.Sleep(50 * time.Millisecond)

	// Second request with identical body — served from cache. The gock mock was
	// Times(1), so if we re-hit the upstream we'd get a different outcome.
	resp2, err := svmProjectForward(ctx, net, common.NewNormalizedRequest(body))
	require.NoError(t, err)
	require.NotNil(t, resp2)
	jrr2, err := resp2.JsonRpcResponse()
	require.NoError(t, err)
	assert.Contains(t, string(jrr2.GetResultBytes()), "cached-block-42",
		"second identical request must be served from cache")
}

// TestSvm_MixedProjectWithEvm verifies that a single PreparedProject can route
// traffic for both an EVM network and an SVM network without the two paths
// contaminating each other. Regression guard for the ArchitectureHandler
// dispatch: if a later refactor accidentally routes SVM through EVM hooks (or
// the reverse), this test fails immediately because the mocks are strictly
// partitioned by Host and by method namespace.
func TestSvm_MixedProjectWithEvm(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	// Boot both architectures' state pollers against isolated hosts.
	util.SetupMocksForEvmStatePoller() // EVM poller uses rpc1.localhost
	util.SetupMocksForSvmStatePoller("svm-mixed-rpc1.localhost", 1000, 990)

	// EVM user-facing mock: eth_call against rpc1.localhost. The host guard is
	// critical — gock evaluates every mock's filter against every request, so
	// without the check this filter would trip on SVM requests routed to a
	// different host.
	gock.New("http://rpc1.localhost").
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			if r.URL.Host != "rpc1.localhost" {
				return false
			}
			body := util.SafeReadBody(r)
			if strings.Contains(body, `"method":"getAccountInfo"`) ||
				strings.Contains(body, `"method":"getSlot"`) {
				t.Errorf("EVM upstream received SVM-shaped body: %s", body)
				return false
			}
			return strings.Contains(body, `"method":"eth_call"`)
		}).
		Reply(200).
		BodyString(`{"jsonrpc":"2.0","id":1,"result":"0xevmresult"}`)

	// SVM user-facing mock: getAccountInfo against svm-mixed-rpc1.localhost.
	gock.New("http://svm-mixed-rpc1.localhost").
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			if r.URL.Host != "svm-mixed-rpc1.localhost" {
				return false
			}
			body := util.SafeReadBody(r)
			if strings.Contains(body, `"method":"eth_`) {
				t.Errorf("SVM upstream received EVM-shaped body: %s", body)
				return false
			}
			return strings.Contains(body, `"method":"getAccountInfo"`)
		}).
		Reply(200).
		BodyString(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":1000},"value":{"lamports":42}}}`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rateLimiters, _ := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{}, &log.Logger)
	ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"},
		},
	})
	require.NoError(t, err)

	prjCfg := &common.ProjectConfig{
		Id: "mixed",
		Networks: []*common.NetworkConfig{
			{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}},
			{
				Architecture: common.ArchitectureSvm,
				Svm:          &common.SvmNetworkConfig{Cluster: "mainnet-beta", Commitment: "confirmed"},
			},
		},
		Upstreams: []*common.UpstreamConfig{
			{
				Id: "evm-rpc", Type: common.UpstreamTypeEvm,
				Endpoint: "http://rpc1.localhost",
				Evm:      &common.EvmUpstreamConfig{ChainId: 123},
			},
			{
				Id: "svm-rpc", Type: common.UpstreamTypeSvm,
				Endpoint: "http://svm-mixed-rpc1.localhost",
				Svm:      &common.SvmUpstreamConfig{Cluster: "mainnet-beta"},
			},
		},
	}
	require.NoError(t, prjCfg.SetDefaults(nil))

	reg, err := NewProjectsRegistry(
		ctx, &log.Logger,
		[]*common.ProjectConfig{prjCfg},
		ssr, nil, nil, rateLimiters,
		thirdparty.NewVendorsRegistry(),
		nil, nil,
	)
	require.NoError(t, err)
	reg.Bootstrap(ctx)
	time.Sleep(300 * time.Millisecond)

	prj, err := reg.GetProject("mixed")
	require.NoError(t, err)

	// Fire an EVM request. Must route only to the EVM upstream.
	evmResp, err := prj.Forward(ctx, "evm:123",
		common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x1","data":"0x"}, "latest"]}`)))
	require.NoError(t, err)
	require.NotNil(t, evmResp)
	evmJrr, err := evmResp.JsonRpcResponse()
	require.NoError(t, err)
	assert.Contains(t, string(evmJrr.GetResultBytes()), "0xevmresult")

	// Fire an SVM request. Must route only to the SVM upstream.
	svmResp, err := prj.Forward(ctx, "svm:mainnet-beta",
		common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"getAccountInfo","params":["pubkey"]}`)))
	require.NoError(t, err)
	require.NotNil(t, svmResp)
	svmJrr, err := svmResp.JsonRpcResponse()
	require.NoError(t, err)
	assert.Contains(t, string(svmJrr.GetResultBytes()), `"lamports":42`)
}

// TestSvm_Consensus_SlotLagFilterExcludesStaleUpstream is the end-to-end
// regression guard for the slot-lag filter wired into networks.go. It proves
// that when a consensus policy is active and the request resolves to Finalized
// finality, an upstream whose FinalizedSlot trails the pool by more than
// MaxFinalizedSlotLag is pruned out of the candidate pool before the consensus
// loop runs — the stale upstream's mock is never consumed.
//
// If the wiring in networks.go regresses (e.g. a refactor moves the filter
// callsite past SetUpstreams, or flips the finalized-predicate), this test
// fails because the stale upstream receives the user request and its filter
// asserts t.Errorf.
func TestSvm_Consensus_SlotLagFilterExcludesStaleUpstream(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	const (
		freshHostA = "svm-cons-fresh-a.localhost"
		freshHostB = "svm-cons-fresh-b.localhost"
		staleHost  = "svm-cons-stale.localhost"
	)
	// State pollers — return identical current/finalized slots for all. We
	// override the stale upstream's finalized slot in-memory AFTER bootstrap,
	// so the poller's background polling doesn't race with our assignment.
	util.SetupMocksForSvmStatePoller(freshHostA, 1000, 990)
	util.SetupMocksForSvmStatePoller(freshHostB, 1000, 990)
	util.SetupMocksForSvmStatePoller(staleHost, 1000, 990)

	// Both fresh upstreams answer getBlock identically — that's the consensus
	// payload. The stale upstream's mock asserts if contacted.
	const consensusResult = `"consensus-block-abc"`
	for _, host := range []string{freshHostA, freshHostB} {
		gock.New("http://" + host).
			Post("").
			Persist().
			Filter(func(r *http.Request) bool {
				if r.URL.Host != host {
					return false
				}
				body := util.SafeReadBody(r)
				return strings.Contains(body, `"method":"getBlock"`)
			}).
			Reply(200).
			BodyString(`{"jsonrpc":"2.0","id":1,"result":` + consensusResult + `}`)
	}
	// Stale upstream's getBlock mock. A healthy filter path never reaches this;
	// if it does, t.Errorf trips. Persist so it remains armed across retries.
	gock.New("http://" + staleHost).
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			if r.URL.Host != staleHost {
				return false
			}
			body := util.SafeReadBody(r)
			if strings.Contains(body, `"method":"getBlock"`) {
				t.Errorf("stale upstream contacted for consensus-eligible getBlock — "+
					"slot-lag filter regressed. Body: %s", body)
				return true // return a result so the caller doesn't hang on no-match
			}
			return false
		}).
		Reply(200).
		BodyString(`{"jsonrpc":"2.0","id":1,"result":"stale-should-not-appear"}`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Build the registry by hand so we can attach a consensus policy on the
	// network and retrieve upstreams to seed their finalized-slot values.
	rateLimiters, _ := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{}, &log.Logger)
	mt := health.NewTracker(&log.Logger, "test", time.Minute)
	vr := thirdparty.NewVendorsRegistry()
	pr, err := thirdparty.NewProvidersRegistry(&log.Logger, vr, []*common.ProviderConfig{}, nil)
	require.NoError(t, err)

	ssCfg := &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"},
		},
		LockMaxWait: common.Duration(200 * time.Millisecond), UpdateMaxWait: common.Duration(200 * time.Millisecond),
		FallbackTimeout: common.Duration(3 * time.Second), LockTtl: common.Duration(4 * time.Second),
	}
	ssCfg.SetDefaults("test")
	ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, ssCfg)
	require.NoError(t, err)

	upCfgs := []*common.UpstreamConfig{
		svmUpstreamConfig("fresh-a", freshHostA),
		svmUpstreamConfig("fresh-b", freshHostB),
		svmUpstreamConfig("stale", staleHost),
	}
	upsReg := upstream.NewUpstreamsRegistry(
		ctx, &log.Logger, "test", upCfgs, ssr, rateLimiters, vr, pr,
		nil, mt, nil,
	)

	// Consensus policy: require 2-of-3 agreement. Scoped to Finalized finality so
	// only our target method path activates it.
	netCfg := &common.NetworkConfig{
		Architecture: common.ArchitectureSvm,
		Svm: &common.SvmNetworkConfig{
			Cluster:             "mainnet-beta",
			Commitment:          "finalized",
			MaxFinalizedSlotLag: 100,
		},
		Failsafe: []*common.FailsafeConfig{{
			MatchMethod:   "*",
			MatchFinality: []common.DataFinalityState{common.DataFinalityStateFinalized},
			Consensus: &common.ConsensusPolicyConfig{
				MaxParticipants:    3,
				AgreementThreshold: 2,
			},
		}},
	}
	// SetDefaults populates the required fields (SvmNetworkConfig defaults,
	// failsafe consensus defaults); without it the consensus policy config is
	// missing required timeouts.
	require.NoError(t, netCfg.SetDefaults(upCfgs, nil))

	net, err := NewNetwork(ctx, &log.Logger, "test", netCfg, rateLimiters, upsReg, mt, nil)
	require.NoError(t, err)

	upsReg.Bootstrap(ctx)
	time.Sleep(100 * time.Millisecond)
	require.NoError(t, upsReg.PrepareUpstreamsForNetwork(ctx, util.SvmNetworkId("", "mainnet-beta")))
	time.Sleep(300 * time.Millisecond) // let pollers run at least one tick

	// Seed per-upstream finalized slots. The counter only moves forward
	// (CounterInt64SharedVariable uses rollback protection), so we advance the
	// fresh upstreams to a much higher slot instead of trying to pull the stale
	// one backwards.
	//
	//   fresh-a, fresh-b: 10_000   (manual advance via SuggestFinalizedSlot)
	//   stale:            ~990     (poller-published, unchanged)
	//
	// HighestFinalizedSlot over the pool → 10000.
	// Lag: fresh=0, stale = 10000 - 990 = 9010 → far beyond MaxFinalizedSlotLag=100.
	// The filter must exclude `stale` from the consensus pool.
	for _, u := range upsReg.GetNetworkUpstreams(ctx, util.SvmNetworkId("", "mainnet-beta")) {
		sp := u.SvmStatePoller()
		if sp == nil || sp.IsObjectNull() {
			continue
		}
		if u.Id() == "fresh-a" || u.Id() == "fresh-b" {
			sp.SuggestFinalizedSlot(10_000)
		}
	}
	// Give the shared-state counter a moment to persist the writes.
	time.Sleep(50 * time.Millisecond)

	// Fire a finalized-commitment getBlock.
	req := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"getBlock","params":[100,{"commitment":"finalized"}]}`))
	resp, err := svmProjectForward(ctx, net, req)
	require.NoError(t, err, "consensus forward must succeed when all upstreams are within slot-lag bound")
	require.NotNil(t, resp)

	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	assert.Contains(t, string(jrr.GetResultBytes()), "consensus-block-abc",
		"consensus payload must match what the fresh upstreams returned")
}
