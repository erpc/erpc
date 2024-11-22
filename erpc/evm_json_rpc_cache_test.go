package erpc

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/erpc/erpc/vendors"
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

func createCacheTestFixtures(upstreamConfigs []upsTestCfg) ([]*data.MockConnector, *Network, []*upstream.Upstream, *EvmJsonRpcCache) {
	logger := log.Logger

	mockConnector1 := data.NewMockConnector("mock1")
	mockConnector2 := data.NewMockConnector("mock2")
	mockNetwork := &Network{
		NetworkId: "evm:123",
		Logger:    &logger,
		cfg: &common.NetworkConfig{
			Architecture: common.ArchitectureEvm,
			Evm: &common.EvmNetworkConfig{
				ChainId: 123,
			},
		},
	}

	vnr := vendors.NewVendorsRegistry()
	clr := upstream.NewClientRegistry(&logger)
	mockNetwork.evmStatePollers = make(map[string]*upstream.EvmStatePoller)
	upstreams := make([]*upstream.Upstream, 0, len(upstreamConfigs))

	for _, cfg := range upstreamConfigs {
		mockUpstream, err := upstream.NewUpstream(context.Background(), "test", &common.UpstreamConfig{
			Id:       cfg.id,
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}, clr, nil, vnr, &logger, nil)
		mockUpstream.SetEvmSyncingState(cfg.syncing)
		if err != nil {
			panic(err)
		}

		metricsTracker := health.NewTracker("prjA", 100*time.Second)
		poller, err := upstream.NewEvmStatePoller(context.Background(), &logger, mockNetwork, mockUpstream, metricsTracker)
		if err != nil {
			panic(err)
		}
		poller.SuggestFinalizedBlock(cfg.finBn)
		poller.SuggestLatestBlock(cfg.lstBn)
		mockNetwork.evmStatePollers[cfg.id] = poller
		upstreams = append(upstreams, mockUpstream)
	}

	cache := &EvmJsonRpcCache{
		logger:  &logger,
		network: mockNetwork,
	}

	return []*data.MockConnector{mockConnector1, mockConnector2}, mockNetwork, upstreams, cache
}

