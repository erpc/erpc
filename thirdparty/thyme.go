package thirdparty

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
)

// thymeNetworkSubdomains maps the chains Thyme Gate serves (https://gate.thymelabs.io)
// to their subdomain under thymeHost.
var thymeNetworkSubdomains = map[int64]string{
	1:        "ethereum-mainnet",
	10:       "optimism-mainnet",
	56:       "bnb-mainnet",
	137:      "polygon-mainnet",
	1301:     "unichain-sepolia",
	80002:    "polygon-amoy",
	11155111: "ethereum-sepolia",
}

// thymeHost is the parent domain of every Thyme Gate endpoint:
// https://{subdomain}.rpc.thyme.sh/{apiKey}, or /public without a key.
const thymeHost = "rpc.thyme.sh"

// thymePublicKey in the apiKey setting selects the keyless public endpoint
// (rate limited per client IP, a subset of methods) instead of a keyed one.
// Thyme API keys are 64 hex characters, so it never collides with a real key.
const thymePublicKey = "public"

type ThymeVendor struct {
	common.Vendor
}

func CreateThymeVendor() common.Vendor {
	return &ThymeVendor{}
}

func (v *ThymeVendor) Name() string {
	return "thyme"
}

func (v *ThymeVendor) SupportsNetwork(ctx context.Context, logger *zerolog.Logger, settings common.VendorSettings, networkId string) (bool, error) {
	if !strings.HasPrefix(networkId, "evm:") {
		return false, nil
	}
	chainID, err := strconv.ParseInt(strings.TrimPrefix(networkId, "evm:"), 10, 64)
	if err != nil {
		return false, err
	}
	_, ok := thymeNetworkSubdomains[chainID]
	return ok, nil
}

func (v *ThymeVendor) GenerateConfigs(ctx context.Context, logger *zerolog.Logger, upstream *common.UpstreamConfig, settings common.VendorSettings) ([]*common.UpstreamConfig, error) {
	if upstream.JsonRpc == nil {
		upstream.JsonRpc = &common.JsonRpcUpstreamConfig{}
	}
	if upstream.Endpoint != "" {
		return []*common.UpstreamConfig{upstream}, nil
	}

	apiKey, _ := settings["apiKey"].(string)
	if apiKey == "" {
		return nil, fmt.Errorf("apiKey is required in thyme settings (set it to %q for the keyless public endpoint)", thymePublicKey)
	}
	if upstream.Evm == nil || upstream.Evm.ChainId == 0 {
		return nil, fmt.Errorf("thyme vendor requires upstream.evm.chainId to be defined")
	}
	subdomain, ok := thymeNetworkSubdomains[upstream.Evm.ChainId]
	if !ok {
		return nil, fmt.Errorf("unsupported network chain ID for Thyme: %d", upstream.Evm.ChainId)
	}

	// The key is the only path segment; "public" selects the keyless endpoint.
	upstream.Endpoint = fmt.Sprintf("https://%s.%s/%s", subdomain, thymeHost, url.PathEscape(apiKey))
	upstream.Type = common.UpstreamTypeEvm

	return []*common.UpstreamConfig{upstream}, nil
}

// GetVendorSpecificErrorIfAny adds nothing: the generic EVM normalizer already
// maps Thyme's errors, as thyme_test.go pins.
func (v *ThymeVendor) GetVendorSpecificErrorIfAny(req *common.NormalizedRequest, resp *http.Response, jrr interface{}, details map[string]interface{}) error {
	return nil
}

func (v *ThymeVendor) OwnsUpstream(ups *common.UpstreamConfig) bool {
	if strings.HasPrefix(ups.Endpoint, "thyme://") || strings.HasPrefix(ups.Endpoint, "evm+thyme://") {
		return true
	}
	return strings.Contains(ups.Endpoint, "."+thymeHost+"/")
}
