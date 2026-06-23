package erpc

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/clients"
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

// receiptBody builds an eth_getTransactionReceipt JSON-RPC response whose logs
// carry the given logIndexes. Underflowed values exercise the integrity engine.
func receiptBody(logIndexes ...string) string {
	logs := make([]string, len(logIndexes))
	for i, li := range logIndexes {
		logs[i] = fmt.Sprintf(`{"address":"0x27b26e88f007ec9109648c6da522fcaba06c74d7","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"],"data":"0x","logIndex":"%s","removed":false}`, li)
	}
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":{"blockHash":"0x5a28cc00c288af5a055bba9ea5b202b8406e86138ec94ddfc8e96978c752c28a","blockNumber":"0xe57e13","status":"0x1","transactionIndex":"0x0","logs":[%s]}}`, strings.Join(logs, ","))
}

func mockReceipt(url, body string, times int) {
	gock.New(url).
		Post("").
		Times(times).
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "eth_getTransactionReceipt")
		}).
		Reply(200).
		JSON([]byte(body))
}

// buildIntegrityNetwork wires a real two-upstream network (rpc1, rpc2) with a
// retry failsafe — only the HTTP transport is mocked (via gock). Every other
// object (registries, upstreams, clients, network, the post-forward hook and
// the integrity engine) is real.
func buildIntegrityNetwork(t *testing.T, ctx context.Context) *Network {
	t.Helper()
	vr := thirdparty.NewVendorsRegistry()
	pr, err := thirdparty.NewProvidersRegistry(&log.Logger, vr, []*common.ProviderConfig{}, nil)
	require.NoError(t, err)
	clr := clients.NewClientRegistry(&log.Logger, "prjA", nil, evm.NewJsonRpcErrorExtractor())
	rlr, err := upstream.NewRateLimitersRegistry(ctx, &common.RateLimiterConfig{Budgets: []*common.RateLimitBudgetConfig{}}, &log.Logger)
	require.NoError(t, err)
	mt := health.NewTracker(&log.Logger, "prjA", 2*time.Second)

	up1 := &common.UpstreamConfig{Id: "rpc1", Type: common.UpstreamTypeEvm, Endpoint: "http://rpc1.localhost", Evm: &common.EvmUpstreamConfig{ChainId: 123}}
	up2 := &common.UpstreamConfig{Id: "rpc2", Type: common.UpstreamTypeEvm, Endpoint: "http://rpc2.localhost", Evm: &common.EvmUpstreamConfig{ChainId: 123}}

	ssCfg := &common.SharedStateConfig{
		Connector:     &common.ConnectorConfig{Driver: common.DriverMemory, Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"}},
		LockMaxWait:   common.Duration(200 * time.Millisecond),
		UpdateMaxWait: common.Duration(200 * time.Millisecond),
	}
	ssCfg.SetDefaults("test")
	ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, ssCfg)
	require.NoError(t, err)

	upr := upstream.NewUpstreamsRegistry(ctx, &log.Logger, "prjA", []*common.UpstreamConfig{up1, up2}, ssr, rlr, vr, pr, nil, mt, nil)
	upr.Bootstrap(ctx)
	time.Sleep(100 * time.Millisecond)
	require.NoError(t, upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123)))
	for _, uc := range []*common.UpstreamConfig{up1, up2} {
		pup, err := upr.NewUpstream(uc)
		require.NoError(t, err)
		cl, err := clr.GetOrCreateClient(ctx, pup)
		require.NoError(t, err)
		pup.Client = cl
	}

	fsCfg := &common.FailsafeConfig{Retry: &common.RetryPolicyConfig{MaxAttempts: 3}}
	tru := true
	ntw, err := NewNetwork(ctx, &log.Logger, "prjA", &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
		Failsafe:     []*common.FailsafeConfig{fsCfg},
		// Enable the intrinsic logIndex magnitude check (the corrupt-receipt
		// tests rely on it). Integrity is opt-in, so it must be configured.
		Integrity: &common.IntegrityConfig{
			IntegritySettings: common.IntegritySettings{
				Checks: map[string]*common.IntegrityCheckConfig{
					"indexMagnitude": {Enabled: &tru},
				},
			},
		},
	}, rlr, upr, mt, nil)
	require.NoError(t, err)
	ntw.Bootstrap(ctx)
	time.Sleep(100 * time.Millisecond)
	upr.OverrideOrderForTest(util.EvmNetworkId(123))
	// Seed a finalized head so corroboration's per-finality verdict is
	// deterministic (any block below this is finalized). Harmless for the
	// intrinsic-check tests, which don't consult finality.
	for _, ups := range upr.GetNetworkUpstreams(ctx, util.EvmNetworkId(123)) {
		if sp := ups.EvmStatePoller(); sp != nil && !sp.IsObjectNull() {
			sp.SuggestFinalizedBlock(0x11117FFF)
		}
	}
	return ntw
}

