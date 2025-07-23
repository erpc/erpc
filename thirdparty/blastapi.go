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
	1301:        "unichain-sepolia",
	53302:       "superseed-sepolia",
	130:         "unichain-mainnet",
	1329:        "sei-mainnet",
	1328:        "sei-testnet",
	480:         "worldchain-mainnet",
	4801:        "worldchain-sepolia",
	5003:        "mantle-sepolia",
	66:          "oktc-mainnet",
	11297108109: "palm-mainnet",
	11297108099: "palm-testnet",
	62298:       "citrea-signet",
	18071918:    "mande-mainnet",
	43114:       "ava-mainnet",
	250:         "fantom-mainnet",
	10:          "optimism-mainnet",
	1:           "eth-mainnet",
	763373:      "ink-sepolia",
	1952959480:  "lumia-testnet",
	994873017:   "lumia-mainnet",
	60808:       "bob-mainnet",
	808813:      "bob-sepolia",
	111:         "dymension-testnet",
	1100:        "dymension-mainnet",
	100:         "gnosis-mainnet",
	534351:      "scroll-sepolia",
	80002:       "polygon-amoy",
	5000:        "mantle-mainnet",
	59141:       "linea-sepolia",
	9001:        "evmos-mainnet",
	43113:       "ava-testnet",
	33139:       "apechain-mainnet",
	59144:       "linea-mainnet",
	84532:       "base-sepolia",
	42161:       "arbitrum-one",
	421614:      "arbitrum-sepolia",
	57054:       "sonic-blaze",
	1088:        "metis-mainnet",
	10200:       "gnosis-chiado",
	56:          "bsc-mainnet",
	42170:       "arbitrum-nova",
	1101:        "polygon-zkevm-mainnet",
	80001:       "polygon-testnet",
	204:         "opbnb-mainnet",
	31:          "rootstock-testnet",
	919:         "mode-sepolia",
	8453:        "base-mainnet",
	300:         "zksync-sepolia",
	30:          "rootstock-mainnet",
	81457:       "blastl2-mainnet",
	11155420:    "optimism-sepolia",
	7000:        "zetachain-mainnet",
	2442:        "polygon-zkevm-cardona",
	137:         "polygon-mainnet",
	97:          "bsc-testnet",
	33111:       "apechain-curtis",
	324:         "zksync-mainnet",
	11155111:    "eth-sepolia",
	1946:        "soneium-sepolia",
	37111:       "lens-testnet",
	34443:       "mode-mainnet",
	168587773:   "blastl2-sepolia",
	5611:        "opbnb-testnet",
	534352:      "scroll-mainnet",
	1868:        "soneium-mainnet",
	7001:        "zetachain-testnet",
	2741:        "abstract-mainnet",
	80094:       "berachain-mainnet",
	11124:       "abstract-testnet",
	146:         "sonic-mainnet",
	17000:       "eth-holesky",
	1284:        "moonbeam",
	592:         "astar",
	1231:        "rivalz-testnet",
	11011:       "shape-sepolia",
	360:         "shape-mainnet",
	2020:        "ronin-mainnet",
	4157:        "crossfi-testnet",
	4158:        "crossfi-mainnet",
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

func (v *BlastApiVendor) GenerateConfigs(ctx context.Context, logger *zerolog.Logger, upstream *common.UpstreamConfig, settings common.VendorSettings) ([]*common.UpstreamConfig, error) {
	if upstream.JsonRpc == nil {
		upstream.JsonRpc = &common.JsonRpcUpstreamConfig{}
	}

	if upstream.Endpoint == "" {
		if apiKey, ok := settings["apiKey"].(string); ok && apiKey != "" {
			if upstream.Evm == nil {
				return nil, fmt.Errorf("blastapi vendor requires upstream.evm to be defined")
			}
			chainID := upstream.Evm.ChainId
			if chainID == 0 {
				return nil, fmt.Errorf("blastapi vendor requires upstream.evm.chainId to be defined")
			}
			netName, ok := blastapiNetworkNames[chainID]
			if !ok {
				return nil, fmt.Errorf("unsupported network chain ID for BlastAPI: %d", chainID)
			}
			blastapiURL := fmt.Sprintf("https://%s.blastapi.io/%s", netName, apiKey)
			if netName == "ava-mainnet" || netName == "ava-testnet" {
				// Avalanche endpoints need an extra path `/ext/bc/C/rpc`
				blastapiURL = fmt.Sprintf("%s/ext/bc/C/rpc", blastapiURL)
			}
			parsedURL, err := url.Parse(blastapiURL)
			if err != nil {
				return nil, err
			}

			upstream.Endpoint = parsedURL.String()
			upstream.Type = common.UpstreamTypeEvm
		} else {
			return nil, fmt.Errorf("apiKey is required in blastapi settings")
		}
	}

	return []*common.UpstreamConfig{upstream}, nil
}

func (v *BlastApiVendor) GetVendorSpecificErrorIfAny(req *common.NormalizedRequest, resp *http.Response, jrr interface{}, details map[string]interface{}) error {
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
