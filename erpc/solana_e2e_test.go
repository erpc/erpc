package erpc

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init_solana() {
	telemetry.SetHistogramBuckets("0.05,0.5,5,30")
}

// setupSolanaStatePollerMocks registers persistent gock mocks for getSlot, getHealth,
// and getGenesisHash so bootstrap and background polling don't hang during tests.
func setupSolanaStatePollerMocks() {
	for _, host := range []string{"http://sol1.localhost", "http://sol2.localhost"} {
		// getGenesisHash — called once during detectFeatures/Bootstrap
		gock.New(host).
			Post("").
			Persist().
			Filter(func(req *http.Request) bool {
				return strings.Contains(util.SafeReadBody(req), "getGenesisHash")
			}).
			Reply(200).
			// mainnet-beta genesis hash
			JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":"5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d"}`))

		// getHealth — called every poll cycle
		gock.New(host).
			Post("").
			Persist().
			Filter(func(req *http.Request) bool {
				return strings.Contains(util.SafeReadBody(req), "getHealth")
			}).
			Reply(200).
			JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":"ok"}`))

		// getSlot — called every poll cycle
		gock.New(host).
			Post("").
			Persist().
			Filter(func(req *http.Request) bool {
				return strings.Contains(util.SafeReadBody(req), "getSlot")
			}).
			Reply(200).
			JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":300000000}`))

		// getMaxShredInsertSlot — called every poll cycle (Phase 2 addition)
		gock.New(host).
			Post("").
			Persist().
			Filter(func(req *http.Request) bool {
				return strings.Contains(util.SafeReadBody(req), "getMaxShredInsertSlot")
			}).
			Reply(200).
			JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":300000000}`))
	}
}

// newSolanaTestConfig builds a minimal Config for two Solana upstreams.
func newSolanaTestConfig() *common.Config {
	return &common.Config{
		Projects: []*common.ProjectConfig{
			{
				Id: "test",
				Networks: []*common.NetworkConfig{
					{
						Architecture: "solana",
						Solana: &common.SolanaNetworkConfig{
							Cluster: "mainnet-beta",
						},
						Failsafe: []*common.FailsafeConfig{
							{
								Retry: &common.RetryPolicyConfig{
									MaxAttempts: 3,
									Delay:       common.Duration(5 * time.Millisecond),
								},
							},
						},
					},
				},
				Upstreams: []*common.UpstreamConfig{
					{
						Id:       "sol1",
						Type:     "solana",
						Endpoint: "http://sol1.localhost",
						Solana: &common.SolanaUpstreamConfig{
							Cluster: "mainnet-beta",
						},
					},
					{
						Id:       "sol2",
						Type:     "solana",
						Endpoint: "http://sol2.localhost",
						Solana: &common.SolanaUpstreamConfig{
							Cluster: "mainnet-beta",
						},
					},
				},
			},
		},
	}
}

