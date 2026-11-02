package svm

import (
	"context"
	"testing"

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

// TestSvmJsonRpcCache_CacheableConsensusPolicies exercises the write-side
// admission allowlist on the SVM path — same contract as EvmJsonRpcCache.
func TestSvmJsonRpcCache_CacheableConsensusPolicies(t *testing.T) {
	ctx := context.Background()

	newCache := func(t *testing.T, conn data.Connector, allowlist map[string]struct{}) *SvmJsonRpcCache {
		t.Helper()
		policy, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Network:   "*",
			Method:    "*",
			Finality:  common.DataFinalityStateFinalized,
			Connector: conn.Id(),
		}, conn)
		require.NoError(t, err)
		return &SvmJsonRpcCache{
			projectId:                  "test-project",
			logger:                     &log.Logger,
			policies:                   []*data.CachePolicy{policy},
			cacheableConsensusPolicies: allowlist,
		}
	}

	newReqResp := func(stampedPolicy string) (*common.NormalizedRequest, *common.NormalizedResponse) {
		body := []byte(`{"jsonrpc":"2.0","id":1,"method":"getBlock","params":[100,{"commitment":"finalized"}]}`)
		req := common.NewNormalizedRequest(body)
		req.SetNetwork(finalizedNetwork{})
		if stampedPolicy != "" {
			req.ExecState().SetConsensusPolicy(stampedPolicy)
		}
		jrr, err := common.NewJsonRpcResponse(1, map[string]interface{}{"blockhash": "abc"}, nil)
		require.NoError(t, err)
		resp := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
		return req, resp
	}

	defaultList := map[string]struct{}{common.ConsensusDefaultPolicyName: {}}

	t.Run("Allowlist_NamedPolicyNotCached", func(t *testing.T) {
		conn := data.NewMockConnector("conn")
		conn.On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
		cache := newCache(t, conn, defaultList)

		req, resp := newReqResp("fallback")
		require.NoError(t, cache.Set(ctx, req, resp))
		conn.AssertNotCalled(t, "Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	})

	t.Run("FilterOff_NamedPolicyCached", func(t *testing.T) {
		conn := data.NewMockConnector("conn")
		conn.On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
		cache := newCache(t, conn, nil)

		req, resp := newReqResp("fallback")
		require.NoError(t, cache.Set(ctx, req, resp))
		conn.AssertCalled(t, "Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	})

	t.Run("Allowlist_DefaultPolicyCached", func(t *testing.T) {
		conn := data.NewMockConnector("conn")
		conn.On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
		cache := newCache(t, conn, defaultList)

		req, resp := newReqResp(common.ConsensusDefaultPolicyName)
		require.NoError(t, cache.Set(ctx, req, resp))
		conn.AssertCalled(t, "Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	})

	t.Run("Allowlist_NoConsensusStampCached", func(t *testing.T) {
		conn := data.NewMockConnector("conn")
		conn.On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
		cache := newCache(t, conn, defaultList)

		req, resp := newReqResp("")
		require.NoError(t, cache.Set(ctx, req, resp))
		conn.AssertCalled(t, "Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	})

	t.Run("ExtendedAllowlist_NamedPolicyCached", func(t *testing.T) {
		conn := data.NewMockConnector("conn")
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

// TestNewSvmJsonRpcCache_CacheableConsensusPoliciesWiring checks that
// nil/empty keep the historical skip, and an explicit list is opt-in.
func TestNewSvmJsonRpcCache_CacheableConsensusPoliciesWiring(t *testing.T) {
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
		cache, err := NewSvmJsonRpcCache(ctx, &logger, newCfg(nil))
		require.NoError(t, err)
		assert.Nil(t, cache.cacheableConsensusPolicies)
		assert.False(t, cache.AllowsSelectorCache(), "omitted list must not open the selector cache path")
	})

	t.Run("EmptyKeepsHistoricalSkip", func(t *testing.T) {
		cache, err := NewSvmJsonRpcCache(ctx, &logger, newCfg([]string{}))
		require.NoError(t, err)
		assert.Nil(t, cache.cacheableConsensusPolicies)
		assert.False(t, cache.AllowsSelectorCache())
	})

	t.Run("ExplicitListOptIn", func(t *testing.T) {
		cache, err := NewSvmJsonRpcCache(ctx, &logger, newCfg([]string{common.ConsensusDefaultPolicyName}))
		require.NoError(t, err)
		assert.True(t, cache.AllowsSelectorCache(), "listing policies is the opt-in that opens the selector cache path")
		_, ok := cache.cacheableConsensusPolicies[common.ConsensusDefaultPolicyName]
		assert.True(t, ok)
		cloned := cache.WithProjectId("other-project")
		assert.True(t, cloned.AllowsSelectorCache())
	})
}
