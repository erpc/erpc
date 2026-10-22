package thirdparty

import (
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
)

func TestBlastApiVendor_Name(t *testing.T) {
	assert.Equal(t, "blastapi", CreateBlastApiVendor().Name())
}

func TestBlastApiVendor_SupportsNetwork(t *testing.T) {
	svtCheckSupports(t, CreateBlastApiVendor(), common.VendorSettings{}, []svtSupportCase{
		{"evm:1", true, false},
		{"evm:11297108109", true, false},
		{"evm:999999999", false, false},
		{"solana:mainnet", false, false},
		{"evm:abc", false, true},
	})
}

func TestBlastApiVendor_GenerateConfigs(t *testing.T) {
	key := common.VendorSettings{"apiKey": "KEY"}
	svtCheckURLs(t, CreateBlastApiVendor, key, map[int64]string{
		1:     "https://eth-mainnet.blastapi.io/KEY",
		8453:  "https://base-mainnet.blastapi.io/KEY",
		43114: "https://ava-mainnet.blastapi.io/KEY/ext/bc/C/rpc",
		43113: "https://ava-testnet.blastapi.io/KEY/ext/bc/C/rpc",
	})
	svtCheckGenErrors(t, CreateBlastApiVendor, []svtErrCase{
		{"missing apiKey", svtEvm(1), common.VendorSettings{}, "apiKey is required in blastapi settings"},
		{"non-string apiKey", svtEvm(1), common.VendorSettings{"apiKey": 42}, "apiKey is required in blastapi settings"},
		{"nil evm", &common.UpstreamConfig{}, key, "blastapi vendor requires upstream.evm to be defined"},
		{"zero chain", svtEvm(0), key, "blastapi vendor requires upstream.evm.chainId to be defined"},
		{"unsupported chain", svtEvm(999999999), key, "unsupported network chain ID for BlastAPI: 999999999"},
		{"unparsable url", svtEvm(1), common.VendorSettings{"apiKey": "%zz"}, "invalid URL escape"},
	})
	svtCheckPreset(t, CreateBlastApiVendor(), "https://eth-mainnet.blastapi.io/own")
}

func TestBlastApiVendor_GetVendorSpecificErrorIfAny(t *testing.T) {
	svtCheckDataOnly(t, CreateBlastApiVendor())
}

func TestBlastApiVendor_OwnsUpstream(t *testing.T) {
	svtAssertOwns(t, CreateBlastApiVendor(), map[string]bool{
		"blastapi://key":                      true,
		"evm+blastapi://key":                  true,
		"https://eth-mainnet.blastapi.io/key": true,
		"https://rpc.ankr.com/eth/key":        false,
	})
}
