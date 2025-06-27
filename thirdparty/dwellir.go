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

var dwellirNetworkSubdomains = map[int64]string{
	1:         "api-ethereum-mainnet.n",
	10:        "api-optimism-mainnet-archive.n",
	25:        "api-cronos-mainnet-archive.n",
	44:        "api-darwiniacrab.n",
	46:        "api-darwinia.n",
	40:        "api-xdc-mainnet.n",
	56:        "api-bsc-mainnet-full.n",
	88:        "api-viction-mainnet.n",
	97:        "api-bsc-testnet-full.n",
	100:       "api-gnosis-mainnet.n",
	130:       "api-unichain-mainnet.n",
	137:       "api-polygon-mainnet-full.n",
	146:       "api-sonic-mainnet-archive.n",
	169:       "api-manta-pacific-archive.n",
	204:       "api-opbnb-mainnet.n",
	250:       "api-fantom-mainnet-archive.n",
	288:       "api-boba-mainnet.n",
	324:       "api-zksync-era-mainnet-full.n",
	336:       "api-shiden.n",
	369:       "api-pulse-mainnet.n",
	545:       "api-flow-evm-gateway-testnet.n",
	592:       "api-astar.n",
	747:       "api-flow-evm-gateway-mainnet.n",
	943:       "api-pulsechain-testnet-v4.n",
	945:       "api-bittensor-testnet.n",
	964:       "api-bittensor-mainnet.n",
	996:       "api-bifrost-polkadot.n",
	1101:      "api-polygon-zkevm-mainnet.n",
	1135:      "api-lisk-mainnet.n",
	1284:      "api-moonbeam.n",
	1285:      "api-moonriver.n",
	1287:      "api-moonbase-alpha.n",
	1301:      "api-unichain-sepolia.n",
	1625:      "api-gravity-alpha-mainnet.n",
	2020:      "api-ronin-mainnet.n",
	2031:      "api-centrifuge.n",
	2039:      "api-aleph-zero-evm-testnet.n",
	2442:      "api-polygon-zkevm-sepolia.n",
	3338:      "api-peaq.n",
	4202:      "api-lisk-sepolia.n",
	5000:      "api-mantle-mainnet.n",
	5003:      "api-mantle-sepolia.n",
	5234:      "api-humanode.n",
	5611:      "api-opbnb-testnet.n",
	5845:      "api-tangle-mainnet.n",
	7000:      "api-zetachain-mainnet-archive.n",
	8453:      "api-base-mainnet-archive.n",
	8880:      "api-unique.n",
	8881:      "api-quartz.n",
	10200:     "api-chiado.n",
	13371:     "api-immutable-zkevm-mainnet.n",
	17000:     "api-ethereum-holesky.n",
	41455:     "api-aleph-zero-evm-mainnet.n",
	42161:     "api-arbitrum-mainnet-archive.n",
	42220:     "api-celo-mainnet-archive.n",
	43114:     "api-avalanche-mainnet-archive.n",
	59144:     "api-linea-mainnet-archive.n",
	80002:     "api-polygon-amoy.n",
	80069:     "api-berachain-bepolia.n",
	80094:     "api-berachain-mainnet.n",
	81457:     "api-blast-mainnet-archive.n",
	84532:     "api-base-sepolia-archive.n",
	88882:     "api-chiliz-spicy.n",
	88888:     "api-chiliz-mainnet.n",
	212013:    "api-heima.n",
	222222:    "api-hydration.n",
	421614:    "api-arbitrum-sepolia.n",
	534352:    "api-scroll-mainnet.n",
	7777777:   "api-zora-mainnet.n",
	11155111:  "api-ethereum-sepolia.n",
	11155420:  "api-optimism-sepolia.n",
	168587773: "api-blast-sepolia-archive.n",
	728126428: "api-tron-mainnet-jsonrpc.n",
	999999999: "api-zora-sepolia-archive.n",
}

