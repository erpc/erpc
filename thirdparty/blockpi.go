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

var BlockPiNetworkNames = map[int64]string{
	1:         "ethereum",
	10:        "optimism",
	56:        "bsc",
	97:        "bsc-testnet",
	100:       "gnosis",
	130:       "unichain",
	137:       "polygon",
	250:       "fantom",
	300:       "zksync-era-sepolia",
	324:       "zksync-era",
	1001:      "kaia-kairos",
	1030:      "conflux-espace",
	1088:      "metis",
	1101:      "polygon-zkevm",
	1301:      "unichain-sepolia",
	1329:      "sei-evm",
	1514:      "story",
	2442:      "polygon-zkevm-cardona",
	2741:      "abstract",
	4200:      "merlin",
	5000:      "mantle",
	7000:      "zetachain-evm",
	7001:      "zetachain-athens-evm",
	8217:      "kaia",
	8453:      "base",
	10143:     "monad-testnet",
	17000:     "ethereum-holesky",
	25:        "cronos",
	82:        "meter",
	88:        "viction",
	126:       "movement",
	196:       "xlayer",
	248:       "oasys",
	334:       "t3rn-b2n",
	42161:     "arbitrum",
	42170:     "arbitrum-nova",
	42220:     "celo",
	43113:     "avalanche-fuji",
	43114:     "avalanche",
	534351:    "scroll-sepolia",
	534352:    "scroll",
	57054:     "sonic-blaze",
	57073:     "ink",
	59141:     "linea-sepolia",
	59144:     "linea",
	80002:     "polygon-amoy",
	80094:     "berachain",
	81457:     "blast",
	84532:     "base-sepolia",
	167000:    "taiko",
	167009:    "taiko-hekla",
	421614:    "arbitrum-sepolia",
	686868:    "merlin-testnet",
	11155111:  "ethereum-sepolia",
	11155420:  "optimism-sepolia",
	168587773: "blast-sepolia",
}

type BlockPiVendor struct {
	common.Vendor
}

func CreateBlockPiVendor() common.Vendor {
	return &BlockPiVendor{}
}

func (v *BlockPiVendor) Name() string {
	return "blockpi"
}

func (v *BlockPiVendor) SupportsNetwork(ctx context.Context, logger *zerolog.Logger, settings common.VendorSettings, networkId string) (bool, error) {
	if !strings.HasPrefix(networkId, "evm:") {
		return false, nil
	}

	chainID, err := strconv.ParseInt(strings.TrimPrefix(networkId, "evm:"), 10, 64)
	if err != nil {
		return false, err
	}
	_, ok := BlockPiNetworkNames[chainID]
	return ok, nil
}

func (v *BlockPiVendor) GenerateConfigs(ctx context.Context, logger *zerolog.Logger, upstream *common.UpstreamConfig, settings common.VendorSettings) ([]*common.UpstreamConfig, error) {
	if upstream.JsonRpc == nil {
		upstream.JsonRpc = &common.JsonRpcUpstreamConfig{}
	}

	if upstream.Endpoint == "" {
		if apiKey, ok := settings["apiKey"].(string); ok && apiKey != "" {
			if upstream.Evm == nil {
				return nil, fmt.Errorf("BlockPi vendor requires upstream.evm to be defined")
			}
			chainID := upstream.Evm.ChainId
			if chainID == 0 {
				return nil, fmt.Errorf("BlockPi vendor requires upstream.evm.chainId to be defined")
			}
			netName, ok := BlockPiNetworkNames[chainID]
			if !ok {
				return nil, fmt.Errorf("unsupported network chain ID for BlockPi: %d", chainID)
			}

			blockPiURL := fmt.Sprintf("https://%s.blockpi.network/v1/rpc/%s", netName, url.QueryEscape(apiKey))

			if netName == "movement" {
				blockPiURL = fmt.Sprintf("https://%s.blockpi.network/rpc/v1/%s/v1", netName, url.QueryEscape(apiKey))
			}

			parsedURL, err := url.Parse(blockPiURL)
			if err != nil {
				return nil, err
			}

			upstream.Endpoint = parsedURL.String()
			upstream.Type = common.UpstreamTypeEvm
		} else {
			return nil, fmt.Errorf("apiKey is required in BlockPi settings")
		}
	}

	return []*common.UpstreamConfig{upstream}, nil
}

func (v *BlockPiVendor) GetVendorSpecificErrorIfAny(req *common.NormalizedRequest, resp *http.Response, jrr interface{}, details map[string]interface{}) error {
	bodyMap, ok := jrr.(*common.JsonRpcResponse)
	if !ok {
		return nil
	}

	err := bodyMap.Error
	if err.Data != "" {
		details["data"] = err.Data
	}

	if msg := strings.ToLower(err.Message); msg != "" {
		if strings.Contains(msg, "apikey is on another chain") || strings.Contains(msg, "api key is on another chain") {
			return common.NewErrEndpointUnauthorized(
				common.NewErrJsonRpcExceptionInternal(
					int(common.JsonRpcErrorUnauthorized),
					common.JsonRpcErrorUnauthorized,
					bodyMap.Error.Message,
					nil,
					details,
				),
			)
		}
	}

	return nil
}

func (v *BlockPiVendor) OwnsUpstream(ups *common.UpstreamConfig) bool {
	if strings.HasPrefix(ups.Endpoint, "blockpi://") || strings.HasPrefix(ups.Endpoint, "evm+blockpi://") {
		return true
	}

	return strings.Contains(ups.Endpoint, ".blockpi.network")
}
