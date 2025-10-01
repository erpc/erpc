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

var ankrNetworkNames = map[int64]string{
	1:      "eth",
	10:     "optimism",
	14:     "flare",
	40:     "telos",
	50:     "xdc",
	56:     "bsc",
	57:     "syscoin",
	88:     "velas",
	100:    "gnosis",
	128:    "heco",
	137:    "polygon",
	196:    "xlayer",
	199:    "bttc",
	239:    "tac",
	250:    "fantom",
	324:    "zksync_era",
	570:    "rollux",
	1088:   "metis",
	1101:   "polygon_zkevm",
	1116:   "core",
	1284:   "moonbeam",
	1329:   "sei_evm",
	1514:   "story_mainnet",
	1559:   "tenet_evm",
	1923:   "swell",
	2222:   "kava_evm",
	4689:   "iotex",
	5000:   "mantle",
	5165:   "bahamut",
	7332:   "horizen_eon",
	7887:   "kinto",
	8217:   "kaia",
	8453:   "base",
	8822:   "iota_evm",
	16661:  "0g_mainnet_evm",
	42161:  "arbitrum",
	42170:  "arbitrum_nova",
	42220:  "celo",
	42793:  "etherlink_mainnet",
	43114:  "avalanche",
	59144:  "linea",
	81457:  "blast",
	88888:  "chiliz",
	167000: "taiko",
	534352: "scroll",
	660279: "xai",
	5201:   "electroneum",
}

type AnkrVendor struct {
	common.Vendor
}

func CreateAnkrVendor() common.Vendor {
	return &AnkrVendor{}
}

func (v *AnkrVendor) Name() string {
	return "ankr"
}

func (v *AnkrVendor) SupportsNetwork(ctx context.Context, logger *zerolog.Logger, settings common.VendorSettings, networkId string) (bool, error) {
	if !strings.HasPrefix(networkId, "evm:") {
		return false, nil
	}

	chainID, err := strconv.ParseInt(strings.TrimPrefix(networkId, "evm:"), 10, 64)
	if err != nil {
		return false, err
	}
	_, ok := ankrNetworkNames[chainID]
	return ok, nil
}

func (v *AnkrVendor) GenerateConfigs(ctx context.Context, logger *zerolog.Logger, upstream *common.UpstreamConfig, settings common.VendorSettings) ([]*common.UpstreamConfig, error) {
	if upstream.JsonRpc == nil {
		upstream.JsonRpc = &common.JsonRpcUpstreamConfig{}
	}

	if upstream.Endpoint == "" {
		if apiKey, ok := settings["apiKey"].(string); ok && apiKey != "" {
			if upstream.Evm == nil {
				return nil, fmt.Errorf("ankr vendor requires upstream.evm to be defined")
			}
			chainID := upstream.Evm.ChainId
			if chainID == 0 {
				return nil, fmt.Errorf("ankr vendor requires upstream.evm.chainId to be defined")
			}
			netName, ok := ankrNetworkNames[chainID]
			if !ok {
				return nil, fmt.Errorf("unsupported network chain ID for Ankr: %d", chainID)
			}
			ankrURL := fmt.Sprintf("https://rpc.ankr.com/%s/%s", netName, url.QueryEscape(apiKey))
			parsedURL, err := url.Parse(ankrURL)
			if err != nil {
				return nil, err
			}

			upstream.Endpoint = parsedURL.String()
			upstream.Type = common.UpstreamTypeEvm
		} else {
			return nil, fmt.Errorf("apiKey is required in ankr settings")
		}
	}

	return []*common.UpstreamConfig{upstream}, nil
}

func (v *AnkrVendor) GetVendorSpecificErrorIfAny(req *common.NormalizedRequest, resp *http.Response, jrr interface{}, details map[string]interface{}) error {
	bodyMap, ok := jrr.(*common.JsonRpcResponse)
	if !ok {
		return nil
	}

	err := bodyMap.Error
	if err.Data != "" {
		details["data"] = err.Data
	}

	return nil
}

func (v *AnkrVendor) OwnsUpstream(ups *common.UpstreamConfig) bool {
	if strings.HasPrefix(ups.Endpoint, "ankr://") || strings.HasPrefix(ups.Endpoint, "evm+ankr://") {
		return true
	}

	return strings.Contains(ups.Endpoint, "rpc.ankr.com")
}
