package thirdparty

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRepository_FetchRemoteData(t *testing.T) {
	ctx := context.Background()

	t.Run("parses numeric chain keys and skips others", func(t *testing.T) {
		srv := newJSONServer(t, http.StatusOK, `{
			"1": {"endpoints": ["https://eth.a", "https://eth.b"]},
			"137": {"endpoints": []},
			"name": {"endpoints": ["https://ignored"]}
		}`)
		got, err := fetchRemoteData(ctx, srv.URL)
		require.NoError(t, err)
		assert.Equal(t, map[int64][]string{
			1:   {"https://eth.a", "https://eth.b"},
			137: {},
		}, got)
	})

	t.Run("non-2xx status is an error", func(t *testing.T) {
		srv := newJSONServer(t, http.StatusInternalServerError, `{}`)
		_, err := fetchRemoteData(ctx, srv.URL)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "500")
	})

	t.Run("invalid json is an error", func(t *testing.T) {
		srv := newJSONServer(t, http.StatusOK, `[`)
		_, err := fetchRemoteData(ctx, srv.URL)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to parse remote repository data")
	})

	t.Run("invalid url and unreachable host are errors", func(t *testing.T) {
		_, err := fetchRemoteData(ctx, "http://bad host/")
		assert.Error(t, err)
		_, err = fetchRemoteData(ctx, "http://127.0.0.1:1/")
		assert.Error(t, err)
	})
}

func TestRepositoryVendor_SupportsNetwork(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	t.Run("rejects non-evm and malformed ids", func(t *testing.T) {
		v := CreateRepositoryVendor()
		ok, err := v.SupportsNetwork(ctx, &logger, nil, "solana:mainnet")
		assert.NoError(t, err)
		assert.False(t, ok)
		_, err = v.SupportsNetwork(ctx, &logger, nil, "evm:1x")
		assert.Error(t, err)
	})

	t.Run("cold cache then populated from repository url", func(t *testing.T) {
		srv := newJSONServer(t, http.StatusOK, `{"1":{"endpoints":["https://eth.a"]},"10":{"endpoints":[]}}`)
		v := CreateRepositoryVendor()
		settings := common.VendorSettings{"repositoryUrl": srv.URL, "recheckInterval": time.Hour}

		ok, err := v.SupportsNetwork(ctx, &logger, settings, "evm:1")
		assert.ErrorIs(t, err, ErrRemoteCacheCold)
		assert.False(t, ok)

		require.Eventually(t, func() bool {
			ok, err := v.SupportsNetwork(ctx, &logger, settings, "evm:1")
			return err == nil && ok
		}, 5*time.Second, 10*time.Millisecond)

		ok, err = v.SupportsNetwork(ctx, &logger, settings, "evm:10")
		assert.NoError(t, err)
		assert.False(t, ok, "chain with no endpoints is unsupported")
	})

	t.Run("default repository url is used when unset", func(t *testing.T) {
		v := CreateRepositoryVendor().(*RepositoryVendor)
		primeRemoteCache(t, v.cache, DefaultRepositoryURL, map[int64][]string{1: {"https://eth.a"}})
		ok, err := v.SupportsNetwork(ctx, &logger, common.VendorSettings{}, "evm:1")
		require.NoError(t, err)
		assert.True(t, ok)
	})
}

