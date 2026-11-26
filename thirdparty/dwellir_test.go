package thirdparty

import (
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
)

func TestDwellirVendor_Name(t *testing.T) {
	assert.Equal(t, "dwellir", CreateDwellirVendor().Name())
}

func TestDwellirVendor_SupportsNetwork(t *testing.T) {
	svtCheckSupports(t, CreateDwellirVendor(), common.VendorSettings{}, []svtSupportCase{
		{"evm:1", true, false},
		{"evm:999999999", true, false},
		{"evm:123456789", false, false},
		{"solana:mainnet", false, false},
		{"evm:abc", false, false}, // dwellir swallows parse errors
	})
}

func TestDwellirVendor_GenerateConfigs(t *testing.T) {
	key := common.VendorSettings{"apiKey": "KEY"}
	svtCheckURLs(t, CreateDwellirVendor, key, map[int64]string{
		1:     "https://api-ethereum-mainnet.n.dwellir.com/KEY",
		8453:  "https://api-base-mainnet-archive.n.dwellir.com/KEY",
		43114: "https://api-avalanche-mainnet-archive.n.dwellir.com/KEY/ext/bc/C/rpc",
	})
	svtCheckGenErrors(t, CreateDwellirVendor, []svtErrCase{
		{"missing apiKey", svtEvm(1), common.VendorSettings{}, "apiKey is required in dwellir provider settings"},
		{"nil evm", &common.UpstreamConfig{}, key, "dwellir vendor requires upstream.evm to be defined"},
		{"zero chain", svtEvm(0), key, "dwellir vendor requires upstream.evm.chainId to be defined"},
		{"unsupported chain", svtEvm(123456789), key, "unsupported network chain ID for Dwellir: 123456789"},
		{"unparsable url", svtEvm(1), common.VendorSettings{"apiKey": "%zz"}, "failed to parse generated Dwellir URL"},
	})
	cfg := svtCheckPreset(t, CreateDwellirVendor(), "https://api-ethereum-mainnet.n.dwellir.com/own")
	assert.Empty(t, cfg.Type)
}

func TestDwellirVendor_GetVendorSpecificErrorIfAny(t *testing.T) {
	v := CreateDwellirVendor()
	err, _ := vendorErr(t, v, "eth_getLogs", 401, -32000, "unauthorized", "d")
	assert.NoError(t, err, "dwellir defers every error to generic handling")
	assert.NoError(t, v.GetVendorSpecificErrorIfAny(svtReq(), nil, "x", map[string]interface{}{}))
}

func TestDwellirVendor_OwnsUpstream(t *testing.T) {
	svtAssertOwns(t, CreateDwellirVendor(), map[string]bool{
		"dwellir://key": true,
		"https://api-ethereum-mainnet.n.dwellir.com/key": true,
		"evm+dwellir://key":            false,
		"https://rpc.ankr.com/eth/key": false,
	})
}
