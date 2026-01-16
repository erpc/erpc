package consensus

import (
	"encoding/json"
	"math/big"
	"strconv"
	"strings"

	"github.com/erpc/erpc/common"
)

// extractFieldValues extracts numeric values from a response for the given field paths.
// Returns nil if any field cannot be extracted or parsed.
func extractFieldValues(resp *common.NormalizedResponse, fields []string) []*big.Int {
	if resp == nil || len(fields) == 0 {
		return nil
	}

	jrr, err := resp.JsonRpcResponse()
	if err != nil || jrr == nil {
		return nil
	}

	resultBytes := jrr.GetResultBytes()
	if len(resultBytes) == 0 {
		return nil
	}

	values := make([]*big.Int, 0, len(fields))
	for _, field := range fields {
		var rawValue interface{}

		if field == "result" {
			// Direct result value (for simple responses like eth_getTransactionCount)
			// The result could be a string like "0x5" or a number
			var strVal string
			if err := common.SonicCfg.Unmarshal(resultBytes, &strVal); err == nil {
				rawValue = strVal
			} else {
				// Try as number
				var numVal float64
				if err := common.SonicCfg.Unmarshal(resultBytes, &numVal); err == nil {
					rawValue = numVal
				}
			}
		} else {
			// Parse result as object and extract nested field
			var resultObj map[string]interface{}
			if err := common.SonicCfg.Unmarshal(resultBytes, &resultObj); err == nil {
				rawValue = resultObj[field]
			}
		}

		val := parseNumericValue(rawValue)
		if val == nil {
			return nil // If any field can't be parsed, return nil
		}
		values = append(values, val)
	}

	return values
}

// parseNumericValue converts various types to big.Int.
// Handles hex strings (0x...), decimal strings, json.Number, and numeric types.
// For float64, converts via string representation to preserve precision for large values.
func parseNumericValue(v interface{}) *big.Int {
	if v == nil {
		return nil
	}

	switch val := v.(type) {
	case string:
		val = strings.TrimSpace(val)
		if val == "" {
			return nil
		}
		if strings.HasPrefix(val, "0x") || strings.HasPrefix(val, "0X") {
			n, ok := new(big.Int).SetString(val[2:], 16)
			if ok {
				return n
			}
			return nil
		}
		n, ok := new(big.Int).SetString(val, 10)
		if ok {
			return n
		}
		return nil
	case json.Number:
		// json.Number preserves the exact string representation from JSON
		n, ok := new(big.Int).SetString(string(val), 10)
		if ok {
			return n
		}
		return nil
	case float64:
		// Convert via string to avoid int64 truncation for large values.
		// FormatFloat with 'f' and precision -1 gives the shortest representation
		// that will round-trip correctly (i.e., preserves all significant digits).
		// Note: float64 only has ~15-17 significant decimal digits, so values > 2^53
		// will already have lost precision before reaching this point.
		str := strconv.FormatFloat(val, 'f', -1, 64)
		// Remove decimal point if present (for values like 1.0)
		if idx := strings.Index(str, "."); idx != -1 {
			str = str[:idx]
		}
		n, ok := new(big.Int).SetString(str, 10)
		if ok {
			return n
		}
		return nil
	case int64:
		return big.NewInt(val)
	case int:
		return big.NewInt(int64(val)) // #nosec G115
	}
	return nil
}

// valuesToKey converts a slice of big.Int values to a string key for grouping.
// This allows responses with the same numeric value but different formatting to be grouped together.
func valuesToKey(values []*big.Int) string {
	if len(values) == 0 {
		return ""
	}
	parts := make([]string, len(values))
	for i, v := range values {
		if v == nil {
			parts[i] = "nil"
		} else {
			parts[i] = v.String()
		}
	}
	return strings.Join(parts, ":")
}

// compareValueChains compares two value chains. Returns:
// +1 if a > b, -1 if a < b, 0 if equal.
// Values are compared in order (first field is primary, second is tie-breaker, etc.)
func compareValueChains(a, b []*big.Int) int {
	if a == nil && b == nil {
		return 0
	}
	if a == nil {
		return -1 // b has values, a doesn't
	}
	if b == nil {
		return 1 // a has values, b doesn't
	}

	// Compare each value in order
	minLen := len(a)
	if len(b) < minLen {
		minLen = len(b)
	}

	for i := 0; i < minLen; i++ {
		cmp := a[i].Cmp(b[i])
		if cmp != 0 {
			return cmp
		}
	}

	// All compared values are equal
	return 0
}
