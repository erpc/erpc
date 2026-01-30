package data

import (
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
)

func TestCachePolicy_ParamToString(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name     string
		input    interface{}
		expected string
	}{
		{
			name:     "Nil",
			input:    nil,
			expected: "",
		},
		{
			name:     "String",
			input:    "test",
			expected: "test",
		},
		{
			name:     "Number",
			input:    float64(123),
			expected: "123",
		},
		{
			name:     "Boolean",
			input:    true,
			expected: "true",
		},
		{
			name:     "Integer",
			input:    42,
			expected: "42",
		},
		{
			name:     "HexString",
			input:    "0x123abc",
			expected: "0x123abc",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := paramToString(tc.input)
			assert.Equal(t, tc.expected, result)
		})
	}
}

func TestCachePolicy_MatchParam(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name     string
		pattern  interface{}
		param    interface{}
		expected bool
	}{
		// Primitive types
		{
			name:     "ExactStringMatch",
			pattern:  "test",
			param:    "test",
			expected: true,
		},
		{
			name:     "WildcardStringMatch",
			pattern:  "0x*",
			param:    "0x123abc",
			expected: true,
		},
		{
			name:     "NumberMatch",
			pattern:  float64(123),
			param:    float64(123),
			expected: true,
		},
		{
			name:     "BooleanMatch",
			pattern:  true,
			param:    true,
			expected: true,
		},
		{
			name:     "NilMatch",
			pattern:  nil,
			param:    nil,
			expected: true,
		},

		// Simple objects
		{
			name: "SimpleObjectExactMatch",
			pattern: map[string]interface{}{
				"address": "0x123",
			},
			param: map[string]interface{}{
				"address": "0x123",
			},
			expected: true,
		},
		{
			name: "SimpleObjectWildcardMatch",
			pattern: map[string]interface{}{
				"address": "0x*",
			},
			param: map[string]interface{}{
				"address": "0x123abc",
			},
			expected: true,
		},

		// Arrays
		{
			name:     "SimpleArrayExactMatch",
			pattern:  []interface{}{"0x123", "0x456"},
			param:    []interface{}{"0x123", "0x456"},
			expected: true,
		},
		{
			name:     "SimpleArrayWildcardMatch",
			pattern:  []interface{}{"0x*", "0x456"},
			param:    []interface{}{"0x123", "0x456"},
			expected: true,
		},

		// Complex nested structures
		{
			name: "ComplexObjectMatch",
			pattern: map[string]interface{}{
				"fromBlock": "0x*",
				"toBlock":   "0x*",
				"address":   "0x123",
				"topics": []interface{}{
					"0xabc*",
					nil,
				},
			},
			param: map[string]interface{}{
				"fromBlock": "0x100",
				"toBlock":   "0x200",
				"address":   "0x123",
				"topics": []interface{}{
					"0xabcdef",
					nil,
				},
			},
			expected: true,
		},

		// Negative cases
		{
			name:     "StringMismatch",
			pattern:  "test",
			param:    "other",
			expected: false,
		},
		{
			name: "MissingField",
			pattern: map[string]interface{}{
				"required": "value",
			},
			param: map[string]interface{}{
				"other": "value",
			},
			expected: false,
		},
		{
			name:     "ArrayLengthMismatch",
			pattern:  []interface{}{"a", "b"},
			param:    []interface{}{"a"},
			expected: false,
		},
		{
			name: "TypeMismatch",
			pattern: map[string]interface{}{
				"value": "string",
			},
			param:    []interface{}{"string"},
			expected: false,
		},

		// Edge cases
		{
			name:     "EmptyObjects",
			pattern:  map[string]interface{}{},
			param:    map[string]interface{}{},
			expected: true,
		},
		{
			name:     "EmptyArrays",
			pattern:  []interface{}{},
			param:    []interface{}{},
			expected: true,
		},
		{
			name: "MixedTypes",
			pattern: map[string]interface{}{
				"string": "value",
				"number": float64(123),
				"bool":   true,
				"null":   nil,
				"array":  []interface{}{"a", "b"},
				"object": map[string]interface{}{"key": "value"},
			},
			param: map[string]interface{}{
				"string": "value",
				"number": float64(123),
				"bool":   true,
				"null":   nil,
				"array":  []interface{}{"a", "b"},
				"object": map[string]interface{}{"key": "value"},
			},
			expected: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := matchParam(tc.pattern, tc.param)
			assert.NoError(t, err)
			assert.Equal(t, tc.expected, result)
		})
	}
}

func TestCachePolicy_MatchParams(t *testing.T) {
	t.Parallel()
	mockConnector := NewMockConnector("test")

	testCases := []struct {
		name     string
		config   *common.CachePolicyConfig
		params   []interface{}
		expected bool
	}{
		{
			name: "SimpleMatch",
			config: &common.CachePolicyConfig{
				Params: []interface{}{"0x*"},
			},
			params:   []interface{}{"0x123"},
			expected: true,
		},
		{
			name: "ComplexMatch",
			config: &common.CachePolicyConfig{
				Params: []interface{}{
					map[string]interface{}{
						"fromBlock": "0x*",
						"toBlock":   "0x*",
						"address":   "0x123",
					},
				},
			},
			params: []interface{}{
				map[string]interface{}{
					"fromBlock": "0x100",
					"toBlock":   "0x200",
					"address":   "0x123",
				},
			},
			expected: true,
		},
		{
			name: "NoParams",
			config: &common.CachePolicyConfig{
				Params: []interface{}{},
			},
			params:   []interface{}{"anything"},
			expected: true,
		},
		{
			name: "NotEnoughParams",
			config: &common.CachePolicyConfig{
				Params: []interface{}{"a", "b"},
			},
			params:   []interface{}{"a"},
			expected: false,
		},
		{
			name: "ExtraParams",
			config: &common.CachePolicyConfig{
				Params: []interface{}{"a"},
			},
			params:   []interface{}{"a", "extra"},
			expected: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			policy, err := NewCachePolicy(tc.config, mockConnector)
			assert.NoError(t, err)
			result, err := policy.matchParams(tc.params)
			assert.NoError(t, err)
			assert.Equal(t, tc.expected, result)
		})
	}
}
