package thirdparty

import (
	"strconv"

	"github.com/erpc/erpc/common"
)

// sqdChainToDataset maps EVM chainId to SQD Portal dataset name.
// Dataset slug is the last segment of the Portal gateway URL.
// Kept for supported-chain detection and vendor settings overrides.
var sqdChainToDataset = map[int64]string{
	// Mainnets
	1:       "ethereum-mainnet",
	10:      "optimism-mainnet",
	56:      "binance-mainnet",
	100:     "gnosis-mainnet",
	137:     "polygon-mainnet",
	250:     "fantom-mainnet",
	324:     "zksync-mainnet",
	8453:    "base-mainnet",
	42161:   "arbitrum-one",
	42170:   "arbitrum-nova",
	43114:   "avalanche-mainnet",
	59144:   "linea-mainnet",
	534352:  "scroll-mainnet",
	81457:   "blast-mainnet",
	7777777: "zora-mainnet",

	// Testnets (commonly used)
	11155111: "ethereum-sepolia",
	84532:    "base-sepolia",
	421614:   "arbitrum-sepolia",
	11155420: "optimism-sepolia",
}

// SqdSupportedChainIds returns a slice of all supported chain IDs.
func SqdSupportedChainIds() []int64 {
	chains := make([]int64, 0, len(sqdChainToDataset))
	for chainId := range sqdChainToDataset {
		chains = append(chains, chainId)
	}
	return chains
}

func sqdDatasetFromSettings(settings common.VendorSettings, chainId int64) (string, bool) {
	if settings == nil {
		return "", false
	}

	if dataset, ok := settings["dataset"].(string); ok && dataset != "" {
		return dataset, true
	}

	if raw, ok := settings["datasetByChainId"]; ok {
		switch m := raw.(type) {
		case map[string]interface{}:
			if ds, ok := m[strconv.FormatInt(chainId, 10)].(string); ok && ds != "" {
				return ds, true
			}
		case map[interface{}]interface{}:
			if ds, ok := m[chainId].(string); ok && ds != "" {
				return ds, true
			}
			if ds, ok := m[strconv.FormatInt(chainId, 10)].(string); ok && ds != "" {
				return ds, true
			}
		}
	}

	return "", false
}

func sqdUseDefaultDatasets(settings common.VendorSettings) bool {
	if settings == nil {
		return true
	}

	raw, ok := settings["useDefaultDatasets"]
	if !ok {
		return true
	}

	switch val := raw.(type) {
	case bool:
		return val
	case string:
		parsed, err := strconv.ParseBool(val)
		if err == nil {
			return parsed
		}
	case int:
		return val != 0
	case int64:
		return val != 0
	case float64:
		return val != 0
	}

	return true
}
