package thirdparty

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Shared helpers for the static-vendor tests (svt = static vendor test).
// Error normalization uses vendorErr / normalizedCode from helpers_test.go.

func svtReq() *common.NormalizedRequest {
	return common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[]}`))
}

func svtGenerate(t *testing.T, v common.Vendor, ups *common.UpstreamConfig, settings common.VendorSettings) (*common.UpstreamConfig, error) {
	t.Helper()
	logger := zerolog.Nop()
	cfgs, err := v.GenerateConfigs(context.Background(), &logger, ups, settings)
	if err != nil {
		assert.Nil(t, cfgs)
		return nil, err
	}
	require.Len(t, cfgs, 1)
	return cfgs[0], nil
}

func svtSupports(t *testing.T, v common.Vendor, settings common.VendorSettings, networkId string) (bool, error) {
	t.Helper()
	logger := zerolog.Nop()
	return v.SupportsNetwork(context.Background(), &logger, settings, networkId)
}

func svtEvm(chainId int64) *common.UpstreamConfig {
	return &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: chainId}}
}

func svtAssertOwns(t *testing.T, v common.Vendor, cases map[string]bool) {
	t.Helper()
	for endpoint, want := range cases {
		assert.Equal(t, want, v.OwnsUpstream(&common.UpstreamConfig{Endpoint: endpoint}), "OwnsUpstream(%q)", endpoint)
	}
}

type svtSupportCase struct {
	id      string
	want    bool
	wantErr bool
}

func svtCheckSupports(t *testing.T, v common.Vendor, settings common.VendorSettings, cases []svtSupportCase) {
	t.Helper()
	for _, c := range cases {
		ok, err := svtSupports(t, v, settings, c.id)
		if c.wantErr {
			assert.Error(t, err, "SupportsNetwork(%q)", c.id)
		} else {
			assert.NoError(t, err, "SupportsNetwork(%q)", c.id)
		}
		assert.Equal(t, c.want, ok, "SupportsNetwork(%q)", c.id)
	}
}

type svtErrCase struct {
	name string
	ups  *common.UpstreamConfig
	set  common.VendorSettings
	msg  string
}

func svtCheckGenErrors(t *testing.T, mk func() common.Vendor, cases []svtErrCase) {
	t.Helper()
	for _, c := range cases {
		_, err := svtGenerate(t, mk(), c.ups, c.set)
		if assert.Error(t, err, c.name) {
			assert.Contains(t, err.Error(), c.msg, c.name)
		}
	}
}

// svtCheckURLs asserts the exact generated endpoint for each chain id, plus
// the defaults every generated config carries (evm type, non-nil JsonRpc).
func svtCheckURLs(t *testing.T, mk func() common.Vendor, settings common.VendorSettings, want map[int64]string) {
	t.Helper()
	for chainId, u := range want {
		cfg, err := svtGenerate(t, mk(), svtEvm(chainId), settings)
		if !assert.NoError(t, err, "chain %d", chainId) {
			continue
		}
		assert.Equal(t, u, cfg.Endpoint, "chain %d", chainId)
		assert.Equal(t, common.UpstreamTypeEvm, cfg.Type, "chain %d", chainId)
		assert.NotNil(t, cfg.JsonRpc, "chain %d", chainId)
	}
}

// svtCheckPreset asserts a preset endpoint is returned untouched even with
// no settings, and that JsonRpc is defaulted.
func svtCheckPreset(t *testing.T, v common.Vendor, endpoint string) *common.UpstreamConfig {
	t.Helper()
	cfg, err := svtGenerate(t, v, &common.UpstreamConfig{Endpoint: endpoint}, common.VendorSettings{})
	require.NoError(t, err)
	assert.Equal(t, endpoint, cfg.Endpoint)
	assert.NotNil(t, cfg.JsonRpc)
	return cfg
}

// svtCheckDataOnly asserts a vendor that only forwards error data: no
// normalized error, details["data"] set iff Data is non-empty, and nil for
// a non-JsonRpcResponse input.
func svtCheckDataOnly(t *testing.T, v common.Vendor) {
	t.Helper()
	err, details := vendorErr(t, v, "eth_getLogs", 400, -32000, "some error", "extra")
	assert.NoError(t, err)
	assert.Equal(t, "extra", details["data"])

	err, details = vendorErr(t, v, "eth_getLogs", 400, -32000, "some error", "")
	assert.NoError(t, err)
	assert.NotContains(t, details, "data")

	assert.NoError(t, v.GetVendorSpecificErrorIfAny(svtReq(), nil, "not a response", map[string]interface{}{}))
}

func TestAnkrVendor_Name(t *testing.T) {
	assert.Equal(t, "ankr", CreateAnkrVendor().Name())
}

func TestAnkrVendor_SupportsNetwork(t *testing.T) {
	svtCheckSupports(t, CreateAnkrVendor(), common.VendorSettings{}, []svtSupportCase{
		{"evm:1", true, false},
		{"evm:5201", true, false},
		{"evm:999999999", false, false},
		{"solana:mainnet", false, false},
		{"evm:abc", false, true},
	})
}

func TestAnkrVendor_GenerateConfigs(t *testing.T) {
	key := common.VendorSettings{"apiKey": "k/ey"}
	svtCheckURLs(t, CreateAnkrVendor, key, map[int64]string{
		1:     "https://rpc.ankr.com/eth/k%2Fey",
		42161: "https://rpc.ankr.com/arbitrum/k%2Fey",
		16661: "https://rpc.ankr.com/0g_mainnet_evm/k%2Fey",
	})
	svtCheckGenErrors(t, CreateAnkrVendor, []svtErrCase{
		{"missing apiKey", svtEvm(1), common.VendorSettings{}, "apiKey is required in ankr settings"},
		{"empty apiKey", svtEvm(1), common.VendorSettings{"apiKey": ""}, "apiKey is required in ankr settings"},
		{"nil evm", &common.UpstreamConfig{}, key, "ankr vendor requires upstream.evm to be defined"},
		{"zero chain", svtEvm(0), key, "ankr vendor requires upstream.evm.chainId to be defined"},
		{"unsupported chain", svtEvm(999999999), key, "unsupported network chain ID for Ankr: 999999999"},
	})
	cfg := svtCheckPreset(t, CreateAnkrVendor(), "https://rpc.ankr.com/eth/own")
	assert.Empty(t, cfg.Type, "preset endpoint keeps caller's type")
}

func TestAnkrVendor_GetVendorSpecificErrorIfAny(t *testing.T) {
	svtCheckDataOnly(t, CreateAnkrVendor())
}

func TestAnkrVendor_OwnsUpstream(t *testing.T) {
	svtAssertOwns(t, CreateAnkrVendor(), map[string]bool{
		"ankr://key":                   true,
		"evm+ankr://key":               true,
		"https://rpc.ankr.com/eth/key": true,
		"https://eth.llamarpc.com/key": false,
	})
}
