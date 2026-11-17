package erpc

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/internal/policy"
	policystdlib "github.com/erpc/erpc/internal/policy/stdlib"
	"github.com/erpc/erpc/thirdparty"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mevRoutingPolicy sends custom_sendMevProtected to the mev relays first
// and keeps every other upstream behind them as the fallback; other
// methods keep the plain list (relays skip them through allowMethods).
const mevRoutingPolicy = `(upstreams, ctx) =>
  methodMatches('custom_sendMevProtected')
    ? upstreams.byTag('mev').sortByScore(PREFER_FASTEST).concat(upstreams.excludeTag('mev'))
    : upstreams.excludeTag('mev')`

// setupMevRewriteNetwork builds the topology of a custom method served by
// a relay group with a method rewrite, falling back to a regular node:
//
//	regular  — serves everything; renames the custom method too, so it can
//	           take the custom method as the fallback.
//	relay-a  — mev relay, only the custom method, renamed.
//	relay-b  — mev relay, only the custom method, renamed.
func setupMevRewriteNetwork(t *testing.T, ctx context.Context) *Network {
	t.Helper()

	rewrite := map[string]string{"custom_sendMevProtected": "eth_sendRawTransaction"}
	ups := []*common.UpstreamConfig{
		{
			Id: "regular", Type: common.UpstreamTypeEvm, Endpoint: upstreamHostFromID("regular"),
			Evm:           &common.EvmUpstreamConfig{ChainId: 123},
			MethodRewrite: rewrite,
		},
		{
			Id: "relay-a", Type: common.UpstreamTypeEvm, Endpoint: upstreamHostFromID("relay-a"),
			Tags:          []string{"mev"},
			Evm:           &common.EvmUpstreamConfig{ChainId: 123},
			IgnoreMethods: []string{"*"},
			AllowMethods:  []string{"custom_sendMevProtected"},
			MethodRewrite: rewrite,
		},
		{
			Id: "relay-b", Type: common.UpstreamTypeEvm, Endpoint: upstreamHostFromID("relay-b"),
			Tags:          []string{"mev"},
			Evm:           &common.EvmUpstreamConfig{ChainId: 123},
			IgnoreMethods: []string{"*"},
			AllowMethods:  []string{"custom_sendMevProtected"},
			MethodRewrite: rewrite,
		},
	}
	for _, c := range ups {
		mockStatePollerForHost(c.Endpoint)
		c.Routing = &common.UpstreamRoutingConfig{Probe: common.ProbeModeOff}
	}

	rlr, err := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{}, &log.Logger)
	require.NoError(t, err)
	mt := health.NewTracker(&log.Logger, "test", time.Minute)
	vr := thirdparty.NewVendorsRegistry()
	pr, err := thirdparty.NewProvidersRegistry(&log.Logger, vr, []*common.ProviderConfig{}, nil)
	require.NoError(t, err)
	ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: "memory",
			Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"},
		},
	})
	require.NoError(t, err)

	upr := upstream.NewUpstreamsRegistry(ctx, &log.Logger, "test", ups, ssr, rlr, vr, pr, nil, mt, nil)

	ntwCfg := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
		Failsafe: []*common.FailsafeConfig{{
			MatchMethod: "*",
			Retry:       &common.RetryPolicyConfig{MaxAttempts: 3},
		}},
		SelectionPolicy: &common.SelectionPolicyConfig{
			EvalInterval: 0, // frozen — the test drives ticks
			EvalScope:    common.EvalScopeNetworkMethod,
			EvalFunc:     mevRoutingPolicy,
		},
	}

	engine := policy.NewEngine(ctx, &log.Logger, "test", mt, policystdlib.Install, nil)
	ntw, err := NewNetwork(ctx, &log.Logger, "test", ntwCfg, rlr, upr, mt, engine)
	stopAtEnd(t, ntw)
	require.NoError(t, err)

	upr.Bootstrap(ctx)
	time.Sleep(50 * time.Millisecond)
	require.NoError(t, upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123)))
	require.NoError(t, ntw.Bootstrap(ctx))
	time.Sleep(50 * time.Millisecond)
	return ntw
}

// mockSendRaw answers eth_sendRawTransaction on host with status and body,
// and fails the test if the custom method itself ever reaches the host.
func mockSendRaw(t *testing.T, host string, status int, body string) {
	t.Helper()
	gock.New(host).Post("").Persist().
		Filter(func(r *http.Request) bool {
			b := util.SafeReadBody(r)
			if strings.Contains(b, "custom_sendMevProtected") {
				t.Errorf("%s received the client method instead of the rewritten one: %s", host, b)
			}
			return strings.Contains(b, `"method":"eth_sendRawTransaction"`)
		}).
		Reply(status).
		JSON([]byte(body))
}

const sendRawOk = `{"jsonrpc":"2.0","id":1,"result":"0x2222222222222222222222222222222222222222222222222222222222222222"}`

