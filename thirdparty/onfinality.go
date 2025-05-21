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

var onfinalityNetworkNames = map[int64]string{
	1:        "eth",
	10:       "optimism",
	56:       "bnb",
	97:       "bnb-testnet",
	100:      "gnosis",
	130:      "unichain",
	137:      "polygon",
	146:      "sonic",
	250:      "fantom",
	324:      "zksync",
	1088:     "metis",
	1301:     "unichain-sepolia",
	2741:     "abstract",
	4689:     "iotex",
	5000:     "mantle",
	8217:     "klaytn",
	8453:     "base",
	11155111: "eth-sepolia",
	11155420: "optimism-sepolia",
	42161:    "arbitrum",
	42220:    "celo",
	80002:    "polygon-amoy",
	84532:    "base-sepolia",
	421614:   "arbitrum-sepolia",
	534352:   "scroll",
}

type OnFinalityVendor struct {
	common.Vendor
}

func CreateOnFinalityVendor() common.Vendor {
	return &OnFinalityVendor{}
}

func (v *OnFinalityVendor) Name() string {
	return "onfinality"
}

func (v *OnFinalityVendor) SupportsNetwork(ctx context.Context, logger *zerolog.Logger, settings common.VendorSettings, networkId string) (bool, error) {
	if !strings.HasPrefix(networkId, "evm:") {
		return false, nil
	}

	chainID, err := strconv.ParseInt(strings.TrimPrefix(networkId, "evm:"), 10, 64)
	if err != nil {
		return false, err
	}
	_, ok := onfinalityNetworkNames[chainID]
	return ok, nil
}

func (v *OnFinalityVendor) GenerateConfigs(upstream *common.UpstreamConfig, settings common.VendorSettings) ([]*common.UpstreamConfig, error) {
	if upstream.JsonRpc == nil {
		upstream.JsonRpc = &common.JsonRpcUpstreamConfig{}
	}

	if upstream.Endpoint == "" {
		if apiKey, ok := settings["apiKey"].(string); ok && apiKey != "" {
			if upstream.Evm == nil {
				return nil, fmt.Errorf("onfinality vendor requires upstream.evm to be defined")
			}
			chainID := upstream.Evm.ChainId
			if chainID == 0 {
				return nil, fmt.Errorf("onfinality vendor requires upstream.evm.chainId to be defined")
			}
			netName, ok := onfinalityNetworkNames[chainID]
			if !ok {
				return nil, fmt.Errorf("unsupported network chain ID for OnFinality: %d", chainID)
			}
			onfinalityURL := fmt.Sprintf("https://%s.api.onfinality.io/rpc?apikey=%s", netName, url.QueryEscape(apiKey))
			parsedURL, err := url.Parse(onfinalityURL)
			if err != nil {
				return nil, err
			}

			upstream.Endpoint = parsedURL.String()
			upstream.Type = common.UpstreamTypeEvm
		} else {
			return nil, fmt.Errorf("apiKey is required in onfinality settings")
		}
	}

	return []*common.UpstreamConfig{upstream}, nil
}

func (v *OnFinalityVendor) GetVendorSpecificErrorIfAny(req *common.NormalizedRequest, resp *http.Response, jrr interface{}, details map[string]interface{}) error {
	bodyMap, ok := jrr.(*common.JsonRpcResponse)
	if !ok {
		return nil
	}

	err := bodyMap.Error
	if code := err.Code; code != 0 {
		msg := err.Message
		if err.Data != "" {
			details["data"] = err.Data
		}

		if strings.Contains(msg, "invalid api key") || strings.Contains(msg, "unauthorized") {
			return common.NewErrEndpointUnauthorized(
				common.NewErrJsonRpcExceptionInternal(
					code,
					common.JsonRpcErrorUnauthorized,
					msg,
					nil,
					details,
				),
			)
		} else if strings.Contains(msg, "rate limit") || strings.Contains(msg, "too many requests") {
			return common.NewErrEndpointCapacityExceeded(
				common.NewErrJsonRpcExceptionInternal(
					code,
					common.JsonRpcErrorCapacityExceeded,
					msg,
					nil,
					details,
				),
			)
		}
	}

	// Other errors can be properly handled by generic error handling
	return nil
}

func (v *OnFinalityVendor) OwnsUpstream(ups *common.UpstreamConfig) bool {
	if strings.HasPrefix(ups.Endpoint, "onfinality://") || strings.HasPrefix(ups.Endpoint, "evm+onfinality://") {
		return true
	}

	return strings.Contains(ups.Endpoint, ".api.onfinality.io")
}
