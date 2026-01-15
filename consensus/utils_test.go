package consensus

import (
	"math/big"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseNumericValue(t *testing.T) {
	tests := []struct {
		name     string
		input    interface{}
		expected *big.Int
	}{
		// Hex strings
		{
			name:     "hex_string_lowercase",
			input:    "0x5",
			expected: big.NewInt(5),
		},
		{
			name:     "hex_string_uppercase",
			input:    "0X5",
			expected: big.NewInt(5),
		},
		{
			name:     "hex_string_large",
			input:    "0xffffff",
			expected: big.NewInt(16777215),
		},
		{
			name:     "hex_string_with_leading_zeros",
			input:    "0x0000000a",
			expected: big.NewInt(10),
		},
		// Decimal strings
		{
			name:     "decimal_string",
			input:    "12345",
			expected: big.NewInt(12345),
		},
		{
			name:     "decimal_string_zero",
			input:    "0",
			expected: big.NewInt(0),
		},
		// Numeric types
		{
			name:     "float64",
			input:    float64(42),
			expected: big.NewInt(42),
		},
		{
			name:     "int64",
			input:    int64(100),
			expected: big.NewInt(100),
		},
		{
			name:     "int",
			input:    int(200),
			expected: big.NewInt(200),
		},
		// Edge cases
		{
			name:     "nil_input",
			input:    nil,
			expected: nil,
		},
		{
			name:     "empty_string",
			input:    "",
			expected: nil,
		},
		{
			name:     "whitespace_string",
			input:    "   ",
			expected: nil,
		},
		{
			name:     "invalid_hex",
			input:    "0xGGG",
			expected: nil,
		},
		{
			name:     "invalid_decimal",
			input:    "not_a_number",
			expected: nil,
		},
		{
			name:     "hex_string_trimmed",
			input:    "  0xa  ",
			expected: big.NewInt(10),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := parseNumericValue(tc.input)
			if tc.expected == nil {
				assert.Nil(t, result)
			} else {
				assert.NotNil(t, result)
				assert.Equal(t, 0, tc.expected.Cmp(result), "expected %s but got %s", tc.expected.String(), result.String())
			}
		})
	}
}

func TestValuesToKey(t *testing.T) {
	tests := []struct {
		name     string
		values   []*big.Int
		expected string
	}{
		{
			name:     "empty_slice",
			values:   []*big.Int{},
			expected: "",
		},
		{
			name:     "single_value",
			values:   []*big.Int{big.NewInt(5)},
			expected: "5",
		},
		{
			name:     "multiple_values",
			values:   []*big.Int{big.NewInt(5), big.NewInt(10), big.NewInt(15)},
			expected: "5:10:15",
		},
		{
			name:     "with_nil_value",
			values:   []*big.Int{big.NewInt(5), nil, big.NewInt(15)},
			expected: "5:nil:15",
		},
		{
			name:     "large_values",
			values:   []*big.Int{new(big.Int).SetUint64(18446744073709551615)},
			expected: "18446744073709551615",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := valuesToKey(tc.values)
			assert.Equal(t, tc.expected, result)
		})
	}
}

func TestCompareValueChains(t *testing.T) {
	tests := []struct {
		name     string
		a        []*big.Int
		b        []*big.Int
		expected int
	}{
		{
			name:     "both_nil",
			a:        nil,
			b:        nil,
			expected: 0,
		},
		{
			name:     "a_nil_b_has_values",
			a:        nil,
			b:        []*big.Int{big.NewInt(1)},
			expected: -1,
		},
		{
			name:     "a_has_values_b_nil",
			a:        []*big.Int{big.NewInt(1)},
			b:        nil,
			expected: 1,
		},
		{
			name:     "single_value_a_greater",
			a:        []*big.Int{big.NewInt(10)},
			b:        []*big.Int{big.NewInt(5)},
			expected: 1,
		},
		{
			name:     "single_value_b_greater",
			a:        []*big.Int{big.NewInt(5)},
			b:        []*big.Int{big.NewInt(10)},
			expected: -1,
		},
		{
			name:     "single_value_equal",
			a:        []*big.Int{big.NewInt(10)},
			b:        []*big.Int{big.NewInt(10)},
			expected: 0,
		},
		{
			name:     "multiple_values_first_wins",
			a:        []*big.Int{big.NewInt(10), big.NewInt(5)},
			b:        []*big.Int{big.NewInt(5), big.NewInt(100)},
			expected: 1, // First value wins
		},
		{
			name:     "multiple_values_tie_break_on_second",
			a:        []*big.Int{big.NewInt(10), big.NewInt(20)},
			b:        []*big.Int{big.NewInt(10), big.NewInt(15)},
			expected: 1, // First equal, second a wins
		},
		{
			name:     "multiple_values_all_equal",
			a:        []*big.Int{big.NewInt(10), big.NewInt(20), big.NewInt(30)},
			b:        []*big.Int{big.NewInt(10), big.NewInt(20), big.NewInt(30)},
			expected: 0,
		},
		{
			name:     "different_lengths_compared_on_common",
			a:        []*big.Int{big.NewInt(10), big.NewInt(20)},
			b:        []*big.Int{big.NewInt(10)},
			expected: 0, // Only compares common length, both equal on first
		},
		{
			name:     "large_numbers",
			a:        []*big.Int{new(big.Int).SetUint64(18446744073709551615)}, // max uint64
			b:        []*big.Int{new(big.Int).SetUint64(18446744073709551614)},
			expected: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := compareValueChains(tc.a, tc.b)
			assert.Equal(t, tc.expected, result)
		})
	}
}