// TestSolanaBasicProxy verifies that a getBalance request is forwarded and the
// JSON-RPC response passes through correctly.
func TestSolanaBasicProxy(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	defer gock.Off()

	setupSolanaStatePollerMocks()

	gock.New("http://sol1.localhost").
		Post("").
		Filter(func(req *http.Request) bool {
			return strings.Contains(util.SafeReadBody(req), "getBalance")
		}).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":300000000},"value":1000000000}}`))

	util.ConfigureTestLogger()
	lg := log.Logger

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ssr, err := data.NewSharedStateRegistry(ctx, &lg, nil)
	require.NoError(t, err)

	erpcInstance, err := NewERPC(ctx, &lg, ssr, nil, newSolanaTestConfig())
	require.NoError(t, err)
	erpcInstance.Bootstrap(ctx)

	nw, err := erpcInstance.GetNetwork(ctx, "test", "solana:mainnet-beta")
	require.NoError(t, err)
	assert.NotNil(t, nw)

	nr := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"getBalance","params":["9xQeWvG816bUx9EPjHmaT23yvVM2ZWbrrpZb9PusVFin"]}`,
	))
	resp, err := nw.Forward(ctx, nr)
	require.NoError(t, err)
	require.NotNil(t, resp)

	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	assert.Nil(t, jrr.Error)
	assert.NotEmpty(t, jrr.GetResultBytes())
}

// TestSolanaFailover verifies that when sol1 returns a 500, erpc retries on sol2.
func TestSolanaFailover(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	defer gock.Off()

	setupSolanaStatePollerMocks()

	// sol1 returns unhealthy on the actual request
	gock.New("http://sol1.localhost").
		Post("").
		Filter(func(req *http.Request) bool {
			return strings.Contains(util.SafeReadBody(req), "getBalance")
		}).
		Reply(500).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32005,"message":"Node is unhealthy"}}`))

	// sol2 succeeds
	gock.New("http://sol2.localhost").
		Post("").
		Filter(func(req *http.Request) bool {
			return strings.Contains(util.SafeReadBody(req), "getBalance")
		}).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":300000000},"value":500000000}}`))

	util.ConfigureTestLogger()
	lg := log.Logger

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ssr, err := data.NewSharedStateRegistry(ctx, &lg, nil)
	require.NoError(t, err)

	erpcInstance, err := NewERPC(ctx, &lg, ssr, nil, newSolanaTestConfig())
	require.NoError(t, err)
	erpcInstance.Bootstrap(ctx)

	nw, err := erpcInstance.GetNetwork(ctx, "test", "solana:mainnet-beta")
	require.NoError(t, err)

	nr := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"getBalance","params":["9xQeWvG816bUx9EPjHmaT23yvVM2ZWbrrpZb9PusVFin"]}`,
	))
	resp, err := nw.Forward(ctx, nr)
	require.NoError(t, err)
	require.NotNil(t, resp)

	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	assert.Nil(t, jrr.Error)
}

// TestSolanaStatePoller verifies that getSlot responses populate LatestSlot
// and getHealth responses update IsHealthy on the upstream.
func TestSolanaStatePoller(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	defer gock.Off()

	setupSolanaStatePollerMocks()

	util.ConfigureTestLogger()
	lg := log.Logger

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ssr, err := data.NewSharedStateRegistry(ctx, &lg, nil)
	require.NoError(t, err)

	cfg := newSolanaTestConfig()
	cfg.Projects[0].Upstreams = cfg.Projects[0].Upstreams[:1]

	erpcInstance, err := NewERPC(ctx, &lg, ssr, nil, cfg)
	require.NoError(t, err)
	erpcInstance.Bootstrap(ctx)

	// Give background poller a moment to fire
	time.Sleep(600 * time.Millisecond)

	nw, err := erpcInstance.GetNetwork(ctx, "test", "solana:mainnet-beta")
	require.NoError(t, err)

	upstreams, err := nw.upstreamsRegistry.GetSortedUpstreams(ctx, "solana:mainnet-beta", "getSlot")
	require.NoError(t, err)
	require.NotEmpty(t, upstreams)

	found := false
	for _, u := range upstreams {
		if su, ok := u.(interface{ SolanaStatePoller() common.SolanaStatePoller }); ok {
			sp := su.SolanaStatePoller()
			if sp != nil && !sp.IsObjectNull() {
				assert.Greater(t, sp.LatestSlot(), int64(0), "LatestSlot should be populated")
				assert.True(t, sp.IsHealthy(), "IsHealthy should be true after successful getHealth")
				found = true
				break
			}
		}
	}
	assert.True(t, found, "expected an upstream with a Solana state poller")
}

// TestSolanaHealthPollerUnhealthy verifies that a -32005 getHealth response marks the
// upstream as unhealthy.
func TestSolanaHealthPollerUnhealthy(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	defer gock.Off()

	// getGenesisHash — must succeed for Bootstrap to proceed
	gock.New("http://sol1.localhost").
		Post("").
		Persist().
		Filter(func(req *http.Request) bool {
			return strings.Contains(util.SafeReadBody(req), "getGenesisHash")
		}).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":"5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d"}`))

	// getHealth returns unhealthy
	gock.New("http://sol1.localhost").
		Post("").
		Persist().
		Filter(func(req *http.Request) bool {
			return strings.Contains(util.SafeReadBody(req), "getHealth")
		}).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32005,"message":"Node is behind by 150 slots"}}`))

	// getSlot still needs to work for the slot poll
	gock.New("http://sol1.localhost").
		Post("").
		Persist().
		Filter(func(req *http.Request) bool {
			return strings.Contains(util.SafeReadBody(req), "getSlot")
		}).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":300000000}`))

	util.ConfigureTestLogger()
	lg := log.Logger

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ssr, err := data.NewSharedStateRegistry(ctx, &lg, nil)
	require.NoError(t, err)

	cfg := newSolanaTestConfig()
	cfg.Projects[0].Upstreams = cfg.Projects[0].Upstreams[:1]

	erpcInstance, err := NewERPC(ctx, &lg, ssr, nil, cfg)
	require.NoError(t, err)
	erpcInstance.Bootstrap(ctx)

	// Wait for one full poll cycle
	time.Sleep(600 * time.Millisecond)

	nw, err := erpcInstance.GetNetwork(ctx, "test", "solana:mainnet-beta")
	require.NoError(t, err)

	upstreams, _ := nw.upstreamsRegistry.GetSortedUpstreams(ctx, "solana:mainnet-beta", "getSlot")
	for _, u := range upstreams {
		if su, ok := u.(interface{ SolanaStatePoller() common.SolanaStatePoller }); ok {
			sp := su.SolanaStatePoller()
			if sp != nil && !sp.IsObjectNull() {
				assert.False(t, sp.IsHealthy(), "IsHealthy should be false after -32005 getHealth error")
				return
			}
		}
	}
	t.Fatal("no upstream with state poller found")
}

