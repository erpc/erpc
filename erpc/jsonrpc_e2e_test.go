// End-to-end guards for the generic `jsonrpc` architecture (issue #1203).
//
// Conventions follow svm_hardening_e2e_test.go: real Network + real
// UpstreamsRegistry + real health tracker, only the upstream HTTP endpoints
// mocked with gock. gock is process-global state, so nothing in this file may
// run t.Parallel(), and every test uses its own set of hostnames.
//
// The replay-safety contract is asserted on PHYSICAL outbound calls, counted
// inside the gock filter, rather than on a retry counter: a leak through any
// of retry / hedge / the upstream sweep at either scope shows up as a second
// request on the wire.
package erpc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

const (
	jsonrpcE2ESlug  = "starknet"
	jsonrpcE2EHost1 = "jsonrpc-starknet-rpc1.localhost"
	jsonrpcE2EHost2 = "jsonrpc-starknet-rpc2.localhost"
)

// jsonrpcCountedMock registers a gock mock for (host, method) and hands back a
// counter of how many requests for that method actually reached that host.
// See svmCountedMock for why counting inside the Filter is exact.
func jsonrpcCountedMock(host, method string, times int, status int, body string) *atomic.Int64 {
	n := &atomic.Int64{}
	m := gock.New("http://" + host).Post("")
	if times > 0 {
		m = m.Times(times)
	} else {
		m = m.Persist()
	}
	m.Filter(func(r *http.Request) bool {
		if r.URL.Host != host {
			return false
		}
		if !strings.Contains(util.SafeReadBody(r), `"method":"`+method+`"`) {
			return false
		}
		n.Add(1)
		return true
	}).
		Reply(status).
		BodyString(body)
	return n
}

// jsonrpcE2EFailsafe is retry+hedge at ONE scope; it is applied at both the
// network and the upstream scope so any leak compounds into extra wire calls.
func jsonrpcE2EFailsafe() []*common.FailsafeConfig {
	return []*common.FailsafeConfig{{
		MatchMethod: "*",
		Retry:       &common.RetryPolicyConfig{MaxAttempts: 3},
		Hedge:       &common.HedgePolicyConfig{Delay: common.NewStaticDuration(10 * time.Millisecond), MaxCount: 1},
	}}
}

func jsonrpcUpstreamConfig(id, host string) *common.UpstreamConfig {
	return &common.UpstreamConfig{
		Id:       id,
		Type:     common.UpstreamTypeJsonRpc,
		Endpoint: "http://" + host,
		Generic:  &common.GenericUpstreamConfig{Slug: jsonrpcE2ESlug},
		Failsafe: jsonrpcE2EFailsafe(),
	}
}

func jsonrpcNetworkConfig() *common.NetworkConfig {
	return &common.NetworkConfig{
		Architecture: common.ArchitectureJsonRpc,
		Generic: &common.GenericNetworkConfig{
			Slug:              jsonrpcE2ESlug,
			ReplaySafeMethods: []string{"starknet_blockNumber", "starknet_get*"},
		},
		Failsafe: jsonrpcE2EFailsafe(),
	}
}

// setupTestJsonRpcNetwork builds a jsonrpc:starknet network backed by the
// given upstreams and pins rpc1 before rpc2 so the dispatch order is
// deterministic.
func setupTestJsonRpcNetwork(t *testing.T, ctx context.Context, upstreams []*common.UpstreamConfig) *Network {
	t.Helper()

	rateLimitersRegistry, _ := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{}, &log.Logger)
	metricsTracker := health.NewTracker(&log.Logger, "test", time.Minute)

	vr := thirdparty.NewVendorsRegistry()
	pr, err := thirdparty.NewProvidersRegistry(&log.Logger, vr, []*common.ProviderConfig{}, nil)
	require.NoError(t, err)

	ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"},
		},
	})
	require.NoError(t, err)

	upstreamsRegistry := upstream.NewUpstreamsRegistry(
		ctx, &log.Logger, "test",
		upstreams, ssr, rateLimitersRegistry, vr, pr,
		nil, metricsTracker, nil,
	)

	nwCfg := jsonrpcNetworkConfig()
	require.NoError(t, nwCfg.SetDefaults(upstreams, nil))
	network, err := NewNetwork(
		ctx, &log.Logger, "test", nwCfg,
		rateLimitersRegistry, upstreamsRegistry, metricsTracker, nil,
	)
	require.NoError(t, err)
	handler, err := common.GetArchitectureHandler(common.ArchitectureJsonRpc)
	require.NoError(t, err)
	network.architectureHandler = handler

	upstreamsRegistry.Bootstrap(ctx)
	time.Sleep(100 * time.Millisecond)
	require.NoError(t, upstreamsRegistry.PrepareUpstreamsForNetwork(ctx, util.JsonRpcNetworkId(jsonrpcE2ESlug)))
	network.PinUpstreamOrderForTest("rpc1", "rpc2")
	return network
}