func TestRepositoryVendor_GenerateConfigs(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()
	const repoUrl = "https://repo.example/endpoints"

	newPrimed := func(t *testing.T) *RepositoryVendor {
		v := CreateRepositoryVendor().(*RepositoryVendor)
		primeRemoteCache(t, v.cache, repoUrl, map[int64][]string{
			1:  {"https://eth.a/key", "wss://eth.ws", "HTTP://eth.b"},
			10: {},
		})
		return v
	}

	t.Run("generates one http upstream per endpoint with copied settings", func(t *testing.T) {
		v := newPrimed(t)
		budget := "public"
		ups := &common.UpstreamConfig{
			Id:                "repo",
			Tags:              []string{"public"},
			Evm:               &common.EvmUpstreamConfig{ChainId: 1},
			JsonRpc:           &common.JsonRpcUpstreamConfig{BatchMaxSize: 7},
			Failsafe:          []*common.FailsafeConfig{{MatchMethod: "eth_call"}},
			RateLimitAutoTune: &common.RateLimitAutoTuneConfig{},
			RateLimitBudget:   budget,
			IgnoreMethods:     []string{"debug_*"},
		}
		got, err := v.GenerateConfigs(ctx, &logger, ups, common.VendorSettings{"repositoryUrl": repoUrl, "recheckInterval": time.Hour})
		require.NoError(t, err)
		require.Len(t, got, 2, "non-http endpoints are skipped")

		assert.Equal(t, "https://eth.a/key", got[0].Endpoint)
		assert.Equal(t, "HTTP://eth.b", got[1].Endpoint)
		for _, c := range got {
			assert.Equal(t, common.UpstreamTypeEvm, c.Type)
			assert.Equal(t, "repo-"+util.RedactEndpoint(c.Endpoint), c.Id)
			assert.Equal(t, []string{"public"}, c.Tags)
			assert.Equal(t, budget, c.RateLimitBudget)
			assert.Equal(t, []string{"debug_*"}, c.IgnoreMethods)
			require.NotNil(t, c.AutoIgnoreUnsupportedMethods)
			assert.True(t, *c.AutoIgnoreUnsupportedMethods, "auto-ignore defaults to true")
			require.NotNil(t, c.Evm)
			assert.NotSame(t, ups.Evm, c.Evm, "evm config is deep-copied")
			assert.Equal(t, int64(1), c.Evm.ChainId)
			require.NotNil(t, c.JsonRpc)
			assert.NotSame(t, ups.JsonRpc, c.JsonRpc)
			assert.Equal(t, 7, c.JsonRpc.BatchMaxSize)
			require.Len(t, c.Failsafe, 1)
			assert.NotSame(t, ups.Failsafe[0], c.Failsafe[0])
			assert.Equal(t, "eth_call", c.Failsafe[0].MatchMethod)
			require.NotNil(t, c.RateLimitAutoTune)
			assert.NotSame(t, ups.RateLimitAutoTune, c.RateLimitAutoTune)
		}
	})

	t.Run("explicit auto-ignore false is kept", func(t *testing.T) {
		v := newPrimed(t)
		ups := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}, AutoIgnoreUnsupportedMethods: util.BoolPtr(false)}
		got, err := v.GenerateConfigs(ctx, &logger, ups, common.VendorSettings{"repositoryUrl": repoUrl})
		require.NoError(t, err)
		require.NotEmpty(t, got)
		assert.False(t, *got[0].AutoIgnoreUnsupportedMethods)
		assert.Nil(t, got[0].Tags)
		assert.Nil(t, got[0].Failsafe)
	})

	t.Run("validation errors", func(t *testing.T) {
		v := newPrimed(t)
		s := common.VendorSettings{"repositoryUrl": repoUrl}

		_, err := v.GenerateConfigs(ctx, &logger, &common.UpstreamConfig{}, s)
		assert.ErrorContains(t, err, "requires upstream.evm to be defined")

		_, err = v.GenerateConfigs(ctx, &logger, &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{}}, s)
		assert.ErrorContains(t, err, "requires upstream.evm.chainId")

		_, err = v.GenerateConfigs(ctx, &logger, &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 10}}, s)
		assert.ErrorContains(t, err, "chain ID 10 not found")

		_, err = v.GenerateConfigs(ctx, &logger, &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 99}}, s)
		assert.ErrorContains(t, err, "chain ID 99 not found")
	})

	t.Run("cold cache on default url is retryable", func(t *testing.T) {
		v := CreateRepositoryVendor().(*RepositoryVendor)
		// Mark a refresh for the default URL as in flight so nothing dials out.
		v.cache.inflight[DefaultRepositoryURL] = struct{}{}
		_, err := v.GenerateConfigs(ctx, &logger, &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}}, common.VendorSettings{})
		assert.ErrorIs(t, err, ErrRemoteCacheCold)
	})
}

func TestRepositoryVendor_OwnershipAndErrors(t *testing.T) {
	v := CreateRepositoryVendor()
	assert.Equal(t, "repository", v.Name())
	assert.True(t, v.OwnsUpstream(&common.UpstreamConfig{Endpoint: "repository://evm-public-endpoints.erpc.cloud"}))
	assert.True(t, v.OwnsUpstream(&common.UpstreamConfig{Endpoint: "evm+repository://x"}))
	assert.False(t, v.OwnsUpstream(&common.UpstreamConfig{Endpoint: "https://evm-public-endpoints.erpc.cloud"}))

	err, _ := vendorErr(t, v, "eth_call", 500, -32000, "boom", "")
	assert.NoError(t, err)
}