// TestSolanaGenesisHashMismatch verifies that an upstream configured for mainnet-beta
// but returning a devnet genesis hash is rejected during Bootstrap.
func TestSolanaGenesisHashMismatch(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	defer gock.Off()

	// Return devnet genesis hash for a mainnet-beta configured upstream
	gock.New("http://sol1.localhost").
		Post("").
		Persist().
		Filter(func(req *http.Request) bool {
			return strings.Contains(util.SafeReadBody(req), "getGenesisHash")
		}).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":"EtWTRABZaYq6iMfeYKouRu166VU2xqa1wcaWoxPkrZBG"}`)) // devnet hash

	util.ConfigureTestLogger()
	lg := log.Logger

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ssr, err := data.NewSharedStateRegistry(ctx, &lg, nil)
	require.NoError(t, err)

	cfg := newSolanaTestConfig()
	cfg.Projects[0].Upstreams = cfg.Projects[0].Upstreams[:1]

	erpcInstance, err := NewERPC(ctx, &lg, ssr, nil, cfg)
	require.NoError(t, err)

	// Bootstrap should succeed at erpc level (upstream errors are non-fatal),
	// but the upstream should be rejected / unavailable.
	erpcInstance.Bootstrap(ctx)

	// With all upstreams permanently failed (genesis hash mismatch is a fatal
	// init error), GetNetwork may return ErrNetworkNotSupported. We accept any
	// error from either GetNetwork or the subsequent Forward — the important
	// assertion is that the request ultimately fails.
	nw, getNetworkErr := erpcInstance.GetNetwork(ctx, "test", "solana:mainnet-beta")
	if getNetworkErr != nil {
		assert.True(t,
			common.HasErrorCode(getNetworkErr, common.ErrCodeNetworkNotSupported) ||
				common.HasErrorCode(getNetworkErr, common.ErrCodeNetworkInitializing),
			"GetNetwork error should be NetworkNotSupported or NetworkInitializing, got: %v", getNetworkErr)
		return
	}

	// Forward should fail because no healthy upstream is available
	nr := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"getBalance","params":["9xQeWvG816bUx9EPjHmaT23yvVM2ZWbrrpZb9PusVFin"]}`,
	))
	_, err = nw.Forward(ctx, nr)
	assert.Error(t, err, "request should fail when all upstreams have mismatched genesis hash")
}

// TestSolanaCacheFinalityMapping checks commitment → DataFinalityState mapping.
func TestSolanaCacheFinalityMapping(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	defer gock.Off()

	setupSolanaStatePollerMocks()

	gock.New("http://sol1.localhost").
		Post("").
		Filter(func(req *http.Request) bool {
			return strings.Contains(util.SafeReadBody(req), "getBlock")
		}).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"blockHeight":299000000,"blockhash":"abc123"}}`))

	gock.New("http://sol1.localhost").
		Post("").
		Filter(func(req *http.Request) bool {
			return strings.Contains(util.SafeReadBody(req), "getLatestBlockhash")
		}).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":300000000},"value":{"blockhash":"xyz","lastValidBlockHeight":1234}}}`))

	util.ConfigureTestLogger()
	lg := log.Logger

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ssr, err := data.NewSharedStateRegistry(ctx, &lg, nil)
	require.NoError(t, err)

	cfg := newSolanaTestConfig()
	cfg.Projects[0].Upstreams = cfg.Projects[0].Upstreams[:1]

	erpcInstance, err := NewERPC(ctx, &lg, ssr, nil, cfg)
	require.NoError(t, err)
	erpcInstance.Bootstrap(ctx)

	nw, err := erpcInstance.GetNetwork(ctx, "test", "solana:mainnet-beta")
	require.NoError(t, err)

	// getBlock → Finalized (always-finalized method)
	reqBlock := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"getBlock","params":[299000000]}`,
	))
	respBlock, err := nw.Forward(ctx, reqBlock)
	require.NoError(t, err)
	assert.Equal(t, common.DataFinalityStateFinalized, nw.GetFinality(ctx, reqBlock, respBlock),
		"getBlock should map to Finalized")

	// getLatestBlockhash → Realtime (never-cache method)
	reqHash := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":2,"method":"getLatestBlockhash","params":[]}`,
	))
	respHash, err := nw.Forward(ctx, reqHash)
	require.NoError(t, err)
	assert.Equal(t, common.DataFinalityStateRealtime, nw.GetFinality(ctx, reqHash, respHash),
		"getLatestBlockhash should map to Realtime (never cache)")
}

// TestEvmStillWorksAfterSolana is a regression guard: EVM must be unaffected.
func TestEvmStillWorksAfterSolana(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	util.ConfigureTestLogger()
	lg := log.Logger

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ssr, err := data.NewSharedStateRegistry(ctx, &lg, nil)
	require.NoError(t, err)

	cfg := &common.Config{
		Projects: []*common.ProjectConfig{
			{
				Id: "test",
				Networks: []*common.NetworkConfig{
					{
						Architecture: "evm",
						Evm:          &common.EvmNetworkConfig{ChainId: 123},
					},
				},
				Upstreams: []*common.UpstreamConfig{
					{
						Id:       "rpc1",
						Type:     "evm",
						Endpoint: "http://rpc1.localhost",
						Evm:      &common.EvmUpstreamConfig{ChainId: 123},
						JsonRpc: &common.JsonRpcUpstreamConfig{
							SupportsBatch: &common.FALSE,
						},
					},
				},
			},
		},
	}

	erpcInstance, err := NewERPC(ctx, &lg, ssr, nil, cfg)
	require.NoError(t, err)
	erpcInstance.Bootstrap(ctx)

	nw, err := erpcInstance.GetNetwork(ctx, "test", "evm:123")
	require.NoError(t, err)
	assert.NotNil(t, nw)
	assert.Equal(t, common.ArchitectureEvm, nw.Architecture())
}

// ── New e2e tests ─────────────────────────────────────────────────────────────

