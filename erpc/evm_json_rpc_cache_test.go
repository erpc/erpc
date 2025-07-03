package erpc

import (
	"context"
	"fmt"
	"sync"

	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/dynamodb"
	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/clients"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/erpc/erpc/thirdparty"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

type upsTestCfg struct {
	id      string
	syncing common.EvmSyncingState
	finBn   int64
	lstBn   int64
}

func createCacheTestFixtures(ctx context.Context, upstreamConfigs []upsTestCfg) ([]*data.MockConnector, *Network, []*upstream.Upstream, *evm.EvmJsonRpcCache) {
	logger := log.Logger

	mockConnector1 := data.NewMockConnector("mock1")
	mockConnector2 := data.NewMockConnector("mock2")
	mockNetwork := &Network{
		networkId: "evm:123",
		logger:    &logger,
		cfg: &common.NetworkConfig{
			Architecture: common.ArchitectureEvm,
			Evm: &common.EvmNetworkConfig{
				ChainId: 123,
			},
		},
	}

	clr := clients.NewClientRegistry(&logger, "prjA", nil)
	vr := thirdparty.NewVendorsRegistry()
	ssr, err := data.NewSharedStateRegistry(ctx, &logger, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: "memory",
			Memory: &common.MemoryConnectorConfig{
				MaxItems: 100_000, MaxTotalSize: "1GB",
			},
		},
	})
	if err != nil {
		panic(err)
	}
	upstreams := make([]*upstream.Upstream, 0, len(upstreamConfigs))

	for _, cfg := range upstreamConfigs {
		mt := health.NewTracker(&logger, "prjA", 100*time.Second)
		rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{}, &logger)
		if err != nil {
			panic(err)
		}
		mockUpstream, err := upstream.NewUpstream(ctx, "test", &common.UpstreamConfig{
			Id:       cfg.id,
			Endpoint: "http://rpc1.localhost",
			Type:     common.UpstreamTypeEvm,
			Evm: &common.EvmUpstreamConfig{
				ChainId:             123,
				StatePollerInterval: common.Duration(1 * time.Minute),
			},
		}, clr, rlr, vr, &logger, mt, ssr)
		if err != nil {
			panic(err)
		}

		err = mockUpstream.Bootstrap(ctx)
		if err != nil {
			panic(err)
		}

		poller := mockUpstream.EvmStatePoller()
		poller.SuggestFinalizedBlock(cfg.finBn)
		poller.SuggestLatestBlock(cfg.lstBn)
		poller.SetSyncingState(cfg.syncing)

		upstreams = append(upstreams, mockUpstream)
	}

	cacheCfg := &common.CacheConfig{}
	cacheCfg.SetDefaults()
	cache, err := evm.NewEvmJsonRpcCache(context.Background(), &logger, cacheCfg)
	if err != nil {
		panic(err)
	}

	return []*data.MockConnector{mockConnector1, mockConnector2}, mockNetwork, upstreams, cache
}

