package thirdparty

import (
	"fmt"
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
		case map[string]string:
			if ds, ok := sqdDatasetFromStringKeyMapString(m, chainId); ok {
				return ds, true
			}
		case map[string]interface{}:
			if ds, ok := sqdDatasetFromStringKeyMap(m, chainId); ok {
				return ds, true
			}
		case map[int]string:
			if ds, ok := sqdDatasetFromIntKeyMapString(m, chainId); ok {
				return ds, true
			}
		case map[int]interface{}:
			if ds, ok := sqdDatasetFromIntKeyMap(m, chainId); ok {
				return ds, true
			}
		case map[int64]string:
			if ds, ok := sqdDatasetFromInt64KeyMapString(m, chainId); ok {
				return ds, true
			}
		case map[int64]interface{}:
			if ds, ok := sqdDatasetFromInt64KeyMap(m, chainId); ok {
				return ds, true
			}
		case map[float64]string:
			if ds, ok := sqdDatasetFromFloatKeyMapString(m, chainId); ok {
				return ds, true
			}
		case map[float64]interface{}:
			if ds, ok := sqdDatasetFromFloatKeyMap(m, chainId); ok {
				return ds, true
			}
		case map[interface{}]interface{}:
			if ds, ok := sqdDatasetFromInterfaceKeyMap(m, chainId); ok {
				return ds, true
			}
		default:
			log.Warn().Str("type", fmt.Sprintf("%T", raw)).Msg("sqd datasetByChainId unsupported type")
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
		if parsed, ok := sqdParseBoolString(val); ok {
			return parsed
		}
		log.Warn().Str("value", val).Msg("sqd useDefaultDatasets invalid string, defaulting to true")
	case int:
		return val != 0
	case int64:
		return val != 0
	case float64:
		return val != 0
	default:
		log.Warn().Str("type", fmt.Sprintf("%T", raw)).Msg("sqd useDefaultDatasets unsupported type, defaulting to true")
	}

	return true
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

func sqdDatasetFromStringKeyMap(m map[string]interface{}, chainId int64) (string, bool) {
	if ds, ok := m[strconv.FormatInt(chainId, 10)].(string); ok && ds != "" {
		return ds, true
	}
	return "", false
}

func sqdDatasetFromStringKeyMapString(m map[string]string, chainId int64) (string, bool) {
	if ds, ok := m[strconv.FormatInt(chainId, 10)]; ok && ds != "" {
		return ds, true
	}
	return "", false
}

func sqdDatasetFromIntKeyMap(m map[int]interface{}, chainId int64) (string, bool) {
	if !sqdChainIdFitsInt(chainId) {
		return "", false
	}
	if ds, ok := m[int(chainId)].(string); ok && ds != "" {
		return ds, true
	}
	return "", false
}

func sqdDatasetFromIntKeyMapString(m map[int]string, chainId int64) (string, bool) {
	if !sqdChainIdFitsInt(chainId) {
		return "", false
	}
	if ds, ok := m[int(chainId)]; ok && ds != "" {
		return ds, true
	}
	return "", false
}

func sqdDatasetFromInt64KeyMap(m map[int64]interface{}, chainId int64) (string, bool) {
	if ds, ok := m[chainId].(string); ok && ds != "" {
		return ds, true
	}
	return "", false
}

func sqdDatasetFromInt64KeyMapString(m map[int64]string, chainId int64) (string, bool) {
	if ds, ok := m[chainId]; ok && ds != "" {
		return ds, true
	}
	return "", false
}

func sqdDatasetFromFloatKeyMap(m map[float64]interface{}, chainId int64) (string, bool) {
	if ds, ok := m[float64(chainId)].(string); ok && ds != "" {
		return ds, true
	}
	return "", false
}

func sqdDatasetFromFloatKeyMapString(m map[float64]string, chainId int64) (string, bool) {
	if ds, ok := m[float64(chainId)]; ok && ds != "" {
		return ds, true
	}
	return "", false
}

func sqdDatasetFromInterfaceKeyMap(m map[interface{}]interface{}, chainId int64) (string, bool) {
	chainIdStr := strconv.FormatInt(chainId, 10)
	for key, value := range m {
		dataset, ok := value.(string)
		if !ok || dataset == "" {
			continue
		}
		if sqdChainIdKeyMatches(key, chainId, chainIdStr) {
			return dataset, true
		}
	}
	return "", false
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
		return int64(k) == chainId
	case uint32:
		return int64(k) == chainId
	case uint64:
		return k == uint64(chainId)
	case float64:
		return k == float64(chainId)
	case string:
		return strings.TrimSpace(k) == chainIdStr
	default:
		return false
	}
}

func sqdChainIdFitsInt(chainId int64) bool {
	maxInt := int64(^uint(0) >> 1)
	minInt := -maxInt - 1
	return chainId >= minInt && chainId <= maxInt
}
