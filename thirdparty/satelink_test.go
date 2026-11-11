package thirdparty

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSatelinkVendor_SupportsNetwork(t *testing.T) {
	vendor := CreateSatelinkVendor()
	ctx := context.Background()
	logger := zerolog.Nop()

	cases := []struct {
		networkId string
		expected  bool
	}{
		{"evm:137", true},     // Polygon mainnet
		{"evm:1", false},      // Ethereum — not supported
		{"evm:999999", false}, // unknown
		{"solana:mainnet", false},
	}

	for _, c := range cases {
		t.Run(c.networkId, func(t *testing.T) {
			ok, err := vendor.SupportsNetwork(ctx, &logger, common.VendorSettings{}, c.networkId)
			assert.NoError(t, err)
			assert.Equal(t, c.expected, ok)
		})
	}
}

func TestSatelinkVendor_GenerateConfigs(t *testing.T) {
	vendor := CreateSatelinkVendor()
	ctx := context.Background()
	logger := zerolog.Nop()

	t.Run("paid tier with api key", func(t *testing.T) {
		ups := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 137}}
		cfgs, err := vendor.GenerateConfigs(ctx, &logger, ups, common.VendorSettings{"apiKey": "sk_test_123"})
		assert.NoError(t, err)
		assert.Len(t, cfgs, 1)
		assert.Equal(t, "https://rpc.satelink.network/rpc/polygon", cfgs[0].Endpoint)
		assert.Equal(t, "sk_test_123", cfgs[0].JsonRpc.Headers["X-API-Key"])
	})

	t.Run("free tier without api key", func(t *testing.T) {
		ups := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 137}}
		cfgs, err := vendor.GenerateConfigs(ctx, &logger, ups, common.VendorSettings{})
		assert.NoError(t, err)
		assert.Len(t, cfgs, 1)
		assert.Equal(t, "https://rpc.satelink.network/rpc/polygon", cfgs[0].Endpoint)
		_, hasKey := cfgs[0].JsonRpc.Headers["X-API-Key"]
		assert.False(t, hasKey)
	})

	t.Run("unsupported chain returns empty", func(t *testing.T) {
		ups := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}}
		cfgs, err := vendor.GenerateConfigs(ctx, &logger, ups, common.VendorSettings{"apiKey": "sk_test_123"})
		assert.NoError(t, err)
		assert.Empty(t, cfgs)
	})
}

func TestSatelinkVendor_Name(t *testing.T) {
	assert.Equal(t, "satelink", CreateSatelinkVendor().Name())
}

func TestSatelinkVendor_SupportsNetworkInvalidChain(t *testing.T) {
	svtCheckSupports(t, CreateSatelinkVendor(), common.VendorSettings{}, []svtSupportCase{
		{"evm:abc", false, true},
	})
}

func TestSatelinkVendor_GenerateConfigsBranches(t *testing.T) {
	svtCheckGenErrors(t, CreateSatelinkVendor, []svtErrCase{
		{"nil evm", &common.UpstreamConfig{}, common.VendorSettings{}, "satelink vendor requires upstream.evm to be defined"},
		{"zero chain", svtEvm(0), common.VendorSettings{}, "satelink vendor requires upstream.evm.chainId to be defined"},
	})

	t.Run("explicit free sentinel sends no key header", func(t *testing.T) {
		cfg, err := svtGenerate(t, CreateSatelinkVendor(), svtEvm(137), common.VendorSettings{"apiKey": "free"})
		require.NoError(t, err)
		assert.Equal(t, "https://rpc.satelink.network/rpc/polygon", cfg.Endpoint)
		assert.Equal(t, common.UpstreamTypeEvm, cfg.Type)
		assert.NotContains(t, cfg.JsonRpc.Headers, "X-API-Key")
	})

	t.Run("caller-set key header wins", func(t *testing.T) {
		for _, h := range []string{"X-API-Key", "x-api-key"} {
			ups := svtEvm(137)
			ups.JsonRpc = &common.JsonRpcUpstreamConfig{Headers: map[string]string{h: "mine"}}
			cfg, err := svtGenerate(t, CreateSatelinkVendor(), ups, common.VendorSettings{"apiKey": "sk"})
			require.NoError(t, err)
			assert.Equal(t, map[string]string{h: "mine"}, cfg.JsonRpc.Headers, h)
		}
	})

	t.Run("preset endpoint passes through", func(t *testing.T) {
		cfg := svtCheckPreset(t, CreateSatelinkVendor(), "https://rpc.satelink.network/rpc/polygon")
		assert.Nil(t, cfg.JsonRpc.Headers)
	})
}

func TestSatelinkVendor_GetVendorSpecificErrorIfAny(t *testing.T) {
	v := CreateSatelinkVendor()

	t.Run("402 is a billing issue with payment pointers", func(t *testing.T) {
		err, details := vendorErr(t, v, "eth_getLogs", 402, -32000, "no credits", "d")
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointBillingIssue), "got %v", err)
		assert.Equal(t, common.JsonRpcErrorCapacityExceeded, normalizedCode(t, err))
		assert.Contains(t, err.Error(), "satelink credits exhausted")
		assert.Contains(t, err.Error(), "no credits")
		for k, want := range map[string]interface{}{
			"data":                "d",
			"paymentManifestUrl":  "https://rpc.satelink.network/.well-known/satelink.json",
			"paymentVaultAddress": "0x577D3716d6Ad5b676d230f5409deF9838FABaCEF",
			"paymentTokenAddress": "0xc2132D05D31c914a87C6611C10748AEb04B58e8F",
			"paymentChainId":      137,
			"paymentCalldataUrl":  "https://rpc.satelink.network/credits/deposit/initiate?amount=<usdt>",
			"pricingUrl":          "https://rpc.satelink.network/v1/pricing",
		} {
			assert.Equal(t, want, details[k], k)
		}
	})

	t.Run("429 is capacity exceeded", func(t *testing.T) {
		err, _ := vendorErr(t, v, "eth_getLogs", 429, -32000, "daily limit", "")
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointCapacityExceeded), "got %v", err)
		assert.Equal(t, common.JsonRpcErrorCapacityExceeded, normalizedCode(t, err))
	})

	t.Run("401 is unauthorized", func(t *testing.T) {
		err, _ := vendorErr(t, v, "eth_getLogs", 401, -32000, "bad key", "")
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointUnauthorized), "got %v", err)
		assert.Equal(t, common.JsonRpcErrorUnauthorized, normalizedCode(t, err))
	})

	t.Run("other status and nil response are generic", func(t *testing.T) {
		svtCheckDataOnly(t, v)
		jrr, err := common.NewJsonRpcResponse(1, nil, common.NewErrJsonRpcExceptionExternal(-32000, "x", "d"))
		require.NoError(t, err)
		details := map[string]interface{}{}
		assert.NoError(t, v.GetVendorSpecificErrorIfAny(svtReq(), nil, jrr, details))
		assert.Equal(t, "d", details["data"])
	})
}

func TestSatelinkVendor_OwnsUpstream(t *testing.T) {
	svtAssertOwns(t, CreateSatelinkVendor(), map[string]bool{
		"satelink://key":                           true,
		"evm+satelink://key":                       true,
		"https://rpc.satelink.network/rpc/polygon": true,
		"https://rpc.ankr.com/eth/key":             false,
	})
}
