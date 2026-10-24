package erpc

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/internal/policy"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type seededSafeCache struct {
	reads atomic.Int32
}

func (c *seededSafeCache) Get(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	c.reads.Add(1)
	return common.NewNormalizedResponse().
		WithRequest(req).
		WithJsonRpcResponse(common.MustNewJsonRpcResponseFromBytes([]byte(`1`), []byte(`"0xcached"`), nil)), nil
}

func (*seededSafeCache) Set(context.Context, *common.NormalizedRequest, *common.NormalizedResponse) error {
	return nil
}

func (*seededSafeCache) IsObjectNull() bool { return false }

func TestNetwork_Forward_RoutesSafeBeforeCacheAndSelection(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	var sourceBody string
	var sourceCalls, otherCalls atomic.Int32
	gock.New("http://rpc1.localhost").Post("").
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			if !strings.Contains(body, `"method":"eth_getBalance"`) {
				return false
			}
			sourceBody = body
			sourceCalls.Add(1)
			return true
		}).
		Times(1).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":"0xsource"}`))
	gock.New("http://rpc2.localhost").Post("").Persist().
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			if strings.Contains(body, `"method":"eth_getBalance"`) {
				otherCalls.Add(1)
				return true
			}
			return false
		}).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":"0xother"}`))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	network := setupTestNetwork(t, ctx, []*common.UpstreamConfig{
		{Id: "source", Type: common.UpstreamTypeEvm, Endpoint: "http://rpc1.localhost", Tags: []string{"tier:source"}, Evm: &common.EvmUpstreamConfig{ChainId: 123}},
		{Id: "other", Type: common.UpstreamTypeEvm, Endpoint: "http://rpc2.localhost", Tags: []string{"tier:other"}, Evm: &common.EvmUpstreamConfig{ChainId: 123}},
	}, &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123, SafeBlockSource: "tier:source"},
	})
	cache := &seededSafeCache{}
	network.cacheDal = cache

	req := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xabc","safe"]}`,
	))
	req.SetDirectives(&common.RequestDirectives{UseUpstream: "tier:other", SkipInterpolation: true})
	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	defer resp.Release()

	jrr, err := resp.JsonRpcResponse(ctx)
	require.NoError(t, err)
	assert.Equal(t, `"0xsource"`, jrr.GetResultString())
	assert.Equal(t, int32(1), sourceCalls.Load())
	assert.Zero(t, otherCalls.Load(), "non-source upstream must not receive the user safe request")
	assert.Zero(t, cache.reads.Load(), "a provider-defined safe cache entry must not satisfy operator routing")
	assert.Contains(t, sourceBody, `"safe"`, "the selected source must receive the original safe tag")
}

// A safe-tagged request takes the safe source's Head, not the network-wide
// one: its toBlock "latest" never names a block above what the source lane
// serves, though the project reads the Head before Network.Forward.
func TestHttpServer_SafeGetLogsTakesTheSafeLaneHead(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	// Both upstreams sit on rpc1's poller mocks (head 0x11118888); the path
	// tells them apart. "other" then moves 5 blocks ahead.
	const sourceHead = int64(0x11118888)
	var sourceBody atomic.Value
	var otherCalls atomic.Int32
	gock.New("http://rpc1.localhost").Post("").Persist().
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			if !strings.Contains(body, `"eth_getLogs"`) {
				return false
			}
			if r.URL.Path == "/other" {
				otherCalls.Add(1)
			} else {
				sourceBody.Store(body)
			}
			return true
		}).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":[]}`))

	cfg := &common.Config{
		Server: &common.ServerConfig{MaxTimeout: common.Duration(10 * time.Second).Ptr()},
		Projects: []*common.ProjectConfig{{
			Id: "test_project",
			Networks: []*common.NetworkConfig{{
				Architecture: "evm",
				Evm: &common.EvmNetworkConfig{
					ChainId:         123,
					SafeBlockSource: "tier:source",
					ServedTip:       &common.EvmServedTipConfig{Quorum: 1},
				},
			}},
			Upstreams: []*common.UpstreamConfig{
				{Id: "source", Type: common.UpstreamTypeEvm, Endpoint: "http://rpc1.localhost/source", Tags: []string{"tier:source"},
					Evm: &common.EvmUpstreamConfig{ChainId: 123, StatePollerInterval: common.Duration(10 * time.Second)}},
				{Id: "other", Type: common.UpstreamTypeEvm, Endpoint: "http://rpc1.localhost/other", Tags: []string{"tier:other"},
					Evm: &common.EvmUpstreamConfig{ChainId: 123, StatePollerInterval: common.Duration(10 * time.Second)}},
			},
		}},
	}
	sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
	defer shutdown()
	prj, err := erpcInstance.GetProject("test_project")
	require.NoError(t, err)
	policy.OverrideAllForTest(prj.policyEngine)
	nw, err := prj.GetNetwork(context.Background(), "evm:123")
	require.NoError(t, err)

	for _, u := range nw.upstreamsRegistry.GetNetworkUpstreams(context.Background(), "evm:123") {
		require.Eventually(t, func() bool { return u.EvmStatePoller().LatestBlock() == sourceHead }, 5*time.Second, 5*time.Millisecond)
		if u.Id() == "other" {
			u.EvmStatePoller().SuggestLatestBlock(sourceHead + 5)
		}
	}
	require.Eventually(t, func() bool { return servedHead(nw, context.Background()).Latest == sourceHead+5 }, 5*time.Second, 5*time.Millisecond,
		"network-wide head: %+v", servedHead(nw, context.Background()))
	require.Equal(t, sourceHead, nw.headFor("tier:source").Latest, "the safe lane is 5 blocks behind the network-wide lane")

	status, _, body := sendRequest(`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{"fromBlock":"safe","toBlock":"latest"}]}`, nil, nil)
	require.Equal(t, http.StatusOK, status, "body=%s", body)
	sent, _ := sourceBody.Load().(string)
	require.NotEmpty(t, sent, "the safe source must receive the request; body=%s", body)
	assert.Contains(t, sent, `"toBlock":"0x11118888"`, "toBlock must be the safe lane's Latest, never the network-wide one")
	assert.Contains(t, sent, `"fromBlock":"safe"`)
	assert.Zero(t, otherCalls.Load(), "only the safe source serves a safe request")
}
