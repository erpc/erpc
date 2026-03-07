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