func forwardReceipt(t *testing.T, ctx context.Context, ntw *Network) (*common.NormalizedResponse, error) {
	t.Helper()
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getTransactionReceipt","params":["0xabf61f02a6c77b28a9465a2256e26d2fe25714b60bb8edabb7d0ce794fba932e"],"id":1}`))
	req.ApplyDirectiveDefaults(ntw.Config().DirectiveDefaults)
	return ntw.Forward(ctx, req)
}

var canonicalIdx = []string{"0x0", "0x1", "0x2", "0x3", "0x4", "0x5", "0x6", "0x7", "0x8"}
var underflowedIdx = []string{"0xfffffff7", "0xfffffff8", "0xfffffff9", "0xfffffffa", "0xfffffffb", "0xfffffffc", "0xfffffffd", "0xfffffffe", "0xffffffff"}

// TestIntegrity_Network_CorruptReceiptFailsOver is the end-to-end real-world
// scenario: the first upstream returns a receipt with underflowed logIndex
// values; the network's post-forward integrity engine rejects it as a
// content-validation error, which is retryable-toward-network, so the request
// fails over to the second upstream and the canonical receipt is served.
func TestIntegrity_Network_CorruptReceiptFailsOver(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockReceipt("http://rpc1.localhost", receiptBody(underflowedIdx...), 1) // corrupt → rejected
	mockReceipt("http://rpc2.localhost", receiptBody(canonicalIdx...), 1)   // canonical → served

	ntw := buildIntegrityNetwork(t, ctx)
	resp, err := forwardReceipt(t, ctx, ntw)
	require.NoError(t, err, "must fail over to the healthy upstream")
	require.NotNil(t, resp)

	jrr, err := resp.JsonRpcResponse(ctx)
	require.NoError(t, err)
	li, err := jrr.PeekStringByPath(ctx, "logs", 0, "logIndex")
	require.NoError(t, err)
	assert.Equal(t, "0x0", li, "the served receipt must be the canonical one, not the corrupt upstream's")
}

// TestIntegrity_Network_CleanReceiptPasses asserts a canonical receipt is served
// untouched (the engine does not over-reject real data).
func TestIntegrity_Network_CleanReceiptPasses(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockReceipt("http://rpc1.localhost", receiptBody(canonicalIdx...), 1)

	ntw := buildIntegrityNetwork(t, ctx)
	resp, err := forwardReceipt(t, ctx, ntw)
	require.NoError(t, err)
	require.NotNil(t, resp)
	jrr, err := resp.JsonRpcResponse(ctx)
	require.NoError(t, err)
	li, err := jrr.PeekStringByPath(ctx, "logs", 0, "logIndex")
	require.NoError(t, err)
	assert.Equal(t, "0x0", li)
}

// TestIntegrity_Network_SavedRequestFlagged proves the "saved" signal: a corrupt
// response is rejected, the request fails over to a healthy upstream, and the
// request is flagged IntegrityCaught — so project.Forward emits
// integrity_saved_total (a wrong response was prevented).
func TestIntegrity_Network_SavedRequestFlagged(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockReceipt("http://rpc1.localhost", receiptBody(underflowedIdx...), 1) // corrupt → rejected
	mockReceipt("http://rpc2.localhost", receiptBody(canonicalIdx...), 1)   // canonical → served

	ntw := buildIntegrityNetwork(t, ctx)
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getTransactionReceipt","params":["0xabf61f02a6c77b28a9465a2256e26d2fe25714b60bb8edabb7d0ce794fba932e"],"id":1}`))
	req.ApplyDirectiveDefaults(ntw.Config().DirectiveDefaults)

	resp, err := ntw.Forward(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)

	assert.True(t, req.IntegrityCaught(),
		"a reject-then-recover request must be flagged so project.Forward counts it as saved")
	jrr, err := resp.JsonRpcResponse(ctx)
	require.NoError(t, err)
	li, err := jrr.PeekStringByPath(ctx, "logs", 0, "logIndex")
	require.NoError(t, err)
	assert.Equal(t, "0x0", li, "the served receipt is the canonical one")
}