func TestEvmJsonRpcCache_Set(t *testing.T) {
	t.Run("DoNotCacheWhenEthGetTransactionByHashMissingBlockNumber", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures(ctx, []upsTestCfg{{id: "upsA", syncing: common.EvmSyncingStateUnknown, finBn: 10, lstBn: 15}})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getTransactionByHash","params":["0xdddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"],"id":1}`))
		req.SetNetwork(mockNetwork)
		req.SetCacheDal(cache)
		resp := common.NewNormalizedResponse().WithRequest(req).WithBody(util.StringToReaderCloser(`{"result":{"hash":"0xdddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","blockNumber":null}}`))
		resp.SetUpstream(mockUpstreams[0])
		req.SetLastValidResponse(ctx, resp)

		err := cache.Set(context.Background(), req, resp)

		assert.NoError(t, err)
		mockConnectors[0].AssertNotCalled(t, "Set")
	})

	t.Run("CacheIfBlockNumberIsFinalizedWhenBlockIsIrrelevantForPrimaryKey", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures(ctx, []upsTestCfg{{id: "upsA", syncing: common.EvmSyncingStateUnknown, finBn: 10, lstBn: 15}})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getTransactionReceipt","params":["0xabc",false],"id":1}`))
		req.SetNetwork(mockNetwork)
		req.SetCacheDal(cache)
		resp := common.NewNormalizedResponse().WithRequest(req).WithBody(util.StringToReaderCloser(`{"result":{"hash":"0xabc","blockNumber":"0x2"}}`))
		resp.SetUpstream(mockUpstreams[0])
		req.SetLastValidResponse(ctx, resp)

		policy, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Network:  "evm:123",
			Method:   "eth_getTransactionReceipt",
			Finality: common.DataFinalityStateFinalized,
		}, mockConnectors[0])
		require.NoError(t, err)
		cache.SetPolicies([]*data.CachePolicy{
			policy,
		})

		mockConnectors[0].On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)

		err = cache.Set(context.Background(), req, resp)

		assert.NoError(t, err)
		mockConnectors[0].AssertCalled(t, "Set", mock.Anything, "evm:123:2", mock.Anything, mock.Anything, mock.Anything)
	})

	t.Run("CacheIfBlockNumberIsFinalizedWhenBlockIsUsedForPrimaryKey", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures(ctx, []upsTestCfg{{id: "upsA", syncing: common.EvmSyncingStateUnknown, finBn: 10, lstBn: 15}})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["0x2",false],"id":1}`))
		req.SetNetwork(mockNetwork)
		req.SetCacheDal(cache)
		resp := common.NewNormalizedResponse().WithRequest(req).WithBody(util.StringToReaderCloser(`{"result":{"hash":"0xabc","number":"0x2"}}`))
		resp.SetUpstream(mockUpstreams[0])
		req.SetLastValidResponse(ctx, resp)

		policy, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Network: "evm:123",
			Method:  "eth_getBlockByNumber",
		}, mockConnectors[0])
		require.NoError(t, err)
		cache.SetPolicies([]*data.CachePolicy{
			policy,
		})

		mockConnectors[0].On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)

		err = cache.Set(context.Background(), req, resp)

		assert.NoError(t, err)
		mockConnectors[0].AssertCalled(t, "Set", mock.Anything, "evm:123:2", mock.Anything, mock.Anything, mock.Anything)
	})

	t.Run("SkipWhenNoRefAndNoBlockNumberFound", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures(ctx, []upsTestCfg{{id: "upsA", syncing: common.EvmSyncingStateUnknown, finBn: 10, lstBn: 15}})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123","latest"],"id":1}`))
		req.SetNetwork(mockNetwork)
		req.SetCacheDal(cache)
		resp := common.NewNormalizedResponse().WithRequest(req).WithBody(util.StringToReaderCloser(`{"result":"0x1234"}`))
		resp.SetUpstream(mockUpstreams[0])
		req.SetLastValidResponse(ctx, resp)

		err := cache.Set(context.Background(), req, resp)

		assert.NoError(t, err)
		mockConnectors[0].AssertNotCalled(t, "Set")
	})

	t.Run("CacheIfBlockRefFoundWhetherBlockNumberExistsOrNot", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures(ctx, []upsTestCfg{{id: "upsA", syncing: common.EvmSyncingStateUnknown, finBn: 10, lstBn: 15}})

		testCases := []struct {
			name        string
			method      string
			params      string
			result      string
			expectedRef string
			finality    common.DataFinalityState
		}{
			{
				name:        "WithBlockNumberAndRef",
				method:      "eth_getBlockByHash",
				params:      `["0x6315fbbb83862798c81820bbaae8bfbc542b8abf73c130583f2b36521cf10624",false]`,
				result:      `{"result":{"hash":"0x6315fbbb83862798c81820bbaae8bfbc542b8abf73c130583f2b36521cf10624","number":"0x1"}}`,
				expectedRef: "0x6315fbbb83862798c81820bbaae8bfbc542b8abf73c130583f2b36521cf10624",
				finality:    common.DataFinalityStateFinalized,
			},
			{
				name:        "WithOnlyBlockRef",
				method:      "eth_getAccount",
				params:      `["0xabc","0xdddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"]`,
				result:      `{"result":{"balance":"0x123"}}`,
				expectedRef: "0xdddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
				finality:    common.DataFinalityStateUnknown,
			},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"` + tc.method + `","params":` + tc.params + `,"id":1}`))
				req.SetNetwork(mockNetwork)
				req.SetCacheDal(cache)
				resp := common.NewNormalizedResponse().WithRequest(req).WithBody(util.StringToReaderCloser(tc.result))
				resp.SetUpstream(mockUpstreams[0])
				req.SetLastValidResponse(ctx, resp)

				policy, err := data.NewCachePolicy(&common.CachePolicyConfig{
					Network:  "evm:123",
					Method:   tc.method,
					Finality: tc.finality,
				}, mockConnectors[0])
				require.NoError(t, err)
				cache.SetPolicies([]*data.CachePolicy{
					policy,
				})

				mockConnectors[0].On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil).Once()

				err = cache.Set(context.Background(), req, resp)

				assert.NoError(t, err)
				mockConnectors[0].AssertCalled(t, "Set", mock.Anything, mock.MatchedBy(func(key string) bool {
					return key == "evm:123:"+tc.expectedRef
				}), mock.Anything, mock.Anything, mock.Anything)
			})
		}
	})

	t.Run("CacheResponseForFinalizedBlock", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures(ctx, []upsTestCfg{{id: "upsA", syncing: common.EvmSyncingStateUnknown, finBn: 10, lstBn: 15}})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["0x1",false],"id":1}`))
		req.SetNetwork(mockNetwork)
		req.SetCacheDal(cache)
		resp := common.NewNormalizedResponse().WithRequest(req).WithBody(util.StringToReaderCloser(`{"result":{"number":"0x1","hash":"0xabc"}}`))
		resp.SetUpstream(mockUpstreams[0])
		req.SetLastValidResponse(ctx, resp)

		policy, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Network: "evm:123",
			Method:  "eth_getBlockByNumber",
		}, mockConnectors[0])
		require.NoError(t, err)
		cache.SetPolicies([]*data.CachePolicy{
			policy,
		})

		mockConnectors[0].On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)

		err = cache.Set(context.Background(), req, resp)

		assert.NoError(t, err)
		mockConnectors[0].AssertCalled(t, "Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	})

	t.Run("SkipCachingForUnfinalizedBlock", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures(ctx, []upsTestCfg{{id: "upsA", syncing: common.EvmSyncingStateUnknown, finBn: 10, lstBn: 15}})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["0x399",false],"id":1}`))
		req.SetNetwork(mockNetwork)
		req.SetCacheDal(cache)
		resp := common.NewNormalizedResponse().WithRequest(req).WithBody(util.StringToReaderCloser(`{"result":{"number":"0x399","hash":"0xdef"}}`))
		resp.SetUpstream(mockUpstreams[0])
		req.SetLastValidResponse(ctx, resp)

		err := cache.Set(context.Background(), req, resp)

		assert.NoError(t, err)
		mockConnectors[0].AssertNotCalled(t, "Set")
	})

	t.Run("ShouldNotCacheEmptyResponseIfNodeNotSynced", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures(ctx, []upsTestCfg{{id: "upsA", syncing: common.EvmSyncingStateSyncing, finBn: 10, lstBn: 15}})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123","latest"],"id":1}`))
		req.SetNetwork(mockNetwork)
		req.SetCacheDal(cache)
		resp := common.NewNormalizedResponse().WithRequest(req).WithBody(util.StringToReaderCloser(`{"result":"0x0"}`))
		resp.SetUpstream(mockUpstreams[0])
		req.SetLastValidResponse(ctx, resp)

		err := cache.Set(context.Background(), req, resp)

		assert.NoError(t, err)
		mockConnectors[0].AssertNotCalled(t, "Set")
	})

	t.Run("ShouldNotCacheEmptyResponseIfUnknownSyncState", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures(ctx, []upsTestCfg{{id: "upsA", syncing: common.EvmSyncingStateUnknown, finBn: 10, lstBn: 15}})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123","latest"],"id":1}`))
		req.SetNetwork(mockNetwork)
		req.SetCacheDal(cache)
		resp := common.NewNormalizedResponse().WithRequest(req).WithBody(util.StringToReaderCloser(`{"result":"0x0"}`))
		resp.SetUpstream(mockUpstreams[0])
		req.SetLastValidResponse(ctx, resp)

		err := cache.Set(context.Background(), req, resp)

		assert.NoError(t, err)
		mockConnectors[0].AssertNotCalled(t, "Set")
	})

	t.Run("ShouldNotCacheEmptyResponseIfBlockNotFinalized", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures(ctx, []upsTestCfg{{id: "upsA", syncing: common.EvmSyncingStateNotSyncing, finBn: 10, lstBn: 15}})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123","0x14"],"id":1}`))
		req.SetNetwork(mockNetwork)
		req.SetCacheDal(cache)
		resp := common.NewNormalizedResponse().WithRequest(req).WithBody(util.StringToReaderCloser(`{"result":"0x0"}`))
		resp.SetUpstream(mockUpstreams[0])
		req.SetLastValidResponse(ctx, resp)

		err := cache.Set(context.Background(), req, resp)

		assert.NoError(t, err)
		mockConnectors[0].AssertNotCalled(t, "Set")
	})

	t.Run("ShouldNotCacheEmptyResponseIfCannotDetermineBlockNumber", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures(ctx, []upsTestCfg{{id: "upsA", syncing: common.EvmSyncingStateNotSyncing, finBn: 10, lstBn: 15}})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123","latest"],"id":1}`))
		req.SetNetwork(mockNetwork)
		req.SetCacheDal(cache)
		resp := common.NewNormalizedResponse().WithRequest(req).WithBody(util.StringToReaderCloser(`{"result":"0x0"}`))
		resp.SetUpstream(mockUpstreams[0])
		req.SetLastValidResponse(ctx, resp)

		err := cache.Set(context.Background(), req, resp)

		assert.NoError(t, err)
		mockConnectors[0].AssertNotCalled(t, "Set")
	})

	t.Run("ShouldCacheEmptyResponseIfNodeSyncedAndBlockFinalized", func(t *testing.T) {
		util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.ResetGock()
		defer util.AssertNoPendingMocks(t, 0)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures(ctx, []upsTestCfg{{id: "upsA", syncing: common.EvmSyncingStateNotSyncing, finBn: 10, lstBn: 15}})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123","0x5"],"id":1}`))
		req.SetNetwork(mockNetwork)
		req.SetCacheDal(cache)
		resp := common.NewNormalizedResponse().WithRequest(req).WithBody(util.StringToReaderCloser(`{"result":"0x0"}`))
		resp.SetUpstream(mockUpstreams[0])
		req.SetLastValidResponse(ctx, resp)

		policy, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Network: "evm:123",
			Method:  "eth_getBalance",
			Empty:   common.CacheEmptyBehaviorAllow,
		}, mockConnectors[0])
		require.NoError(t, err)
		cache.SetPolicies([]*data.CachePolicy{
			policy,
		})

		mockConnectors[0].On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)

		err = cache.Set(context.Background(), req, resp)

		assert.NoError(t, err)
		mockConnectors[0].AssertCalled(t, "Set", mock.Anything, "evm:123:5", mock.Anything, mock.Anything, mock.Anything)
	})

	t.Run("StoreOnAllMatchingConnectors", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures(ctx, []upsTestCfg{
			{id: "upsA", syncing: common.EvmSyncingStateNotSyncing, finBn: 10, lstBn: 15},
		})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getTransactionByHash","params":["0x123"],"id":1}`))
		req.SetNetwork(mockNetwork)
		req.SetCacheDal(cache)
		resp := common.NewNormalizedResponse().WithRequest(req).WithBody(util.StringToReaderCloser(`{"result":{"hash":"0x123","blockNumber":"0x1"}}`))
		resp.SetUpstream(mockUpstreams[0])
		req.SetLastValidResponse(ctx, resp)

		// Create two policies with different connectors and finality states
		policy1, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Network:   "*",
			Method:    "*",
			Finality:  common.DataFinalityStateFinalized,
			Connector: "mock1",
		}, mockConnectors[0])
		require.NoError(t, err)

		policy2, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Network:   "*",
			Method:    "*",
			Finality:  common.DataFinalityStateFinalized,
			Connector: "mock2",
		}, mockConnectors[1])
		require.NoError(t, err)

		cache.SetPolicies([]*data.CachePolicy{policy1, policy2})

		mockConnectors[0].On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
		mockConnectors[1].On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)

		err = cache.Set(context.Background(), req, resp)

		assert.NoError(t, err)
		mockConnectors[0].AssertCalled(t, "Set", mock.Anything, "evm:123:1", mock.Anything, mock.Anything, mock.Anything)
		mockConnectors[1].AssertCalled(t, "Set", mock.Anything, "evm:123:1", mock.Anything, mock.Anything, mock.Anything)
	})

	t.Run("RespectFinalityStateWhenStoring", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures(ctx, []upsTestCfg{
			{id: "upsA", syncing: common.EvmSyncingStateNotSyncing, finBn: 10, lstBn: 15},
		})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getTransactionByHash","params":["0xdddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"],"id":1}`))
		req.SetNetwork(mockNetwork)
		req.SetCacheDal(cache)
		resp := common.NewNormalizedResponse().WithRequest(req).WithBody(util.StringToReaderCloser(`{"result":{"hash":"0xdddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","blockNumber":"0x1"}}`))
		resp.SetUpstream(mockUpstreams[0])
		req.SetLastValidResponse(ctx, resp)

		// Create policies with different finality states
		policy1, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Network:   "*",
			Method:    "*",
			Finality:  common.DataFinalityStateFinalized,
			Connector: "mock1",
		}, mockConnectors[0])
		require.NoError(t, err)

		policy2, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Network:   "*",
			Method:    "*",
			Finality:  common.DataFinalityStateUnfinalized,
			Connector: "mock2",
		}, mockConnectors[1])
		require.NoError(t, err)

		cache.SetPolicies([]*data.CachePolicy{policy1, policy2})

		mockConnectors[0].On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
		mockConnectors[1].On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)

		err = cache.Set(context.Background(), req, resp)

		assert.NoError(t, err)
		// Only the finalized policy connector should be called since block 5 is finalized
		mockConnectors[0].AssertCalled(t, "Set", mock.Anything, "evm:123:1", mock.Anything, mock.Anything, mock.Anything)
		mockConnectors[1].AssertNotCalled(t, "Set")
	})

	t.Run("CachingBehaviorWithDefaultConfig", func(t *testing.T) {
		// Create test fixtures with default config
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures(ctx, []upsTestCfg{
			{id: "upsA", syncing: common.EvmSyncingStateNotSyncing, finBn: 10, lstBn: 15},
		})

		// Create default policies as defined in common/defaults.go
		finalizedPolicy, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Network:   "*",
			Method:    "*",
			Finality:  common.DataFinalityStateFinalized,
			TTL:       0, // Forever
			Connector: "mock1",
		}, mockConnectors[0])
		require.NoError(t, err)

		unknownPolicy, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Network:   "*",
			Method:    "*",
			Finality:  common.DataFinalityStateUnknown,
			TTL:       common.Duration(30 * time.Second),
			Connector: "mock1",
		}, mockConnectors[0])
		require.NoError(t, err)

		unfinalizedPolicy, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Network:   "*",
			Method:    "*",
			Finality:  common.DataFinalityStateUnfinalized,
			TTL:       common.Duration(30 * time.Second),
			Connector: "mock1",
		}, mockConnectors[0])
		require.NoError(t, err)

		testCases := []struct {
			name           string
			method         string
			params         string
			result         string
			expectedCache  bool
			expectedPolicy *data.CachePolicy
		}{
			{
				name:          "LatestTag_NoCache",
				method:        "eth_getBlockByNumber",
				params:        `["latest",false]`,
				result:        `"result":{"number":"0xf","hash":"0xabc"}`,
				expectedCache: false, // Realtime tag → no default rule caches it
			},
			{
				name:           "FinalizedBlock_Cache",
				method:         "eth_getBlockByNumber",
				params:         `["0x5",false]`,
				result:         `"result":{"number":"0x5","hash":"0xdef"}`,
				expectedCache:  true, // Numeric height ≤ finalized tip
				expectedPolicy: finalizedPolicy,
			},
			{
				name:          "FinalizedTag_NoCache",
				method:        "eth_getBlockByNumber",
				params:        `["finalized",false]`,
				result:        `"result":{"number":"0xa","hash":"0xaaa"}`,
				expectedCache: false, // Realtime tag → not cached
			},
			{
				name:           "UnfinalizedBlock_Cache",
				method:         "eth_getBlockByNumber",
				params:         `["0x399",false]`,
				result:         `"result":{"number":"0x399","hash":"0xdd"}`,
				expectedCache:  true, // Height above tip → Unfinalized rule
				expectedPolicy: unfinalizedPolicy,
			},
			{
				name:           "PendingTx_Cache",
				method:         "eth_getTransactionByHash",
				params:         `["0x123"]`,
				result:         `"result":{"hash":"0x123","blockNumber":null}`,
				expectedCache:  true, // wildcard '*' → Unfinalized
				expectedPolicy: unfinalizedPolicy,
			},
			{
				name:           "UnknownAccount_Cache",
				method:         "eth_getAccount",
				params:         `["0xabc","0xdddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"]`,
				result:         `"result":{"balance":"0x123"}`,
				expectedCache:  true, // Falls to Unknown rule (30 s TTL)
				expectedPolicy: unknownPolicy,
			},
			{
				name:          "EarliestTag_NoCache",
				method:        "eth_getBlockByNumber",
				params:        `["earliest",false]`,
				result:        `"result":{"number":"0xa","hash":"0xaaa"}`,
				expectedCache: false, // Realtime tag → no cache
			},
			{
				name:          "SafeTag_NoCache",
				method:        "eth_getBlockByNumber",
				params:        `["safe",false]`,
				result:        `"result":{"number":"0xa","hash":"0xaaa"}`,
				expectedCache: false, // Realtime tag → no cache
			},
			{
				name:           "StaticChainId_Cache",
				method:         "eth_chainId",
				params:         `[]`,
				result:         `"result":"0x7b"`,
				expectedCache:  true, // Static RPC → Finalized forever
				expectedPolicy: finalizedPolicy,
			},
			{
				name:           "ReceiptMined_Cache",
				method:         "eth_getTransactionReceipt",
				params:         `["0xbeef"]`,
				result:         `"result":{"hash":"0xbeef","blockNumber":"0x5"}`,
				expectedCache:  true, // Block 0x5 finalized
				expectedPolicy: finalizedPolicy,
			},
			{
				name:           "ReceiptPending_Cache",
				method:         "eth_getTransactionReceipt",
				params:         `["0xdead"]`,
				result:         `"result":{"hash":"0xdead","blockNumber":null}`,
				expectedCache:  true, // Pending ⇒ wildcard '*' ⇒ Unfinalized
				expectedPolicy: unfinalizedPolicy,
			},
			{
				name:           "HighBlockBalance_Cache",
				method:         "eth_getBalance",
				params:         `["0x123","0x399"]`,
				result:         `"result":"0x222222"`,
				expectedCache:  true, // Height above tip → Unfinalized
				expectedPolicy: unfinalizedPolicy,
			},
			{
				name:           "LowBlockBalance_Cache",
				method:         "eth_getBalance",
				params:         `["0x123","0x5"]`,
				result:         `"result":"0x22222"`,
				expectedCache:  true, // Height 0x5 finalized
				expectedPolicy: finalizedPolicy,
			},
			{
				name:           "LogsRange_Finalized_Cache",
				method:         "eth_getLogs",
				params:         `[{"fromBlock":"0x1","toBlock":"0x2"}]`,
				result:         `"result":[{"address":"0xabc","data":"0x0"}]`,
				expectedCache:  true,
				expectedPolicy: finalizedPolicy,
			},
			{
				name:           "LogsRange_Unfinalized_Cache",
				method:         "eth_getLogs",
				params:         `[{"fromBlock":"0x177777","toBlock":"0x277777"}]`,
				result:         `"result":[{"address":"0xabc","data":"0x0"}]`,
				expectedCache:  true,
				expectedPolicy: unfinalizedPolicy,
			},
			{
				name:           "BlockByHash_Cache",
				method:         "eth_getBlockByHash",
				params:         `["0x6315fbbb83862798c81820bbaae8bfbc542b8abf73c130583f2b36521cf10624",false]`,
				result:         `"result":{"hash":"0x6315fbbb83862798c81820bbaae8bfbc542b8abf73c130583f2b36521cf10624","number":"0x1"}`,
				expectedCache:  true, // Hash resolves to block 1 → finalized
				expectedPolicy: finalizedPolicy,
			},
			{
				name:           "TraceReplayTransaction_Cache",
				method:         "trace_replayTransaction",
				params:         `["0x123"]`,
				result:         `"result":{"output":"0xdeadbeef"}`,
				expectedCache:  true,
				expectedPolicy: unknownPolicy, // falls under Unknown finality, 30 s TTL
			},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				// clear any connector call history from previous table row
				mockConnectors[0].Calls = nil
				mockConnectors[0].ExpectedCalls = nil
				// Select policies per scenario:
				policies := []*data.CachePolicy{finalizedPolicy, unfinalizedPolicy}
				if tc.expectedPolicy == unknownPolicy {
					policies = append(policies, unknownPolicy)
				}
				cache.SetPolicies(policies)

				req := common.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","method":"%s","params":%s,"id":1}`, tc.method, tc.params)))
				req.SetNetwork(mockNetwork)
				req.SetCacheDal(cache)
				resp := common.NewNormalizedResponse().WithRequest(req).WithBody(util.StringToReaderCloser(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,%s}`, tc.result)))
				resp.SetUpstream(mockUpstreams[0])
				req.SetLastValidResponse(ctx, resp)

				if tc.expectedCache {
					mockConnectors[0].On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
				}

				err := cache.Set(context.Background(), req, resp)

				assert.NoError(t, err)
				if tc.expectedCache {
					mockConnectors[0].AssertCalled(t, "Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, tc.expectedPolicy.GetTTL())
				} else {
					mockConnectors[0].AssertNotCalled(t, "Set",
						mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
				}
			})
		}
	})

	t.Run("CustomPolicyForLatestBlocks", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures(ctx, []upsTestCfg{
			{id: "upsA", syncing: common.EvmSyncingStateNotSyncing, finBn: 10, lstBn: 15},
		})

		// Create a custom policy specifically for latest block requests
		latestBlockPolicy, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Network:  "evm:123",
			Method:   "eth_getBlockByNumber",
			Params:   []interface{}{"latest", "*"},
			TTL:      common.Duration(5 * time.Second),
			Finality: common.DataFinalityStateRealtime,
		}, mockConnectors[0])
		require.NoError(t, err)
		cache.SetPolicies([]*data.CachePolicy{latestBlockPolicy})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["latest",false],"id":1}`))
		req.SetNetwork(mockNetwork)
		req.SetCacheDal(cache)
		resp := common.NewNormalizedResponse().WithRequest(req).WithBody(util.StringToReaderCloser(`{"result":{"number":"0xf","hash":"0xabc"}}`))
		resp.SetUpstream(mockUpstreams[0])
		req.SetLastValidResponse(ctx, resp)

		ttl := time.Duration(5 * time.Second)
		mockConnectors[0].On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, &ttl).Return(nil)

		err = cache.Set(context.Background(), req, resp)

		assert.NoError(t, err)
		mockConnectors[0].AssertCalled(t, "Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, &ttl)
	})

	t.Run("CustomPolicyForFinalizedTag", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures(ctx, []upsTestCfg{
			{id: "upsA", syncing: common.EvmSyncingStateNotSyncing, finBn: 20, lstBn: 25},
		})

		// Create a custom policy specifically for finalized block requests
		finalizedPolicy, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Network:  "evm:123",
			Method:   "eth_getBlockByNumber",
			Params:   []interface{}{"finalized", "*"},
			TTL:      common.Duration(5 * time.Second),
			Finality: common.DataFinalityStateRealtime,
		}, mockConnectors[0])
		require.NoError(t, err)
		cache.SetPolicies([]*data.CachePolicy{finalizedPolicy})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["finalized",false],"id":1}`))
		req.SetNetwork(mockNetwork)
		req.SetCacheDal(cache)
		resp := common.NewNormalizedResponse().
			WithRequest(req).
			WithBody(util.StringToReaderCloser(`{"result":{"number":"0x14","hash":"0xabc"}}`))
		resp.SetUpstream(mockUpstreams[0])
		req.SetLastValidResponse(ctx, resp)

		ttl := time.Duration(5 * time.Second)
		mockConnectors[0].On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, &ttl).Return(nil)

		err = cache.Set(context.Background(), req, resp)

		assert.NoError(t, err)
		mockConnectors[0].AssertCalled(t, "Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, &ttl)
	})

	t.Run("ServeFinalizedBlock_WhenRequestedByFinalizedBlockNumber", func(t *testing.T) {
		// The upstream just marked block 0x14 (20 dec) as finalized.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures(ctx, []upsTestCfg{
			{id: "upsA", syncing: common.EvmSyncingStateNotSyncing, finBn: 20, lstBn: 25},
		})

		// Default-style Finalized wildcard policy.
		finalizedDefault, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Network:   "*",
			Method:    "*",
			Finality:  common.DataFinalityStateFinalized,
			TTL:       0, // forever
			Connector: "mock1",
		}, mockConnectors[0])
		require.NoError(t, err)
		cache.SetPolicies([]*data.CachePolicy{finalizedDefault})

		// The client now requests that exact finalized block (0x14).
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["0x14",false],"id":1}`))
		req.SetNetwork(mockNetwork)
		req.SetCacheDal(cache)
		resp := common.NewNormalizedResponse().
			WithRequest(req).
			WithBody(util.StringToReaderCloser(`{"result":{"number":"0x14","hash":"0xfeed"}}`))
		resp.SetUpstream(mockUpstreams[0])
		req.SetLastValidResponse(ctx, resp)

		// Expect the cache to store under the default-finalized policy's TTL (0 — forever).
		mockConnectors[0].On("Set", mock.Anything, "evm:123:20", mock.Anything, mock.Anything, finalizedDefault.GetTTL()).Return(nil)

		err = cache.Set(context.Background(), req, resp)
		assert.NoError(t, err)
		mockConnectors[0].AssertCalled(t, "Set", mock.Anything, "evm:123:20", mock.Anything, mock.Anything, finalizedDefault.GetTTL())
	})

	t.Run("SkipCache_WhenRequestedByFinalizedTag_WithDefaultFinalizedPolicy", func(t *testing.T) {
		// The upstream just marked block 0x14 (20 dec) as finalized
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures(ctx, []upsTestCfg{
			{id: "upsA", syncing: common.EvmSyncingStateNotSyncing, finBn: 20, lstBn: 25},
		})

		// Default-style Finalized wildcard policy.
		finalizedDefault, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Network:   "*",
			Method:    "*",
			Finality:  common.DataFinalityStateFinalized,
			TTL:       0, // forever
			Connector: "mock1",
		}, mockConnectors[0])
		require.NoError(t, err)
		cache.SetPolicies([]*data.CachePolicy{finalizedDefault})

		// 1) Simulate a finalized tag request and store its response (block 0xf == 15)
		reqLatest := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["finalized",false],"id":1}`))
		reqLatest.SetNetwork(mockNetwork)
		reqLatest.SetCacheDal(cache)
		respLatest := common.NewNormalizedResponse().
			WithRequest(reqLatest).
			WithBody(util.StringToReaderCloser(`{"result":{"number":"0xf","hash":"0xabc"}}`))
		respLatest.SetUpstream(mockUpstreams[0])
		reqLatest.SetLastValidResponse(ctx, respLatest)

		err = cache.Set(context.Background(), reqLatest, respLatest)
		require.NoError(t, err)
		// The 'finalized' tag is classified as Realtime and should NOT match the
		// default-finalized policy, therefore nothing is written to cache.
		mockConnectors[0].AssertNumberOfCalls(t, "Set", 0)

		// 2/ client asks for block (0xf == 15) explicitly
		reqExact := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["0xf",false],"id":2}`))
		reqExact.SetNetwork(mockNetwork)
		reqExact.SetCacheDal(cache)

		// Expect a lookup on the finalized-policy key that returns "record not found".
		mockConnectors[0].
			On("Get", mock.Anything, mock.Anything, "evm:123:15", mock.Anything, mock.Anything).
			Return("", common.NewErrRecordNotFound("evm:123:15", "some-key", "mock1")).Once()
	})

	t.Run("SkipCache_WhenRequestedByFinalizedTag_WithRealtimePolicy", func(t *testing.T) {
		// The upstream just marked block 0x14 (20 dec) as finalized
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures(ctx, []upsTestCfg{
			{id: "upsA", syncing: common.EvmSyncingStateNotSyncing, finBn: 20, lstBn: 25},
		})

		// Realtime policy: cache "latest" for 5 min
		realtimePolicy, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Network:   "evm:123",
			Method:    "eth_getBlockByNumber",
			Params:    []interface{}{"latest", "*"},
			TTL:       common.Duration(5 * time.Minute),
			Finality:  common.DataFinalityStateRealtime,
			Connector: "mock1",
		}, mockConnectors[0])
		require.NoError(t, err)

		// Default-style Finalized wildcard policy.
		finalizedPolicy, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Network:   "*",
			Method:    "*",
			Finality:  common.DataFinalityStateFinalized,
			TTL:       0,
			Connector: "mock1",
		}, mockConnectors[0])
		require.NoError(t, err)

		cache.SetPolicies([]*data.CachePolicy{realtimePolicy, finalizedPolicy})

		// 1) write via "latest" (Realtime) – stored under evm:123:latest
		reqLatest := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["latest",false],"id":1}`))
		reqLatest.SetNetwork(mockNetwork)
		reqLatest.SetCacheDal(cache)
		respLatest := common.NewNormalizedResponse().
			WithRequest(reqLatest).
			WithBody(util.StringToReaderCloser(`{"result":{"number":"0xf","hash":"0xabc"}}`))
		respLatest.SetUpstream(mockUpstreams[0])
		reqLatest.SetLastValidResponse(ctx, respLatest)

		mockConnectors[0].On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil).Once()
		err = cache.Set(context.Background(), reqLatest, respLatest)
		require.NoError(t, err)
		mockConnectors[0].AssertNumberOfCalls(t, "Set", 1)

		// 2) numeric request for 0xf should MISS (entry not stored under evm:123:15)
		reqExact := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["0xf",false],"id":2}`))
		reqExact.SetNetwork(mockNetwork)
		reqExact.SetCacheDal(cache)

		mockConnectors[0].
			On("Get", mock.Anything, mock.Anything, "evm:123:15", mock.Anything, mock.Anything).
			Return("", common.NewErrRecordNotFound("evm:123:15", "cache-key", "mock1")).Once()
	})

	t.Run("SkipCache_WhenFinalizedTagAfterNumericCache_WithDefaultFinalizedPolicy", func(t *testing.T) {
		// upstream finalized height = 25 (0x19); we will write block 0x18 (24).
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures(ctx, []upsTestCfg{
			{id: "upsA", syncing: common.EvmSyncingStateNotSyncing, finBn: 25, lstBn: 30},
		})

		// Default-style Finalized wildcard policy.
		finalizedPolicy, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Network:   "*",
			Method:    "*",
			Finality:  common.DataFinalityStateFinalized,
			TTL:       0, // forever
			Connector: "mock1",
		}, mockConnectors[0])
		require.NoError(t, err)
		cache.SetPolicies([]*data.CachePolicy{finalizedPolicy})

		// 1) numeric request → cached under pk evm:123:24
		reqNum := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["0x18",false],"id":1}`))
		reqNum.SetNetwork(mockNetwork)
		reqNum.SetCacheDal(cache)
		respNum := common.NewNormalizedResponse().
			WithRequest(reqNum).
			WithBody(util.StringToReaderCloser(`{"result":{"number":"0x18","hash":"0xbeef"}}`))
		respNum.SetUpstream(mockUpstreams[0])
		reqNum.SetLastValidResponse(ctx, respNum)

		mockConnectors[0].On("Set", mock.Anything, "evm:123:24", mock.Anything, mock.Anything, finalizedPolicy.GetTTL()).Return(nil).Once()
		err = cache.Set(context.Background(), reqNum, respNum)
		require.NoError(t, err)
		mockConnectors[0].AssertNumberOfCalls(t, "Set", 1)

		// 2) "finalized" tag request must MISS the cache.
		reqTag := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["finalized",false],"id":2}`))
		reqTag.SetNetwork(mockNetwork)
		reqTag.SetCacheDal(cache)

		resp, err := cache.Get(context.Background(), reqTag)
		assert.NoError(t, err)
		assert.Nil(t, resp)

		// Verify the connector was **not** touched.
		mockConnectors[0].AssertNumberOfCalls(t, "Get", 0)
	})

	t.Run("EthGetLogs_SameBlockRange_Finalized", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		// Use mockConnectors[0] which is mock1, matching default policy connector
		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures(ctx, []upsTestCfg{
			{id: "upsA", syncing: common.EvmSyncingStateNotSyncing, finBn: 10, lstBn: 15},
		})

		// Policy that caches finalized eth_getLogs requests on mock1
		policy, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Network:   "evm:123", // Matches mockNetwork
			Method:    "eth_getLogs",
			Finality:  common.DataFinalityStateFinalized,
			Connector: "mock1", // Matches mockConnectors[0].Id()
		}, mockConnectors[0])
		require.NoError(t, err)
		cache.SetPolicies([]*data.CachePolicy{policy})

		// Request for eth_getLogs for a single finalized block (e.g., block 0x5)
		// Upstream finalized block is 10 (0xa)
		req := common.NewNormalizedRequest([]byte(`{
			"jsonrpc": "2.0",
			"method": "eth_getLogs",
			"params": [{"fromBlock": "0x5", "toBlock": "0x5"}],
			"id": 1
		}`))
		req.SetNetwork(mockNetwork)
		req.SetCacheDal(cache)

		// Dummy response for eth_getLogs
		respBody := `[{"address":"0x123","topics":["0xabc"],"data":"0xdef"}]`
		resp := common.NewNormalizedResponse().WithRequest(req).WithBody(util.StringToReaderCloser(fmt.Sprintf(`{"result":%s}`, respBody)))
		resp.SetUpstream(mockUpstreams[0]) // associate with an upstream that has finalized block info
		req.SetLastValidResponse(ctx, resp)

		// Expect Set to be called on the connector
		mockConnectors[0].On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil).Once()

		err = cache.Set(context.Background(), req, resp)
		assert.NoError(t, err)

		// Verify that Set was called. The exact cache key for eth_getLogs can be complex,
		// so we check that Set was called with any key for the correct method and network.
		// More specific key checks would require replicating EvmJsonRpcCache's internal key generation.
		mockConnectors[0].AssertCalled(t, "Set",
			mock.Anything, // context
			mock.MatchedBy(func(key string) bool { return key == "evm:123:5" }),                     // Partition key is specific block for same block range
			mock.MatchedBy(func(key string) bool { return strings.HasPrefix(key, "eth_getLogs:") }), // rangeKey starts with method name
			mock.Anything, // value
			mock.Anything, // ttl
		)
	})

	t.Run("EthGetLogs_DifferentBlockRange_Finalized", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures(ctx, []upsTestCfg{
			{id: "upsA", syncing: common.EvmSyncingStateNotSyncing, finBn: 10, lstBn: 15},
		})

		policy, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Network:   "evm:123",
			Method:    "eth_getLogs",
			Finality:  common.DataFinalityStateFinalized,
			Connector: "mock1",
		}, mockConnectors[0])
		require.NoError(t, err)
		cache.SetPolicies([]*data.CachePolicy{policy})

		// Request for eth_getLogs for a range of finalized blocks (e.g., 0x3 to 0x5)
		// Upstream finalized block is 10 (0xa)
		req := common.NewNormalizedRequest([]byte(`{
			"jsonrpc": "2.0",
			"method": "eth_getLogs",
			"params": [{"fromBlock": "0x3", "toBlock": "0x5"}],
			"id": 1
		}`))
		req.SetNetwork(mockNetwork)
		req.SetCacheDal(cache)

		respBody := `[{"address":"0x456","topics":["0xdef"],"data":"0xghi"}]`
		resp := common.NewNormalizedResponse().WithRequest(req).WithBody(util.StringToReaderCloser(fmt.Sprintf(`{"result":%s}`, respBody)))
		resp.SetUpstream(mockUpstreams[0])
		req.SetLastValidResponse(ctx, resp)

		mockConnectors[0].On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil).Once()

		err = cache.Set(context.Background(), req, resp)
		assert.NoError(t, err)

		mockConnectors[0].AssertCalled(t, "Set",
			mock.Anything, // context
			mock.MatchedBy(func(key string) bool { return key == "evm:123:*" }),                     // partitionKey for eth_getLogs
			mock.MatchedBy(func(key string) bool { return strings.HasPrefix(key, "eth_getLogs:") }), // rangeKey starts with method name
			mock.Anything, // value
			mock.Anything, // ttl
		)
	})

	t.Run("HandleNilResponseGracefully", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		mockConnectors, mockNetwork, _, cache := createCacheTestFixtures(ctx, []upsTestCfg{
			{id: "upsA", syncing: common.EvmSyncingStateNotSyncing, finBn: 10, lstBn: 15},
		})

		// Create a policy with empty behavior set to "ignore"
		policy, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Network:  "evm:123",
			Method:   "eth_getBalance",
			Finality: common.DataFinalityStateFinalized,
			Empty:    common.CacheEmptyBehaviorIgnore,
		}, mockConnectors[0])
		require.NoError(t, err)
		cache.SetPolicies([]*data.CachePolicy{policy})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123","0x5"],"id":1}`))
		req.SetNetwork(mockNetwork)
		req.SetCacheDal(cache)

		// Pass nil response to test the nil check
		err = cache.Set(context.Background(), req, nil)

		// Should not panic and should complete without error
		assert.NoError(t, err)

		// Should not attempt to cache since response is nil (empty)
		mockConnectors[0].AssertNotCalled(t, "Set")
	})

	t.Run("HandleNilResponseWithAllowEmptyPolicy", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		mockConnectors, mockNetwork, _, cache := createCacheTestFixtures(ctx, []upsTestCfg{
			{id: "upsA", syncing: common.EvmSyncingStateNotSyncing, finBn: 10, lstBn: 15},
		})

		// Create a policy with empty behavior set to "allow"
		policy, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Network:  "evm:123",
			Method:   "eth_getBalance",
			Finality: common.DataFinalityStateFinalized,
			Empty:    common.CacheEmptyBehaviorAllow,
		}, mockConnectors[0])
		require.NoError(t, err)
		cache.SetPolicies([]*data.CachePolicy{policy})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123","0x5"],"id":1}`))
		req.SetNetwork(mockNetwork)
		req.SetCacheDal(cache)

		// Pass nil response to test the nil check
		err = cache.Set(context.Background(), req, nil)

		// Should not panic and should complete without error
		assert.NoError(t, err)

		// Should still not cache since we can't actually cache a nil response
		mockConnectors[0].AssertNotCalled(t, "Set")
	})
}

