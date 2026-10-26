package thirdparty

import (
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
)

func TestOnFinalityVendor_Name(t *testing.T) {
	assert.Equal(t, "onfinality", CreateOnFinalityVendor().Name())
}

func TestOnFinalityVendor_SupportsNetwork(t *testing.T) {
	svtCheckSupports(t, CreateOnFinalityVendor(), common.VendorSettings{}, []svtSupportCase{
		{"evm:1", true, false},
		{"evm:534352", true, false},
		{"evm:999999999", false, false},
		{"solana:mainnet", false, false},
		{"evm:abc", false, true},
	})
}

func TestOnFinalityVendor_GenerateConfigs(t *testing.T) {
	key := common.VendorSettings{"apiKey": "a&b"}
	svtCheckURLs(t, CreateOnFinalityVendor, key, map[int64]string{
		1:  "https://eth.api.onfinality.io/rpc?apikey=a%26b",
		56: "https://bnb.api.onfinality.io/rpc?apikey=a%26b",
	})
	svtCheckGenErrors(t, CreateOnFinalityVendor, []svtErrCase{
		{"missing apiKey", svtEvm(1), common.VendorSettings{}, "apiKey is required in onfinality settings"},
		{"nil evm", &common.UpstreamConfig{}, key, "onfinality vendor requires upstream.evm to be defined"},
		{"zero chain", svtEvm(0), key, "onfinality vendor requires upstream.evm.chainId to be defined"},
		{"unsupported chain", svtEvm(999999999), key, "unsupported network chain ID for OnFinality: 999999999"},
	})
	svtCheckPreset(t, CreateOnFinalityVendor(), "https://eth.api.onfinality.io/rpc?apikey=own")
}

func TestOnFinalityVendor_GetVendorSpecificErrorIfAny(t *testing.T) {
	svtCheckDataOnly(t, CreateOnFinalityVendor())
}

func TestOnFinalityVendor_OwnsUpstream(t *testing.T) {
	svtAssertOwns(t, CreateOnFinalityVendor(), map[string]bool{
		"onfinality://key":                          true,
		"evm+onfinality://key":                      true,
		"https://eth.api.onfinality.io/rpc?apikey=": true,
		"https://onfinality.io/docs":                false,
	})
}
