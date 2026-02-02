package thirdparty

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog/log"
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

// sqdSupportedChainIds returns a slice of all supported chain IDs.
func sqdSupportedChainIds() []int64 {
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

	raw, ok := settings["datasetByChainId"]
	if !ok {
		return "", false
	}

	return sqdDatasetFromMap(raw, chainId)
}

// sqdDatasetFromMap normalizes any map type from YAML/JSON deserialization
// and looks up the dataset for the given chainId.
func sqdDatasetFromMap(raw interface{}, chainId int64) (string, bool) {
	chainIdStr := strconv.FormatInt(chainId, 10)

	switch m := raw.(type) {
	case map[string]interface{}:
		return sqdExtractStringValue(m, chainIdStr, chainId)
	case map[string]string:
		if ds, ok := m[chainIdStr]; ok && ds != "" {
			return ds, true
		}
		return "", false
	case map[interface{}]interface{}:
		return sqdDatasetFromInterfaceMap(m, chainId, chainIdStr)
	case map[int]interface{}:
		return sqdExtractStringValue(m, int(chainId), chainId)
	case map[int]string:
		if ds, ok := m[int(chainId)]; ok && ds != "" {
			return ds, true
		}
		return "", false
	case map[int64]interface{}:
		return sqdExtractStringValue(m, chainId, chainId)
	case map[int64]string:
		if ds, ok := m[chainId]; ok && ds != "" {
			return ds, true
		}
		return "", false
	case map[float64]interface{}:
		return sqdExtractStringValue(m, float64(chainId), chainId)
	case map[float64]string:
		if ds, ok := m[float64(chainId)]; ok && ds != "" {
			return ds, true
		}
		return "", false
	default:
		log.Warn().
			Int64("chainId", chainId).
			Str("type", fmt.Sprintf("%T", raw)).
			Msg("sqd datasetByChainId unsupported type")
	}

	return "", false
}

// sqdExtractStringValue looks up a key in a map[K]interface{} and asserts the value is a string.
// Logs a warning if the key exists but the value is not a string.
func sqdExtractStringValue[K comparable](m map[K]interface{}, key K, chainId int64) (string, bool) {
	raw, exists := m[key]
	if !exists {
		return "", false
	}
	ds, ok := raw.(string)
	if !ok || ds == "" {
		if !ok {
			log.Warn().
				Int64("chainId", chainId).
				Str("valueType", fmt.Sprintf("%T", raw)).
				Msg("sqd datasetByChainId value is not a string, ignoring")
		}
		return "", false
	}
	return ds, true
}

func sqdDatasetFromInterfaceMap(m map[interface{}]interface{}, chainId int64, chainIdStr string) (string, bool) {
	for key, value := range m {
		if !sqdChainIdKeyMatches(key, chainId, chainIdStr) {
			continue
		}
		dataset, ok := value.(string)
		if !ok || dataset == "" {
			if !ok {
				log.Warn().
					Int64("chainId", chainId).
					Str("valueType", fmt.Sprintf("%T", value)).
					Msg("sqd datasetByChainId value is not a string, ignoring")
			}
			return "", false
		}
		return dataset, true
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
		if parsed, ok := sqdParseBoolString(val); ok {
			return parsed
		}
		log.Warn().Str("value", val).Msg("sqd useDefaultDatasets unrecognized string, defaulting to false")
		return false
	case int:
		return val != 0
	case int64:
		return val != 0
	case float64:
		return val != 0
	default:
		log.Warn().Str("type", fmt.Sprintf("%T", raw)).Msg("sqd useDefaultDatasets unsupported type, defaulting to false")
		return false
	}
}

func sqdParseBoolString(raw string) (bool, bool) {
	normalized := strings.ToLower(strings.TrimSpace(raw))
	switch normalized {
	case "true", "t", "1", "yes", "y", "on":
		return true, true
	case "false", "f", "0", "no", "n", "off":
		return false, true
	default:
		return false, false
	}
}

func sqdChainIdKeyMatches(key interface{}, chainId int64, chainIdStr string) bool {
	switch k := key.(type) {
	case int:
		return int64(k) == chainId
	case int32:
		return int64(k) == chainId
	case int64:
		return k == chainId
	case uint:
		if uint64(k) > math.MaxInt64 {
			return false
		}
		return int64(k) == chainId
	case uint32:
		return int64(k) == chainId
	case uint64:
		if k > math.MaxInt64 {
			return false
		}
		return int64(k) == chainId
	case float64:
		return k == float64(chainId)
	case string:
		return strings.TrimSpace(k) == chainIdStr
	default:
		return false
	}
}
