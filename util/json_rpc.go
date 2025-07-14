package util

import (
	"encoding/hex"
	"fmt"
	"math"
	"math/rand"
	"strings"
)

// RandomID returns a value appropriate for a JSON-RPC ID field (i.e int64 type but with 32 bit range) to avoid overflow during conversions and reading/sending to upstreams
func RandomID() int64 {
	return int64(rand.Intn(math.MaxInt32)) // #nosec G404
}

// ParseBlockParameter handles complex block parameters including objects with blockHash
// Returns blockNumber string, blockHash bytes, and error
func ParseBlockParameter(param interface{}) (blockNumber string, blockHash []byte, err error) {
	switch v := param.(type) {
	case string:
		if strings.HasPrefix(v, "0x") && len(v) == 66 {
			// This is a block hash
			blockHash, err = parseHexBytes(v)
			if err != nil {
				return "", nil, fmt.Errorf("failed to parse block hash: %w", err)
			}
			return "", blockHash, nil
		}
		// This is a block number or tag
		blockNumber = v
	case float64:
		// JSON numbers are parsed as float64
		blockNumber = fmt.Sprintf("0x%x", uint64(v)) // #nosec G115
	case int64:
		blockNumber = fmt.Sprintf("0x%x", uint64(v)) // #nosec G115
	case uint64:
		blockNumber = fmt.Sprintf("0x%x", v)
	case map[string]interface{}:
		// Handle complex block parameter object
		if blockHashValue, exists := v["blockHash"]; exists {
			if bh, ok := blockHashValue.(string); ok {
				blockHash, err = parseHexBytes(bh)
				if err != nil {
					return "", nil, fmt.Errorf("failed to parse blockHash from object: %w", err)
				}
				return "", blockHash, nil
			}
		}
		// If no blockHash, check for blockNumber
		if blockNumberValue, exists := v["blockNumber"]; exists {
			if bn, ok := blockNumberValue.(string); ok {
				blockNumber = bn
			}
		}
		// Check for blockTag
		if blockTagValue, exists := v["blockTag"]; exists {
			if bt, ok := blockTagValue.(string); ok {
				blockNumber = bt
			}
		}
		if blockNumber == "" && blockHash == nil {
			return "", nil, fmt.Errorf("block parameter object must contain blockHash, blockNumber, or blockTag")
		}
	default:
		return "", nil, fmt.Errorf("invalid block parameter type: %T value: %v", param, param)
	}

	return blockNumber, blockHash, nil
}

// parseHexBytes helper function to parse hex strings to bytes
func parseHexBytes(hexStr string) ([]byte, error) {
	hexStr = strings.TrimPrefix(hexStr, "0x")
	return hex.DecodeString(hexStr)
}