func TestEvmJsonRpcCache_Set(t *testing.T) {
	t.Run("DoNotCacheWhenEthGetTransactionByHashMissingBlockNumber", func(t *testing.T) {
		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures([]upsTestCfg{{id: "upsA", syncing: common.EvmSyncingStateUnknown, finBn: 10, lstBn: 15}})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getTransactionByHash","params":["0xdddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"],"id":1}`))
		req.SetNetwork(mockNetwork)
		resp := common.NewNormalizedResponse().WithRequest(req).WithBody(util.StringToReaderCloser(`{"result":{"hash":"0xdddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","blockNumber":null}}`))
		resp.SetUpstream(mockUpstreams[0])
		req.SetLastValidResponse(resp)

		err := cache.Set(context.Background(), req, resp)

		assert.NoError(t, err)
		mockConnectors[0].AssertNotCalled(t, "Set")
	})

	t.Run("CacheIfBlockNumberIsFinalizedWhenBlockIsIrrelevantForPrimaryKey", func(t *testing.T) {
		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures([]upsTestCfg{{id: "upsA", syncing: common.EvmSyncingStateUnknown, finBn: 10, lstBn: 15}})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getTransactionReceipt","params":["0xabc",false],"id":1}`))
		req.SetNetwork(mockNetwork)
		resp := common.NewNormalizedResponse().WithRequest(req).WithBody(util.StringToReaderCloser(`{"result":{"hash":"0xabc","blockNumber":"0x2"}}`))
		resp.SetUpstream(mockUpstreams[0])
		req.SetLastValidResponse(resp)

		policy, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Network:  "evm:123",
			Method:   "eth_getTransactionReceipt",
			Finality: common.DataFinalityStateFinalized,
		}, mockConnectors[0])
		require.NoError(t, err)
		cache.policies = []*data.CachePolicy{
			policy,
		}

		mockConnectors[0].On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)

		err = cache.Set(context.Background(), req, resp)

		assert.NoError(t, err)
		mockConnectors[0].AssertCalled(t, "Set", mock.Anything, "evm:123:*", mock.Anything, mock.Anything, mock.Anything)
	})

	t.Run("CacheIfBlockNumberIsFinalizedWhenBlockIsUsedForPrimaryKey", func(t *testing.T) {
		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures([]upsTestCfg{{id: "upsA", syncing: common.EvmSyncingStateUnknown, finBn: 10, lstBn: 15}})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["0x2",false],"id":1}`))
		req.SetNetwork(mockNetwork)
		resp := common.NewNormalizedResponse().WithRequest(req).WithBody(util.StringToReaderCloser(`{"result":{"hash":"0xabc","number":"0x2"}}`))
		resp.SetUpstream(mockUpstreams[0])
		req.SetLastValidResponse(resp)

		policy, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Network: "evm:123",
			Method:  "eth_getBlockByNumber",
		}, mockConnectors[0])
		require.NoError(t, err)
		cache.policies = []*data.CachePolicy{
			policy,
		}

		mockConnectors[0].On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)

		err = cache.Set(context.Background(), req, resp)

		assert.NoError(t, err)
		mockConnectors[0].AssertCalled(t, "Set", mock.Anything, "evm:123:2", mock.Anything, mock.Anything, mock.Anything)
	})

	t.Run("SkipWhenNoRefAndNoBlockNumberFound", func(t *testing.T) {
		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures([]upsTestCfg{{id: "upsA", syncing: common.EvmSyncingStateUnknown, finBn: 10, lstBn: 15}})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123","latest"],"id":1}`))
		req.SetNetwork(mockNetwork)
		resp := common.NewNormalizedResponse().WithRequest(req).WithBody(util.StringToReaderCloser(`{"result":"0x1234"}`))
		resp.SetUpstream(mockUpstreams[0])
		req.SetLastValidResponse(resp)

		err := cache.Set(context.Background(), req, resp)

		assert.NoError(t, err)
		mockConnectors[0].AssertNotCalled(t, "Set")
	})

	t.Run("CacheIfBlockRefFoundWhetherBlockNumberExistsOrNot", func(t *testing.T) {
		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures([]upsTestCfg{{id: "upsA", syncing: common.EvmSyncingStateUnknown, finBn: 10, lstBn: 15}})

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
				resp := common.NewNormalizedResponse().WithRequest(req).WithBody(util.StringToReaderCloser(tc.result))
				resp.SetUpstream(mockUpstreams[0])
				req.SetLastValidResponse(resp)

				policy, err := data.NewCachePolicy(&common.CachePolicyConfig{
					Network:  "evm:123",
					Method:   tc.method,
					Finality: tc.finality,
				}, mockConnectors[0])
				require.NoError(t, err)
				cache.policies = []*data.CachePolicy{
					policy,
				}

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
		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures([]upsTestCfg{{id: "upsA", syncing: common.EvmSyncingStateUnknown, finBn: 10, lstBn: 15}})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["0x1",false],"id":1}`))
		req.SetNetwork(mockNetwork)
		resp := common.NewNormalizedResponse().WithRequest(req).WithBody(util.StringToReaderCloser(`{"result":{"number":"0x1","hash":"0xabc"}}`))
		resp.SetUpstream(mockUpstreams[0])
		req.SetLastValidResponse(resp)

		policy, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Network: "evm:123",
			Method:  "eth_getBlockByNumber",
		}, mockConnectors[0])
		require.NoError(t, err)
		cache.policies = []*data.CachePolicy{
			policy,
		}

		mockConnectors[0].On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)

		err = cache.Set(context.Background(), req, resp)

		assert.NoError(t, err)
		mockConnectors[0].AssertCalled(t, "Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	})

	t.Run("SkipCachingForUnfinalizedBlock", func(t *testing.T) {
		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures([]upsTestCfg{{id: "upsA", syncing: common.EvmSyncingStateUnknown, finBn: 10, lstBn: 15}})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["0x399",false],"id":1}`))
		req.SetNetwork(mockNetwork)
		resp := common.NewNormalizedResponse().WithRequest(req).WithBody(util.StringToReaderCloser(`{"result":{"number":"0x399","hash":"0xdef"}}`))
		resp.SetUpstream(mockUpstreams[0])
		req.SetLastValidResponse(resp)

		err := cache.Set(context.Background(), req, resp)

		assert.NoError(t, err)
		mockConnectors[0].AssertNotCalled(t, "Set")
	})

	t.Run("ShouldNotCacheEmptyResponseIfNodeNotSynced", func(t *testing.T) {
		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures([]upsTestCfg{{id: "upsA", syncing: common.EvmSyncingStateSyncing, finBn: 10, lstBn: 15}})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123","latest"],"id":1}`))
		req.SetNetwork(mockNetwork)
		resp := common.NewNormalizedResponse().WithRequest(req).WithBody(util.StringToReaderCloser(`{"result":"0x0"}`))
		resp.SetUpstream(mockUpstreams[0])
		req.SetLastValidResponse(resp)

		err := cache.Set(context.Background(), req, resp)

		assert.NoError(t, err)
		mockConnectors[0].AssertNotCalled(t, "Set")
	})

	t.Run("ShouldNotCacheEmptyResponseIfUnknownSyncState", func(t *testing.T) {
		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures([]upsTestCfg{{id: "upsA", syncing: common.EvmSyncingStateUnknown, finBn: 10, lstBn: 15}})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123","latest"],"id":1}`))
		req.SetNetwork(mockNetwork)
		resp := common.NewNormalizedResponse().WithRequest(req).WithBody(util.StringToReaderCloser(`{"result":"0x0"}`))
		resp.SetUpstream(mockUpstreams[0])
		req.SetLastValidResponse(resp)

		err := cache.Set(context.Background(), req, resp)

		assert.NoError(t, err)
		mockConnectors[0].AssertNotCalled(t, "Set")
	})

	t.Run("ShouldNotCacheEmptyResponseIfBlockNotFinalized", func(t *testing.T) {
		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures([]upsTestCfg{{id: "upsA", syncing: common.EvmSyncingStateNotSyncing, finBn: 10, lstBn: 15}})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123","0x14"],"id":1}`))
		req.SetNetwork(mockNetwork)
		resp := common.NewNormalizedResponse().WithRequest(req).WithBody(util.StringToReaderCloser(`{"result":"0x0"}`))
		resp.SetUpstream(mockUpstreams[0])
		req.SetLastValidResponse(resp)

		err := cache.Set(context.Background(), req, resp)

		assert.NoError(t, err)
		mockConnectors[0].AssertNotCalled(t, "Set")
	})

	t.Run("ShouldNotCacheEmptyResponseIfCannotDetermineBlockNumber", func(t *testing.T) {
		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures([]upsTestCfg{{id: "upsA", syncing: common.EvmSyncingStateNotSyncing, finBn: 10, lstBn: 15}})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123","latest"],"id":1}`))
		req.SetNetwork(mockNetwork)
		resp := common.NewNormalizedResponse().WithRequest(req).WithBody(util.StringToReaderCloser(`{"result":"0x0"}`))
		resp.SetUpstream(mockUpstreams[0])
		req.SetLastValidResponse(resp)

		err := cache.Set(context.Background(), req, resp)

		assert.NoError(t, err)
		mockConnectors[0].AssertNotCalled(t, "Set")
	})

	t.Run("ShouldCacheEmptyResponseIfNodeSyncedAndBlockFinalized", func(t *testing.T) {
		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures([]upsTestCfg{{id: "upsA", syncing: common.EvmSyncingStateNotSyncing, finBn: 10, lstBn: 15}})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123","0x5"],"id":1}`))
		req.SetNetwork(mockNetwork)
		resp := common.NewNormalizedResponse().WithRequest(req).WithBody(util.StringToReaderCloser(`{"result":"0x0"}`))
		resp.SetUpstream(mockUpstreams[0])
		req.SetLastValidResponse(resp)

		policy, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Network: "evm:123",
			Method:  "eth_getBalance",
		}, mockConnectors[0])
		require.NoError(t, err)
		cache.policies = []*data.CachePolicy{
			policy,
		}

		mockConnectors[0].On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)

		err = cache.Set(context.Background(), req, resp)

		assert.NoError(t, err)
		mockConnectors[0].AssertCalled(t, "Set", mock.Anything, "evm:123:5", mock.Anything, mock.Anything, mock.Anything)
	})

	t.Run("StoreOnAllMatchingConnectors", func(t *testing.T) {
		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures([]upsTestCfg{
			{id: "upsA", syncing: common.EvmSyncingStateNotSyncing, finBn: 10, lstBn: 15},
		})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getTransactionByHash","params":["0x123"],"id":1}`))
		req.SetNetwork(mockNetwork)
		resp := common.NewNormalizedResponse().WithRequest(req).WithBody(util.StringToReaderCloser(`{"result":{"hash":"0x123","blockNumber":"0x1"}}`))
		resp.SetUpstream(mockUpstreams[0])
		req.SetLastValidResponse(resp)

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

		cache.policies = []*data.CachePolicy{policy1, policy2}

		mockConnectors[0].On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
		mockConnectors[1].On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)

		err = cache.Set(context.Background(), req, resp)

		assert.NoError(t, err)
		mockConnectors[0].AssertCalled(t, "Set", mock.Anything, "evm:123:*", mock.Anything, mock.Anything, mock.Anything)
		mockConnectors[1].AssertCalled(t, "Set", mock.Anything, "evm:123:*", mock.Anything, mock.Anything, mock.Anything)
	})

	t.Run("RespectFinalityStateWhenStoring", func(t *testing.T) {
		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures([]upsTestCfg{
			{id: "upsA", syncing: common.EvmSyncingStateNotSyncing, finBn: 10, lstBn: 15},
		})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getTransactionByHash","params":["0xdddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"],"id":1}`))
		req.SetNetwork(mockNetwork)
		resp := common.NewNormalizedResponse().WithRequest(req).WithBody(util.StringToReaderCloser(`{"result":{"hash":"0xdddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","blockNumber":"0x1"}}`))
		resp.SetUpstream(mockUpstreams[0])
		req.SetLastValidResponse(resp)

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

		cache.policies = []*data.CachePolicy{policy1, policy2}

		mockConnectors[0].On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
		mockConnectors[1].On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)

		err = cache.Set(context.Background(), req, resp)

		assert.NoError(t, err)
		// Only the finalized policy connector should be called since block 5 is finalized
		mockConnectors[0].AssertCalled(t, "Set", mock.Anything, "evm:123:*", mock.Anything, mock.Anything, mock.Anything)
		mockConnectors[1].AssertNotCalled(t, "Set")
	})

	t.Run("CachingBehaviorWithDefaultConfig", func(t *testing.T) {
		// Create test fixtures with default config
		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures([]upsTestCfg{
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
			TTL:       30 * time.Second,
			Connector: "mock1",
		}, mockConnectors[0])
		require.NoError(t, err)

		unfinalizedPolicy, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Network:   "*",
			Method:    "*",
			Finality:  common.DataFinalityStateUnfinalized,
			TTL:       30 * time.Second,
			Connector: "mock1",
		}, mockConnectors[0])
		require.NoError(t, err)

		cache.policies = []*data.CachePolicy{finalizedPolicy, unknownPolicy, unfinalizedPolicy}

		testCases := []struct {
			name           string
			method         string
			params         string
			result         string
			expectedCache  bool
			expectedPolicy *data.CachePolicy
		}{
			{
				name:           "Latest Block Request",
				method:         "eth_getBlockByNumber",
				params:         `["latest",false]`,
				result:         `"result":{"number":"0xf","hash":"0xabc"}`,
				expectedCache:  true,
				expectedPolicy: unfinalizedPolicy, // Should match unfinalized policy with 30s TTL
			},
			{
				name:           "Finalized Block Request",
				method:         "eth_getBlockByNumber",
				params:         `["0x5",false]`,
				result:         `"result":{"number":"0x5","hash":"0xdef"}`,
				expectedCache:  true,
				expectedPolicy: finalizedPolicy, // Should match finalized policy with no TTL
			},
			{
				name:           "Pending Transaction Request",
				method:         "eth_getTransactionByHash",
				params:         `["0x123"]`,
				result:         `"result":{"hash":"0x123","blockNumber":null}`,
				expectedCache:  true,
				expectedPolicy: unfinalizedPolicy, // Should match unfinalized policy with 30s TTL
			},
			{
				name:           "Unknown Block Number Request",
				method:         "eth_getAccount",
				params:         `["0xabc","0xdddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"]`,
				result:         `"result":{"balance":"0x123"}`,
				expectedCache:  true,
				expectedPolicy: unknownPolicy, // Should match unknown policy with 30s TTL
			},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				req := common.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","method":"%s","params":%s,"id":1}`, tc.method, tc.params)))
				req.SetNetwork(mockNetwork)
				resp := common.NewNormalizedResponse().WithRequest(req).WithBody(util.StringToReaderCloser(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,%s}`, tc.result)))
				resp.SetUpstream(mockUpstreams[0])
				req.SetLastValidResponse(resp)

				mockConnectors[0].On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)

				err := cache.Set(context.Background(), req, resp)

				assert.NoError(t, err)
				if tc.expectedCache {
					mockConnectors[0].AssertCalled(t, "Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, tc.expectedPolicy.GetTTL())
				} else {
					mockConnectors[0].AssertNotCalled(t, "Set")
				}
			})
		}
	})

	t.Run("CustomPolicyForLatestBlocks", func(t *testing.T) {
		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures([]upsTestCfg{
			{id: "upsA", syncing: common.EvmSyncingStateNotSyncing, finBn: 10, lstBn: 15},
		})

		// Create a custom policy specifically for latest block requests
		latestBlockPolicy, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Network:  "evm:123",
			Method:   "eth_getBlockByNumber",
			Params:   []interface{}{"latest", "*"},
			TTL:      5 * time.Second,
			Finality: common.DataFinalityStateUnfinalized,
		}, mockConnectors[0])
		require.NoError(t, err)
		cache.policies = []*data.CachePolicy{latestBlockPolicy}

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["latest",false],"id":1}`))
		req.SetNetwork(mockNetwork)
		resp := common.NewNormalizedResponse().WithRequest(req).WithBody(util.StringToReaderCloser(`{"result":{"number":"0xf","hash":"0xabc"}}`))
		resp.SetUpstream(mockUpstreams[0])
		req.SetLastValidResponse(resp)

		ttl := 5 * time.Second
		mockConnectors[0].On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, &ttl).Return(nil)

		err = cache.Set(context.Background(), req, resp)

		assert.NoError(t, err)
		mockConnectors[0].AssertCalled(t, "Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, &ttl)
	})
}

