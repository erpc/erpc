package thirdparty

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog/log"
)

// sqdDatasetFromSettings extracts a dataset name from vendor settings for a given chainId.
// Returns the dataset and true if found, empty string and false otherwise.
//
// Supported settings:
//   - "dataset": string - use this dataset for all chains
//   - "datasetByChainId": map[chainId]dataset - per-chain dataset mapping
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

func sqdChainIdKeyMatches(key interface{}, chainId int64, chainIdStr string) bool {
	switch k := key.(type) {
	case int:
		return int64(k) == chainId
	case int32:
		return int64(k) == chainId
	case int64:
		return k == chainId
	case uint:
		return strconv.FormatUint(uint64(k), 10) == chainIdStr
	case uint32:
		return strconv.FormatUint(uint64(k), 10) == chainIdStr
	case uint64:
		return strconv.FormatUint(k, 10) == chainIdStr
	case float64:
		return k == float64(chainId)
	case string:
		return strings.TrimSpace(k) == chainIdStr
	default:
		return false
	}
}