func customSendReq() *common.NormalizedRequest {
	return common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"custom_sendMevProtected","params":["0x02f86b0180843b9aca00850c92a69c0082520894000000000000000000000000000000000000000080c001a0aa"]}`))
}

func TestNetwork_MethodRewrite_CustomMethodRoutedToRelays(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)
	setupGockObserver(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockSendRaw(t, upstreamHostFromID("relay-a"), 200, sendRawOk)
	mockSendRaw(t, upstreamHostFromID("relay-b"), 200, sendRawOk)
	mockSendRaw(t, upstreamHostFromID("regular"), 200, sendRawOk)

	ntw := setupMevRewriteNetwork(t, ctx)

	// Create the per-method slot, then tick it so the policy order applies.
	resp, err := ntw.Forward(ctx, customSendReq())
	require.NoError(t, err)
	resp.Release()
	policy.TickForTest(ntw.policyEngine, ntw.networkId, "custom_sendMevProtected")

	for i := 0; i < 5; i++ {
		resp, err := ntw.Forward(ctx, customSendReq())
		require.NoError(t, err)
		require.NotNil(t, resp)
		assert.Contains(t, []string{"relay-a", "relay-b"}, resp.Upstream().Id(), "custom method must be served by a mev relay")
		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Contains(t, jrr.GetResultString(), "0x2222", "relay response must return to the client unchanged")
		resp.Release()
	}
}

func TestNetwork_MethodRewrite_FallsBackToRegularWhenRelaysFail(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)
	setupGockObserver(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockSendRaw(t, upstreamHostFromID("relay-a"), 503, `{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"relay unavailable"}}`)
	mockSendRaw(t, upstreamHostFromID("relay-b"), 503, `{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"relay unavailable"}}`)
	mockSendRaw(t, upstreamHostFromID("regular"), 200, sendRawOk)

	ntw := setupMevRewriteNetwork(t, ctx)

	resp, err := ntw.Forward(ctx, customSendReq())
	require.NoError(t, err)
	resp.Release()
	policy.TickForTest(ntw.policyEngine, ntw.networkId, "custom_sendMevProtected")

	hitsMu.Lock()
	hits = map[string]int{}
	hitsMu.Unlock()
	resp, err = ntw.Forward(ctx, customSendReq())
	require.NoError(t, err)
	require.NotNil(t, resp)
	defer resp.Release()
	assert.Equal(t, "regular", resp.Upstream().Id(), "regular node must take the custom method once every relay failed")
	assert.Positive(t, gockHits(upstreamHostFromID("relay-a"))+gockHits(upstreamHostFromID("relay-b")), "relays must be tried before the fallback")
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	assert.Contains(t, jrr.GetResultString(), "0x2222")
}

func TestNetwork_MethodRewrite_OtherMethodsSkipRelays(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)
	setupGockObserver(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockClean(t, upstreamHostFromID("regular"), "eth_getBalance", "0x1111")
	for _, id := range []string{"relay-a", "relay-b"} {
		host := upstreamHostFromID(id)
		gock.New(host).Post("").Persist().
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
			}).
			Reply(200).
			JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":"0xbad"}`))
	}

	ntw := setupMevRewriteNetwork(t, ctx)
	policy.TickForTest(ntw.policyEngine, ntw.networkId, "*")

	resp, err := ntw.Forward(ctx, common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xabc","latest"]}`)))
	require.NoError(t, err)
	require.NotNil(t, resp)
	defer resp.Release()
	assert.Equal(t, "regular", resp.Upstream().Id())
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	assert.Equal(t, `"0x1111"`, jrr.GetResultString())
}

func customSendSignedReq() *common.NormalizedRequest {
	return common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"custom_sendMevProtected","params":["` + sampleSignedTx + `"]}`))
}

// A renamed send carries eth_sendRawTransaction's idempotency: when one
// relay fails after it may have broadcast and the retry reaches a relay
// that answers "already known", the client gets the transaction hash, not
// an error.
func TestNetwork_MethodRewrite_AlreadyKnownIsIdempotentSuccess(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)
	setupGockObserver(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockSendRaw(t, upstreamHostFromID("relay-a"), 503, `{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"relay unavailable"}}`)
	mockSendRaw(t, upstreamHostFromID("relay-b"), 200, `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"already known"}}`)
	mockSendRaw(t, upstreamHostFromID("regular"), 200, `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"regular must not be reached"}}`)

	ntw := setupMevRewriteNetwork(t, ctx)
	resp, _ := ntw.Forward(ctx, customSendSignedReq())
	if resp != nil {
		resp.Release()
	}
	policy.TickForTest(ntw.policyEngine, ntw.networkId, "custom_sendMevProtected")

	hitsMu.Lock()
	hits = map[string]int{}
	hitsMu.Unlock()
	resp, err := ntw.Forward(ctx, customSendSignedReq())
	require.NoError(t, err, "already known from a relay must become a success")
	require.NotNil(t, resp)
	defer resp.Release()
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	assert.Regexp(t, `^"0x[0-9a-f]{64}"$`, jrr.GetResultString(), "the hook answers with the transaction hash")
	assert.Equal(t, 0, gockHits(upstreamHostFromID("regular")), "the transaction is already known, the fallback node must not get it")
}

// A relay whose simulation reverts is retryable toward the network for a
// renamed send, as for eth_sendRawTransaction, so the fallback node is tried.
func TestNetwork_MethodRewrite_RelayRevertFallsBack(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)
	setupGockObserver(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	revert := `{"jsonrpc":"2.0","id":1,"error":{"code":3,"message":"execution reverted"}}`
	mockSendRaw(t, upstreamHostFromID("relay-a"), 200, revert)
	mockSendRaw(t, upstreamHostFromID("relay-b"), 200, revert)
	mockSendRaw(t, upstreamHostFromID("regular"), 200, sendRawOk)

	ntw := setupMevRewriteNetwork(t, ctx)
	resp, _ := ntw.Forward(ctx, customSendReq())
	if resp != nil {
		resp.Release()
	}
	policy.TickForTest(ntw.policyEngine, ntw.networkId, "custom_sendMevProtected")

	resp, err := ntw.Forward(ctx, customSendReq())
	require.NoError(t, err)
	require.NotNil(t, resp)
	defer resp.Release()
	assert.Equal(t, "regular", resp.Upstream().Id())
}
