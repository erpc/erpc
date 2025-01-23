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

var blastapiNetworkNames = map[int64]string{
	1:           "eth-mainnet",
	10:          "optimism-mainnet",
	100:         "gnosis-mainnet",
	10000:       "crynux-testnet",
	10200:       "gnosis-chiado",
	1075:        "iota-testnet-evm",
	1088:        "metis-mainnet",
	1100:        "dymension-mainnet",
	1101:        "polygon-zkevm-mainnet",
	111:         "dymension-testnet",
	11124:       "abstract-testnet",
	11155111:    "eth-sepolia",
	11155420:    "optimism-sepolia",
	1122:        "nim-mainnet",
	11297108099: "palm-testnet",
	11297108109: "palm-mainnet",
	1231:        "rivalz-testnet",
	1284:        "moonbeam",
	1285:        "moonriver",
	1287:        "moonbase-alpha",
	137:         "polygon-mainnet",
	168587773:   "blastl2-sepolia",
	17000:       "eth-holesky",
	18071918:    "mande-mainnet",
	204:         "opbnb-mainnet",
	2442:        "polygon-zkevm-cardona",
	250:         "fantom-mainnet",
	300:         "zksync-sepolia",
	324:         "zksync-mainnet",
	336:         "shiden",
	34443:       "mode-mainnet",
	4002:        "fantom-testnet",
	4157:        "crossfi-testnet",
	420:         "optimism-goerli",
	42161:       "arbitrum-one",
	421614:      "arbitrum-sepolia",
	42170:       "arbitrum-nova",
	43113:       "ava-testnet",
	43114:       "ava-mainnet",
	5:           "eth-goerli",
	5000:        "mantle-mainnet",
	5003:        "mantle-sepolia",
	534351:      "scroll-sepolia",
	534352:      "scroll-mainnet",
	56:          "bsc-mainnet",
	5611:        "opbnb-testnet",
	59140:       "linea-goerli",
	59141:       "linea-sepolia",
	59144:       "linea-mainnet",
	592:         "astar",
	60808:       "bob-mainnet",
	62298:       "citrea-signet",
	66:          "oktc-mainnet",
	7000:        "zetachain-mainnet",
	7001:        "zetachain-testnet",
	80001:       "polygon-testnet",
	80002:       "polygon-amoy",
	80084:       "berachain-bartio",
	808813:      "bob-sepolia",
	81:          "shibuya",
	81457:       "blastl2-mainnet",
	8453:        "base-mainnet",
	84531:       "base-goerli",
	84532:       "base-sepolia",
	8822:        "iota-mainnet-evm",
	9001:        "evmos-mainnet",
	919:         "mode-sepolia",
	97:          "bsc-testnet",
}

type BlastApiVendor struct {
	common.Vendor
}

func CreateBlastApiVendor() common.Vendor {
	return &BlastApiVendor{}
}

func (v *BlastApiVendor) Name() string {
	return "blastapi"
}

func (v *BlastApiVendor) SupportsNetwork(ctx context.Context, logger *zerolog.Logger, settings common.VendorSettings, networkId string) (bool, error) {
	if !strings.HasPrefix(networkId, "evm:") {
		return false, nil
	}

	chainId, err := strconv.ParseInt(strings.TrimPrefix(networkId, "evm:"), 10, 64)
	if err != nil {
		return false, err
	}
	_, ok := blastapiNetworkNames[chainId]
	return ok, nil
}

func (v *BlastApiVendor) PrepareConfig(upstream *common.UpstreamConfig, settings common.VendorSettings) error {
	if upstream.JsonRpc == nil {
		upstream.JsonRpc = &common.JsonRpcUpstreamConfig{}
	}

	if upstream.Endpoint == "" {
		if apiKey, ok := settings["apiKey"].(string); ok && apiKey != "" {
			if upstream.Evm == nil {
				return fmt.Errorf("blastapi vendor requires upstream.evm to be defined")
			}
			chainID := upstream.Evm.ChainId
			if chainID == 0 {
				return fmt.Errorf("blastapi vendor requires upstream.evm.chainId to be defined")
			}
			netName, ok := blastapiNetworkNames[chainID]
			if !ok {
				return fmt.Errorf("unsupported network chain ID for BlastAPI: %d", chainID)
			}
			blastapiURL := fmt.Sprintf("https://%s.blastapi.io/%s", netName, apiKey)
			if netName == "ava-mainnet" || netName == "ava-testnet" {
				// Avalanche endpoints need an extra path `/ext/bc/C/rpc`
				blastapiURL = fmt.Sprintf("%s/ext/bc/C/rpc", blastapiURL)
			}
			parsedURL, err := url.Parse(blastapiURL)
			if err != nil {
				return err
			}

			upstream.Endpoint = parsedURL.String()
			upstream.Type = common.UpstreamTypeEvm
		} else {
			return fmt.Errorf("apiKey is required in blastapi settings")
		}
	}

	return nil
}

func (v *BlastApiVendor) GetVendorSpecificErrorIfAny(resp *http.Response, jrr interface{}, details map[string]interface{}) error {
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

func (v *BlastApiVendor) OwnsUpstream(ups *common.UpstreamConfig) bool {
	if strings.HasPrefix(ups.Endpoint, "blastapi://") || strings.HasPrefix(ups.Endpoint, "evm+blastapi://") {
		return true
	}

	return strings.Contains(ups.Endpoint, ".blastapi.io")
}