// receiptOneLog builds an eth_getTransactionReceipt with a single log at the
// given logIndex for a finalized block (number well below the poller's
// finalized head 0x11117777).
func receiptOneLog(logIndex string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":{"blockHash":"0x5a28cc00c288af5a055bba9ea5b202b8406e86138ec94ddfc8e96978c752c28a","blockNumber":"0xe57e13","transactionHash":"0xabf61f02a6c77b28a9465a2256e26d2fe25714b60bb8edabb7d0ce794fba932e","transactionIndex":"0x0","logs":[{"address":"0x27b26e88f007ec9109648c6da522fcaba06c74d7","data":"0x","logIndex":"%s"}]}}`, logIndex)
}

func blockReceiptsOneLog(logIndex string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":[{"blockHash":"0x5a28cc00c288af5a055bba9ea5b202b8406e86138ec94ddfc8e96978c752c28a","blockNumber":"0xe57e13","transactionHash":"0xabf61f02a6c77b28a9465a2256e26d2fe25714b60bb8edabb7d0ce794fba932e","transactionIndex":"0x0","logs":[{"address":"0x27b26e88f007ec9109648c6da522fcaba06c74d7","data":"0x","logIndex":"%s"}]}]}`, logIndex)
}

func mockMethod(url, method, body string) {
	gock.New(url).Post("").Persist().
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), method) }).
		Reply(200).JSON([]byte(body))
}

// TestIntegrity_Network_ConfigLevelDrivesCorroboration proves the config model
// end-to-end: a network configured with integrity level "authoritative" runs
// the corroboration tier with no per-request directive — the same plausible-
// but-wrong receipt is force-fetched, rejected, and failed over.
func TestIntegrity_Network_ConfigLevelDrivesCorroboration(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gock.New("http://rpc1.localhost").Post("").Times(1).
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getTransactionReceipt") }).
		Reply(200).JSON([]byte(receiptOneLog("0x5")))
	gock.New("http://rpc2.localhost").Post("").Times(1).
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getTransactionReceipt") }).
		Reply(200).JSON([]byte(receiptOneLog("0x0")))
	mockMethod("http://rpc1.localhost", "eth_getBlockReceipts", blockReceiptsOneLog("0x0"))
	mockMethod("http://rpc2.localhost", "eth_getBlockReceipts", blockReceiptsOneLog("0x0"))

	ntw := buildIntegrityNetwork(t, ctx)
	// Enable the authoritative tier via CONFIG, not a per-request directive.
	ntw.Config().Integrity = &common.IntegrityConfig{
		IntegritySettings: common.IntegritySettings{Level: "authoritative"},
	}

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getTransactionReceipt","params":["0xabf61f02a6c77b28a9465a2256e26d2fe25714b60bb8edabb7d0ce794fba932e"],"id":1}`))
	resp, err := ntw.Forward(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)

	jrr, err := resp.JsonRpcResponse(ctx)
	require.NoError(t, err)
	li, err := jrr.PeekStringByPath(ctx, "logs", 0, "logIndex")
	require.NoError(t, err)
	assert.Equal(t, "0x0", li, "config level:authoritative must drive corroboration without any directive")
}

// TestIntegrity_Network_AllUpstreamsCorrupt asserts that when every upstream
// returns corrupt data the request fails (rather than serving garbage).
func TestIntegrity_Network_AllUpstreamsCorrupt(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockReceipt("http://rpc1.localhost", receiptBody(underflowedIdx...), 1)
	mockReceipt("http://rpc2.localhost", receiptBody(underflowedIdx...), 1)

	ntw := buildIntegrityNetwork(t, ctx)
	_, err := forwardReceipt(t, ctx, ntw)
	require.Error(t, err, "no upstream returned valid data, so the request must fail")
}