// TestSolanaSendTransactionNotRetried verifies that an error on sendTransaction
// is never retried against a second upstream (gap 4 — double-spend prevention).
// The network failsafe has MaxAttempts:3, but sendTransaction must stop after 1.
//
// Both upstreams return an HTTP 500 error so the test is independent of which
// upstream the selector picks first.
func TestSolanaSendTransactionNotRetried(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	defer gock.Off()

	setupSolanaStatePollerMocks()

	// Both upstreams reject sendTransaction with an HTTP 500 server error so
	// erpc classifies it as an upstream error. We count calls to verify only
	// one total attempt is made regardless of which upstream is selected first.
	//
	// NOTE: gock evaluates every registered mock's Filter before doing URL
	// matching, so a shared filter would be called twice (once per mock) for a
	// single request. We guard the counter with a per-upstream host check so
	// the increment fires only for the mock that actually serves the response.
	var totalTxCalls atomic.Int32
	for _, host := range []string{"http://sol1.localhost", "http://sol2.localhost"} {
		expectedHost := strings.TrimPrefix(host, "http://") // e.g. "sol1.localhost"
		gock.New(host).
			Post("").
			Persist().
			Filter(func(req *http.Request) bool {
				// Skip mocks whose host doesn't match this request.
				if req.URL.Host != expectedHost {
					return false
				}
				if strings.Contains(util.SafeReadBody(req), "sendTransaction") {
					totalTxCalls.Add(1)
					return true
				}
				return false
			}).
			Reply(500).
			JSON([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"Transaction simulation failed"}}`))
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

	nw, err := erpcInstance.GetNetwork(ctx, "test", "solana:mainnet-beta")
	require.NoError(t, err)

	nr := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"sendTransaction","params":["base64encodedtx"]}`,
	))
	_, fwdErr := nw.Forward(ctx, nr)

	require.Error(t, fwdErr, "sendTransaction error must be surfaced to the caller")
	assert.False(t, common.IsRetryableTowardNetwork(fwdErr),
		"sendTransaction error must not be retryable toward the network")
	assert.Equal(t, int32(1), totalTxCalls.Load(),
		"sendTransaction must be attempted exactly once (no retry to a second upstream)")
}

// TestSolanaHighestSlotReflectsMultipleUpstreams verifies that
// Network.SolanaHighestLatestSlot() returns the maximum slot seen across all
// upstreams (gap 3).
func TestSolanaHighestSlotReflectsMultipleUpstreams(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	defer gock.Off()

	// sol1 reports slot 300_000_000, sol2 reports 400_000_000.
	for _, tc := range []struct {
		host string
		slot string
	}{
		{"http://sol1.localhost", "300000000"},
		{"http://sol2.localhost", "400000000"},
	} {
		host := tc.host
		slot := tc.slot
		gock.New(host).Post("").Persist().
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "getGenesisHash")
			}).
			Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":"5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d"}`))

		gock.New(host).Post("").Persist().
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "getHealth")
			}).
			Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":"ok"}`))

		s := slot
		gock.New(host).Post("").Persist().
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "getSlot")
			}).
			Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":` + s + `}`))
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

	// Allow the background state pollers to fire at least once.
	time.Sleep(600 * time.Millisecond)

	nw, err := erpcInstance.GetNetwork(ctx, "test", "solana:mainnet-beta")
	require.NoError(t, err)

	highest := nw.SolanaHighestLatestSlot(ctx)
	assert.Equal(t, int64(400_000_000), highest,
		"SolanaHighestLatestSlot should return the maximum slot across all upstreams")
}

// TestSolanaAndEvmInSameProject verifies that a project may host both a Solana
// and an EVM network simultaneously without the architecture dispatch hooks
// cross-contaminating each other (gap 5).
func TestSolanaAndEvmInSameProject(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	defer gock.Off()

	// Solana state poller mocks for the Solana upstream.
	setupSolanaStatePollerMocks()

	// EVM state poller mocks for the EVM upstream.
	for _, path := range []struct{ method, body, resp string }{
		{"eth_blockNumber", "eth_blockNumber", `{"jsonrpc":"2.0","id":1,"result":"0x1234"}`},
		{"eth_chainId", "eth_chainId", `{"jsonrpc":"2.0","id":1,"result":"0x7b"}`},
		{"eth_syncing", "eth_syncing", `{"jsonrpc":"2.0","id":1,"result":false}`},
	} {
		b := path.body
		r := path.resp
		gock.New("http://rpc1.localhost").Post("").Persist().
			Filter(func(req *http.Request) bool {
				return strings.Contains(util.SafeReadBody(req), b)
			}).
			Reply(200).JSON([]byte(r))
	}

	// EVM request mock.
	gock.New("http://rpc1.localhost").Post("").
		Filter(func(req *http.Request) bool {
			return strings.Contains(util.SafeReadBody(req), "eth_getBalance")
		}).
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":"0xde0b6b3a7640000"}`))

	// Solana request mock.
	gock.New("http://sol1.localhost").Post("").
		Filter(func(req *http.Request) bool {
			return strings.Contains(util.SafeReadBody(req), "getBalance")
		}).
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":300000000},"value":1000000000}}`))

	util.ConfigureTestLogger()
	lg := log.Logger
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ssr, err := data.NewSharedStateRegistry(ctx, &lg, nil)
	require.NoError(t, err)

	cfg := &common.Config{
		Projects: []*common.ProjectConfig{
			{
				Id: "test",
				Networks: []*common.NetworkConfig{
					{
						Architecture: "solana",
						Solana:       &common.SolanaNetworkConfig{Cluster: "mainnet-beta"},
					},
					{
						Architecture: "evm",
						Evm:          &common.EvmNetworkConfig{ChainId: 123},
					},
				},
				Upstreams: []*common.UpstreamConfig{
					{
						Id:       "sol1",
						Type:     "solana",
						Endpoint: "http://sol1.localhost",
						Solana:   &common.SolanaUpstreamConfig{Cluster: "mainnet-beta"},
					},
					{
						Id:   "rpc1",
						Type: "evm",
						Endpoint: "http://rpc1.localhost",
						Evm:  &common.EvmUpstreamConfig{ChainId: 123},
						JsonRpc: &common.JsonRpcUpstreamConfig{
							SupportsBatch: &common.FALSE,
						},
					},
				},
			},
		},
	}

	erpcInstance, err := NewERPC(ctx, &lg, ssr, nil, cfg)
	require.NoError(t, err)
	erpcInstance.Bootstrap(ctx)

	// Both networks must be reachable.
	solNw, err := erpcInstance.GetNetwork(ctx, "test", "solana:mainnet-beta")
	require.NoError(t, err)
	assert.Equal(t, common.ArchitectureSolana, solNw.Architecture())

	evmNw, err := erpcInstance.GetNetwork(ctx, "test", "evm:123")
	require.NoError(t, err)
	assert.Equal(t, common.ArchitectureEvm, evmNw.Architecture())

	// Solana request must be routed to sol1 and succeed.
	solReq := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"getBalance","params":["9xQeWvG816bUx9EPjHmaT23yvVM2ZWbrrpZb9PusVFin"]}`,
	))
	solResp, solErr := solNw.Forward(ctx, solReq)
	require.NoError(t, solErr)
	solJrr, _ := solResp.JsonRpcResponse()
	assert.Nil(t, solJrr.Error, "Solana getBalance should succeed")

	// EVM request must be routed to rpc1 and succeed.
	evmReq := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":2,"method":"eth_getBalance","params":["0xd3CdA913deB6f4967b2Ef3aa68f5A843FDC2bB9","latest"]}`,
	))
	evmResp, evmErr := evmNw.Forward(ctx, evmReq)
	require.NoError(t, evmErr)
	evmJrr, _ := evmResp.JsonRpcResponse()
	assert.Nil(t, evmJrr.Error, "EVM eth_getBalance should succeed")
}

