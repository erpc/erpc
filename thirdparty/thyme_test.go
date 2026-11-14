package thirdparty

import (
	"net/http"
	"strings"
	"testing"

	archEvm "github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestThymeVendor_Name(t *testing.T) {
	assert.Equal(t, "thyme", CreateThymeVendor().Name())
}

func TestThymeVendor_SupportsNetwork(t *testing.T) {
	svtCheckSupports(t, CreateThymeVendor(), common.VendorSettings{}, []svtSupportCase{
		{"evm:1", true, false},
		{"evm:137", true, false},
		{"evm:11155111", true, false},
		{"evm:8453", false, false},
		{"solana:mainnet", false, false},
		{"evm:abc", false, true},
	})
}

func TestThymeVendor_GenerateConfigs(t *testing.T) {
	key := common.VendorSettings{"apiKey": "KEY"}
	svtCheckURLs(t, CreateThymeVendor, key, map[int64]string{
		1:        "https://ethereum-mainnet.rpc.thyme.sh/KEY",
		10:       "https://optimism-mainnet.rpc.thyme.sh/KEY",
		56:       "https://bnb-mainnet.rpc.thyme.sh/KEY",
		137:      "https://polygon-mainnet.rpc.thyme.sh/KEY",
		1301:     "https://unichain-sepolia.rpc.thyme.sh/KEY",
		80002:    "https://polygon-amoy.rpc.thyme.sh/KEY",
		11155111: "https://ethereum-sepolia.rpc.thyme.sh/KEY",
	})
	svtCheckURLs(t, CreateThymeVendor, common.VendorSettings{"apiKey": "public"}, map[int64]string{
		137: "https://polygon-mainnet.rpc.thyme.sh/public",
	})
	svtCheckURLs(t, CreateThymeVendor, common.VendorSettings{"apiKey": "a/b"}, map[int64]string{
		1: "https://ethereum-mainnet.rpc.thyme.sh/a%2Fb",
	})
	svtCheckGenErrors(t, CreateThymeVendor, []svtErrCase{
		{"missing apiKey", svtEvm(1), common.VendorSettings{}, `apiKey is required in thyme settings (set it to "public" for the keyless public endpoint)`},
		{"nil evm", &common.UpstreamConfig{}, key, "thyme vendor requires upstream.evm.chainId to be defined"},
		{"zero chain", svtEvm(0), key, "thyme vendor requires upstream.evm.chainId to be defined"},
		{"unsupported chain", svtEvm(8453), key, "unsupported network chain ID for Thyme: 8453"},
	})
	svtCheckPreset(t, CreateThymeVendor(), "https://polygon-mainnet.rpc.thyme.sh/own")
}

// Thyme's error bodies (live responses, except the capacity, billing, plan
// and node-down cases, which are the gateway's messages for those states)
// map to the right normalized errors with no vendor-specific handling.
func TestThymeVendor_ErrorsUseGenericNormalization(t *testing.T) {
	err, _ := vendorErr(t, CreateThymeVendor(), "eth_getLogs", 429, -32005, "rate limit exceeded", "")
	assert.NoError(t, err, "thyme defers every error to generic handling")

	cases := []struct {
		name   string
		status int
		body   string
		want   common.ErrorCode
	}{
		{"public: per-IP rate limit", 429,
			`{"jsonrpc":"2.0","id":1,"error":{"message":"Rate limit exceeded: the public endpoint allows 22 units per second per IP (eth_getLogs and eth_getBlockReceipts cost 4). Get a free API key at https://gate.thymelabs.io","code":-32005}}`,
			common.ErrCodeEndpointCapacityExceeded},
		{"public: chain at capacity", 429,
			`{"jsonrpc":"2.0","id":1,"error":{"message":"The public endpoint is at capacity, retry shortly. API keys from https://gate.thymelabs.io are not affected","code":-32005}}`,
			common.ErrCodeEndpointCapacityExceeded},
		{"public: method not served", 403,
			`{"jsonrpc":"2.0","id":1,"error":{"message":"Method debug_traceTransaction is not available on the public endpoint. Use an API key from https://gate.thymelabs.io","code":-32601}}`,
			common.ErrCodeEndpointUnsupported},
		{"public: filtered eth_getLogs range", 400,
			`{"jsonrpc":"2.0","id":1,"error":{"message":"eth_getLogs is limited to 5000 blocks per call on the public endpoint. Use an API key from https://gate.thymelabs.io","code":-32602}}`,
			common.ErrCodeEndpointRequestTooLarge},
		{"public: unfiltered eth_getLogs range", 400,
			`{"jsonrpc":"2.0","id":1,"error":{"message":"eth_getLogs is limited to 1000 blocks per call without an address or topic filter on the public endpoint. Use an API key from https://gate.thymelabs.io","code":-32602}}`,
			common.ErrCodeEndpointRequestTooLarge},
		{"public: node down", 503,
			`{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"The polygon-mainnet node behind the public endpoint is unavailable. Retry shortly, or use an API key from https://gate.thymelabs.io. Status: https://status.thyme.sh"}}`,
			common.ErrCodeEndpointServerSideException},
		{"keyed: invalid key (plain body)", 401,
			`{"message":"Invalid API key in request"}`,
			common.ErrCodeEndpointUnauthorized},
		{"keyed: subscription suspended", 402,
			`{"error":{"code":-32024,"message":"Payment required: subscription suspended"}}`,
			common.ErrCodeEndpointBillingIssue},
		{"keyed: compute units used up", 402,
			`{"error":{"code":-32030,"message":"CU quota exceeded. Upgrade your plan."}}`,
			common.ErrCodeEndpointBillingIssue},
		{"keyed: no active plan", 403,
			`{"error":{"code":-32020,"message":"No active rpc plan"}}`,
			common.ErrCodeEndpointUnauthorized},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			jr := &common.JsonRpcResponse{}
			require.NoError(t, jr.ParseFromStream(nil, strings.NewReader(c.body), len(c.body)))
			err := archEvm.ExtractJsonRpcError(&http.Response{StatusCode: c.status, Header: http.Header{}}, nil, jr, nil)
			require.Error(t, err)
			assert.True(t, common.HasErrorCode(err, c.want), "want %s, got %v", c.want, err)
		})
	}
}

func TestThymeVendor_OwnsUpstream(t *testing.T) {
	svtAssertOwns(t, CreateThymeVendor(), map[string]bool{
		"thyme://key":     true,
		"evm+thyme://key": true,
		"https://polygon-mainnet.rpc.thyme.sh/key":     true,
		"https://ethereum-mainnet.rpc.thyme.sh/public": true,
		"https://rpc.thyme.sh.example.com/key":         false,
		"https://rpc.ankr.com/eth/key":                 false,
	})

	v := NewVendorsRegistry().LookupByUpstream(&common.UpstreamConfig{Endpoint: "https://polygon-mainnet.rpc.thyme.sh/key"})
	require.NotNil(t, v)
	assert.Equal(t, "thyme", v.Name())
}
