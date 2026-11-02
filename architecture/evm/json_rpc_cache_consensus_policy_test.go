package evm

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

// TestEvmJsonRpcCache_CacheableConsensusPolicies exercises the write-side
// admission allowlist: when the operator sets a non-empty list, a response
// governed by a non-allowlisted consensus policy is never persisted. When
// the list is nil/empty the filter is off (Forward keeps !selectorOn).
func TestEvmJsonRpcCache_CacheableConsensusPolicies(t *testing.T) {
	ctx := context.Background()
	logger := log.Logger
	now := time.Now().Unix()

	newCache := func(t *testing.T, conn data.Connector, allowlist map[string]struct{}) *EvmJsonRpcCache {
		t.Helper()
		return &EvmJsonRpcCache{
			projectId:                  "test-project",
			logger:                     &logger,
			policies:                   []*data.CachePolicy{tsPolicy(t, conn, time.Minute)},
			cacheableConsensusPolicies: allowlist,
		}
	}

	newReqResp := func(stampedPolicy string) (*common.NormalizedRequest, *common.NormalizedResponse) {
		req := realtimeReq("eth_getBlockByNumber", `["latest",false]`)
		if stampedPolicy != "" {
			req.ExecState().SetConsensusPolicy(stampedPolicy)
		}
		resp := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(blockJrr(now))
		return req, resp
	}

	defaultList := map[string]struct{}{common.ConsensusDefaultPolicyName: {}}

	t.Run("Allowlist_NamedPolicyNotCached", func(t *testing.T) {
		conn := plainConnector("conn")
		conn.On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
		cache := newCache(t, conn, defaultList)

		req, resp := newReqResp("fallback")
		require.NoError(t, cache.Set(ctx, req, resp))
		conn.AssertNotCalled(t, "Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	})

	t.Run("FilterOff_NamedPolicyCached", func(t *testing.T) {
		conn := plainConnector("conn")
		conn.On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
		cache := newCache(t, conn, nil) // nil/empty config → filter off

		req, resp := newReqResp("fallback")
		require.NoError(t, cache.Set(ctx, req, resp))
		conn.AssertCalled(t, "Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	})

	t.Run("Allowlist_DefaultPolicyCached", func(t *testing.T) {
		conn := plainConnector("conn")
		conn.On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
		cache := newCache(t, conn, defaultList)

		req, resp := newReqResp(common.ConsensusDefaultPolicyName)
		require.NoError(t, cache.Set(ctx, req, resp))
		conn.AssertCalled(t, "Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	})

	t.Run("Allowlist_NoConsensusStampCached", func(t *testing.T) {
		conn := plainConnector("conn")
		conn.On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
		cache := newCache(t, conn, defaultList)

		req, resp := newReqResp("")
		require.NoError(t, cache.Set(ctx, req, resp))
		conn.AssertCalled(t, "Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	})

	t.Run("ExtendedAllowlist_NamedPolicyCached", func(t *testing.T) {
		conn := plainConnector("conn")
		conn.On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
		cache := newCache(t, conn, map[string]struct{}{
			common.ConsensusDefaultPolicyName: {},
			"strict":                          {},
		})

		req, resp := newReqResp("strict")
		require.NoError(t, cache.Set(ctx, req, resp))
		conn.AssertCalled(t, "Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	})
}

// TestNewEvmJsonRpcCache_CacheableConsensusPoliciesWiring checks that
// nil/empty keep the historical skip, and an explicit list is opt-in.
func TestNewEvmJsonRpcCache_CacheableConsensusPoliciesWiring(t *testing.T) {
	ctx := context.Background()
	logger := log.Logger

	newCfg := func(list []string) *common.CacheConfig {
		return &common.CacheConfig{
			Connectors: []*common.ConnectorConfig{{
				Id:     "conn",
				Driver: common.DriverMemory,
				Memory: &common.MemoryConnectorConfig{MaxItems: 1000, MaxTotalSize: "10MB"},
			}},
			Policies: []*common.CachePolicyConfig{{
				Connector: "conn",
				Network:   "*",
				Method:    "*",
				Finality:  common.DataFinalityStateRealtime,
			}},
			CacheableConsensusPolicies: list,
		}
	}

	t.Run("NilKeepsHistoricalSkip", func(t *testing.T) {
		cache, err := NewEvmJsonRpcCache(ctx, &logger, newCfg(nil))
		require.NoError(t, err)
		assert.Nil(t, cache.cacheableConsensusPolicies)
		assert.False(t, cache.AllowsSelectorCache(), "omitted list must not open the selector cache path")
	})

	t.Run("EmptyKeepsHistoricalSkip", func(t *testing.T) {
		cache, err := NewEvmJsonRpcCache(ctx, &logger, newCfg([]string{}))
		require.NoError(t, err)
		assert.Nil(t, cache.cacheableConsensusPolicies)
		assert.False(t, cache.AllowsSelectorCache())
	})

	t.Run("ExplicitListOptIn", func(t *testing.T) {
		cache, err := NewEvmJsonRpcCache(ctx, &logger, newCfg([]string{common.ConsensusDefaultPolicyName}))
		require.NoError(t, err)
		assert.True(t, cache.AllowsSelectorCache(), "listing policies is the opt-in that opens the selector cache path")
		_, ok := cache.cacheableConsensusPolicies[common.ConsensusDefaultPolicyName]
		assert.True(t, ok)
		cloned := cache.WithProjectId("other-project")
		assert.True(t, cloned.AllowsSelectorCache())
	})
}

func TestResolveCacheableConsensusPolicies(t *testing.T) {
	assert.Nil(t, resolveCacheableConsensusPolicies(nil))
	assert.Nil(t, resolveCacheableConsensusPolicies([]string{}))
	assert.Nil(t, resolveCacheableConsensusPolicies([]string{""}))
	assert.Equal(t, map[string]struct{}{"default": {}, "strict": {}},
		resolveCacheableConsensusPolicies([]string{"default", "strict", ""}))
}