func TestEvmJsonRpcCache_Set_WithTTL(t *testing.T) {
	t.Run("ShouldSetTTLWhenPolicyDefinesIt", func(t *testing.T) {
		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures([]upsTestCfg{
			{id: "upsA", syncing: common.EvmSyncingStateNotSyncing, finBn: 10, lstBn: 15},
		})

		ttl := 5 * time.Minute
		policy, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Network: "evm:123",
			Method:  "eth_getBalance",
			TTL:     ttl,
		}, mockConnectors[0])
		require.NoError(t, err)
		cache.policies = []*data.CachePolicy{
			policy,
		}

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123","0x5"],"id":1}`))
		req.SetNetwork(mockNetwork)
		resp := common.NewNormalizedResponse().WithRequest(req).WithBody(util.StringToReaderCloser(`{"result":"0x0"}`))
		resp.SetUpstream(mockUpstreams[0])
		req.SetLastValidResponse(resp)

		mockConnectors[0].On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, &ttl).Return(nil)

		err = cache.Set(context.Background(), req, resp)

		assert.NoError(t, err)
		mockConnectors[0].AssertCalled(t, "Set", mock.Anything, "evm:123:5", mock.Anything, mock.Anything, &ttl)
	})

	t.Run("ShouldNotSetTTLWhenPolicyDoesNotDefineIt", func(t *testing.T) {
		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures([]upsTestCfg{{id: "upsA", syncing: common.EvmSyncingStateNotSyncing, finBn: 10, lstBn: 15}})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123","0x5"],"id":1}`))
		req.SetNetwork(mockNetwork)
		resp := common.NewNormalizedResponse().WithRequest(req).WithBody(util.StringToReaderCloser(`{"result":"0x0"}`))
		resp.SetUpstream(mockUpstreams[0])
		req.SetLastValidResponse(resp)

		err := cache.Set(context.Background(), req, resp)

		assert.NoError(t, err)
		mockConnectors[0].AssertNotCalled(t, "Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, (*time.Duration)(nil))
	})

	t.Run("ShouldRespectPolicyNetworkAndMethodMatching", func(t *testing.T) {
		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures([]upsTestCfg{{id: "upsA", syncing: common.EvmSyncingStateNotSyncing, finBn: 10, lstBn: 15}})

		policy0, err0 := data.NewCachePolicy(&common.CachePolicyConfig{
			Network: "evm:123",
			Method:  "eth_getBlockByNumber",
			TTL:     2 * time.Minute,
		}, mockConnectors[0])
		require.NoError(t, err0)

		ttl := 6 * time.Minute
		policy1, err1 := data.NewCachePolicy(&common.CachePolicyConfig{
			Network: "evm:123",
			Method:  "eth_getBalance",
			TTL:     ttl,
		}, mockConnectors[1])
		require.NoError(t, err1)

		cache.policies = []*data.CachePolicy{
			policy0,
			policy1,
		}

		req1 := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123","0x5"],"id":1}`))
		req1.SetNetwork(mockNetwork)
		resp1 := common.NewNormalizedResponse().WithRequest(req1).WithBody(util.StringToReaderCloser(`{"result":"0x0"}`))
		resp1.SetUpstream(mockUpstreams[0])
		req1.SetLastValidResponse(resp1)

		mockConnectors[0].On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
		mockConnectors[1].On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)

		err := cache.Set(context.Background(), req1, resp1)

		assert.NoError(t, err)
		mockConnectors[0].AssertNotCalled(t, "Set")
		mockConnectors[1].AssertCalled(t, "Set", mock.Anything, "evm:123:5", mock.Anything, mock.Anything, &ttl)
	})
}