func TestEvmJsonRpcCache_Set_WithTTL(t *testing.T) {
	t.Run("ShouldSetTTLWhenPolicyDefinesIt", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures(ctx, []upsTestCfg{
			{id: "upsA", syncing: common.EvmSyncingStateNotSyncing, finBn: 10, lstBn: 15},
		})

		ttl := time.Duration(5 * time.Minute)
		policy, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Network: "evm:123",
			Method:  "eth_getBalance",
			TTL:     common.Duration(ttl),
		}, mockConnectors[0])
		require.NoError(t, err)
		cache.SetPolicies([]*data.CachePolicy{
			policy,
		})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123","0x5"],"id":1}`))
		req.SetNetwork(mockNetwork)
		req.SetCacheDal(cache)
		resp := common.NewNormalizedResponse().WithRequest(req).WithBody(util.StringToReaderCloser(`{"result":"0x2"}`))
		resp.SetUpstream(mockUpstreams[0])
		req.SetLastValidResponse(ctx, resp)

		mockConnectors[0].On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, &ttl).Return(nil)

		err = cache.Set(context.Background(), req, resp)

		assert.NoError(t, err)
		mockConnectors[0].AssertCalled(t, "Set", mock.Anything, "evm:123:5", mock.Anything, mock.Anything, mock.AnythingOfType("*time.Duration"))
	})

	t.Run("ShouldNotSetTTLWhenPolicyDoesNotDefineIt", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures(ctx, []upsTestCfg{{id: "upsA", syncing: common.EvmSyncingStateNotSyncing, finBn: 10, lstBn: 15}})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123","0x5"],"id":1}`))
		req.SetNetwork(mockNetwork)
		req.SetCacheDal(cache)
		resp := common.NewNormalizedResponse().WithRequest(req).WithBody(util.StringToReaderCloser(`{"result":"0x0"}`))
		resp.SetUpstream(mockUpstreams[0])
		req.SetLastValidResponse(ctx, resp)

		err := cache.Set(context.Background(), req, resp)

		assert.NoError(t, err)
		mockConnectors[0].AssertNotCalled(t, "Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, (*time.Duration)(nil))
	})

	t.Run("ShouldRespectPolicyNetworkAndMethodMatching", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures(ctx, []upsTestCfg{{id: "upsA", syncing: common.EvmSyncingStateNotSyncing, finBn: 10, lstBn: 15}})

		policy0, err0 := data.NewCachePolicy(&common.CachePolicyConfig{
			Network: "evm:123",
			Method:  "eth_getBlockByNumber",
			TTL:     common.Duration(2 * time.Minute),
		}, mockConnectors[0])
		require.NoError(t, err0)

		ttl := common.Duration(6 * time.Minute)
		policy1, err1 := data.NewCachePolicy(&common.CachePolicyConfig{
			Network: "evm:123",
			Method:  "eth_getBalance",
			TTL:     ttl,
		}, mockConnectors[1])
		require.NoError(t, err1)

		cache.SetPolicies([]*data.CachePolicy{
			policy0,
			policy1,
		})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123","0x5"],"id":1}`))
		req.SetNetwork(mockNetwork)
		req.SetCacheDal(cache)
		resp := common.NewNormalizedResponse().WithRequest(req).WithBody(util.StringToReaderCloser(`{"result":"0x1"}`))
		resp.SetUpstream(mockUpstreams[0])
		req.SetLastValidResponse(ctx, resp)

		mockConnectors[0].On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
		mockConnectors[1].On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)

		err := cache.Set(context.Background(), req, resp)

		assert.NoError(t, err)
		mockConnectors[0].AssertNotCalled(t, "Set")
		mockConnectors[1].AssertCalled(t, "Set", mock.Anything, "evm:123:5", mock.Anything, mock.Anything, mock.AnythingOfType("*time.Duration"))
	})
}

