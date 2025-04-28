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

var etherspotMainnets = map[int64]string{
	1:         "ethereum",
	137:       "polygon",
	10:        "optimism",
	42161:     "arbitrum",
	122:       "fuse",
	5000:      "mantle",
	100:       "gnosis",
	8453:      "base",
	43114:     "avalanche",
	56:        "bsc",
	59144:     "linea",
	14:        "flare",
	534352:    "scroll",
	30:        "rootstock",
	888888888: "ancient8",
}

var etherspotTestnets = map[int64]struct{}{
	80002:    {}, // Amoy
	11155111: {}, // Sepolia
	84532:    {}, // Base Sepolia
	421614:   {}, // Arbitrum Sepolia
	11155420: {}, // OP Sepolia
	534351:   {}, // Scroll Sepolia
	5003:     {}, // Mantle Sepolia
	114:      {}, // Flare Testnet Coston2
	123:      {}, // Fuse Sparknet
	28122024: {}, // Ancient8 Testnet
	31:       {}, // Rootstock Testnet
}

type EtherspotVendor struct {
	common.Vendor
}

func CreateEtherspotVendor() common.Vendor {
	return &EtherspotVendor{}
}

func (v *EtherspotVendor) Name() string {
	return "etherspot"
}

func (v *EtherspotVendor) SupportsNetwork(ctx context.Context, logger *zerolog.Logger, settings common.VendorSettings, networkId string) (bool, error) {
	if !strings.HasPrefix(networkId, "evm:") {
		return false, nil
	}

	chainID, err := strconv.ParseInt(strings.TrimPrefix(networkId, "evm:"), 10, 64)
	if err != nil {
		return false, err
	}
	_, ok := etherspotMainnets[chainID]
	if ok {
		return true, nil
	}
	_, ok = etherspotTestnets[chainID]
	return ok, nil
}

func (v *EtherspotVendor) GenerateConfigs(upstream *common.UpstreamConfig, settings common.VendorSettings) ([]*common.UpstreamConfig, error) {
	if upstream.JsonRpc == nil {
		upstream.JsonRpc = &common.JsonRpcUpstreamConfig{}
	}

	if upstream.IgnoreMethods == nil {
		upstream.IgnoreMethods = []string{"*"}
	}
	if upstream.AllowMethods == nil {
		upstream.AllowMethods = []string{
			"skandha_config",
			"skandha_feeHistory",
			"skandha_getGasPrice",
			"eth_getUserOperationReceipt",
			"eth_getUserOperationByHash",
			"eth_sendUserOperation",
		}
	}

	if upstream.Endpoint == "" {
		if apiKey, ok := settings["apiKey"].(string); ok && apiKey != "" {
			chainID := upstream.Evm.ChainId
			if chainID == 0 {
				return nil, fmt.Errorf("etherspot vendor requires upstream.evm.chainId to be defined")
			}
			parsedURL, err := v.generateUrl(chainID, apiKey)
			if err != nil {
				return nil, err
			}

			upstream.Endpoint = parsedURL.String()
			upstream.Type = common.UpstreamTypeEvm
		} else {
			return nil, fmt.Errorf("apiKey is required in etherspot settings")
		}
	}

	return []*common.UpstreamConfig{upstream}, nil
}

func (v *EtherspotVendor) GetVendorSpecificErrorIfAny(req *common.NormalizedRequest, resp *http.Response, jrr interface{}, details map[string]interface{}) error {
	return nil
}

func (v *EtherspotVendor) OwnsUpstream(ups *common.UpstreamConfig) bool {
	return strings.HasPrefix(ups.Endpoint, "etherspot") ||
		strings.HasPrefix(ups.Endpoint, "evm+etherspot") ||
		strings.Contains(ups.Endpoint, "etherspot.io")
}

func (v *EtherspotVendor) generateUrl(chainId int64, apiKey string) (*url.URL, error) {
	var etherspotURL string
	if networkName, ok := etherspotMainnets[chainId]; ok {
		// Mainnet URL structure using network name
		etherspotURL = fmt.Sprintf("https://%s-bundler.etherspot.io/", networkName)
	} else if _, ok := etherspotTestnets[chainId]; ok {
		// Testnet URL structure
		etherspotURL = fmt.Sprintf("https://testnet-rpc.etherspot.io/v1/%d", chainId)
	}

	if apiKey != "public" && apiKey != "" {
		etherspotURL = fmt.Sprintf("%s?apikey=%s", etherspotURL, apiKey)
	}

	parsedURL, err := url.Parse(etherspotURL)
	if err != nil {
		return nil, err
	}

	return parsedURL, nil
}