func TestEvmJsonRpcCache_Get(t *testing.T) {
	t.Run("ReturnCachedResponseForFinalizedBlock", func(t *testing.T) {
		mockConnectors, mockNetwork, _, cache := createCacheTestFixtures([]upsTestCfg{{id: "upsA", syncing: common.EvmSyncingStateUnknown, finBn: 10, lstBn: 15}})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["0x1",false],"id":1}`))
		req.SetNetwork(mockNetwork)

		policy, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Network: "evm:123",
			Method:  "eth_getBlockByNumber",
		}, mockConnectors[0])
		require.NoError(t, err)
		cache.policies = []*data.CachePolicy{
			policy,
		}

		cachedResponse := `{"number":"0x1","hash":"0xabc"}`
		mockConnectors[0].On("Get", mock.Anything, mock.Anything, "evm:123:1", mock.Anything, mock.Anything).Return(cachedResponse, nil)

		resp, err := cache.Get(context.Background(), req)

		assert.NoError(t, err)
		assert.NotNil(t, resp)
		assert.True(t, resp.FromCache())
		jrr, err := resp.JsonRpcResponse()
		assert.NoError(t, err)
		assert.Equal(t, cachedResponse, string(jrr.Result))
	})

	t.Run("SkipCacheForUnfinalizedBlock", func(t *testing.T) {
		mockConnectors, mockNetwork, _, cache := createCacheTestFixtures([]upsTestCfg{{id: "upsA", syncing: common.EvmSyncingStateUnknown, finBn: 10, lstBn: 15}})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["0x32345",false],"id":1}`))
		req.SetNetwork(mockNetwork)
		resp, err := cache.Get(context.Background(), req)

		assert.NoError(t, err)
		assert.Nil(t, resp)
		mockConnectors[0].AssertNotCalled(t, "Get")
	})

	t.Run("CheckAllConnectorsInOrder", func(t *testing.T) {
		mockConnectors, mockNetwork, _, cache := createCacheTestFixtures([]upsTestCfg{
			{id: "upsA", syncing: common.EvmSyncingStateUnknown, finBn: 10, lstBn: 15},
		})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["0x1",false],"id":1}`))
		req.SetNetwork(mockNetwork)

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

		cache.policies = []*data.CachePolicy{policy1, policy2}

		// First connector returns nil
		mockConnectors[0].On("Get", mock.Anything, mock.Anything, "evm:123:1", mock.Anything, mock.Anything).Return("", common.NewErrRecordNotFound("test", "mock1"))

		// Second connector returns data
		cachedResponse := `{"number":"0x1","hash":"0xabc"}`
		mockConnectors[1].On("Get", mock.Anything, mock.Anything, "evm:123:1", mock.Anything, mock.Anything).Return(cachedResponse, nil)

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
		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures([]upsTestCfg{{id: "upsA", syncing: common.EvmSyncingStateSyncing, finBn: 10, lstBn: 15}})

		req := common.NewNormalizedRequest([]byte(`{
			"jsonrpc": "2.0",
			"method": "eth_getBalance",
			"params": ["0x123", "0x5"],
			"id": 1
		}`))
		req.SetNetwork(mockNetwork)

		resp := common.NewNormalizedResponse().WithBody(util.StringToReaderCloser(`{"result":null}`)).WithRequest(req)
		resp.SetUpstream(mockUpstreams[0])
		req.SetLastValidResponse(resp)

		err := cache.Set(context.Background(), req, resp)
		assert.NoError(t, err)
		mockConnectors[0].AssertNotCalled(t, "Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	})

	t.Run("ShouldNotCacheEmptyResponseWhenBlockNotFinalizedOnSpecificUpstream", func(t *testing.T) {
		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures([]upsTestCfg{
			{id: "upsA", syncing: common.EvmSyncingStateNotSyncing, finBn: 3, lstBn: 15},
			{id: "upsB", syncing: common.EvmSyncingStateNotSyncing, finBn: 10, lstBn: 16},
		})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123","0x5"],"id":1}`))
		req.SetNetwork(mockNetwork)
		resp := common.NewNormalizedResponse().WithBody(util.StringToReaderCloser(`{"result":null}`)).WithRequest(req)
		resp.SetUpstream(mockUpstreams[0])
		req.SetLastValidResponse(resp)

		err := cache.Set(context.Background(), req, resp)

		assert.NoError(t, err)
		mockConnectors[0].AssertNotCalled(t, "Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	})

	t.Run("ShouldCacheEmptyResponseWhenBlockFinalizedOnSpecificUpstream", func(t *testing.T) {
		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures([]upsTestCfg{
			{id: "upsA", syncing: common.EvmSyncingStateNotSyncing, finBn: 3, lstBn: 15},
			{id: "upsB", syncing: common.EvmSyncingStateNotSyncing, finBn: 10, lstBn: 16},
		})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123","0x5"],"id":1}`))
		req.SetNetwork(mockNetwork)
		resp := common.NewNormalizedResponse().WithBody(util.StringToReaderCloser(`{"result":null}`)).WithRequest(req)
		resp.SetUpstream(mockUpstreams[1])
		req.SetLastValidResponse(resp)

		policy, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Network: "evm:123",
			Method:  "eth_getBalance",
			Empty:   common.CacheEmptyBehaviorAllow,
		}, mockConnectors[0])
		require.NoError(t, err)
		cache.policies = []*data.CachePolicy{
			policy,
		}

		mockConnectors[0].On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)

		err = cache.Set(context.Background(), req, resp)

		assert.NoError(t, err)
		mockConnectors[0].AssertCalled(t, "Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	})

	t.Run("ShouldCacheEmptyResponseWhenBlockFinalizedOnSpecificNonSyncedUpstream", func(t *testing.T) {
		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures([]upsTestCfg{
			{id: "upsA", syncing: common.EvmSyncingStateNotSyncing, finBn: 3, lstBn: 15},
			{id: "upsB", syncing: common.EvmSyncingStateSyncing, finBn: 10, lstBn: 16},
		})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123","0x5"],"id":1}`))
		req.SetNetwork(mockNetwork)
		resp := common.NewNormalizedResponse().WithBody(util.StringToReaderCloser(`{"result":null}`)).WithRequest(req)
		resp.SetUpstream(mockUpstreams[1])
		req.SetLastValidResponse(resp)

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
			matchesSet, err := policy.MatchesForSet(tc.config.Network, tc.method, tc.params, common.DataFinalityStateFinalized)
			require.NoError(t, err)
			matchesGet, err := policy.MatchesForGet(tc.config.Network, tc.method, tc.params)
			require.NoError(t, err)

			assert.Equal(t, tc.matches, matchesSet, "MatchesForSet returned unexpected result")
			assert.Equal(t, tc.matches, matchesGet, "MatchesForGet returned unexpected result")
		})
	}
}