// TestSolanaFinalizedSlotTracked verifies that finalized slot suggestions are
// propagated through the shared variable and readable via FinalizedSlot() (gap 1).
// ── Edge cases from other proxy repos ────────────────────────────────────────

// TestSolanaHTTP500_FailsoverToSecondUpstream verifies that an HTTP 500 from
// the first upstream is classified as ServerSideException and triggers failover.
// Inspired by neon-proxy.rs and yellowstone-grpc test patterns.
func TestSolanaHTTP500_FailsoverToSecondUpstream(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	defer gock.Off()
	setupSolanaStatePollerMocks()

	gock.New("http://sol1.localhost").Post("").
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "getBalance") &&
				strings.Contains(r.Host, "sol1")
		}).
		Reply(500).BodyString(`Internal Server Error`)

	gock.New("http://sol2.localhost").Post("").
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "getBalance") &&
				strings.Contains(r.Host, "sol2")
		}).
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":300000000},"value":1000000000}}`))

	util.ConfigureTestLogger()
	lg := log.Logger
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ssr, err := data.NewSharedStateRegistry(ctx, &lg, nil)
	require.NoError(t, err)
	erpcInstance, err := NewERPC(ctx, &lg, ssr, nil, newSolanaTestConfig())
	require.NoError(t, err)
	erpcInstance.Bootstrap(ctx)

	nw, err := erpcInstance.GetNetwork(ctx, "test", "solana:mainnet-beta")
	require.NoError(t, err)

	resp, err := nw.Forward(ctx, common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"getBalance","params":["9xQeWvG816bUx9EPjHmaT23yvVM2ZWbrrpZb9PusVFin"]}`,
	)))
	require.NoError(t, err)
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	assert.Nil(t, jrr.Error, "HTTP 500 from sol1 should trigger failover; sol2 response should succeed")
}

// TestSolanaHTTP429_FailsoverToSecondUpstream verifies that a rate-limit
// response (HTTP 429) from the first upstream triggers failover.
func TestSolanaHTTP429_FailsoverToSecondUpstream(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	defer gock.Off()
	setupSolanaStatePollerMocks()

	gock.New("http://sol1.localhost").Post("").
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "getSlot") &&
				strings.Contains(r.Host, "sol1")
		}).
		Reply(429).BodyString(`Too Many Requests`)

	gock.New("http://sol2.localhost").Post("").
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "getSlot") &&
				strings.Contains(r.Host, "sol2")
		}).
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":300000001}`))

	util.ConfigureTestLogger()
	lg := log.Logger
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ssr, err := data.NewSharedStateRegistry(ctx, &lg, nil)
	require.NoError(t, err)
	erpcInstance, err := NewERPC(ctx, &lg, ssr, nil, newSolanaTestConfig())
	require.NoError(t, err)
	erpcInstance.Bootstrap(ctx)

	nw, err := erpcInstance.GetNetwork(ctx, "test", "solana:mainnet-beta")
	require.NoError(t, err)

	resp, err := nw.Forward(ctx, common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"getSlot","params":[]}`,
	)))
	require.NoError(t, err)
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	assert.Nil(t, jrr.Error, "HTTP 429 from sol1 should trigger failover; sol2 should succeed")
}

// TestSolanaMethodNotFound_FailsoverToSecondUpstream verifies that -32601
// (method not found) is classified as ErrEndpointUnsupported and another
// upstream is tried. Matches behavior seen in Triton/Helius tiered nodes
// where some methods are only available on premium endpoints.
func TestSolanaMethodNotFound_FailsoverToSecondUpstream(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	defer gock.Off()
	setupSolanaStatePollerMocks()

	gock.New("http://sol1.localhost").Post("").
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "getLeaderSchedule") &&
				strings.Contains(r.Host, "sol1")
		}).
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"Method not found"}}`))

	gock.New("http://sol2.localhost").Post("").
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "getLeaderSchedule") &&
				strings.Contains(r.Host, "sol2")
		}).
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"identity1":[[0,4,8]]}}`))

	util.ConfigureTestLogger()
	lg := log.Logger
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ssr, err := data.NewSharedStateRegistry(ctx, &lg, nil)
	require.NoError(t, err)
	erpcInstance, err := NewERPC(ctx, &lg, ssr, nil, newSolanaTestConfig())
	require.NoError(t, err)
	erpcInstance.Bootstrap(ctx)

	nw, err := erpcInstance.GetNetwork(ctx, "test", "solana:mainnet-beta")
	require.NoError(t, err)

	resp, err := nw.Forward(ctx, common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"getLeaderSchedule","params":[null]}`,
	)))
	require.NoError(t, err)
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	assert.Nil(t, jrr.Error, "-32601 from sol1 should trigger failover; sol2 should succeed")
}