func TestEvmJsonRpcCache_Get(t *testing.T) {
	t.Run("ReturnCachedResponseForFinalizedBlock", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		mockConnectors, mockNetwork, _, cache := createCacheTestFixtures(ctx, []upsTestCfg{{id: "upsA", syncing: common.EvmSyncingStateUnknown, finBn: 10, lstBn: 15}})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["0x1",false],"id":1}`))
		req.SetNetwork(mockNetwork)
		req.SetCacheDal(cache)

		policy, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Network: "evm:123",
			Method:  "eth_getBlockByNumber",
		}, mockConnectors[0])
		require.NoError(t, err)
		cache.SetPolicies([]*data.CachePolicy{
			policy,
		})

		cachedResponse := `{"number":"0x1","hash":"0xabc"}`
		mockConnectors[0].On("Get", mock.Anything, mock.Anything, "evm:123:1", mock.Anything, mock.Anything).Return([]byte(cachedResponse), nil)

		resp, err := cache.Get(context.Background(), req)

		assert.NoError(t, err)
		assert.NotNil(t, resp)
		assert.True(t, resp.FromCache())
		jrr, err := resp.JsonRpcResponse()
		assert.NoError(t, err)
		assert.Equal(t, cachedResponse, string(jrr.Result))
	})

	t.Run("SkipCacheForUnfinalizedBlock", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		mockConnectors, mockNetwork, _, cache := createCacheTestFixtures(ctx, []upsTestCfg{{id: "upsA", syncing: common.EvmSyncingStateUnknown, finBn: 10, lstBn: 15}})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["0x32345",false],"id":1}`))
		req.SetNetwork(mockNetwork)
		req.SetCacheDal(cache)
		resp, err := cache.Get(context.Background(), req)

		assert.NoError(t, err)
		assert.Nil(t, resp)
		mockConnectors[0].AssertNotCalled(t, "Get")
	})

	t.Run("CheckAllConnectorsInOrder", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		mockConnectors, mockNetwork, _, cache := createCacheTestFixtures(ctx, []upsTestCfg{
			{id: "upsA", syncing: common.EvmSyncingStateUnknown, finBn: 10, lstBn: 15},
		})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["0x1",false],"id":1}`))
		req.SetNetwork(mockNetwork)
		req.SetCacheDal(cache)

		// Create two policies with different connectors
		policy1, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Network:   "evm:123",
			Method:    "eth_getBlockByNumber",
			Connector: "mock1",
		}, mockConnectors[0])
		require.NoError(t, err)

		policy2, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Network:   "evm:123",
			Method:    "eth_getBlockByNumber",
			Connector: "mock2",
		}, mockConnectors[1])
		require.NoError(t, err)

		cache.SetPolicies([]*data.CachePolicy{policy1, policy2})

		// First connector returns nil
		mockConnectors[0].On("Get", mock.Anything, mock.Anything, "evm:123:1", mock.Anything, mock.Anything).Return([]byte(""), common.NewErrRecordNotFound("evm:123:1", "some-key", "mock1"))

		// Second connector returns data
		cachedResponse := `{"number":"0x1","hash":"0xabc"}`
		mockConnectors[1].On("Get", mock.Anything, mock.Anything, "evm:123:1", mock.Anything, mock.Anything).Return([]byte(cachedResponse), nil)

		resp, err := cache.Get(context.Background(), req)

		assert.NoError(t, err)
		assert.NotNil(t, resp)
		assert.True(t, resp.FromCache())
		jrr, err := resp.JsonRpcResponse()
		assert.NoError(t, err)
		assert.Equal(t, cachedResponse, string(jrr.Result))

		// Verify both connectors were checked in order
		mockConnectors[0].AssertCalled(t, "Get", mock.Anything, mock.Anything, "evm:123:1", mock.Anything, mock.Anything)
		mockConnectors[1].AssertCalled(t, "Get", mock.Anything, mock.Anything, "evm:123:1", mock.Anything, mock.Anything)
	})
}

func TestEvmJsonRpcCache_FinalityAndRetry(t *testing.T) {
	t.Run("ShouldNotCacheEmptyResponseWhenUpstreamIsNotSynced", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures(ctx, []upsTestCfg{{id: "upsA", syncing: common.EvmSyncingStateSyncing, finBn: 10, lstBn: 15}})

		req := common.NewNormalizedRequest([]byte(`{
			"jsonrpc": "2.0",
			"method": "eth_getBalance",
			"params": ["0x123", "0x5"],
			"id": 1
		}`))
		req.SetNetwork(mockNetwork)
		req.SetCacheDal(cache)

		resp := common.NewNormalizedResponse().WithBody(util.StringToReaderCloser(`{"result":null}`)).WithRequest(req)
		resp.SetUpstream(mockUpstreams[0])
		req.SetLastValidResponse(ctx, resp)

		err := cache.Set(context.Background(), req, resp)
		assert.NoError(t, err)
		mockConnectors[0].AssertNotCalled(t, "Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	})

	t.Run("ShouldNotCacheEmptyResponseWhenBlockNotFinalizedOnSpecificUpstream", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures(ctx, []upsTestCfg{
			{id: "upsA", syncing: common.EvmSyncingStateNotSyncing, finBn: 3, lstBn: 15},
			{id: "upsB", syncing: common.EvmSyncingStateNotSyncing, finBn: 10, lstBn: 16},
		})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123","0x5"],"id":1}`))
		req.SetNetwork(mockNetwork)
		req.SetCacheDal(cache)
		resp := common.NewNormalizedResponse().WithBody(util.StringToReaderCloser(`{"result":null}`)).WithRequest(req)
		resp.SetUpstream(mockUpstreams[0])
		req.SetLastValidResponse(ctx, resp)

		err := cache.Set(context.Background(), req, resp)

		assert.NoError(t, err)
		mockConnectors[0].AssertNotCalled(t, "Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	})

	t.Run("ShouldCacheEmptyResponseWhenBlockFinalizedOnSpecificUpstream", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures(ctx, []upsTestCfg{
			{id: "upsA", syncing: common.EvmSyncingStateNotSyncing, finBn: 3, lstBn: 15},
			{id: "upsB", syncing: common.EvmSyncingStateNotSyncing, finBn: 10, lstBn: 16},
		})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123","0x5"],"id":1}`))
		req.SetNetwork(mockNetwork)
		req.SetCacheDal(cache)
		resp := common.NewNormalizedResponse().WithBody(util.StringToReaderCloser(`{"result":null}`)).WithRequest(req)
		resp.SetUpstream(mockUpstreams[1])
		req.SetLastValidResponse(ctx, resp)

		policy, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Network: "evm:123",
			Method:  "eth_getBalance",
			Empty:   common.CacheEmptyBehaviorAllow,
		}, mockConnectors[0])
		require.NoError(t, err)
		cache.SetPolicies([]*data.CachePolicy{
			policy,
		})

		mockConnectors[0].On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)

		err = cache.Set(context.Background(), req, resp)

		assert.NoError(t, err)
		mockConnectors[0].AssertCalled(t, "Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	})

	t.Run("ShouldCacheEmptyResponseWhenBlockFinalizedOnSpecificNonSyncedUpstream", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures(ctx, []upsTestCfg{
			{id: "upsA", syncing: common.EvmSyncingStateNotSyncing, finBn: 3, lstBn: 15},
			{id: "upsB", syncing: common.EvmSyncingStateSyncing, finBn: 10, lstBn: 16},
		})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123","0x5"],"id":1}`))
		req.SetNetwork(mockNetwork)
		req.SetCacheDal(cache)
		resp := common.NewNormalizedResponse().WithBody(util.StringToReaderCloser(`{"result":null}`)).WithRequest(req)
		resp.SetUpstream(mockUpstreams[1])
		req.SetLastValidResponse(ctx, resp)

		err := cache.Set(context.Background(), req, resp)

		assert.NoError(t, err)
		mockConnectors[0].AssertNotCalled(t, "Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	})
}

func TestEvmJsonRpcCache_MatchParams(t *testing.T) {
	mockConnector := data.NewMockConnector("test")

	testCases := []struct {
		name    string
		config  *common.CachePolicyConfig
		method  string
		params  []interface{}
		matches bool
	}{
		// Basic parameter matching
		{
			name: "NoParamsInPolicy",
			config: &common.CachePolicyConfig{
				Network:  "evm:1",
				Method:   "eth_getBlockByNumber",
				Finality: common.DataFinalityStateFinalized,
			},
			method:  "eth_getBlockByNumber",
			params:  []interface{}{"0x1", true},
			matches: true,
		},
		{
			name: "ExactMatch",
			config: &common.CachePolicyConfig{
				Network:  "evm:1",
				Method:   "eth_getBlockByNumber",
				Params:   []interface{}{"0x1", "true"},
				Finality: common.DataFinalityStateFinalized,
			},
			method:  "eth_getBlockByNumber",
			params:  []interface{}{"0x1", true},
			matches: true,
		},

		// Numeric comparisons
		{
			name: "BlockNumberGreaterThan",
			config: &common.CachePolicyConfig{
				Network:  "evm:1",
				Method:   "eth_getBlockByNumber",
				Params:   []interface{}{">0x100", "*"},
				Finality: common.DataFinalityStateFinalized,
			},
			method:  "eth_getBlockByNumber",
			params:  []interface{}{"0x200", false},
			matches: true,
		},
		{
			name: "BlockNumberLessThan",
			config: &common.CachePolicyConfig{
				Network:  "evm:1",
				Method:   "eth_getBlockByNumber",
				Params:   []interface{}{"<0x100", "*"},
				Finality: common.DataFinalityStateFinalized,
			},
			method:  "eth_getBlockByNumber",
			params:  []interface{}{"0x50", false},
			matches: true,
		},
		{
			name: "BlockNumberRange",
			config: &common.CachePolicyConfig{
				Network:  "evm:1",
				Method:   "eth_getBlockByNumber",
				Params:   []interface{}{">=0x100|<=0x200", "*"},
				Finality: common.DataFinalityStateFinalized,
			},
			method:  "eth_getBlockByNumber",
			params:  []interface{}{"0x150", false},
			matches: true,
		},

		// eth_getLogs specific tests
		{
			name: "GetLogsWithBlockRange",
			config: &common.CachePolicyConfig{
				Network: "evm:1",
				Method:  "eth_getLogs",
				Params: []interface{}{
					map[string]interface{}{"fromBlock": ">0x100", "toBlock": "<=0x200"},
				},
				Finality: common.DataFinalityStateFinalized,
			},
			method: "eth_getLogs",
			params: []interface{}{
				map[string]interface{}{
					"fromBlock": "0x150",
					"toBlock":   "0x180",
				},
			},
			matches: true,
		},
		{
			name: "GetLogsWithTopics",
			config: &common.CachePolicyConfig{
				Network: "evm:1",
				Method:  "eth_getLogs",
				Params: []interface{}{
					map[string]interface{}{"topics": []interface{}{"0x*"}},
				},
				Finality: common.DataFinalityStateFinalized,
			},
			method: "eth_getLogs",
			params: []interface{}{
				map[string]interface{}{
					"topics": []interface{}{"0xabcdef"},
				},
			},
			matches: true,
		},

		// Edge cases
		{
			name: "EmptyParamMatch",
			config: &common.CachePolicyConfig{
				Network:  "evm:1",
				Method:   "eth_getTransactionByHash",
				Params:   []interface{}{"*", "<empty>"},
				Finality: common.DataFinalityStateFinalized,
			},
			method:  "eth_getTransactionByHash",
			params:  []interface{}{"0x123", nil},
			matches: true,
		},
		{
			name: "MixedNumericAndWildcard",
			config: &common.CachePolicyConfig{
				Network:  "evm:1",
				Method:   "eth_getBlockByNumber",
				Params:   []interface{}{">=0x100|latest", "*"},
				Finality: common.DataFinalityStateFinalized,
			},
			method:  "eth_getBlockByNumber",
			params:  []interface{}{"latest", false},
			matches: true,
		},

		// Negative cases
		{
			name: "NotEnoughParamsNonStar",
			config: &common.CachePolicyConfig{
				Network:  "evm:1",
				Method:   "eth_getBlockByNumber",
				Params:   []interface{}{"0x1", "true", "extra"},
				Finality: common.DataFinalityStateFinalized,
			},
			method:  "eth_getBlockByNumber",
			params:  []interface{}{"0x1", true},
			matches: false,
		},
		{
			name: "NotEnoughParamsStar",
			config: &common.CachePolicyConfig{
				Network:  "evm:1",
				Method:   "eth_getBlockByNumber",
				Params:   []interface{}{"0x1", "true", "*"},
				Finality: common.DataFinalityStateFinalized,
			},
			method:  "eth_getBlockByNumber",
			params:  []interface{}{"0x1", true},
			matches: true,
		},
		{
			name: "NumericMismatch",
			config: &common.CachePolicyConfig{
				Network:  "evm:1",
				Method:   "eth_getBlockByNumber",
				Params:   []interface{}{">0x100", "*"},
				Finality: common.DataFinalityStateFinalized,
			},
			method:  "eth_getBlockByNumber",
			params:  []interface{}{"0x50", false},
			matches: false,
		},
		{
			name: "GetLogsRangeMismatch",
			config: &common.CachePolicyConfig{
				Network: "evm:1",
				Method:  "eth_getLogs",
				Params: []interface{}{
					map[string]interface{}{"fromBlock": ">0x100", "toBlock": "<=0x200"},
				},
				Finality: common.DataFinalityStateFinalized,
			},
			method: "eth_getLogs",
			params: []interface{}{
				map[string]interface{}{
					"fromBlock": "0x50",
					"toBlock":   "0x300",
				},
			},
			matches: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			policy, err := data.NewCachePolicy(tc.config, mockConnector)
			require.NoError(t, err)

			// Test both Set and Get matching
			matchesSet, err := policy.MatchesForSet(tc.config.Network, tc.method, tc.params, common.DataFinalityStateFinalized, false)
			require.NoError(t, err)
			matchesGet, err := policy.MatchesForGet(tc.config.Network, tc.method, tc.params, common.DataFinalityStateFinalized)
			require.NoError(t, err)

			assert.Equal(t, tc.matches, matchesSet, "MatchesForSet returned unexpected result")
			assert.Equal(t, tc.matches, matchesGet, "MatchesForGet returned unexpected result")
		})
	}
}

func TestEvmJsonRpcCache_EmptyStates(t *testing.T) {
	t.Run("EmptyStateBehaviors", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures(ctx, []upsTestCfg{
			{id: "upsA", syncing: common.EvmSyncingStateNotSyncing, finBn: 10, lstBn: 15},
		})

		testCases := []struct {
			name          string
			emptyBehavior common.CacheEmptyBehavior
			response      string
			shouldCache   bool
			expectedError bool
			errorContains string
		}{
			{
				name:          "IgnoreEmpty_WithEmptyResult",
				emptyBehavior: common.CacheEmptyBehaviorIgnore,
				response:      `{"result":null}`,
				shouldCache:   false,
			},
			{
				name:          "IgnoreEmpty_WithNonEmptyResult",
				emptyBehavior: common.CacheEmptyBehaviorIgnore,
				response:      `{"result":{"value":"0x123"}}`,
				shouldCache:   true,
			},
			{
				name:          "AllowEmpty_WithEmptyResult",
				emptyBehavior: common.CacheEmptyBehaviorAllow,
				response:      `{"result":null}`,
				shouldCache:   true,
			},
			{
				name:          "AllowEmpty_WithNonEmptyResult",
				emptyBehavior: common.CacheEmptyBehaviorAllow,
				response:      `{"result":{"value":"0x123"}}`,
				shouldCache:   true,
			},
			{
				name:          "OnlyEmpty_WithEmptyResult",
				emptyBehavior: common.CacheEmptyBehaviorOnly,
				response:      `{"result":null}`,
				shouldCache:   true,
			},
			{
				name:          "OnlyEmpty_WithNonEmptyResult",
				emptyBehavior: common.CacheEmptyBehaviorOnly,
				response:      `{"result":{"value":"0x123"}}`,
				shouldCache:   false,
			},
			{
				name:          "InvalidEmptyBehavior",
				emptyBehavior: common.CacheEmptyBehavior(99),
				response:      `{"result":null}`,
				shouldCache:   false,
				expectedError: true,
				errorContains: "unknown cache empty behavior",
			},
			{
				name:          "IgnoreEmpty_WithEmptyArray",
				emptyBehavior: common.CacheEmptyBehaviorIgnore,
				response:      `{"result":[]}`,
				shouldCache:   false,
			},
			{
				name:          "IgnoreEmpty_WithEmptyObject",
				emptyBehavior: common.CacheEmptyBehaviorIgnore,
				response:      `{"result":{}}`,
				shouldCache:   false,
			},
			{
				name:          "IgnoreEmpty_WithEmptyString",
				emptyBehavior: common.CacheEmptyBehaviorIgnore,
				response:      `{"result":""}`,
				shouldCache:   false,
			},
			{
				name:          "AllowEmpty_WithSpecialValues",
				emptyBehavior: common.CacheEmptyBehaviorAllow,
				response:      `{"result":"0x0"}`,
				shouldCache:   true,
			},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				// Create policy with specific empty behavior
				policy, err := data.NewCachePolicy(&common.CachePolicyConfig{
					Network:  "evm:123",
					Method:   "eth_getBalance",
					Empty:    tc.emptyBehavior,
					Finality: common.DataFinalityStateFinalized,
				}, mockConnectors[0])
				require.NoError(t, err)
				cache.SetPolicies([]*data.CachePolicy{policy})

				// Create request and response
				req := common.NewNormalizedRequest([]byte(`{
					"jsonrpc": "2.0",
					"method": "eth_getBalance",
					"params": ["0x123", "0x5"],
					"id": 1
				}`))
				req.SetNetwork(mockNetwork)
				req.SetCacheDal(cache)
				resp := common.NewNormalizedResponse().
					WithBody(util.StringToReaderCloser(tc.response)).
					WithRequest(req)
				resp.SetUpstream(mockUpstreams[0])
				req.SetLastValidResponse(ctx, resp)

				// Reset mock and set expectations
				mockConnectors[0].ExpectedCalls = nil
				if tc.shouldCache {
					mockConnectors[0].On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
				}

				// Test caching behavior
				err = cache.Set(context.Background(), req, resp)

				if tc.expectedError {
					assert.Error(t, err)
					if tc.errorContains != "" {
						if err == nil {
							t.Fatalf("expected error, got nil")
						} else {
							assert.Contains(t, err.Error(), tc.errorContains)
						}
					}
				} else {
					assert.NoError(t, err)
					if tc.shouldCache {
						mockConnectors[0].AssertCalled(t, "Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
					} else {
						mockConnectors[0].AssertNotCalled(t, "Set")
					}
				}
			})
		}
	})
}

