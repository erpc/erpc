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

var infuraNetworkNames = map[int64]string{
	42161:       "arbitrum-mainnet",
	421614:      "arbitrum-sepolia",
	43114:       "avalanche-mainnet",
	43113:       "avalanche-fuji",
	8453:        "base-mainnet",
	84532:       "base-sepolia",
	56:          "bsc-mainnet",
	97:          "bsc-testnet",
	81457:       "blast-mainnet",
	168587773:   "blast-sepolia",
	42220:       "celo-mainnet",
	44787:       "celo-alfajores",
	1:           "mainnet",
	17000:       "holesky",
	11155111:    "sepolia",
	59144:       "linea-mainnet",
	59141:       "linea-sepolia",
	5000:        "mantle-mainnet",
	5003:        "mantle-sepolia",
	204:         "opbnb-mainnet",
	5611:        "opbnb-testnet",
	10:          "optimism-mainnet",
	11155420:    "optimism-sepolia",
	11297108109: "palm-mainnet",
	11297108099: "palm-testnet",
	137:         "polygon-mainnet",
	80002:       "polygon-amoy",
	534352:      "scroll-mainnet",
	534351:      "scroll-sepolia",
	324:         "zksync-mainnet",
	300:         "zksync-sepolia",
}

type InfuraVendor struct {
	common.Vendor
}

func CreateInfuraVendor() common.Vendor {
	return &InfuraVendor{}
}

func (v *InfuraVendor) Name() string {
	return "infura"
}

func (v *InfuraVendor) SupportsNetwork(ctx context.Context, logger *zerolog.Logger, settings common.VendorSettings, networkId string) (bool, error) {
	if !strings.HasPrefix(networkId, "evm:") {
		return false, nil
	}

	chainId, err := strconv.ParseInt(strings.TrimPrefix(networkId, "evm:"), 10, 64)
	if err != nil {
		return false, err
	}
	_, ok := infuraNetworkNames[chainId]
	return ok, nil
}

func (v *InfuraVendor) GenerateConfigs(ctx context.Context, logger *zerolog.Logger, upstream *common.UpstreamConfig, settings common.VendorSettings) ([]*common.UpstreamConfig, error) {
	if upstream.JsonRpc == nil {
		upstream.JsonRpc = &common.JsonRpcUpstreamConfig{}
	}

	if upstream.Endpoint == "" {
		if apiKey, ok := settings["apiKey"].(string); ok && apiKey != "" {
			chainID := upstream.Evm.ChainId
			if chainID == 0 {
				return nil, fmt.Errorf("infura vendor requires upstream.evm.chainId to be defined")
			}
			netName, ok := infuraNetworkNames[chainID]
			if !ok {
				return nil, fmt.Errorf("unsupported network chain ID for Infura: %d", chainID)
			}
			infuraURL := fmt.Sprintf("https://%s.infura.io/v3/%s", netName, apiKey)
			if netName == "ava-mainnet" || netName == "ava-testnet" {
				// Avalanche endpoints need an extra path `/ext/bc/C/rpc`
				infuraURL = fmt.Sprintf("%s/ext/bc/C/rpc", infuraURL)
			}
			parsedURL, err := url.Parse(infuraURL)
			if err != nil {
				return nil, err
			}

			upstream.Endpoint = parsedURL.String()
			upstream.Type = common.UpstreamTypeEvm
		} else {
			return nil, fmt.Errorf("apiKey is required in infura settings")
		}
	}
	return []*common.UpstreamConfig{upstream}, nil
}

func (v *InfuraVendor) GetVendorSpecificErrorIfAny(req *common.NormalizedRequest, resp *http.Response, jrr interface{}, details map[string]interface{}) error {
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

		if code == -32600 && (strings.Contains(msg, "be authenticated") || strings.Contains(msg, "access key")) {
			return common.NewErrEndpointUnauthorized(
				common.NewErrJsonRpcExceptionInternal(
					code,
					common.JsonRpcErrorUnauthorized,
					msg,
					nil,
					details,
				),
			)
		} else if code == -32001 || code == -32004 {
			return common.NewErrEndpointUnsupported(
				common.NewErrJsonRpcExceptionInternal(
					code,
					common.JsonRpcErrorUnsupportedException,
					msg,
					nil,
					details,
				),
			)
		} else if code == -32005 {
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

func (v *InfuraVendor) OwnsUpstream(ups *common.UpstreamConfig) bool {
	if strings.HasPrefix(ups.Endpoint, "infura://") || strings.HasPrefix(ups.Endpoint, "evm+infura://") {
		return true
	}
	return strings.Contains(ups.Endpoint, ".infura.io")
}
