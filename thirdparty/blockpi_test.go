package thirdparty

import (
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBlockPiVendor_Name(t *testing.T) {
	assert.Equal(t, "blockpi", CreateBlockPiVendor().Name())
}

func TestBlockPiVendor_SupportsNetwork(t *testing.T) {
	svtCheckSupports(t, CreateBlockPiVendor(), common.VendorSettings{}, []svtSupportCase{
		{"evm:1", true, false},
		{"evm:126", true, false},
		{"evm:999999999", false, false},
		{"solana:mainnet", false, false},
		{"evm:abc", false, true},
	})
}

func TestBlockPiVendor_GenerateConfigs(t *testing.T) {
	key := common.VendorSettings{"apiKey": "k y"}
	svtCheckURLs(t, CreateBlockPiVendor, key, map[int64]string{
		1:     "https://ethereum.blockpi.network/v1/rpc/k+y",
		42161: "https://arbitrum.blockpi.network/v1/rpc/k+y",
		126:   "https://movement.blockpi.network/rpc/v1/k+y/v1",
	})
	svtCheckGenErrors(t, CreateBlockPiVendor, []svtErrCase{
		{"missing apiKey", svtEvm(1), common.VendorSettings{}, "apiKey is required in BlockPi settings"},
		{"nil evm", &common.UpstreamConfig{}, key, "BlockPi vendor requires upstream.evm to be defined"},
		{"zero chain", svtEvm(0), key, "BlockPi vendor requires upstream.evm.chainId to be defined"},
		{"unsupported chain", svtEvm(999999999), key, "unsupported network chain ID for BlockPi: 999999999"},
	})
	svtCheckPreset(t, CreateBlockPiVendor(), "https://ethereum.blockpi.network/v1/rpc/own")
}

func TestBlockPiVendor_GetVendorSpecificErrorIfAny(t *testing.T) {
	v := CreateBlockPiVendor()

	for _, msg := range []string{"ApiKey is on another chain", "this API key is on another chain"} {
		err, details := vendorErr(t, v, "eth_getLogs", 400, -32000, msg, "d")
		require.Error(t, err, msg)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointUnauthorized), "%q -> %v", msg, err)
		assert.Equal(t, common.JsonRpcErrorUnauthorized, normalizedCode(t, err))
		assert.Contains(t, err.Error(), msg, "original message is preserved")
		assert.Equal(t, "d", details["data"])
	}

	// Other messages (and empty) fall through to generic handling.
	for _, msg := range []string{"rate limited", ""} {
		err, _ := vendorErr(t, v, "eth_getLogs", 400, -32000, msg, "")
		assert.NoError(t, err, "%q", msg)
	}

	svtCheckDataOnly(t, v)
}

func TestBlockPiVendor_OwnsUpstream(t *testing.T) {
	svtAssertOwns(t, CreateBlockPiVendor(), map[string]bool{
		"blockpi://key":     true,
		"evm+blockpi://key": true,
		"https://ethereum.blockpi.network/v1/rpc/key": true,
		"https://rpc.ankr.com/eth/key":                false,
	})
}