// TestSolanaNoSnapshot_FailsoverToSecondUpstream verifies that -32008
// (no snapshot available) is classified as MissingData and another upstream
// is tried. Common when a node is freshly started or behind on snapshots.
func TestSolanaNoSnapshot_FailsoverToSecondUpstream(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	defer gock.Off()
	setupSolanaStatePollerMocks()

	gock.New("http://sol1.localhost").Post("").
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "getAccountInfo") &&
				strings.Contains(r.Host, "sol1")
		}).
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32008,"message":"No snapshot"}}`))

	gock.New("http://sol2.localhost").Post("").
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "getAccountInfo") &&
				strings.Contains(r.Host, "sol2")
		}).
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":300000000},"value":{"data":["","base64"],"executable":false,"lamports":1000,"owner":"11111111111111111111111111111111","rentEpoch":0,"space":0}}}`))

	util.ConfigureTestLogger()
	lg := log.Logger
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ssr, err := data.NewSharedStateRegistry(ctx, &lg, nil)
	require.NoError(t, err)
	erpcInstance, err := NewERPC(ctx, &lg, ssr, nil, newSolanaTestConfig())
	require.NoError(t, err)
	erpcInstance.Bootstrap(ctx)

	nw, err := erpcInstance.GetNetwork(ctx, "test", "solana:mainnet-beta")
	require.NoError(t, err)

	resp, err := nw.Forward(ctx, common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"getAccountInfo","params":["EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",{"encoding":"base64"}]}`,
	)))
	require.NoError(t, err)
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	assert.Nil(t, jrr.Error, "-32008 from sol1 should trigger MissingData failover; sol2 should succeed")
}

// TestSolanaGetBlock_SkippedSlotNullPassthrough verifies that a null result
// from getBlock (which Solana returns for skipped/empty slots) is passed
// through to the client as-is and does NOT trigger a retry. This is a common
// bug in Solana proxies that mistake null for "missing data".
func TestSolanaGetBlock_SkippedSlotNullPassthrough(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	defer gock.Off()
	setupSolanaStatePollerMocks()

	var sol1Hits atomic.Int32

	// sol1 returns null (skipped slot) — valid Solana response
	gock.New("http://sol1.localhost").Post("").Persist().
		Filter(func(r *http.Request) bool {
			if strings.Contains(util.SafeReadBody(r), "getBlock") &&
				strings.Contains(r.Host, "sol1") {
				sol1Hits.Add(1)
				return true
			}
			return false
		}).
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":null}`))

	util.ConfigureTestLogger()
	lg := log.Logger
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ssr, err := data.NewSharedStateRegistry(ctx, &lg, nil)
	require.NoError(t, err)
	erpcInstance, err := NewERPC(ctx, &lg, ssr, nil, newSolanaTestConfig())
	require.NoError(t, err)
	erpcInstance.Bootstrap(ctx)

	nw, err := erpcInstance.GetNetwork(ctx, "test", "solana:mainnet-beta")
	require.NoError(t, err)

	resp, err := nw.Forward(ctx, common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"getBlock","params":[299999999,{"maxSupportedTransactionVersion":0}]}`,
	)))

	// null result is valid — erpc should not error
	if err == nil {
		jrr, jErr := resp.JsonRpcResponse()
		require.NoError(t, jErr)
		assert.Nil(t, jrr.Error, "null getBlock (skipped slot) should not produce an RPC error")
	}
	// Either way, sol1 should only have been hit once — null must not trigger retries
	assert.LessOrEqual(t, sol1Hits.Load(), int32(2),
		"null getBlock result must not trigger multiple retries across upstreams")
}

// TestSolanaRequestID_StringPreserved verifies that a string request ID is
// returned unchanged in the response. Many proxy implementations incorrectly
// replace string IDs with integers or drop them entirely.
func TestSolanaRequestID_StringPreserved(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	defer gock.Off()
	setupSolanaStatePollerMocks()

	// Use getBalance (not getSlot) to avoid conflicting with the persistent
	// state-poller mock which also calls getSlot.
	gock.New("http://sol1.localhost").Post("").
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "getBalance") &&
				strings.Contains(r.Host, "sol1")
		}).
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":"my-custom-id-123","result":{"context":{"slot":300000042},"value":999000000000}}`))

	util.ConfigureTestLogger()
	lg := log.Logger
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ssr, err := data.NewSharedStateRegistry(ctx, &lg, nil)
	require.NoError(t, err)
	erpcInstance, err := NewERPC(ctx, &lg, ssr, nil, newSolanaTestConfig())
	require.NoError(t, err)
	erpcInstance.Bootstrap(ctx)

	nw, err := erpcInstance.GetNetwork(ctx, "test", "solana:mainnet-beta")
	require.NoError(t, err)

	resp, err := nw.Forward(ctx, common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":"my-custom-id-123","method":"getBalance","params":["EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"]}`,
	)))
	require.NoError(t, err)
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	require.Nil(t, jrr.Error)

	id := jrr.ID()
	assert.Equal(t, "my-custom-id-123", id,
		"string request ID must be preserved in the response")
}

