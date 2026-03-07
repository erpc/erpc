package erpc

import (
	"context"
	"net/http"
	"strings"
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
	// but the upstream should be rejected / unavailable
	erpcInstance.Bootstrap(ctx)

	nw, err := erpcInstance.GetNetwork(ctx, "test", "solana:mainnet-beta")
	require.NoError(t, err)

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
