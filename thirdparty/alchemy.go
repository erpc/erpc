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

var alchemyNetworkSubdomains = map[int64]string{
	1:         "eth-mainnet",
	10:        "opt-mainnet",
	100:       "gnosis-mainnet",
	10200:     "gnosis-chiado",
	1088:      "metis-mainnet",
	1101:      "polygonzkevm-mainnet",
	11011:     "shape-sepolia",
	11155111:  "eth-sepolia",
	11155420:  "opt-sepolia",
	137:       "polygon-mainnet",
	168587773: "blast-sepolia",
	17000:     "eth-holesky",
	204:       "opbnb-mainnet",
	2442:      "polygonzkevm-cardona",
	250:       "fantom-mainnet",
	300:       "zksync-sepolia",
	324:       "zksync-mainnet",
	4002:      "fantom-testnet",
	42161:     "arb-mainnet",
	421614:    "arb-sepolia",
	42170:     "arbnova-mainnet",
	43113:     "avax-fuji",
	43114:     "avax-mainnet",
	5000:      "mantle-mainnet",
	56:        "bnb-mainnet",
	5611:      "opbnb-testnet",
	59141:     "linea-sepolia",
	59144:     "linea-mainnet",
	592:       "astar-mainnet",
	7000:      "zetachain-mainnet",
	7001:      "zetachain-testnet",
	7777777:   "zora-mainnet",
	80002:     "polygon-amoy",
	81457:     "blast-mainnet",
	8453:      "base-mainnet",
	84532:     "base-sepolia",
	97:        "bnb-testnet",
	999999999: "zora-sepolia",
	534351:    "scroll-sepolia",
	534352:    "scroll-mainnet",
}

type AlchemyVendor struct {
	common.Vendor
}

func CreateAlchemyVendor() common.Vendor {
	return &AlchemyVendor{}
}

func (v *AlchemyVendor) Name() string {
	return "alchemy"
}

func (v *AlchemyVendor) SupportsNetwork(ctx context.Context, logger *zerolog.Logger, settings common.VendorSettings, networkId string) (bool, error) {
	chainID, err := strconv.ParseInt(networkId, 10, 64)
	if err != nil {
		return false, err
	}
	_, ok := alchemyNetworkSubdomains[chainID]
	return ok, nil
}

func (v *AlchemyVendor) PrepareConfig(upstream *common.UpstreamConfig, settings common.VendorSettings) error {
	if upstream.JsonRpc == nil {
		upstream.JsonRpc = &common.JsonRpcUpstreamConfig{}
	}

	if upstream.Endpoint == "" && settings != nil {
		if apiKey, ok := settings["apiKey"].(string); ok && apiKey != "" {
			if upstream.Evm == nil {
				return fmt.Errorf("alchemy vendor requires upstream.evm to be defined")
			}
			chainID := upstream.Evm.ChainId
			if chainID == 0 {
				return fmt.Errorf("alchemy vendor requires upstream.evm.chainId to be defined")
			}
			subdomain, ok := alchemyNetworkSubdomains[chainID]
			if !ok {
				return fmt.Errorf("unsupported network chain ID for Alchemy: %d", chainID)
			}
			alchemyURL := fmt.Sprintf("https://%s.g.alchemy.com/v2/%s", subdomain, apiKey)
			parsedURL, err := url.Parse(alchemyURL)
			if err != nil {
				return err
			}

			upstream.Endpoint = parsedURL.String()
		} else {
			return fmt.Errorf("apiKey is required in alchemy settings")
		}
	}

	return nil
}

func (v *AlchemyVendor) GetVendorSpecificErrorIfAny(resp *http.Response, jrr interface{}, details map[string]interface{}) error {
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
		} else if strings.Contains(msg, "Monthly capacity limit exceeded") {
			return common.NewErrEndpointBillingIssue(
				common.NewErrJsonRpcExceptionInternal(
					code,
					common.JsonRpcErrorCapacityExceeded,
					msg,
					nil,
					details,
				),
			)
		} else if strings.Contains(msg, "limit exceeded") {
			return common.NewErrEndpointCapacityExceeded(
				common.NewErrJsonRpcExceptionInternal(
					code,
					common.JsonRpcErrorCapacityExceeded,
					msg,
					nil,
					details,
				),
			)
		} else if strings.Contains(msg, "transaction not found") || strings.Contains(msg, "cannot find transaction") {
			return common.NewErrEndpointMissingData(
				common.NewErrJsonRpcExceptionInternal(
					code,
					common.JsonRpcErrorMissingData,
					msg,
					nil,
					details,
				),
			)
		} else if code >= -32000 && code <= -32099 {
			return common.NewErrEndpointServerSideException(
				common.NewErrJsonRpcExceptionInternal(
					code,
					common.JsonRpcErrorServerSideException,
					msg,
					nil,
					details,
				),
				nil,
			)
		} else if code >= -32099 && code <= -32599 || code >= -32603 && code <= -32699 || code >= -32701 && code <= -32768 {
			return common.NewErrEndpointClientSideException(
				common.NewErrJsonRpcExceptionInternal(
					code,
					common.JsonRpcErrorClientSideException,
					msg,
					nil,
					details,
				),
			)
		} else if code == 3 {
			return common.NewErrEndpointClientSideException(
				common.NewErrJsonRpcExceptionInternal(
					code,
					common.JsonRpcErrorEvmReverted,
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

func (v *AlchemyVendor) OwnsUpstream(ups *common.UpstreamConfig) bool {
	if strings.HasPrefix(ups.Endpoint, "alchemy://") || strings.HasPrefix(ups.Endpoint, "evm+alchemy://") {
		return true
	}

	return strings.Contains(ups.Endpoint, ".alchemy.com") || strings.Contains(ups.Endpoint, ".alchemyapi.io")
}