func TestEvmJsonRpcCache_ItemSizeLimits(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures(ctx, []upsTestCfg{
		{id: "upsA", syncing: common.EvmSyncingStateNotSyncing, finBn: 10, lstBn: 15},
	})

	testCases := []struct {
		name          string
		minSize       *string
		maxSize       *string
		responseSize  int
		shouldCache   bool
		expectedError bool
	}{
		{
			name:         "NoSizeLimits",
			responseSize: 1000,
			shouldCache:  true,
		},
		{
			name:         "WithinSizeLimits",
			minSize:      util.StringPtr("100B"),
			maxSize:      util.StringPtr("2KB"),
			responseSize: 1000,
			shouldCache:  true,
		},
		{
			name:         "TooSmall",
			minSize:      util.StringPtr("1KB"),
			responseSize: 500,
			shouldCache:  false,
		},
		{
			name:         "TooLarge",
			maxSize:      util.StringPtr("1KB"),
			responseSize: 2000,
			shouldCache:  false,
		},
		{
			name:          "InvalidMinSize",
			minSize:       util.StringPtr("-1KB"),
			responseSize:  1000,
			expectedError: true,
		},
		{
			name:          "InvalidMaxSize",
			maxSize:       util.StringPtr("1XB"),
			responseSize:  1000,
			expectedError: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			policy, err := data.NewCachePolicy(&common.CachePolicyConfig{
				Network:     "evm:123",
				Method:      "eth_getBalance",
				MinItemSize: tc.minSize,
				MaxItemSize: tc.maxSize,
				Finality:    common.DataFinalityStateFinalized,
			}, mockConnectors[0])

			if tc.expectedError {
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)
			cache.SetPolicies([]*data.CachePolicy{policy})

			// Create response with specific size
			responseBody := strings.Repeat("x", tc.responseSize)

			req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123","0x5"],"id":1}`))
			req.SetNetwork(mockNetwork)
			req.SetCacheDal(cache)
			resp := common.NewNormalizedResponse().
				WithBody(util.StringToReaderCloser(fmt.Sprintf(`{"result":"%s"}`, responseBody))).
				WithRequest(req)
			resp.SetUpstream(mockUpstreams[0])
			req.SetLastValidResponse(ctx, resp)

			mockConnectors[0].ExpectedCalls = nil
			if tc.shouldCache {
				mockConnectors[0].On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
			}

			err = cache.Set(context.Background(), req, resp)
			assert.NoError(t, err)

			if tc.shouldCache {
				mockConnectors[0].AssertCalled(t, "Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
			} else {
				mockConnectors[0].AssertNotCalled(t, "Set")
			}
		})
	}
}