func TestEvmJsonRpcCache_EmptyStates(t *testing.T) {
	t.Run("EmptyStateBehaviors", func(t *testing.T) {
		mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures([]upsTestCfg{
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
				cache.policies = []*data.CachePolicy{policy}

				// Create request and response
				req := common.NewNormalizedRequest([]byte(`{
					"jsonrpc": "2.0",
					"method": "eth_getBalance",
					"params": ["0x123", "0x5"],
					"id": 1
				}`))
				req.SetNetwork(mockNetwork)
				resp := common.NewNormalizedResponse().
					WithBody(util.StringToReaderCloser(tc.response)).
					WithRequest(req)
				resp.SetUpstream(mockUpstreams[0])
				req.SetLastValidResponse(resp)

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
	mockConnectors, mockNetwork, mockUpstreams, cache := createCacheTestFixtures([]upsTestCfg{
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
			cache.policies = []*data.CachePolicy{policy}

			// Create response with specific size
			responseBody := strings.Repeat("x", tc.responseSize)

			req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123","0x5"],"id":1}`))
			req.SetNetwork(mockNetwork)
			resp := common.NewNormalizedResponse().
				WithBody(util.StringToReaderCloser(fmt.Sprintf(`{"result":"%s"}`, responseBody))).
				WithRequest(req)
			resp.SetUpstream(mockUpstreams[0])
			req.SetLastValidResponse(resp)

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
