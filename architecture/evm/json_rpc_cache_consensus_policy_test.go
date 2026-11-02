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
// admission allowlist: when active (the default ["default"]), a response
// governed by a non-allowlisted consensus policy is never persisted, because
// a later cache hit would serve it with zero validation. Responses that ran
// under an allowlisted policy — or never went through a consensus selector —
// stay cacheable.
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

	t.Run("DefaultAllowlist_NamedPolicyNotCached", func(t *testing.T) {
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
		cache := newCache(t, conn, nil) // explicit empty config → filter off

		req, resp := newReqResp("fallback")
		require.NoError(t, cache.Set(ctx, req, resp))
		conn.AssertCalled(t, "Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	})

	t.Run("DefaultPolicyCached", func(t *testing.T) {
		conn := plainConnector("conn")
		conn.On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
		cache := newCache(t, conn, defaultList)

		req, resp := newReqResp(common.ConsensusDefaultPolicyName)
		require.NoError(t, cache.Set(ctx, req, resp))
		conn.AssertCalled(t, "Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	})

	t.Run("NoConsensusStampCached", func(t *testing.T) {
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

// TestNewEvmJsonRpcCache_CacheableConsensusPoliciesWiring checks nil →
// ["default"], explicit empty → filter off, and the per-project clone.
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

	t.Run("NilMeansDefaultOnly", func(t *testing.T) {
		cache, err := NewEvmJsonRpcCache(ctx, &logger, newCfg(nil))
		require.NoError(t, err)
		_, ok := cache.cacheableConsensusPolicies[common.ConsensusDefaultPolicyName]
		assert.True(t, ok)
		assert.Len(t, cache.cacheableConsensusPolicies, 1)
		assert.True(t, cache.AllowsSelectorCache())
	})

	t.Run("ExplicitDefaultList", func(t *testing.T) {
		cache, err := NewEvmJsonRpcCache(ctx, &logger, newCfg([]string{common.ConsensusDefaultPolicyName}))
		require.NoError(t, err)
		assert.True(t, cache.AllowsSelectorCache())
		_, ok := cache.cacheableConsensusPolicies[common.ConsensusDefaultPolicyName]
		assert.True(t, ok)
	})

	t.Run("ExplicitEmptyDisablesFilter", func(t *testing.T) {
		cache, err := NewEvmJsonRpcCache(ctx, &logger, newCfg([]string{}))
		require.NoError(t, err)
		assert.Nil(t, cache.cacheableConsensusPolicies)
		assert.False(t, cache.AllowsSelectorCache(), "empty list keeps the historical !selectorOn skip")
		cloned := cache.WithProjectId("other-project")
		assert.Nil(t, cloned.cacheableConsensusPolicies)
		assert.False(t, cloned.AllowsSelectorCache())
	})
}

func TestResolveCacheableConsensusPolicies(t *testing.T) {
	assert.Equal(t, map[string]struct{}{common.ConsensusDefaultPolicyName: {}}, resolveCacheableConsensusPolicies(nil))
	assert.Nil(t, resolveCacheableConsensusPolicies([]string{}))
	assert.Equal(t, map[string]struct{}{"default": {}, "strict": {}},
		resolveCacheableConsensusPolicies([]string{"default", "strict", ""}))
}
