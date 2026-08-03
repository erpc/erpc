package erpc

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/erpc/erpc/common"
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