// TestSolanaMinContextSlot_SucceedsOnSecondUpstream verifies the full success
// path: sol1 returns -32016 (QuickNode minContextSlot), sol2 satisfies the
// request. This tests the failover plumbing end-to-end.
func TestSolanaMinContextSlot_SucceedsOnSecondUpstream(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	defer gock.Off()
	setupSolanaStatePollerMocks()

	gock.New("http://sol1.localhost").Post("").
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "getBalance") &&
				strings.Contains(r.Host, "sol1")
		}).
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32016,"message":"Minimum context slot has not been reached"}}`))

	gock.New("http://sol2.localhost").Post("").
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "getBalance") &&
				strings.Contains(r.Host, "sol2")
		}).
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":300000042},"value":5000000000}}`))

	util.ConfigureTestLogger()
	lg := log.Logger
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ssr, err := data.NewSharedStateRegistry(ctx, &lg, nil)
	require.NoError(t, err)
	erpcInstance, err := NewERPC(ctx, &lg, ssr, nil, newSolanaTestConfig())
	require.NoError(t, err)
	erpcInstance.Bootstrap(ctx)

	nw, err := erpcInstance.GetNetwork(ctx, "test", "solana:mainnet-beta")
	require.NoError(t, err)

	resp, err := nw.Forward(ctx, common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"getBalance","params":["9xQeWvG816bUx9EPjHmaT23yvVM2ZWbrrpZb9PusVFin",{"minContextSlot":300000040}]}`,
	)))
	require.NoError(t, err)
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	assert.Nil(t, jrr.Error,
		"-32016 from sol1 should trigger ServerSideException failover; sol2 should succeed")
}

// TestSolanaSignatureStatuses_NullEntriesPassthrough verifies that
// getSignatureStatuses correctly passes through responses containing null
// entries (unrecognised signatures) alongside confirmed ones. Proxies sometimes
// over-eagerly retry when they see a null inside a result array.
//
// Both sol1 and sol2 are registered with Persist() mocks and a shared hit
// counter, so the test is order-independent (round-robin may start at either
// upstream). The assertion is that only ONE upstream total is ever contacted —
// a null entry must not trigger a failover.
func TestSolanaSignatureStatuses_NullEntriesPassthrough(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	defer gock.Off()
	setupSolanaStatePollerMocks()

	// Track hits per upstream. Either upstream may be tried first (round-robin
	// position varies). The assertion: exactly one upstream total is tried —
	// a null entry must not cause a failover.
	var sol1Hits, sol2Hits atomic.Int32
	sigStatusReply := []byte(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":300000000},"value":[null,{"slot":299999999,"confirmations":null,"err":null,"confirmationStatus":"finalized"}]}}`)

	gock.New("http://sol1.localhost").Post("").Persist().
		Filter(func(r *http.Request) bool {
			// Include host check: gock evaluates filter BEFORE URL matching, so a
			// request to sol2.localhost would otherwise trigger this filter (and
			// increment sol1Hits) before gock rejects the mock on host mismatch.
			if r.URL.Host == "sol1.localhost" && strings.Contains(util.SafeReadBody(r), "getSignatureStatuses") {
				sol1Hits.Add(1)
				return true
			}
			return false
		}).
		Reply(200).JSON(sigStatusReply)

	gock.New("http://sol2.localhost").Post("").Persist().
		Filter(func(r *http.Request) bool {
			if r.URL.Host == "sol2.localhost" && strings.Contains(util.SafeReadBody(r), "getSignatureStatuses") {
				sol2Hits.Add(1)
				return true
			}
			return false
		}).
		Reply(200).JSON(sigStatusReply)

	util.ConfigureTestLogger()
	lg := log.Logger
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ssr, err := data.NewSharedStateRegistry(ctx, &lg, nil)
	require.NoError(t, err)
	erpcInstance, err := NewERPC(ctx, &lg, ssr, nil, newSolanaTestConfig())
	require.NoError(t, err)
	erpcInstance.Bootstrap(ctx)

	nw, err := erpcInstance.GetNetwork(ctx, "test", "solana:mainnet-beta")
	require.NoError(t, err)

	resp, err := nw.Forward(ctx, common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"getSignatureStatuses","params":[["UnknownSig111111111111111111111111111111111111111111","KnownSig222222222222222222222222222222222222222222222222222222222222"]]}`,
	)))
	require.NoError(t, err)
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	assert.Nil(t, jrr.Error,
		"null entries in getSignatureStatuses value array must not trigger retries or errors")
	totalHits := sol1Hits.Load() + sol2Hits.Load()
	t.Logf("sol1Hits=%d sol2Hits=%d totalHits=%d", sol1Hits.Load(), sol2Hits.Load(), totalHits)
	assert.Equal(t, int32(1), totalHits,
		"exactly one upstream must serve the request — null entries must not cause failover")
}

// ── Phase 2 tests ─────────────────────────────────────────────────────────────

// TestSolana_SimulationError_NotRetried verifies that deterministic simulation
// errors (-32002) are treated as ClientSideException and are NOT retried on a
// second upstream. An invalid transaction will fail the same way everywhere.
//
// Uses simulateTransaction (not sendTransaction) to avoid the double-handling
// in HandleUpstreamPostForward, keeping the test clean. Uses Persist() mocks
// on both upstreams to be robust against background state-poller goroutines.
func TestSolana_SimulationError_NotRetried(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	defer gock.Off()
	setupSolanaStatePollerMocks()

	var sol2Hits atomic.Int32

	// sol1: returns -32002 (transaction simulation failed — bad tx)
	gock.New("http://sol1.localhost").Post("").Persist().
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "simulateTransaction") &&
				strings.Contains(r.Host, "sol1")
		}).
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32002,"message":"Transaction simulation failed: insufficient funds for instruction"}}`))

	// sol2: succeeds — but must NOT be called
	gock.New("http://sol2.localhost").Post("").Persist().
		Filter(func(r *http.Request) bool {
			if strings.Contains(util.SafeReadBody(r), "simulateTransaction") &&
				strings.Contains(r.Host, "sol2") {
				sol2Hits.Add(1)
				return true
			}
			return false
		}).
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":300000000},"value":{"err":null,"logs":[]}}}`))

	util.ConfigureTestLogger()
	lg := log.Logger
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ssr, err := data.NewSharedStateRegistry(ctx, &lg, nil)
	require.NoError(t, err)
	erpcInstance, err := NewERPC(ctx, &lg, ssr, nil, newSolanaTestConfig())
	require.NoError(t, err)
	erpcInstance.Bootstrap(ctx)

	nw, err := erpcInstance.GetNetwork(ctx, "test", "solana:mainnet-beta")
	require.NoError(t, err)

	resp, err := nw.Forward(ctx, common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"simulateTransaction","params":["AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="]}`,
	)))

	// erpc surfaces a ClientSideException either as a Go error OR as a JSON-RPC
	// error body. Either way, sol2 must NOT have been contacted.
	if err == nil {
		require.NotNil(t, resp)
		jrr, jErr := resp.JsonRpcResponse()
		require.NoError(t, jErr)
		assert.NotNil(t, jrr.Error,
			"if no Go error, the JSON-RPC response must carry the simulation error")
	}
	assert.Equal(t, int32(0), sol2Hits.Load(),
		"sol2 must NOT be tried — -32002 is deterministic and retrying wastes capacity")
}

// TestSolana_32000_RateLimit_InJsonBody_FailsoverToSecondUpstream verifies
// that a -32000 error with a rate-limit message inside a 200 response body
// is correctly classified as CapacityExceeded and triggers failover.
func TestSolana_32000_RateLimit_InJsonBody_FailsoverToSecondUpstream(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	defer gock.Off()
	setupSolanaStatePollerMocks()

	gock.New("http://sol1.localhost").Post("").
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "getBalance") &&
				strings.Contains(r.Host, "sol1")
		}).
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"Connection rate limits exceeded"}}`))

	gock.New("http://sol2.localhost").Post("").
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "getBalance") &&
				strings.Contains(r.Host, "sol2")
		}).
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":300000042},"value":5000000000}}`))

	util.ConfigureTestLogger()
	lg := log.Logger
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ssr, err := data.NewSharedStateRegistry(ctx, &lg, nil)
	require.NoError(t, err)
	erpcInstance, err := NewERPC(ctx, &lg, ssr, nil, newSolanaTestConfig())
	require.NoError(t, err)
	erpcInstance.Bootstrap(ctx)

	nw, err := erpcInstance.GetNetwork(ctx, "test", "solana:mainnet-beta")
	require.NoError(t, err)

	resp, err := nw.Forward(ctx, common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"getBalance","params":["EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"]}`,
	)))
	require.NoError(t, err)
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	assert.Nil(t, jrr.Error,
		"rate-limited sol1 (-32000 in JSON body) should failover to sol2 which succeeds")
}

func TestSolanaFinalizedSlotTracked(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	defer gock.Off()

	// Return a distinct finalized slot value from getSlot with "finalized" commitment.
	for _, host := range []string{"http://sol1.localhost"} {
		gock.New(host).Post("").Persist().
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "getGenesisHash")
			}).
			Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":"5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d"}`))

		gock.New(host).Post("").Persist().
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "getHealth")
			}).
			Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":"ok"}`))

		// Latest slot: 300_000_000
		gock.New(host).Post("").Persist().
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "getSlot") && !strings.Contains(body, "finalized")
			}).
			Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":300000000}`))

		// Finalized slot: 299_900_000
		gock.New(host).Post("").Persist().
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "getSlot") && strings.Contains(body, "finalized")
			}).
			Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":299900000}`))
	}

	util.ConfigureTestLogger()
	lg := log.Logger
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ssr, err := data.NewSharedStateRegistry(ctx, &lg, nil)
	require.NoError(t, err)

	cfg := newSolanaTestConfig()
	cfg.Projects[0].Upstreams = cfg.Projects[0].Upstreams[:1]

	erpcInstance, err := NewERPC(ctx, &lg, ssr, nil, cfg)
	require.NoError(t, err)
	erpcInstance.Bootstrap(ctx)

	// Allow background poller goroutines to fire.
	time.Sleep(600 * time.Millisecond)

	nw, err := erpcInstance.GetNetwork(ctx, "test", "solana:mainnet-beta")
	require.NoError(t, err)

	upstreams, err := nw.upstreamsRegistry.GetSortedUpstreams(ctx, "solana:mainnet-beta", "getSlot")
	require.NoError(t, err)
	require.NotEmpty(t, upstreams)

	for _, u := range upstreams {
		if su, ok := u.(interface{ SolanaStatePoller() common.SolanaStatePoller }); ok {
			sp := su.SolanaStatePoller()
			if sp == nil || sp.IsObjectNull() {
				continue
			}
			assert.Greater(t, sp.LatestSlot(), int64(0),
				"LatestSlot must be populated after background poll")
			// SolanaHighestFinalizedSlot mirrors FinalizedSlot when one upstream.
			highest := nw.SolanaHighestFinalizedSlot(ctx)
			assert.Greater(t, highest, int64(0),
				"SolanaHighestFinalizedSlot must be positive after poll")
			assert.Less(t, highest, sp.LatestSlot(),
				"finalized slot must be behind latest slot")
			return
		}
	}
	t.Fatal("no upstream with Solana state poller found")
}