// dwellirAPIBaseDomain is the base domain for constructing Dwellir RPC URLs.
// Example: https://{subdomain}.dwellir.com/{apiKey}
const dwellirAPIBaseDomain = "dwellir.com"

type DwellirVendor struct {
	common.Vendor
}

func CreateDwellirVendor() common.Vendor {
	return &DwellirVendor{}
}

func (v *DwellirVendor) Name() string {
	return "dwellir"
}

func (v *DwellirVendor) SupportsNetwork(ctx context.Context, logger *zerolog.Logger, settings common.VendorSettings, networkId string) (bool, error) {
	if !strings.HasPrefix(networkId, "evm:") {
		return false, nil
	}

	chainID, err := strconv.ParseInt(strings.TrimPrefix(networkId, "evm:"), 10, 64)
	if err != nil {
		// Ignore parsing errors, maybe it's not a standard EVM ID
		return false, nil
	}
	_, ok := dwellirNetworkSubdomains[chainID]
	return ok, nil
}

func (v *DwellirVendor) GenerateConfigs(ctx context.Context, logger *zerolog.Logger, upstream *common.UpstreamConfig, settings common.VendorSettings) ([]*common.UpstreamConfig, error) {
	if upstream.JsonRpc == nil {
		upstream.JsonRpc = &common.JsonRpcUpstreamConfig{}
	}

	// If a specific endpoint URL is already provided, use it directly.
	// This allows users to bypass the provider logic if needed, assuming they've included credentials.
	if upstream.Endpoint != "" {
		return []*common.UpstreamConfig{upstream}, nil
	}

	// If endpoint is empty, generate it using the apiKey from the provider settings.
	apiKey, ok := settings["apiKey"].(string)
	if !ok || apiKey == "" {
		return nil, fmt.Errorf("apiKey is required in dwellir provider settings")
	}

	if upstream.Evm == nil {
		return nil, fmt.Errorf("dwellir vendor requires upstream.evm to be defined when dynamically generating endpoints")
	}
	chainID := upstream.Evm.ChainId
	if chainID == 0 {
		return nil, fmt.Errorf("dwellir vendor requires upstream.evm.chainId to be defined")
	}
	subdomain, ok := dwellirNetworkSubdomains[chainID]
	if !ok {
		return nil, fmt.Errorf("unsupported network chain ID for Dwellir: %d", chainID)
	}

	// Construct the URL: https://{subdomain}.dwellir.com/{apiKey}
	dwellirURL := fmt.Sprintf("https://%s.%s/%s", subdomain, dwellirAPIBaseDomain, apiKey)
	// Add specific path for Avalanche
	if subdomain == "api-avalanche-mainnet-archive.n" {
		dwellirURL = fmt.Sprintf("%s/ext/bc/C/rpc", dwellirURL)
	}
	parsedURL, err := url.Parse(dwellirURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse generated Dwellir URL: %w", err)
	}

	upstream.Endpoint = parsedURL.String()
	upstream.Type = common.UpstreamTypeEvm // Ensure type is set

	return []*common.UpstreamConfig{upstream}, nil
}

// GetVendorSpecificErrorIfAny checks for Dwellir-specific errors.
// Currently, it returns nil as Dwellir uses standard errors.
func (v *DwellirVendor) GetVendorSpecificErrorIfAny(req *common.NormalizedRequest, resp *http.Response, jrr interface{}, details map[string]interface{}) error {
	// TODO: Add Dwellir specific error handling if any patterns are identified later.
	// Examples: Check resp.StatusCode, or specific codes/messages in jrr.(*common.JsonRpcResponse).Error
	return nil
}

// OwnsUpstream checks if the upstream configuration belongs to Dwellir.
func (v *DwellirVendor) OwnsUpstream(ups *common.UpstreamConfig) bool {
	// Check for the simple scheme format
	if strings.HasPrefix(ups.Endpoint, "dwellir://") {
		return true
	}
	// Check if the endpoint URL contains the Dwellir base domain
	return strings.Contains(ups.Endpoint, "."+dwellirAPIBaseDomain)
}