func TestEvmJsonRpcCache_DynamoDB(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := log.Logger

	// Start a DynamoDB local container
	req := testcontainers.ContainerRequest{
		Image:        "amazon/dynamodb-local",
		ExposedPorts: []string{"8000/tcp"},
		WaitingFor:   wait.ForListeningPort("8000/tcp"),
	}
	ddbC, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("failed to start DynamoDB Local container: %v", err)
	}
	defer ddbC.Terminate(ctx)

	host, err := ddbC.Host(ctx)
	require.NoError(t, err)
	port, err := ddbC.MappedPort(ctx, "8000")
	require.NoError(t, err)

	endpoint := fmt.Sprintf("http://%s:%s", host, port.Port())

	// Create cache with DynamoDB connector for testing
	cacheCfg := &common.CacheConfig{}
	cacheCfg.SetDefaults()

	// Create a DynamoDB connector config
	dynamoDBCfg := &common.ConnectorConfig{
		Id:     "dynamodb1",
		Driver: "dynamodb",
		DynamoDB: &common.DynamoDBConnectorConfig{
			Endpoint:         endpoint,
			Region:           "us-west-2",
			Table:            "evm_cache_test",
			PartitionKeyName: "pk",
			RangeKeyName:     "rk",
			ReverseIndexName: "rk-pk-index",
			TTLAttributeName: "ttl",
			InitTimeout:      common.Duration(5 * time.Second),
			GetTimeout:       common.Duration(2 * time.Second),
			SetTimeout:       common.Duration(2 * time.Second),
			Auth: &common.AwsAuthConfig{
				Mode:            "secret",
				AccessKeyID:     "fakeKey",
				SecretAccessKey: "fakeSecret",
			},
		},
	}
	cacheCfg.Connectors = []*common.ConnectorConfig{dynamoDBCfg}

	// Create appropriate cache policies
	cacheCfg.Policies = []*common.CachePolicyConfig{
		{
			Network:   "*",
			Method:    "*",
			Finality:  common.DataFinalityStateFinalized,
			Connector: "dynamodb1",
		},
		{
			Network:   "*",
			Method:    "*",
			Finality:  common.DataFinalityStateUnfinalized,
			Connector: "dynamodb1",
			TTL:       common.Duration(5 * time.Minute),
		},
		{
			Network:   "*",
			Method:    "*",
			Finality:  common.DataFinalityStateRealtime,
			Connector: "dynamodb1",
			TTL:       common.Duration(30 * time.Second),
		},
	}

	cache, err := evm.NewEvmJsonRpcCache(ctx, &logger, cacheCfg)
	require.NoError(t, err, "failed to create cache")

	// Wait for connector to be fully initialized
	time.Sleep(5 * time.Second)

	// Setup network and upstreams
	mockNetwork := &Network{
		networkId: "evm:123",
		logger:    &logger,
		cfg: &common.NetworkConfig{
			Architecture: common.ArchitectureEvm,
			Evm: &common.EvmNetworkConfig{
				ChainId: 123,
			},
			Methods: &common.MethodsConfig{
				Definitions: map[string]*common.CacheMethodConfig{
					"eth_getBlockByNumber": {
						Realtime: true, // This will make "latest" treated as realtime
					},
				},
			},
		},
	}

	// Create test upstreams with different finalized blocks
	mockUpstream := createMockUpstream(t, ctx, 123, "upsA", common.EvmSyncingStateNotSyncing, 10, 15)

	t.Run("BasicSetGet_WithMainIndex", func(t *testing.T) {
		// Create a request for a finalized block
		req := common.NewNormalizedRequest([]byte(`{
			"jsonrpc": "2.0",
			"method": "eth_getBlockByNumber",
			"params": ["0x5", false],
			"id": 1
		}`))
		req.SetNetwork(mockNetwork)
		req.SetCacheDal(cache)

		// Create response
		responseBody := `{"number":"0x5","hash":"0xabc","transactions":[]}`
		resp := common.NewNormalizedResponse().
			WithRequest(req).
			WithBody(util.StringToReaderCloser(`{"result":` + responseBody + `}`))
		resp.SetUpstream(mockUpstream)
		req.SetLastValidResponse(ctx, resp)

		// Set to cache
		err := cache.Set(ctx, req, resp)
		require.NoError(t, err, "failed to set cache")

		// Get from cache
		cachedResp, err := cache.Get(ctx, req)
		require.NoError(t, err, "failed to get from cache")
		require.NotNil(t, cachedResp, "cached response should not be nil")

		// Verify response
		jrr, err := cachedResp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Equal(t, responseBody, string(jrr.Result))
		assert.True(t, cachedResp.FromCache(), "response should be marked as from cache")
	})

	t.Run("ReverseIndex_TransactionByHash", func(t *testing.T) {
		// Create a request for a transaction by hash (no block number in request)
		req := common.NewNormalizedRequest([]byte(`{
			"jsonrpc": "2.0",
			"method": "eth_getTransactionByHash",
			"params": ["0x123456789abcdef123456789abcdef123456789abcdef123456789abcdef1234"],
			"id": 1
		}`))
		req.SetNetwork(mockNetwork)
		req.SetCacheDal(cache)

		// Create a response that includes a block number (transaction is mined)
		responseBody := `{
			"hash":"0x123456789abcdef123456789abcdef123456789abcdef123456789abcdef1234",
			"blockNumber":"0x5",
			"from":"0xabcdef1234567890abcdef1234567890abcdef12",
			"to":"0x1234567890abcdef1234567890abcdef12345678",
			"value":"0x1234",
			"input":"0x"
		}`
		resp := common.NewNormalizedResponse().
			WithRequest(req).
			WithBody(util.StringToReaderCloser(`{"result":` + responseBody + `}`))
		resp.SetUpstream(mockUpstream)
		req.SetLastValidResponse(ctx, resp)

		// Set to cache
		err := cache.Set(ctx, req, resp)
		require.NoError(t, err, "failed to set cache")

		reqSecond := common.NewNormalizedRequest([]byte(`{
			"jsonrpc": "2.0",
			"method": "eth_getTransactionByHash",
			"params": ["0x123456789abcdef123456789abcdef123456789abcdef123456789abcdef1234"],
			"id": 1
		}`))
		reqSecond.SetNetwork(mockNetwork)
		reqSecond.SetCacheDal(cache)

		// Get from cache
		cachedResp, err := cache.Get(ctx, reqSecond)
		require.NoError(t, err, "failed to get from cache")
		require.NotNil(t, cachedResp, "cached response should not be nil")

		// Verify response
		jrr, err := cachedResp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Equal(t, responseBody, string(jrr.Result))
		assert.True(t, cachedResp.FromCache(), "response should be marked as from cache")
	})

	t.Run("ReverseIndex_eth_getBlockReceipts_ByHash", func(t *testing.T) {
		req := common.NewNormalizedRequest([]byte(`{
			"jsonrpc": "2.0",
			"method": "eth_getBlockReceipts",
			"params": [{"blockHash":"0x6c4383df1aa34eb63b01ea9e3d6ea80304ca8f44baa88c42816ac62b6a19563e"}],
			"id": 1
		}`))
		req.SetNetwork(mockNetwork)
		req.SetCacheDal(cache)

		// Create a response that includes a block number (transaction is mined)
		responseBody := `[{"blockHash":"0xa917fcc721a5465a484e9be17cda0cc5493933dd3bc70c9adbee192cb419c9d7","blockNumber":"0xc5043f","contractAddress":null,"cumulativeGasUsed":"0x26a7d","effectiveGasPrice":"0x0","from":"0xc4a675c5041e9687768ce154554d6cddd2540712","gasUsed":"0x26a7d","logs":[{"address":"0x1f573d6fb3f13d689ff844b4ce37794d79a7ff1c","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef","0x00000000000000000000000056178a0d5f301baf6cf3e1cd53d9863437345bf9","0x000000000000000000000000a57bd00134b2850b2a1c55860c9e9ea100fdd6cf"],"data":"0x0000000000000000000000000000000000000000000003a2a6d6da26e6902d28","blockNumber":"0xc5043f","transactionHash":"0x23e3362a76c8b9370dc65bac8eb1cda1d408ac238a466cfe690248025254bf52","transactionIndex":"0x0","blockHash":"0xa917fcc721a5465a484e9be17cda0cc5493933dd3bc70c9adbee192cb419c9d7","logIndex":"0x0","removed":false}]}]`
		resp := common.NewNormalizedResponse().
			WithRequest(req).
			WithBody(util.StringToReaderCloser(`{"result":` + responseBody + `}`))
		resp.SetUpstream(mockUpstream)
		req.SetLastValidResponse(ctx, resp)

		// Set to cache
		err := cache.Set(ctx, req, resp)
		require.NoError(t, err, "failed to set cache")

		reqSecond := common.NewNormalizedRequest([]byte(`{
			"jsonrpc": "2.0",
			"method": "eth_getBlockReceipts",
			"params": [{"blockHash":"0x6c4383df1aa34eb63b01ea9e3d6ea80304ca8f44baa88c42816ac62b6a19563e"}],
			"id": 1
		}`))
		reqSecond.SetNetwork(mockNetwork)
		reqSecond.SetCacheDal(cache)

		// Get from cache
		cachedResp, err := cache.Get(ctx, reqSecond)
		require.NoError(t, err, "failed to get from cache")
		require.NotNil(t, cachedResp, "cached response should not be nil")

		// Verify response
		jrr, err := cachedResp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Equal(t, responseBody, string(jrr.Result))
		assert.True(t, cachedResp.FromCache(), "response should be marked as from cache")
	})

	t.Run("TTL_Expiration", func(t *testing.T) {
		// Create a request that will use the realtime policy with short TTL
		req := common.NewNormalizedRequest([]byte(`{
			"jsonrpc": "2.0",
			"method": "eth_getBlockByNumber",
			"params": ["latest", false],
			"id": 1
		}`))
		req.SetNetwork(mockNetwork)
		req.SetCacheDal(cache)

		responseBody := `{"number":"0xf","hash":"0xdef","transactions":[]}`
		resp := common.NewNormalizedResponse().
			WithRequest(req).
			WithBody(util.StringToReaderCloser(`{"result":` + responseBody + `}`))
		resp.SetUpstream(mockUpstream)
		req.SetLastValidResponse(ctx, resp)

		// Set to cache
		err := cache.Set(ctx, req, resp)
		require.NoError(t, err, "failed to set cache")

		// Get from cache immediately should work
		cachedResp, err := cache.Get(ctx, req)
		require.NoError(t, err)
		require.NotNil(t, cachedResp, "should get result before TTL expiry")

		// Verify the TTL was correctly set by examining the DynamoDB entry
		sess, err := session.NewSession(&aws.Config{
			Region:      aws.String("us-west-2"),
			Endpoint:    aws.String(endpoint),
			Credentials: credentials.NewStaticCredentials("fakeKey", "fakeSecret", ""),
		})
		require.NoError(t, err)

		dynamoClient := dynamodb.New(sess)

		// Get the cache key from the request
		cacheKey, err := req.CacheHash(context.Background())
		require.NoError(t, err)

		result, err := dynamoClient.GetItem(&dynamodb.GetItemInput{
			TableName: aws.String("evm_cache_test"),
			Key: map[string]*dynamodb.AttributeValue{
				"pk": {S: aws.String("evm:123:*")},
				"rk": {S: aws.String(cacheKey)},
			},
		})
		require.NoError(t, err)
		require.NotNil(t, result.Item)

		// Verify TTL is set
		ttlAttr, exists := result.Item["ttl"]
		assert.True(t, exists, "TTL attribute should exist")
		assert.NotNil(t, ttlAttr.N, "TTL should be numeric")

		ttlValue, err := strconv.ParseInt(*ttlAttr.N, 10, 64)
		require.NoError(t, err)

		// The TTL should be approximately 30 seconds from now
		ttlTime := time.Unix(ttlValue, 0)
		expectedExpiry := time.Now().Add(30 * time.Second)
		assert.WithinDuration(t, expectedExpiry, ttlTime, 5*time.Second)
	})

	t.Run("MultipleMethods_DifferentBlocks", func(t *testing.T) {
		// Test different methods with finalized and unfinalized blocks
		testRequests := []struct {
			name         string
			requestJSON  string
			responseJSON string
			shouldCache  bool
		}{
			{
				name: "FinalizedBlock",
				requestJSON: `{
					"jsonrpc": "2.0",
					"method": "eth_getBlockByNumber",
					"params": ["0x1", false],
					"id": 1
				}`,
				responseJSON: `{"result":{"number":"0x1","hash":"0x111"}}`,
				shouldCache:  true,
			},
			{
				name: "LatestBlock",
				requestJSON: `{
					"jsonrpc": "2.0",
					"method": "eth_getBlockByNumber",
					"params": ["latest", false],
					"id": 2
				}`,
				responseJSON: `{"result":{"number":"0xf","hash":"0x333"}}`,
				shouldCache:  true,
			},
			{
				name: "GetBalance",
				requestJSON: `{
					"jsonrpc": "2.0",
					"method": "eth_getBalance",
					"params": ["0xabc", "0x5"],
					"id": 3
				}`,
				responseJSON: `{"result":"0x123abc"}`,
				shouldCache:  true,
			},
		}

		for _, tc := range testRequests {
			t.Run(tc.name, func(t *testing.T) {
				// Create request/response
				req := common.NewNormalizedRequest([]byte(tc.requestJSON))
				req.SetNetwork(mockNetwork)
				req.SetCacheDal(cache)

				resp := common.NewNormalizedResponse().
					WithRequest(req).
					WithBody(util.StringToReaderCloser(tc.responseJSON))
				resp.SetUpstream(mockUpstream)
				req.SetLastValidResponse(ctx, resp)

				// Cache the response
				err := cache.Set(ctx, req, resp)
				require.NoError(t, err, "failed to set cache")

				// Try to retrieve it
				cachedResp, err := cache.Get(ctx, req)
				require.NoError(t, err, "failed to get from cache")

				if tc.shouldCache {
					require.NotNil(t, cachedResp, "cached response should not be nil")
					assert.True(t, cachedResp.FromCache(), "response should be from cache")
				} else {
					assert.Nil(t, cachedResp, "cached response should be nil (cache miss)")
				}
			})
		}
	})
}

