package common

import (
	"github.com/blockchain-data-standards/manifesto/evm"
)

const KeySeparator = "|"

type ContextKey string

func HexToUint64(hexValue string) (uint64, error) {
	return evm.HexToUint64(hexValue)
}

func HexToInt64(hexValue string) (int64, error) {
	return evm.HexToInt64(hexValue)
}

func NormalizeHex(value interface{}) (string, error) {
	return evm.NormalizeHex(value)
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
