package erpc

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

func responseLimitTestUpstreams(count int, limit int64) []*common.UpstreamConfig {
	upstreams := make([]*common.UpstreamConfig, count)
	for i := range upstreams {
		id := fmt.Sprintf("rpc%d", i+1)
		upstreams[i] = &common.UpstreamConfig{
			Id:       id,
			Type:     common.UpstreamTypeEvm,
			Endpoint: "http://" + id + ".localhost",
			JsonRpc:  &common.JsonRpcUpstreamConfig{MaxResponseBytes: &limit},
			Evm:      &common.EvmUpstreamConfig{ChainId: 123},
		}
	}
	return upstreams
}

func responseLimitNetworkConfig(consensus *common.ConsensusPolicyConfig) *common.NetworkConfig {
	return &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
		Failsafe: []*common.FailsafeConfig{{
			MatchMethod: "*",
			Consensus:   consensus,
		}},
	}
}

func responseLimitMock(endpoint, body string) {
	gock.New(endpoint).
		Post("").
		Filter(func(req *http.Request) bool {
			return strings.Contains(util.SafeReadBody(req), "eth_getBalance")
		}).
		Times(1).
		Reply(200).
		BodyString(body)
}

func TestNetwork_ResponseSizeLimitFallback(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)
	responseLimitMock("http://rpc1.localhost", `{"jsonrpc":"2.0","id":1,"result":"`+strings.Repeat("x", 16<<10)+`"}`)
	responseLimitMock("http://rpc2.localhost", `{"jsonrpc":"2.0","id":1,"result":"0x1"}`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	network := setupTestNetwork(t, ctx, responseLimitTestUpstreams(2, 8<<10), responseLimitNetworkConfig(nil))
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`))
	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	defer resp.Release()
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	require.Equal(t, `"0x1"`, jrr.GetResultString())
}

func TestNetwork_ResponseSizeLimitConsensus(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)
	responseLimitMock("http://rpc1.localhost", `{"jsonrpc":"2.0","id":1,"result":"`+strings.Repeat("x", 16<<10)+`"}`)
	responseLimitMock("http://rpc2.localhost", `{"jsonrpc":"2.0","id":1,"result":"0x1"}`)
	responseLimitMock("http://rpc3.localhost", `{"jsonrpc":"2.0","id":1,"result":"0x1"}`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	consensus := &common.ConsensusPolicyConfig{
		MaxParticipants:         3,
		AgreementThreshold:      2,
		DisputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
		LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
	}
	require.NoError(t, consensus.SetDefaults())
	network := setupTestNetwork(t, ctx, responseLimitTestUpstreams(3, 8<<10), responseLimitNetworkConfig(consensus))
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`))
	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	defer resp.Release()
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	require.Equal(t, `"0x1"`, jrr.GetResultString())
}

func TestNetwork_ResponseSizeLimitNeverCachesPartialResponse(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)
	responseLimitMock("http://rpc1.localhost", `{"jsonrpc":"2.0","id":1,"result":"`+strings.Repeat("x", 16<<10)+`"}`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	network := setupTestNetwork(t, ctx, responseLimitTestUpstreams(1, 8<<10), responseLimitNetworkConfig(nil))
	cache := &common.MockCacheDal{}
	cache.On("Get", mock.Anything, mock.Anything).Return(nil, nil).Maybe()
	network.cacheDal = cache
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`))
	resp, err := network.Forward(ctx, req)
	require.Nil(t, resp)
	require.Error(t, err)
	require.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamResponseTooLarge))
	cache.AssertNotCalled(t, "Set", mock.Anything, mock.Anything, mock.Anything)
}
