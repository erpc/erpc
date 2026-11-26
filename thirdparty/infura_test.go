package thirdparty

import (
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInfuraVendor_Name(t *testing.T) {
	assert.Equal(t, "infura", CreateInfuraVendor().Name())
}

func TestInfuraVendor_SupportsNetwork(t *testing.T) {
	svtCheckSupports(t, CreateInfuraVendor(), common.VendorSettings{}, []svtSupportCase{
		{"evm:1", true, false},
		{"evm:300", true, false},
		{"evm:999999999", false, false},
		{"solana:mainnet", false, false},
		{"evm:abc", false, true},
	})
}

func TestInfuraVendor_GenerateConfigs(t *testing.T) {
	key := common.VendorSettings{"apiKey": "KEY"}
	svtCheckURLs(t, CreateInfuraVendor, key, map[int64]string{
		1:   "https://mainnet.infura.io/v3/KEY",
		137: "https://polygon-mainnet.infura.io/v3/KEY",
	})
	svtCheckGenErrors(t, CreateInfuraVendor, []svtErrCase{
		{"missing apiKey", svtEvm(1), common.VendorSettings{}, "apiKey is required in infura settings"},
		{"zero chain", svtEvm(0), key, "infura vendor requires upstream.evm.chainId to be defined"},
		{"unsupported chain", svtEvm(999999999), key, "unsupported network chain ID for Infura: 999999999"},
		{"unparsable url", svtEvm(1), common.VendorSettings{"apiKey": "%zz"}, "invalid URL escape"},
	})
	svtCheckPreset(t, CreateInfuraVendor(), "https://mainnet.infura.io/v3/own")
}

func TestInfuraVendor_GetVendorSpecificErrorIfAny(t *testing.T) {
	v := CreateInfuraVendor()

	cases := []struct {
		name    string
		code    int
		msg     string
		errCode common.ErrorCode
		norm    common.JsonRpcErrorNumber
	}{
		{"auth required", -32600, "project ID must be authenticated", common.ErrCodeEndpointUnauthorized, common.JsonRpcErrorUnauthorized},
		{"bad access key", -32600, "invalid access key", common.ErrCodeEndpointUnauthorized, common.JsonRpcErrorUnauthorized},
		{"method not supported", -32001, "unsupported", common.ErrCodeEndpointUnsupported, common.JsonRpcErrorUnsupportedException},
		{"method not found", -32004, "not found", common.ErrCodeEndpointUnsupported, common.JsonRpcErrorUnsupportedException},
		{"rate limited", -32005, "daily limit", common.ErrCodeEndpointCapacityExceeded, common.JsonRpcErrorCapacityExceeded},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err, details := vendorErr(t, v, "eth_getLogs", 400, c.code, c.msg, "d")
			require.Error(t, err)
			assert.True(t, common.HasErrorCode(err, c.errCode), "got %v", err)
			assert.Equal(t, c.norm, normalizedCode(t, err))
			assert.Equal(t, "d", details["data"])
		})
	}

	t.Run("-32600 without auth wording is generic", func(t *testing.T) {
		err, details := vendorErr(t, v, "eth_getLogs", 400, -32600, "invalid request", "")
		assert.NoError(t, err)
		assert.NotContains(t, details, "data")
	})
	t.Run("other code is generic", func(t *testing.T) {
		err, details := vendorErr(t, v, "eth_getLogs", 400, -32000, "x", "d")
		assert.NoError(t, err)
		assert.Equal(t, "d", details["data"])
	})
	t.Run("non-response input", func(t *testing.T) {
		assert.NoError(t, v.GetVendorSpecificErrorIfAny(svtReq(), nil, "x", map[string]interface{}{}))
	})
}

func TestInfuraVendor_OwnsUpstream(t *testing.T) {
	svtAssertOwns(t, CreateInfuraVendor(), map[string]bool{
		"infura://key":                     true,
		"evm+infura://key":                 true,
		"https://mainnet.infura.io/v3/key": true,
		"https://rpc.ankr.com/eth/key":     false,
	})
}
