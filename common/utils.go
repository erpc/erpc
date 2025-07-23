package common

import (
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common/hexutil"
)

const KeySeparator = "|"

type ContextKey string

// HexToUint64 converts a hexadecimal string to its decimal representation as a string.
func HexToUint64(hexValue string) (uint64, error) {
	// Create a new big.Int
	n := new(big.Int)

	// Check if the string has the "0x" prefix and remove it
	if len(hexValue) >= 2 && hexValue[:2] == "0x" {
		hexValue = hexValue[2:]
	}

	// Set n to the value of the hex string
	_, ok := n.SetString(hexValue, 16)
	if !ok {
		return 0, fmt.Errorf("invalid hexadecimal string")
	}

	// Return the decimal representation
	return n.Uint64(), nil
}

func HexToInt64(hexValue string) (int64, error) {
	normalizedHex, err := NormalizeHex(hexValue)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(normalizedHex, 0, 64)
}

func NormalizeHex(value interface{}) (string, error) {
	if bn, ok := value.(string); ok {
		// Check if blockNumber is already in hex format
		if strings.HasPrefix(bn, "0x") {
			// Convert hex to integer to remove 0 padding
			value, err := hexutil.DecodeUint64(bn)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("0x%x", value), nil // Convert back to hex without leading 0s
		}

		value, err := strconv.ParseUint(bn, 0, 64)
		if err == nil && value > 0 {
			return fmt.Sprintf("0x%x", value), nil
		}

		// TODO should we return an error here?
		return bn, nil
	}

	// If blockNumber is a number, convert to hex
	switch v := value.(type) {
	case int, int8, int16, int32, int64:
		return fmt.Sprintf("0x%x", v), nil
	case uint, uint8, uint16, uint32, uint64:
		return fmt.Sprintf("0x%x", v), nil
	default:
		return "", fmt.Errorf("value is not a string or number: %+v", v)
	}
}

func RemoveDuplicates(slice []string) []string {
	keys := make(map[string]bool)
	list := []string{}
	for _, entry := range slice {
		if _, value := keys[entry]; !value {
			keys[entry] = true
			list = append(list, entry)
		}
	}
	return list
}

// IsSemiValidJson checks the first byte to see if "potentially" valid json is present.
// This is not a full json.Valid check, but it is good enough for high speed detection of wrong HTML responses
// from upstreams.
func IsSemiValidJson(data []byte) bool {
	return len(data) > 0 && (data[0] == '{' || data[0] == '[' || data[0] == '"' || data[0] == 'n' || data[0] == 't' || data[0] == 'f')
}
