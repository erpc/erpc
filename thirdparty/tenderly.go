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

var tenderlyNetworkNames = map[int64]string{
	1:         "mainnet",
	10:        "optimism",
	100:       "gnosis",
	10200:     "chiado",
	1088:      "metis",
	1101:      "polygon-zkevm",
	11155111:  "sepolia",
	11155420:  "optimism-sepolia",
	137:       "polygon",
	168587773: "blast-sepolia",
	17000:     "holesky",
	204:       "opbnb",
	2442:      "polygon-zkevm-cardona",
	250:       "fantom",
	300:       "zksync-sepolia",
	324:       "zksync",
	4002:      "fantom-testnet",
	42161:     "arbitrum",
	421614:    "arbitrum-sepolia",
	42170:     "arbitrum-nova",
	43113:     "avalanche-fuji",
	43114:     "avalanche",
	5000:      "mantle",
	56:        "bsc",
	5611:      "opbnb-testnet",
	59141:     "linea-sepolia",
	59144:     "linea",
	592:       "astar",
	7000:      "zetachain",
	7001:      "zetachain-testnet",
	7777777:   "zora",
	80002:     "polygon-amoy",
	81457:     "blast",
	8453:      "base",
	84532:     "base-sepolia",
	97:        "bsc-testnet",
	999999999: "zora-sepolia",
	534351:    "scroll-sepolia",
	534352:    "scroll",
}

type TenderlyVendor struct {
	common.Vendor
}

func CreateTenderlyVendor() common.Vendor {
	return &TenderlyVendor{}
}

func (v *TenderlyVendor) Name() string {
	return "tenderly"
}

func (v *TenderlyVendor) SupportsNetwork(ctx context.Context, logger *zerolog.Logger, settings common.VendorSettings, networkId string) (bool, error) {
	if !strings.HasPrefix(networkId, "evm:") {
		return false, nil
	}

	chainID, err := strconv.ParseInt(strings.TrimPrefix(networkId, "evm:"), 10, 64)
	if err != nil {
		return false, err
	}
	_, ok := tenderlyNetworkNames[chainID]
	return ok, nil
}

func (v *TenderlyVendor) GenerateConfigs(upstream *common.UpstreamConfig, settings common.VendorSettings) ([]*common.UpstreamConfig, error) {
	if upstream.JsonRpc == nil {
		upstream.JsonRpc = &common.JsonRpcUpstreamConfig{}
	}

	if upstream.Endpoint == "" {
		if apiKey, ok := settings["apiKey"].(string); ok && apiKey != "" {
			if upstream.Evm == nil {
				return nil, fmt.Errorf("tenderly vendor requires upstream.evm to be defined")
			}
			chainID := upstream.Evm.ChainId
			if chainID == 0 {
				return nil, fmt.Errorf("tenderly vendor requires upstream.evm.chainId to be defined")
			}
			netName, ok := tenderlyNetworkNames[chainID]
			if !ok {
				return nil, fmt.Errorf("unsupported network chain ID for Tenderly: %d", chainID)
			}
			tenderlyURL := fmt.Sprintf("https://%s.gateway.tenderly.co/%s", netName, apiKey)
			parsedURL, err := url.Parse(tenderlyURL)
			if err != nil {
				return nil, err
			}

			upstream.Endpoint = parsedURL.String()
			upstream.Type = common.UpstreamTypeEvm
		} else {
			return nil, fmt.Errorf("apiKey is required in tenderly settings")
		}
	}

	return []*common.UpstreamConfig{upstream}, nil
}

func (v *TenderlyVendor) GetVendorSpecificErrorIfAny(req *common.NormalizedRequest, resp *http.Response, jrr interface{}, details map[string]interface{}) error {
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

func (v *TenderlyVendor) OwnsUpstream(ups *common.UpstreamConfig) bool {
	if strings.HasPrefix(ups.Endpoint, "tenderly://") || strings.HasPrefix(ups.Endpoint, "evm+tenderly://") {
		return true
	}

	return strings.Contains(ups.Endpoint, ".gateway.tenderly.co")
}