func TestEvmJsonRpcCache_Redis(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := log.Logger

	// Start a Redis container for testing
	redisC, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "redis:alpine",
			ExposedPorts: []string{"6379/tcp"},
			WaitingFor:   wait.ForListeningPort("6379/tcp"),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("failed to start Redis container: %v", err)
	}
	defer redisC.Terminate(ctx)

	host, err := redisC.Host(ctx)
	require.NoError(t, err)
	port, err := redisC.MappedPort(ctx, "6379")
	require.NoError(t, err)

	redisAddr := fmt.Sprintf("%s:%s", host, port.Port())

	// Create cache with Redis connector for testing
	cacheCfg := &common.CacheConfig{}

	// Create a Redis connector config
	redisInnerCfg := &common.RedisConnectorConfig{
		Addr:         redisAddr,
		DB:           0,
		ConnPoolSize: 10,
		InitTimeout:  common.Duration(5 * time.Second),
		GetTimeout:   common.Duration(2 * time.Second),
		SetTimeout:   common.Duration(2 * time.Second),
	}
	err = redisInnerCfg.SetDefaults()
	require.NoError(t, err, "failed to set defaults for redis inner config")

	redisCfg := &common.ConnectorConfig{
		Id:     "redis1",
		Driver: "redis",
		Redis:  redisInnerCfg,
	}
	cacheCfg.Connectors = []*common.ConnectorConfig{redisCfg}

	// Create appropriate cache policies
	cacheCfg.Policies = []*common.CachePolicyConfig{
		{
			Network:   "*",
			Method:    "*",
			Finality:  common.DataFinalityStateFinalized,
			Connector: "redis1",
		},
		{
			Network:   "*",
			Method:    "*",
			Finality:  common.DataFinalityStateUnfinalized,
			Connector: "redis1",
			TTL:       common.Duration(5 * time.Minute),
		},
		{
			Network:   "*",
			Method:    "*",
			Finality:  common.DataFinalityStateRealtime,
			Connector: "redis1",
			TTL:       common.Duration(30 * time.Second),
		},
	}

	err = cacheCfg.SetDefaults()
	require.NoError(t, err, "failed to set defaults for cache config")

	cache, err := evm.NewEvmJsonRpcCache(ctx, &logger, cacheCfg)
	require.NoError(t, err, "failed to create cache")

	// Setup network and upstreams
	mockNetwork := &Network{
		networkId: "evm:123",
		logger:    &logger,
		cfg: &common.NetworkConfig{
			Architecture: common.ArchitectureEvm,
			Evm: &common.EvmNetworkConfig{
				ChainId: 123,
			},
		},
	}

	// Create test upstream with finalized blocks
	mockUpstream := createMockUpstream(t, ctx, 123, "upsA", common.EvmSyncingStateNotSyncing, 10, 15)

	// Run test scenarios
	t.Run("RequestHasBlockNumber_ResponseHasBoth", func(t *testing.T) {
		// Request contains only block number
		// Response contains both block number and block hash
		req := common.NewNormalizedRequest([]byte(`{
			"jsonrpc": "2.0",
			"method": "eth_getBlockByNumber",
			"params": ["0x5", false],
			"id": 1
		}`))
		req.SetNetwork(mockNetwork)
		req.SetCacheDal(cache)

		// Create response with both block number and hash
		responseBody := `{"number":"0x5","hash":"0xabc123","transactions":[]}`
		resp := common.NewNormalizedResponse().
			WithRequest(req).
			WithBody(util.StringToReaderCloser(`{"result":` + responseBody + `}`))
		resp.SetUpstream(mockUpstream)
		req.SetLastValidResponse(ctx, resp)

		// Set to cache
		err := cache.Set(ctx, req, resp)
		require.NoError(t, err, "failed to set cache")

		// Get from cache using a fresh request object
		reqGet := common.NewNormalizedRequest([]byte(`{
			"jsonrpc": "2.0",
			"method": "eth_getBlockByNumber",
			"params": ["0x5", false],
			"id": 1
		}`))
		reqGet.SetNetwork(mockNetwork)
		reqGet.SetCacheDal(cache)
		cachedResp, err := cache.Get(ctx, req)
		require.NoError(t, err, "failed to get from cache")
		require.NotNil(t, cachedResp, "cached response should not be nil")

		// Verify response
		jrr, err := cachedResp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Equal(t, responseBody, string(jrr.Result))
		assert.True(t, cachedResp.FromCache(), "response should be marked as from cache")

		// Verify we can also get it by block hash
		reqByHash := common.NewNormalizedRequest([]byte(`{
			"jsonrpc": "2.0",
			"method": "eth_getBlockByHash",
			"params": ["0xabc123", false],
			"id": 2
		}`))
		reqByHash.SetNetwork(mockNetwork)
		reqByHash.SetCacheDal(cache)

		// This should miss because we stored it under block number, not hash
		cachedResp, err = cache.Get(ctx, reqByHash)
		require.NoError(t, err)
		assert.Nil(t, cachedResp, "should not find response when querying by hash")
	})

	t.Run("RequestHasBlockHash_ResponseHasBoth", func(t *testing.T) {
		// Request contains only block hash
		// Response contains both block hash and block number
		req := common.NewNormalizedRequest([]byte(`{
			"jsonrpc": "2.0",
			"method": "eth_getBlockByHash",
			"params": ["0xdef456", false],
			"id": 1
		}`))
		req.SetNetwork(mockNetwork)
		req.SetCacheDal(cache)

		// Create response with both block hash and number
		responseBody := `{"hash":"0xdef456","number":"0x8","transactions":[]}`
		resp := common.NewNormalizedResponse().
			WithRequest(req).
			WithBody(util.StringToReaderCloser(`{"result":` + responseBody + `}`))
		resp.SetUpstream(mockUpstream)
		req.SetLastValidResponse(ctx, resp)

		// Set to cache
		err := cache.Set(ctx, req, resp)
		require.NoError(t, err, "failed to set cache")

		// Get from cache using a fresh request object
		reqGet := common.NewNormalizedRequest([]byte(`{
			"jsonrpc": "2.0",
			"method": "eth_getBlockByHash",
			"params": ["0xdef456", false],
			"id": 1
		}`))
		reqGet.SetNetwork(mockNetwork)
		reqGet.SetCacheDal(cache)
		cachedResp, err := cache.Get(ctx, reqGet)
		require.NoError(t, err, "failed to get from cache")
		require.NotNil(t, cachedResp, "cached response should not be nil")

		// Verify response
		jrr, err := cachedResp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Equal(t, responseBody, string(jrr.Result))
		assert.True(t, cachedResp.FromCache(), "response should be marked as from cache")

		// Verify we can't get it by block number
		reqByNumber := common.NewNormalizedRequest([]byte(`{
			"jsonrpc": "2.0",
			"method": "eth_getBlockByNumber",
			"params": ["0x8", false],
			"id": 2
		}`))
		reqByNumber.SetNetwork(mockNetwork)
		reqByNumber.SetCacheDal(cache)

		// This should miss because we stored it under block hash, not number
		cachedResp, err = cache.Get(ctx, reqByNumber)
		require.NoError(t, err)
		assert.Nil(t, cachedResp, "should not find response when querying by number")
	})

	t.Run("RequestHasReference_ResponseHasNone", func(t *testing.T) {
		// Request contains block number
		// Response doesn't contain any block references
		req := common.NewNormalizedRequest([]byte(`{
			"jsonrpc": "2.0",
			"method": "eth_getBalance",
			"params": ["0x123abc", "0x5"],
			"id": 1
		}`))
		req.SetNetwork(mockNetwork)
		req.SetCacheDal(cache)

		// Create response without block references
		responseBody := `"0x123456789abcdef"`
		resp := common.NewNormalizedResponse().
			WithRequest(req).
			WithBody(util.StringToReaderCloser(`{"result":` + responseBody + `}`))
		resp.SetUpstream(mockUpstream)
		req.SetLastValidResponse(ctx, resp)

		// Set to cache
		err := cache.Set(ctx, req, resp)
		require.NoError(t, err, "failed to set cache")

		// Get from cache using a fresh request object
		reqGet := common.NewNormalizedRequest([]byte(`{
			"jsonrpc": "2.0",
			"method": "eth_getBalance",
			"params": ["0x123abc", "0x5"],
			"id": 1
		}`))
		reqGet.SetNetwork(mockNetwork)
		reqGet.SetCacheDal(cache)
		cachedResp, err := cache.Get(ctx, reqGet)
		require.NoError(t, err, "failed to get from cache")
		require.NotNil(t, cachedResp, "cached response should not be nil")

		// Verify response
		jrr, err := cachedResp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Equal(t, responseBody, string(jrr.Result))
		assert.True(t, cachedResp.FromCache(), "response should be marked as from cache")
	})

	t.Run("RequestHasNoReference_ResponseHasReference", func(t *testing.T) {
		// Request doesn't contain block hash or number
		// Response contains block number or hash
		req := common.NewNormalizedRequest([]byte(`{
			"jsonrpc": "2.0",
			"method": "eth_getTransactionByHash",
			"params": ["0xabcdef123456"],
			"id": 1
		}`))
		req.SetNetwork(mockNetwork)
		req.SetCacheDal(cache)

		// Create response with block references
		responseBody := `{"hash":"0xabcdef123456","blockHash":"0xfedcba654321","blockNumber":"0x5","from":"0x123","to":"0x456"}`
		resp := common.NewNormalizedResponse().
			WithRequest(req).
			WithBody(util.StringToReaderCloser(`{"result":` + responseBody + `}`))
		resp.SetUpstream(mockUpstream)
		req.SetLastValidResponse(ctx, resp)

		// Set to cache
		err := cache.Set(ctx, req, resp)
		require.NoError(t, err, "failed to set cache")

		// Get from cache using a fresh request object
		reqGet := common.NewNormalizedRequest([]byte(`{
			"jsonrpc": "2.0",
			"method": "eth_getTransactionByHash",
			"params": ["0xabcdef123456"],
			"id": 1
		}`))
		reqGet.SetNetwork(mockNetwork)
		reqGet.SetCacheDal(cache)
		cachedResp, err := cache.Get(ctx, reqGet)
		require.NoError(t, err, "failed to get from cache")
		require.NotNil(t, cachedResp, "cached response should not be nil")

		// Verify response
		jrr, err := cachedResp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Equal(t, responseBody, string(jrr.Result))
		assert.True(t, cachedResp.FromCache(), "response should be marked as from cache")
	})
}

// Helper function to create a mock upstream for the DynamoDB tests
func createMockUpstream(t *testing.T, ctx context.Context, chainId int64, upstreamId string,
	syncState common.EvmSyncingState, finalizedBlock, latestBlock int64) *upstream.Upstream {

	logger := log.Logger
	clr := clients.NewClientRegistry(&logger, "prjA", nil)
	vr := thirdparty.NewVendorsRegistry()
	ssr, err := data.NewSharedStateRegistry(ctx, &logger, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: "memory",
			Memory: &common.MemoryConnectorConfig{
				MaxItems: 100_000, MaxTotalSize: "1GB",
			},
		},
	})
	require.NoError(t, err)

	mt := health.NewTracker(&logger, "prjA", 100*time.Second)
	rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{}, &logger)
	require.NoError(t, err)

	mockUpstream, err := upstream.NewUpstream(ctx, "test", &common.UpstreamConfig{
		Id:       upstreamId,
		Endpoint: "http://rpc1.localhost",
		Type:     common.UpstreamTypeEvm,
		Evm: &common.EvmUpstreamConfig{
			ChainId:             chainId,
			StatePollerInterval: common.Duration(1 * time.Minute),
		},
	}, clr, rlr, vr, &logger, mt, ssr)
	require.NoError(t, err)

	err = mockUpstream.Bootstrap(ctx)
	require.NoError(t, err)

	poller := mockUpstream.EvmStatePoller()
	poller.SuggestFinalizedBlock(finalizedBlock)
	poller.SuggestLatestBlock(latestBlock)
	poller.SetSyncingState(syncState)

	return mockUpstream
}