func jsonrpcForward(ctx context.Context, net *Network, body string) (*common.NormalizedResponse, error) {
	req := common.NewNormalizedRequest([]byte(body))
	h := net.architectureHandler
	if handled, resp, err := h.HandleProjectPreForward(ctx, net, req); handled {
		return h.HandleNetworkPostForward(ctx, net, req, resp, err)
	}
	resp, err := net.Forward(ctx, req)
	return h.HandleNetworkPostForward(ctx, net, req, resp, err)
}

func TestJsonRpcArchitecture_E2E(t *testing.T) {
	t.Run("LoadBalancingAndFailover_ReplaySafe", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		healthy1 := jsonrpcCountedMock(jsonrpcE2EHost1, "starknet_blockNumber", 10, 200,
			`{"jsonrpc":"2.0","id":1,"result":1001}`)
		healthy2 := jsonrpcCountedMock(jsonrpcE2EHost2, "starknet_blockNumber", 0, 200,
			`{"jsonrpc":"2.0","id":1,"result":1002}`)

		net := setupTestJsonRpcNetwork(t, ctx, []*common.UpstreamConfig{
			jsonrpcUpstreamConfig("rpc1", jsonrpcE2EHost1),
			jsonrpcUpstreamConfig("rpc2", jsonrpcE2EHost2),
		})
		// Both healthy: unpin so the selection policy may spread the load.
		net.PinUpstreamOrderForTest()

		for i := range 10 {
			resp, err := jsonrpcForward(ctx, net, fmt.Sprintf(
				`{"jsonrpc":"2.0","id":%d,"method":"starknet_blockNumber","params":[]}`, i+1))
			require.NoError(t, err)
			require.NotNil(t, resp)
		}
		// 10 replay-safe reads with both healthy: no retries fire, hedges only
		// fire after 10ms and a mocked reply is immediate, so exactly 10 calls.
		require.EqualValues(t, 10, healthy1.Load()+healthy2.Load(),
			"healthy replay-safe reads must each cost exactly one physical call")

		// rpc1 is now persistently down (503); replay-safe reads fail over.
		util.ResetGock()
		down1 := jsonrpcCountedMock(jsonrpcE2EHost1, "starknet_blockNumber", 0, 503,
			`{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"Service unavailable"}}`)
		up2 := jsonrpcCountedMock(jsonrpcE2EHost2, "starknet_blockNumber", 0, 200,
			`{"jsonrpc":"2.0","id":1,"result":2002}`)
		net.PinUpstreamOrderForTest("rpc1", "rpc2")

		resp, err := jsonrpcForward(ctx, net,
			`{"jsonrpc":"2.0","id":11,"method":"starknet_blockNumber","params":[]}`)
		require.NoError(t, err, "a declared replay-safe read must fail over")
		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Equal(t, "2002", string(jrr.GetResultBytes()), "the healthy upstream's result must be returned verbatim")
		require.GreaterOrEqual(t, down1.Load(), int64(1), "rpc1 was tried first")
		require.GreaterOrEqual(t, up2.Load(), int64(1), "rpc2 served the failover")
		// Upstream-scope retries on rpc1 (maxAttempts), one successful call on
		// rpc2, plus at most one network-scope hedge (maxCount 1) that fires
		// while rpc1 is still backing off and lands on rpc2 as well.
		t.Logf("failover physical calls: rpc1=%d rpc2=%d", down1.Load(), up2.Load())
		require.LessOrEqual(t, down1.Load()+up2.Load(), int64(3+1+1),
			"failover must stay within retry.maxAttempts + 1 + hedge.maxCount")
	})

	t.Run("UndeclaredMethod_SingleDispatchOnUpstreamError", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Primary fails with a SERVER-side error — the one class that would
		// otherwise be retried across upstreams. The request may well have
		// taken effect before the error was produced.
		primary := jsonrpcCountedMock(jsonrpcE2EHost1, "starknet_addInvokeTransaction", 0, 500,
			`{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"Internal error"}}`)
		secondary := jsonrpcCountedMock(jsonrpcE2EHost2, "starknet_addInvokeTransaction", 0, 200,
			`{"jsonrpc":"2.0","id":1,"result":{"transaction_hash":"0xduplicate"}}`)

		net := setupTestJsonRpcNetwork(t, ctx, []*common.UpstreamConfig{
			jsonrpcUpstreamConfig("rpc1", jsonrpcE2EHost1),
			jsonrpcUpstreamConfig("rpc2", jsonrpcE2EHost2),
		})

		resp, err := jsonrpcForward(ctx, net,
			`{"jsonrpc":"2.0","id":1,"method":"starknet_addInvokeTransaction","params":[{"type":"INVOKE"}]}`)

		require.EqualValues(t, 1, primary.Load(),
			"an undeclared method must reach the primary exactly once")
		require.Zero(t, secondary.Load(),
			"an undeclared method was re-dispatched to a second upstream after an effective first attempt")
		require.EqualValues(t, 1, primary.Load()+secondary.Load(),
			"total physical calls must be exactly 1 despite retry+hedge at both scopes")

		// The failure must surface as an upstream failure — never as a success,
		// and never disguised as "rejected before execution".
		require.Error(t, err, "the primary's error must reach the caller")
		require.True(t, common.HasErrorCode(err, common.ErrCodeEndpointServerSideException),
			"the upstream's server-side failure must stay visible in the chain, got: %v", err)
		if resp != nil {
			if jrr, jerr := resp.JsonRpcResponse(); jerr == nil && jrr != nil {
				assert.NotContains(t, string(jrr.GetResultBytes()), "0xduplicate")
			}
		}
	})

	t.Run("UndeclaredMethod_AppErrorUnder200_PassThrough", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		primary := jsonrpcCountedMock(jsonrpcE2EHost1, "starknet_addInvokeTransaction", 0, 200,
			`{"jsonrpc":"2.0","id":1,"error":{"code":20,"message":"Contract not found","data":{"contract":"0xabc"}}}`)
		secondary := jsonrpcCountedMock(jsonrpcE2EHost2, "starknet_addInvokeTransaction", 0, 200,
			`{"jsonrpc":"2.0","id":1,"result":{"transaction_hash":"0xduplicate"}}`)
		healthy := jsonrpcCountedMock(jsonrpcE2EHost1, "starknet_blockNumber", 0, 200,
			`{"jsonrpc":"2.0","id":1,"result":1001}`)

		cfg := &common.Config{
			Projects: []*common.ProjectConfig{{
				Id:       "main",
				Networks: []*common.NetworkConfig{jsonrpcNetworkConfig()},
				Upstreams: []*common.UpstreamConfig{
					jsonrpcUpstreamConfig("rpc1", jsonrpcE2EHost1),
					jsonrpcUpstreamConfig("rpc2", jsonrpcE2EHost2),
				},
			}},
		}
		srv, base := httpPathsStart(t, cfg)
		nw, err := srv.erpc.GetNetwork(context.Background(), "main", util.JsonRpcNetworkId(jsonrpcE2ESlug))
		require.NoError(t, err)
		nw.PinUpstreamOrderForTest("rpc1", "rpc2")

		// Route resolution: /main/jsonrpc/<slug> reaches the network.
		status, _, body := httpPathsDo(t, http.MethodPost, base+"/main/jsonrpc/"+jsonrpcE2ESlug,
			[]byte(`{"jsonrpc":"2.0","id":1,"method":"starknet_blockNumber","params":[]}`), nil)
		require.Equal(t, http.StatusOK, status, "body: %s", body)
		require.Contains(t, body, `"result":1001`)
		require.EqualValues(t, 1, healthy.Load())

		status, _, body = httpPathsDo(t, http.MethodPost, base+"/main/jsonrpc/"+jsonrpcE2ESlug,
			[]byte(`{"jsonrpc":"2.0","id":1,"method":"starknet_addInvokeTransaction","params":[{"type":"INVOKE"}]}`), nil)
		require.Equal(t, http.StatusOK, status, "an application rejection under 200 stays 200, body: %s", body)

		var out struct {
			Error struct {
				Code    int             `json:"code"`
				Message string          `json:"message"`
				Data    json.RawMessage `json:"data"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &out), "body: %s", body)
		assert.Equal(t, 20, out.Error.Code, "the upstream's code must pass through verbatim, body: %s", body)
		assert.Equal(t, "Contract not found", out.Error.Message)
		var errData map[string]interface{}
		require.NoError(t, json.Unmarshal(out.Error.Data, &errData), "error.data must be preserved, body: %s", body)
		assert.Equal(t, "0xabc", errData["contract"])

		require.EqualValues(t, 1, primary.Load(), "the app error must cost exactly one physical call")
		require.Zero(t, secondary.Load(), "an app error under 200 must not be replayed on another upstream")
	})
}
