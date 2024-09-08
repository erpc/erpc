package erpc

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/vendors"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

func createCacheTestFixtures(finBlockNumber int64, latestBlockNumber int64, syncing *bool) (*data.MockConnector, *Network, *EvmJsonRpcCache) {
	logger := zerolog.New(zerolog.NewConsoleWriter())

	mockConnector := &data.MockConnector{}
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
	mockUpstream, err := upstream.NewUpstream("test", &common.UpstreamConfig{
		Endpoint: "http://rpc1.localhost",
		Evm: &common.EvmUpstreamConfig{
			ChainId: 123,
			Syncing: syncing,
		},
	}, clr, nil, vnr, &logger, nil)
	if err != nil {
		panic(err)
	}
	metricsTracker := health.NewTracker("prjA", 100*time.Second)
	poller, err := upstream.NewEvmStatePoller(context.Background(), &logger, mockNetwork, mockUpstream, metricsTracker)
	if err != nil {
		panic(err)
	}
	poller.SuggestFinalizedBlock(finBlockNumber)
	poller.SuggestLatestBlock(latestBlockNumber)
	mockNetwork.evmStatePollers = map[string]*upstream.EvmStatePoller{
		"upsA": poller,
	}
	cache := &EvmJsonRpcCache{
		conn:    mockConnector,
		logger:  &logger,
		network: mockNetwork,
	}
	return mockConnector, mockNetwork, cache
}