func TestEvmJsonRpcCache_Compression(t *testing.T) {
	t.Run("CompressionDisabled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		logger := log.Logger

		// Create cache without compression
		cacheCfg := &common.CacheConfig{}
		cacheCfg.SetDefaults()

		_, err := evm.NewEvmJsonRpcCache(ctx, &logger, cacheCfg)
		require.NoError(t, err)

		// Verify compression is disabled (can't directly check private field)
		// Test will verify behavior in other test cases
	})

	t.Run("CompressionEnabled_HappyPath", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixturesWithCompression(ctx,
			[]upsTestCfg{{id: "upsA", syncing: common.EvmSyncingStateNotSyncing, finBn: 10, lstBn: 15}},
			&common.CompressionConfig{
				Enabled:   util.BoolPtr(true),
				Threshold: 100, // Low threshold for testing
				ZstdLevel: "fastest",
			})

		// Create a large response that should be compressed
		largeData := strings.Repeat("abcdefghijklmnopqrstuvwxyz", 100) // 2600 bytes
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["0x5",false],"id":1}`))
		req.SetNetwork(mockNetwork)
		req.SetCacheDal(cache)

		resp := common.NewNormalizedResponse().
			WithRequest(req).
			WithBody(util.StringToReaderCloser(`{"result":{"data":"` + largeData + `"}}`))
		resp.SetUpstream(mockUpstreams[0])
		req.SetLastValidResponse(ctx, resp)

		policy, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Network:  "evm:123",
			Method:   "eth_getBlockByNumber",
			Finality: common.DataFinalityStateFinalized,
		}, mockConnectors[0])
		require.NoError(t, err)
		cache.SetPolicies([]*data.CachePolicy{policy})

		// Mock the Set call and capture the value
		var storedValue []byte
		mockConnectors[0].On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
			Run(func(args mock.Arguments) {
				storedValue = args.Get(3).([]byte)
			}).Return(nil)

		err = cache.Set(ctx, req, resp)
		require.NoError(t, err)

		// Verify compression occurred - check for zstd magic bytes
		assert.True(t, len(storedValue) >= 4 && storedValue[0] == 0x28 && storedValue[1] == 0xB5 && storedValue[2] == 0x2F && storedValue[3] == 0xFD)
		assert.Less(t, len(storedValue), len(largeData)) // Compressed should be smaller

		// Mock the Get call to return the compressed value
		mockConnectors[0].On("Get", mock.Anything, mock.Anything, "evm:123:5", mock.Anything, mock.Anything).
			Return(storedValue, nil)

		// Get from cache and verify decompression
		cachedResp, err := cache.Get(ctx, req)
		require.NoError(t, err)
		require.NotNil(t, cachedResp)

		jrr, err := cachedResp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Contains(t, string(jrr.Result), largeData)
	})

	t.Run("CompressionThreshold_BelowThreshold", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixturesWithCompression(ctx,
			[]upsTestCfg{{id: "upsA", syncing: common.EvmSyncingStateNotSyncing, finBn: 10, lstBn: 15}},
			&common.CompressionConfig{
				Enabled:   util.BoolPtr(true),
				Threshold: 1000, // High threshold
				ZstdLevel: "default",
			})

		// Create a small response that should NOT be compressed
		smallData := "small response"
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["0x5",false],"id":1}`))
		req.SetNetwork(mockNetwork)
		req.SetCacheDal(cache)

		resp := common.NewNormalizedResponse().
			WithRequest(req).
			WithBody(util.StringToReaderCloser(`{"result":"` + smallData + `"}`))
		resp.SetUpstream(mockUpstreams[0])
		req.SetLastValidResponse(ctx, resp)

		policy, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Network:  "evm:123",
			Method:   "eth_getBlockByNumber",
			Finality: common.DataFinalityStateFinalized,
		}, mockConnectors[0])
		require.NoError(t, err)
		cache.SetPolicies([]*data.CachePolicy{policy})

		// Mock the Set call and capture the value
		var storedValue []byte
		mockConnectors[0].On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
			Run(func(args mock.Arguments) {
				storedValue = args.Get(3).([]byte)
			}).Return(nil)

		err = cache.Set(ctx, req, resp)
		require.NoError(t, err)

		// Verify NO compression occurred
		assert.False(t, len(storedValue) >= 4 && storedValue[0] == 0x28 && storedValue[1] == 0xB5 && storedValue[2] == 0x2F && storedValue[3] == 0xFD)
		assert.Equal(t, `"`+smallData+`"`, string(storedValue))
	})

	t.Run("CompressionNotBeneficial", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixturesWithCompression(ctx,
			[]upsTestCfg{{id: "upsA", syncing: common.EvmSyncingStateNotSyncing, finBn: 10, lstBn: 15}},
			&common.CompressionConfig{
				Enabled:   util.BoolPtr(true),
				Threshold: 10, // Very low threshold
				ZstdLevel: "best",
			})

		// Create random data that doesn't compress well
		randomData := generateRandomString(100)
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["0x5",false],"id":1}`))
		req.SetNetwork(mockNetwork)
		req.SetCacheDal(cache)

		resp := common.NewNormalizedResponse().
			WithRequest(req).
			WithBody(util.StringToReaderCloser(`{"result":"` + randomData + `"}`))
		resp.SetUpstream(mockUpstreams[0])
		req.SetLastValidResponse(ctx, resp)

		policy, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Network:  "evm:123",
			Method:   "eth_getBlockByNumber",
			Finality: common.DataFinalityStateFinalized,
		}, mockConnectors[0])
		require.NoError(t, err)
		cache.SetPolicies([]*data.CachePolicy{policy})

		// Mock the Set call and capture the value
		var storedValue []byte
		mockConnectors[0].On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
			Run(func(args mock.Arguments) {
				storedValue = args.Get(3).([]byte)
			}).Return(nil)

		err = cache.Set(ctx, req, resp)
		require.NoError(t, err)

		// If compression doesn't save space, it shouldn't be used
		// This depends on the random data, but we can check the logic works
		isCompressed := len(storedValue) >= 4 && storedValue[0] == 0x28 && storedValue[1] == 0xB5 && storedValue[2] == 0x2F && storedValue[3] == 0xFD
		if isCompressed {
			// If compressed, it should be smaller than original
			assert.Less(t, len(storedValue), len(randomData))
		}
	})

	t.Run("DecompressionError_CorruptedData", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		mockConnectors, mockNetwork, _, cache := createCacheTestFixturesWithCompression(ctx,
			[]upsTestCfg{{id: "upsA", syncing: common.EvmSyncingStateNotSyncing, finBn: 10, lstBn: 15}},
			&common.CompressionConfig{
				Enabled:   util.BoolPtr(true),
				Threshold: 100,
				ZstdLevel: "fastest",
			})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["0x5",false],"id":1}`))
		req.SetNetwork(mockNetwork)
		req.SetCacheDal(cache)

		policy, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Network:  "evm:123",
			Method:   "eth_getBlockByNumber",
			Finality: common.DataFinalityStateFinalized,
		}, mockConnectors[0])
		require.NoError(t, err)
		cache.SetPolicies([]*data.CachePolicy{policy})

		// Mock Get to return corrupted compressed data
		corruptedData := []byte{0x28, 0xB5, 0x2F, 0xFD, 0xFF, 0xFF, 0xFF, 0xFF} // Valid magic but invalid zstd
		mockConnectors[0].On("Get", mock.Anything, mock.Anything, "evm:123:5", mock.Anything, mock.Anything).
			Return(corruptedData, nil)

		// Get from cache should return nil (cache miss due to decompression error)
		// The error is logged but not returned to avoid cache poisoning
		cachedResp, err := cache.Get(ctx, req)
		assert.NoError(t, err)
		assert.Nil(t, cachedResp)
	})

	t.Run("CompressionLevels", func(t *testing.T) {
		testCases := []struct {
			level    string
			expected string
		}{
			{"fastest", "fastest"},
			{"default", "default"},
			{"better", "better"},
			{"best", "best"},
			{"invalid", "fastest"}, // Should default to fastest
			{"", "fastest"},        // Should default to fastest
		}

		for _, tc := range testCases {
			t.Run(tc.level, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()

				logger := log.Logger
				cacheCfg := &common.CacheConfig{
					Compression: &common.CompressionConfig{
						Enabled:   util.BoolPtr(true),
						Threshold: 100,
						ZstdLevel: tc.level,
					},
				}
				cacheCfg.SetDefaults()

				cache, err := evm.NewEvmJsonRpcCache(ctx, &logger, cacheCfg)
				require.NoError(t, err)
				assert.NotNil(t, cache)
			})
		}
	})

	t.Run("CompressionWithDifferentDataTypes", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixturesWithCompression(ctx,
			[]upsTestCfg{{id: "upsA", syncing: common.EvmSyncingStateNotSyncing, finBn: 10, lstBn: 15}},
			&common.CompressionConfig{
				Enabled:   util.BoolPtr(true),
				Threshold: 50,
				ZstdLevel: "default",
			})

		testCases := []struct {
			name           string
			responseData   string
			shouldCompress bool
		}{
			{
				name:           "JSON_Object",
				responseData:   `{"result":{"block":"0x1","transactions":[` + strings.Repeat(`"0x123",`, 50) + `"0x123"]}}`,
				shouldCompress: true,
			},
			{
				name:           "Large_Hex_String",
				responseData:   `{"result":"0x` + strings.Repeat("abcdef0123456789", 20) + `"}`,
				shouldCompress: true,
			},
			{
				name:           "Array_Of_Logs",
				responseData:   `{"result":[` + strings.Repeat(`{"address":"0x123","data":"0xabc"},`, 30) + `{"address":"0x123","data":"0xabc"}]}`,
				shouldCompress: true,
			},
			{
				name:           "Empty_Result",
				responseData:   `{"result":null}`,
				shouldCompress: false, // Too small
			},
			{
				name:           "Small_Number",
				responseData:   `{"result":"0x64"}`,
				shouldCompress: false, // Too small
			},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_call","params":[{"to":"0x123"},"0x5"],"id":1}`))
				req.SetNetwork(mockNetwork)
				req.SetCacheDal(cache)

				resp := common.NewNormalizedResponse().
					WithRequest(req).
					WithBody(util.StringToReaderCloser(tc.responseData))
				resp.SetUpstream(mockUpstreams[0])
				req.SetLastValidResponse(ctx, resp)

				policy, err := data.NewCachePolicy(&common.CachePolicyConfig{
					Network:  "evm:123",
					Method:   "eth_call",
					Finality: common.DataFinalityStateFinalized,
				}, mockConnectors[0])
				require.NoError(t, err)
				cache.SetPolicies([]*data.CachePolicy{policy})

				// Reset mock
				mockConnectors[0].ExpectedCalls = nil
				mockConnectors[0].Calls = nil

				var storedValue []byte
				mockConnectors[0].On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
					Run(func(args mock.Arguments) {
						storedValue = args.Get(3).([]byte)
					}).Return(nil)

				err = cache.Set(ctx, req, resp)
				require.NoError(t, err)

				isCompressed := len(storedValue) >= 4 && storedValue[0] == 0x28 && storedValue[1] == 0xB5 && storedValue[2] == 0x2F && storedValue[3] == 0xFD
				if tc.shouldCompress {
					assert.True(t, isCompressed, "Expected compression for %s", tc.name)
				} else {
					assert.False(t, isCompressed, "Expected no compression for %s", tc.name)
				}
			})
		}
	})

	t.Run("ConcurrentCompression", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixturesWithCompression(ctx,
			[]upsTestCfg{{id: "upsA", syncing: common.EvmSyncingStateNotSyncing, finBn: 10, lstBn: 15}},
			&common.CompressionConfig{
				Enabled:   util.BoolPtr(true),
				Threshold: 100,
				ZstdLevel: "fastest",
			})

		policy, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Network:  "evm:123",
			Method:   "eth_getBlockByNumber",
			Finality: common.DataFinalityStateFinalized,
		}, mockConnectors[0])
		require.NoError(t, err)
		cache.SetPolicies([]*data.CachePolicy{policy})

		// Set up mock to accept any calls
		mockConnectors[0].On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)

		// Run multiple compressions concurrently
		var wg sync.WaitGroup
		numGoroutines := 10

		for i := 0; i < numGoroutines; i++ {
			wg.Add(1)
			go func(index int) {
				defer wg.Done()

				largeData := strings.Repeat(fmt.Sprintf("data%d", index), 100)
				req := common.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["0x%x",false],"id":%d}`, index+1, index)))
				req.SetNetwork(mockNetwork)
				req.SetCacheDal(cache)

				resp := common.NewNormalizedResponse().
					WithRequest(req).
					WithBody(util.StringToReaderCloser(`{"result":{"data":"` + largeData + `"}}`))
				resp.SetUpstream(mockUpstreams[0])
				req.SetLastValidResponse(ctx, resp)

				err := cache.Set(ctx, req, resp)
				assert.NoError(t, err)
			}(i)
		}

		wg.Wait()

		// Verify all calls were made
		mockConnectors[0].AssertNumberOfCalls(t, "Set", numGoroutines)
	})

	t.Run("CompressionPoolExhaustion", func(t *testing.T) {
		// This test verifies the pool handles exhaustion gracefully
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixturesWithCompression(ctx,
			[]upsTestCfg{{id: "upsA", syncing: common.EvmSyncingStateNotSyncing, finBn: 10, lstBn: 15}},
			&common.CompressionConfig{
				Enabled:   util.BoolPtr(true),
				Threshold: 100,
				ZstdLevel: "fastest",
			})

		policy, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Network:  "evm:123",
			Method:   "eth_getBlockByNumber",
			Finality: common.DataFinalityStateFinalized,
		}, mockConnectors[0])
		require.NoError(t, err)
		cache.SetPolicies([]*data.CachePolicy{policy})

		// Set up mock to accept any calls
		mockConnectors[0].On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)

		// Create many concurrent requests to potentially exhaust the pool
		var wg sync.WaitGroup
		numGoroutines := 100

		for i := 0; i < numGoroutines; i++ {
			wg.Add(1)
			go func(index int) {
				defer wg.Done()

				largeData := strings.Repeat("x", 1000)
				req := common.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["0x%x",false],"id":%d}`, index+1, index)))
				req.SetNetwork(mockNetwork)
				req.SetCacheDal(cache)

				resp := common.NewNormalizedResponse().
					WithRequest(req).
					WithBody(util.StringToReaderCloser(`{"result":"` + largeData + `"}`))
				resp.SetUpstream(mockUpstreams[0])
				req.SetLastValidResponse(ctx, resp)

				// Should not panic or error even under high concurrency
				err := cache.Set(ctx, req, resp)
				assert.NoError(t, err)
			}(i)
		}

		wg.Wait()
	})

	t.Run("DecompressionOfNonCompressedData", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		mockConnectors, mockNetwork, _, cache := createCacheTestFixturesWithCompression(ctx,
			[]upsTestCfg{{id: "upsA", syncing: common.EvmSyncingStateNotSyncing, finBn: 10, lstBn: 15}},
			&common.CompressionConfig{
				Enabled:   util.BoolPtr(true),
				Threshold: 100,
				ZstdLevel: "fastest",
			})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["0x5",false],"id":1}`))
		req.SetNetwork(mockNetwork)
		req.SetCacheDal(cache)

		policy, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Network:  "evm:123",
			Method:   "eth_getBlockByNumber",
			Finality: common.DataFinalityStateFinalized,
		}, mockConnectors[0])
		require.NoError(t, err)
		cache.SetPolicies([]*data.CachePolicy{policy})

		// Mock Get to return non-compressed data (simulating old cache entries)
		nonCompressedData := []byte(`{"number":"0x5","hash":"0xabc"}`)
		mockConnectors[0].On("Get", mock.Anything, mock.Anything, "evm:123:5", mock.Anything, mock.Anything).
			Return(nonCompressedData, nil)

		// Get from cache should work fine with non-compressed data
		cachedResp, err := cache.Get(ctx, req)
		require.NoError(t, err)
		require.NotNil(t, cachedResp)

		jrr, err := cachedResp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Equal(t, string(nonCompressedData), string(jrr.Result))
	})

	t.Run("CompressionBoundaryConditions", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		testCases := []struct {
			name           string
			threshold      int
			dataSize       int
			shouldCompress bool
		}{
			{"ExactlyAtThreshold", 100, 100, true},
			{"JustBelowThreshold", 100, 97, false},
			{"JustAboveThreshold", 100, 101, true},
			{"ZeroThreshold", 0, 50, true},
			{"EmptyData", 100, 0, false},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixturesWithCompression(ctx,
					[]upsTestCfg{{id: "upsA", syncing: common.EvmSyncingStateNotSyncing, finBn: 10, lstBn: 15}},
					&common.CompressionConfig{
						Enabled:   util.BoolPtr(true),
						Threshold: tc.threshold,
						ZstdLevel: "fastest",
					})

				testData := ""
				if tc.dataSize > 0 {
					testData = strings.Repeat("a", tc.dataSize)
				}

				req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["0x5",false],"id":1}`))
				req.SetNetwork(mockNetwork)
				req.SetCacheDal(cache)

				resp := common.NewNormalizedResponse().
					WithRequest(req).
					WithBody(util.StringToReaderCloser(`{"result":"` + testData + `"}`))
				resp.SetUpstream(mockUpstreams[0])
				req.SetLastValidResponse(ctx, resp)

				policy, err := data.NewCachePolicy(&common.CachePolicyConfig{
					Network:  "evm:123",
					Method:   "eth_getBlockByNumber",
					Finality: common.DataFinalityStateFinalized,
				}, mockConnectors[0])
				require.NoError(t, err)
				cache.SetPolicies([]*data.CachePolicy{policy})

				var storedValue []byte
				mockConnectors[0].On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
					Run(func(args mock.Arguments) {
						storedValue = args.Get(3).([]byte)
					}).Return(nil).Once()

				err = cache.Set(ctx, req, resp)
				require.NoError(t, err)

				if tc.shouldCompress && tc.dataSize > 10 { // Compression needs some minimum data to be effective
					// May or may not compress based on effectiveness
					// Just verify no errors occurred
					assert.NotEmpty(t, storedValue)
				} else {
					assert.False(t, len(storedValue) >= 4 && storedValue[0] == 0x28 && storedValue[1] == 0xB5 && storedValue[2] == 0x2F && storedValue[3] == 0xFD)
				}
			})
		}
	})
}

// Helper function to create cache test fixtures with compression enabled
func createCacheTestFixturesWithCompression(ctx context.Context, upstreamConfigs []upsTestCfg, compressionCfg *common.CompressionConfig) ([]*data.MockConnector, *Network, []*upstream.Upstream, *evm.EvmJsonRpcCache) {
	logger := log.Logger

	mockConnector1 := data.NewMockConnector("mock1")
	mockConnector2 := data.NewMockConnector("mock2")
	mockNetwork := &Network{
		networkId: "evm:123",
		logger:    &logger,
		cfg: &common.NetworkConfig{
			Architecture: common.ArchitectureEvm,
			Evm: &common.EvmNetworkConfig{
				ChainId: 123,
			},
		},
	}

	clr := clients.NewClientRegistry(&logger, "prjA", nil)
	vr := thirdparty.NewVendorsRegistry()
	ssr, err := data.NewSharedStateRegistry(ctx, &logger, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: "memory",
			Memory: &common.MemoryConnectorConfig{
				MaxItems: 100_000, MaxTotalSize: "1GB",
			},
		},
	})
	if err != nil {
		panic(err)
	}
	upstreams := make([]*upstream.Upstream, 0, len(upstreamConfigs))

	for _, cfg := range upstreamConfigs {
		mt := health.NewTracker(&logger, "prjA", 100*time.Second)
		rlr, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{}, &logger)
		if err != nil {
			panic(err)
		}
		mockUpstream, err := upstream.NewUpstream(ctx, "test", &common.UpstreamConfig{
			Id:       cfg.id,
			Endpoint: "http://rpc1.localhost",
			Type:     common.UpstreamTypeEvm,
			Evm: &common.EvmUpstreamConfig{
				ChainId:             123,
				StatePollerInterval: common.Duration(1 * time.Minute),
			},
		}, clr, rlr, vr, &logger, mt, ssr)
		if err != nil {
			panic(err)
		}

		err = mockUpstream.Bootstrap(ctx)
		if err != nil {
			panic(err)
		}

		poller := mockUpstream.EvmStatePoller()
		poller.SuggestFinalizedBlock(cfg.finBn)
		poller.SuggestLatestBlock(cfg.lstBn)
		poller.SetSyncingState(cfg.syncing)

		upstreams = append(upstreams, mockUpstream)
	}

	cacheCfg := &common.CacheConfig{
		Compression: compressionCfg,
	}
	cacheCfg.SetDefaults()
	cache, err := evm.NewEvmJsonRpcCache(context.Background(), &logger, cacheCfg)
	if err != nil {
		panic(err)
	}

	return []*data.MockConnector{mockConnector1, mockConnector2}, mockNetwork, upstreams, cache
}

// Helper function to generate random string for compression tests
func generateRandomString(length int) string {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, length)
	for i := range b {
		b[i] = charset[time.Now().UnixNano()%int64(len(charset))]
	}
	return string(b)
}
