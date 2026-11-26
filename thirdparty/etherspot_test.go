package thirdparty

import (
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEtherspotVendor_Name(t *testing.T) {
	assert.Equal(t, "etherspot", CreateEtherspotVendor().Name())
}

func TestEtherspotVendor_SupportsNetwork(t *testing.T) {
	svtCheckSupports(t, CreateEtherspotVendor(), common.VendorSettings{}, []svtSupportCase{
		{"evm:1", true, false},        // mainnet
		{"evm:11155111", true, false}, // testnet
		{"evm:999999999", false, false},
		{"solana:mainnet", false, false},
		{"evm:abc", false, true},
	})
}

func TestEtherspotVendor_GenerateConfigs(t *testing.T) {
	svtCheckURLs(t, CreateEtherspotVendor, common.VendorSettings{"apiKey": "KEY"}, map[int64]string{
		1:        "https://ethereum-bundler.etherspot.io/?apikey=KEY",
		8453:     "https://base-bundler.etherspot.io/?apikey=KEY",
		11155111: "https://testnet-rpc.etherspot.io/v1/11155111?apikey=KEY",
	})
	svtCheckURLs(t, CreateEtherspotVendor, common.VendorSettings{"apiKey": "public"}, map[int64]string{
		1:     "https://ethereum-bundler.etherspot.io/",
		84532: "https://testnet-rpc.etherspot.io/v1/84532",
	})

	key := common.VendorSettings{"apiKey": "KEY"}
	svtCheckGenErrors(t, CreateEtherspotVendor, []svtErrCase{
		{"missing apiKey", svtEvm(1), common.VendorSettings{}, "apiKey is required in etherspot settings"},
		{"zero chain", svtEvm(0), key, "etherspot vendor requires upstream.evm.chainId to be defined"},
		{"unparsable url", svtEvm(1), common.VendorSettings{"apiKey": "a\nb"}, "invalid control character"},
	})

	t.Run("unknown chain yields an apikey-only url", func(t *testing.T) {
		cfg, err := svtGenerate(t, CreateEtherspotVendor(), svtEvm(999999999), key)
		require.NoError(t, err)
		assert.Equal(t, "?apikey=KEY", cfg.Endpoint)
	})

	t.Run("method allowlist defaults to bundler methods", func(t *testing.T) {
		cfg, err := svtGenerate(t, CreateEtherspotVendor(), svtEvm(1), key)
		require.NoError(t, err)
		assert.Equal(t, []string{"*"}, cfg.IgnoreMethods)
		assert.Contains(t, cfg.AllowMethods, "eth_sendUserOperation")
		assert.Contains(t, cfg.AllowMethods, "skandha_config")
	})

	t.Run("caller method lists are kept", func(t *testing.T) {
		ups := svtEvm(1)
		ups.IgnoreMethods = []string{"eth_call"}
		ups.AllowMethods = []string{"eth_chainId"}
		cfg, err := svtGenerate(t, CreateEtherspotVendor(), ups, key)
		require.NoError(t, err)
		assert.Equal(t, []string{"eth_call"}, cfg.IgnoreMethods)
		assert.Equal(t, []string{"eth_chainId"}, cfg.AllowMethods)
	})

	svtCheckPreset(t, CreateEtherspotVendor(), "https://ethereum-bundler.etherspot.io/")
}

func TestEtherspotVendor_GetVendorSpecificErrorIfAny(t *testing.T) {
	v := CreateEtherspotVendor()
	err, _ := vendorErr(t, v, "eth_sendUserOperation", 400, -32000, "aa error", "d")
	assert.NoError(t, err)
}

func TestEtherspotVendor_OwnsUpstream(t *testing.T) {
	svtAssertOwns(t, CreateEtherspotVendor(), map[string]bool{
		"etherspot://key":                       true,
		"evm+etherspot://key":                   true,
		"https://ethereum-bundler.etherspot.io": true,
		"https://rpc.ankr.com/eth/key":          false,
	})
}