func TestEvmJsonRpcCache_Set(t *testing.T) {
	t.Run("DoNotCacheWhenEthGetTransactionByHashMissingBlockNumber", func(t *testing.T) {
		mockConnector, _, cache := createCacheTestFixtures(10, 15, nil)

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getTransactionByHash","params":["0x123"],"id":1}`))
		resp := common.NewNormalizedResponse().WithBody([]byte(`{"hash":"0x123","blockNumber":null}`))

		err := cache.Set(context.Background(), req, resp)

		assert.NoError(t, err)
		mockConnector.AssertNotCalled(t, "Set")
	})

	t.Run("CacheIfBlockNumberIsFinalizedWhenBlockIsIrrelevantForPrimaryKey", func(t *testing.T) {
		mockConnector, mockNetwork, cache := createCacheTestFixtures(10, 15, nil)

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getTransactionReceipt","params":["0xabc",false],"id":1}`))
		req.SetNetwork(mockNetwork)
		resp := common.NewNormalizedResponse().WithBody([]byte(`{"result":{"hash":"0xabc","blockNumber":"0x2"}}`))

		mockConnector.On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)

		err := cache.Set(context.Background(), req, resp)

		assert.NoError(t, err)
		mockConnector.AssertCalled(t, "Set", mock.Anything, "evm:123:*", mock.Anything, mock.Anything)
	})

	t.Run("CacheIfBlockNumberIsFinalizedWhenBlockIsUsedForPrimaryKey", func(t *testing.T) {
		mockConnector, mockNetwork, cache := createCacheTestFixtures(10, 15, nil)

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["0x2",false],"id":1}`))
		req.SetNetwork(mockNetwork)
		resp := common.NewNormalizedResponse().WithBody([]byte(`{"result":{"hash":"0xabc","number":"0x2"}}`))

		mockConnector.On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)

		err := cache.Set(context.Background(), req, resp)

		assert.NoError(t, err)
		mockConnector.AssertCalled(t, "Set", mock.Anything, "evm:123:2", mock.Anything, mock.Anything)
	})

	t.Run("SkipWhenNoRefAndNoBlockNumberFound", func(t *testing.T) {
		mockConnector, _, cache := createCacheTestFixtures(10, 15, nil)

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123","latest"],"id":1}`))
		resp := common.NewNormalizedResponse().WithBody([]byte(`"0x1234"`))

		err := cache.Set(context.Background(), req, resp)

		assert.NoError(t, err)
		mockConnector.AssertNotCalled(t, "Set")
	})

	t.Run("CacheIfBlockRefFoundWhetherBlockNumberExistsOrNot", func(t *testing.T) {
		mockConnector, mockNetwork, cache := createCacheTestFixtures(10, 15, nil)

		testCases := []struct {
			name        string
			method      string
			params      string
			result      string
			expectedRef string
		}{
			{
				name:        "WithBlockNumberAndRef",
				method:      "eth_getBlockByHash",
				params:      `["0xabc",false]`,
				result:      `{"result":{"hash":"0xabc","number":"0x1"}}`,
				expectedRef: "0xabc",
			},
			{
				name:        "WithOnlyBlockRef",
				method:      "eth_getBlockByHash",
				params:      `["0xdef",false]`,
				result:      `{"result":{"hash":"0xdef"}}`,
				expectedRef: "0xdef",
			},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"` + tc.method + `","params":` + tc.params + `,"id":1}`))
				req.SetNetwork(mockNetwork)
				resp := common.NewNormalizedResponse().WithBody([]byte(tc.result))

				mockConnector.On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil).Once()

				err := cache.Set(context.Background(), req, resp)

				assert.NoError(t, err)
				mockConnector.AssertCalled(t, "Set", mock.Anything, mock.MatchedBy(func(key string) bool {
					return key == "evm:123:"+tc.expectedRef
				}), mock.Anything, mock.Anything)
			})
		}
	})

	t.Run("CacheResponseForFinalizedBlock", func(t *testing.T) {
		mockConnector, mockNetwork, cache := createCacheTestFixtures(10, 15, nil)

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["0x1",false],"id":1}`))
		req.SetNetwork(mockNetwork)
		resp := common.NewNormalizedResponse().WithBody([]byte(`{"result":{"number":"0x1","hash":"0xabc"}}`))

		mockConnector.On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)

		err := cache.Set(context.Background(), req, resp)

		assert.NoError(t, err)
		mockConnector.AssertCalled(t, "Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	})

	t.Run("SkipCachingForUnfinalizedBlock", func(t *testing.T) {
		mockConnector, _, cache := createCacheTestFixtures(10, 15, nil)

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["0x399",false],"id":1}`))
		resp := common.NewNormalizedResponse().WithBody([]byte(`{"result":{"number":"0x399","hash":"0xdef"}}`))

		err := cache.Set(context.Background(), req, resp)

		assert.NoError(t, err)
		mockConnector.AssertNotCalled(t, "Set")
	})

	t.Run("ShouldNotCacheEmptyResponseIfNodeNotSynced", func(t *testing.T) {
		mockConnector, mockNetwork, cache := createCacheTestFixtures(10, 15, &common.TRUE)

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123","latest"],"id":1}`))
		req.SetNetwork(mockNetwork)
		resp := common.NewNormalizedResponse().WithBody([]byte(`{"result":"0x0"}`))

		err := cache.Set(context.Background(), req, resp)

		assert.NoError(t, err)
		mockConnector.AssertNotCalled(t, "Set")
	})

	t.Run("ShouldNotCacheEmptyResponseIfUnknownSyncState", func(t *testing.T) {
		mockConnector, mockNetwork, cache := createCacheTestFixtures(10, 15, nil)

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123","latest"],"id":1}`))
		req.SetNetwork(mockNetwork)
		resp := common.NewNormalizedResponse().WithBody([]byte(`{"result":"0x0"}`))

		err := cache.Set(context.Background(), req, resp)

		assert.NoError(t, err)
		mockConnector.AssertNotCalled(t, "Set")
	})

	t.Run("ShouldNotCacheEmptyResponseIfBlockNotFinalized", func(t *testing.T) {
		mockConnector, mockNetwork, cache := createCacheTestFixtures(10, 15, &common.FALSE)

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123","0x14"],"id":1}`))
		req.SetNetwork(mockNetwork)
		resp := common.NewNormalizedResponse().WithBody([]byte(`{"result":"0x0"}`))

		err := cache.Set(context.Background(), req, resp)

		assert.NoError(t, err)
		mockConnector.AssertNotCalled(t, "Set")
	})

	t.Run("ShouldNotCacheEmptyResponseIfCannotDetermineBlockNumber", func(t *testing.T) {
		mockConnector, mockNetwork, cache := createCacheTestFixtures(10, 15, &common.FALSE)

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123","latest"],"id":1}`))
		req.SetNetwork(mockNetwork)
		resp := common.NewNormalizedResponse().WithBody([]byte(`{"result":"0x0"}`))

		err := cache.Set(context.Background(), req, resp)

		assert.NoError(t, err)
		mockConnector.AssertNotCalled(t, "Set")
	})

	t.Run("ShouldCacheEmptyResponseIfNodeSyncedAndBlockFinalized", func(t *testing.T) {
		mockConnector, mockNetwork, cache := createCacheTestFixtures(10, 15, &common.FALSE)

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123","0x5"],"id":1}`))
		req.SetNetwork(mockNetwork)
		resp := common.NewNormalizedResponse().WithBody([]byte(`{"result":"0x0"}`))

		mockConnector.On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)

		err := cache.Set(context.Background(), req, resp)

		assert.NoError(t, err)
		mockConnector.AssertCalled(t, "Set", mock.Anything, "evm:123:5", mock.Anything, mock.Anything)
	})
}

func TestEvmJsonRpcCache_Get(t *testing.T) {
	t.Run("ReturnCachedResponseForFinalizedBlock", func(t *testing.T) {
		mockConnector, mockNetwork, cache := createCacheTestFixtures(10, 15, nil)

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["0x1",false],"id":1}`))
		req.SetNetwork(mockNetwork)

		cachedResponse := `{"number":"0x1","hash":"0xabc"}`
		mockConnector.On("Get", mock.Anything, mock.Anything, "evm:123:1", mock.Anything).Return(cachedResponse, nil)

		resp, err := cache.Get(context.Background(), req)

		assert.NoError(t, err)
		assert.NotNil(t, resp)
		assert.True(t, resp.FromCache())
		jrr, err := resp.JsonRpcResponse()
		assert.NoError(t, err)
		assert.Equal(t, cachedResponse, string(jrr.Result))
	})

	t.Run("SkipCacheForUnfinalizedBlock", func(t *testing.T) {
		mockConnector, mockNetwork, cache := createCacheTestFixtures(10, 15, nil)

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["0x32345",false],"id":1}`))
		req.SetNetwork(mockNetwork)
		resp, err := cache.Get(context.Background(), req)

		assert.NoError(t, err)
		assert.Nil(t, resp)
		mockConnector.AssertNotCalled(t, "Get")
	})
}
