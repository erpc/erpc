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

// setupSolanaStatePollerMocks registers persistent gock mocks for getSlot so the
// Solana state poller's background goroutine doesn't leak or hang during tests.
func setupSolanaStatePollerMocks() {
	gock.New("http://sol1.localhost").
		Post("").
		Persist().
		Filter(func(req *http.Request) bool {
			return strings.Contains(util.SafeReadBody(req), "getSlot")
		}).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":300000000}`))

	gock.New("http://sol2.localhost").
		Post("").
		Persist().
		Filter(func(req *http.Request) bool {
			return strings.Contains(util.SafeReadBody(req), "getSlot")
		}).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":300000000}`))
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

	// Mock getBalance on sol1
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

	cfg := newSolanaTestConfig()

	erpcInstance, err := NewERPC(ctx, &lg, ssr, nil, cfg)
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

	// sol1 always fails with 500
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

	cfg := newSolanaTestConfig()
	// Only one upstream, add sol2 to retry target
	cfg.Projects[0].Upstreams[0].Endpoint = "http://sol1.localhost"
	cfg.Projects[0].Upstreams[1].Endpoint = "http://sol2.localhost"

	erpcInstance, err := NewERPC(ctx, &lg, ssr, nil, cfg)
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

// TestSolanaStatePoller verifies that getSlot responses populate LatestSlot on the upstream.
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
	// Only need sol1 for this test
	cfg.Projects[0].Upstreams = cfg.Projects[0].Upstreams[:1]

	erpcInstance, err := NewERPC(ctx, &lg, ssr, nil, cfg)
	require.NoError(t, err)

	erpcInstance.Bootstrap(ctx)

	// Give the background poller a moment to fire
	time.Sleep(600 * time.Millisecond)

	nw, err := erpcInstance.GetNetwork(ctx, "test", "solana:mainnet-beta")
	require.NoError(t, err)

	upstreams, err := nw.upstreamsRegistry.GetSortedUpstreams(ctx, "solana:mainnet-beta", "getSlot")
	require.NoError(t, err)
	require.NotEmpty(t, upstreams)

	// Look for SolanaStatePoller in the upstream
	found := false
	for _, u := range upstreams {
		if su, ok := u.(interface{ SolanaStatePoller() common.SolanaStatePoller }); ok {
			sp := su.SolanaStatePoller()
			if sp != nil && !sp.IsObjectNull() {
				assert.Greater(t, sp.LatestSlot(), int64(0), "LatestSlot should be populated by the poller")
				found = true
				break
			}
		}
	}
	assert.True(t, found, "expected an upstream with a Solana state poller")
}

// TestSolanaCacheFinalityMapping checks that finalized requests get DataFinalityStateFinalized
// and processed requests get DataFinalityStateRealtime.
func TestSolanaCacheFinalityMapping(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	defer gock.Off()

	setupSolanaStatePollerMocks()

	// Mock getBlock (always-finalized method)
	gock.New("http://sol1.localhost").
		Post("").
		Filter(func(req *http.Request) bool {
			return strings.Contains(util.SafeReadBody(req), "getBlock")
		}).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"blockHeight":299000000,"blockhash":"abc123"}}`))

	// Mock getLatestBlockhash (never-cache method)
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

	// getBlock → should be Finalized
	reqBlock := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"getBlock","params":[299000000]}`,
	))
	respBlock, err := nw.Forward(ctx, reqBlock)
	require.NoError(t, err)
	finalityBlock := nw.GetFinality(ctx, reqBlock, respBlock)
	assert.Equal(t, common.DataFinalityStateFinalized, finalityBlock,
		"getBlock should always map to Finalized")

	// getLatestBlockhash → should be Realtime (never cache)
	reqHash := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":2,"method":"getLatestBlockhash","params":[]}`,
	))
	respHash, err := nw.Forward(ctx, reqHash)
	require.NoError(t, err)
	finalityHash := nw.GetFinality(ctx, reqHash, respHash)
	assert.Equal(t, common.DataFinalityStateRealtime, finalityHash,
		"getLatestBlockhash should always map to Realtime (never cache)")
}

// TestEvmStillWorks is a regression guard: EVM config must be unaffected by Solana additions.
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
