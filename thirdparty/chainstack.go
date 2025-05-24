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

var chainstackNetworkNames = map[int64]string{
	1:         "ethereum-mainnet",
	11155111:  "ethereum-sepolia",
	17000:     "ethereum-holesky",
	560048:    "ethereum-hoodi",
	56:        "bsc-mainnet",
	97:        "bsc-testnet",
	42161:     "arbitrum-mainnet",
	421614:    "arbitrum-sepolia",
	8453:      "base-mainnet",
	84532:     "base-sepolia",
	146:       "sonic-mainnet",
	57054:     "sonic-blaze",
	137:       "polygon-mainnet",
	80002:     "polygon-amoy",
	10:        "optimism-mainnet",
	11155420:  "optimism-sepolia",
	43114:     "avalanche-mainnet",
	43113:     "avalanche-fuji",
	2020:      "ronin-mainnet",
	2021:      "ronin-saigon",
	324:       "zksync-mainnet",
	300:       "zksync-sepolia",
	534352:    "scroll-mainnet",
	534351:    "scroll-sepolia",
	23294:     "oasis-sapphire-mainnet",
	23295:     "oasis-sapphire-testnet",
	42220:     "celo-mainnet",
	8217:      "kaia-mainnet",
	1001:      "kaia-kairos",
	100:       "gnosis-mainnet",
	17049341:  "gnosis-chiado",
	25:        "cronos-mainnet",
	250:       "fantom-mainnet",
}

type ChainstackVendor struct {
	common.Vendor
}

func CreateChainstackVendor() common.Vendor {
	return &ChainstackVendor{}
}

func (v *ChainstackVendor) Name() string {
	return "chainstack"
}

func (v *ChainstackVendor) SupportsNetwork(ctx context.Context, logger *zerolog.Logger, settings common.VendorSettings, networkId string) (bool, error) {
	if !strings.HasPrefix(networkId, "evm:") {
		return false, nil
	}

	chainID, err := strconv.ParseInt(strings.TrimPrefix(networkId, "evm:"), 10, 64)
	if err != nil {
		return false, err
	}
	_, ok := chainstackNetworkNames[chainID]
	return ok, nil
}

func (v *ChainstackVendor) GenerateConfigs(upstream *common.UpstreamConfig, settings common.VendorSettings) ([]*common.UpstreamConfig, error) {
	if upstream.JsonRpc == nil {
		upstream.JsonRpc = &common.JsonRpcUpstreamConfig{}
	}

	if upstream.Endpoint == "" {
		if apiKey, ok := settings["apiKey"].(string); ok && apiKey != "" {
			if upstream.Evm == nil {
				return nil, fmt.Errorf("chainstack vendor requires upstream.evm to be defined")
			}
			chainID := upstream.Evm.ChainId
			if chainID == 0 {
				return nil, fmt.Errorf("chainstack vendor requires upstream.evm.chainId to be defined")
			}
			netName, ok := chainstackNetworkNames[chainID]
			if !ok {
				return nil, fmt.Errorf("unsupported network chain ID for Chainstack: %d", chainID)
			}
			
			var chainstackURL string
			if chainID == 43114 || chainID == 43113 { // Avalanche networks need special path
				chainstackURL = fmt.Sprintf("https://%s.core.chainstack.com/ext/bc/C/rpc/%s", netName, apiKey)
			} else {
				chainstackURL = fmt.Sprintf("https://%s.core.chainstack.com/%s", netName, apiKey)
			}
			
			parsedURL, err := url.Parse(chainstackURL)
			if err != nil {
				return nil, err
			}

			upstream.Endpoint = parsedURL.String()
			upstream.Type = common.UpstreamTypeEvm
		} else {
			return nil, fmt.Errorf("apiKey is required in chainstack settings")
		}
	}

	return []*common.UpstreamConfig{upstream}, nil
}

func (v *ChainstackVendor) GetVendorSpecificErrorIfAny(req *common.NormalizedRequest, resp *http.Response, jrr interface{}, details map[string]interface{}) error {
	bodyMap, ok := jrr.(*common.JsonRpcResponse)
	if !ok {
		return nil
	}

	err := bodyMap.Error
	if err.Data != "" {
		details["data"] = err.Data
	}

	// Other errors can be properly handled by generic error handling
	return nil
}

func (v *ChainstackVendor) OwnsUpstream(ups *common.UpstreamConfig) bool {
	if strings.HasPrefix(ups.Endpoint, "chainstack://") || strings.HasPrefix(ups.Endpoint, "evm+chainstack://") {
		return true
	}

	return strings.Contains(ups.Endpoint, ".core.chainstack.com")
} 