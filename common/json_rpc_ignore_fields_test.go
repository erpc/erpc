package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJsonRpcResponse_CanonicalHashWithIgnoredFields(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name         string
		result1      string
		result2      string
		ignoreFields []string
		shouldMatch  bool
	}{
		{
			name:         "ignore simple field",
			result1:      `{"hash": "0x123", "status": "0x1", "from": "0xabc"}`,
			result2:      `{"hash": "0x123", "status": "0x0", "from": "0xabc"}`,
			ignoreFields: []string{"status"},
			shouldMatch:  true,
		},
		{
			name:         "ignore nested field",
			result1:      `{"hash": "0x123", "receipt": {"status": "0x1", "gasUsed": "0x100"}}`,
			result2:      `{"hash": "0x123", "receipt": {"status": "0x0", "gasUsed": "0x100"}}`,
			ignoreFields: []string{"receipt.status"},
			shouldMatch:  true,
		},
		{
			name:         "ignore array wildcard field",
			result1:      `{"logs": [{"address": "0x123", "blockTimestamp": "0x1"}, {"address": "0x456", "blockTimestamp": "0x2"}]}`,
			result2:      `{"logs": [{"address": "0x123", "blockTimestamp": "0x999"}, {"address": "0x456", "blockTimestamp": "0x888"}]}`,
			ignoreFields: []string{"logs.*.blockTimestamp"},
			shouldMatch:  true,
		},
		{
			name:         "ignore multiple fields",
			result1:      `{"hash": "0x123", "status": "0x1", "from": "0xabc", "to": "0xdef"}`,
			result2:      `{"hash": "0x123", "status": "0x0", "from": "0xabc", "to": "0x999"}`,
			ignoreFields: []string{"status", "to"},
			shouldMatch:  true,
		},
		{
			name:         "no ignored fields should not match",
			result1:      `{"hash": "0x123", "status": "0x1"}`,
			result2:      `{"hash": "0x123", "status": "0x0"}`,
			ignoreFields: []string{},
			shouldMatch:  false,
		},
		{
			name:         "ignore non-existent field",
			result1:      `{"hash": "0x123", "status": "0x1"}`,
			result2:      `{"hash": "0x123", "status": "0x1"}`,
			ignoreFields: []string{"nonexistent", "also.not.there"},
			shouldMatch:  true,
		},
		{
			name:         "complex nested structure",
			result1:      `{"block": {"transactions": [{"hash": "0x1", "input": "0xabc", "v": "0x1c"}, {"hash": "0x2", "input": "0xdef", "v": "0x1b"}]}}`,
			result2:      `{"block": {"transactions": [{"hash": "0x1", "input": "0xabc", "v": "0x99"}, {"hash": "0x2", "input": "0xdef", "v": "0x88"}]}}`,
			ignoreFields: []string{"block.transactions.*.v"},
			shouldMatch:  true,
		},
		{
			name:         "real world shadow response example",
			result1:      `{"hash": "0xa52b", "transactions": [{"hash": "0x18c4", "from": "0x3d2f", "chainId": null, "accessList": []}]}`,
			result2:      `{"transactions": [{"from": "0x3d2f", "hash": "0x18c4"}], "hash": "0xa52b"}`,
			ignoreFields: []string{"transactions.*.chainId", "transactions.*.accessList"},
			shouldMatch:  true,
		},
		{
			name:         "ignore deeply nested field",
			result1:      `{"a": {"b": {"c": {"d": "value1", "e": "same"}}}}`,
			result2:      `{"a": {"b": {"c": {"d": "value2", "e": "same"}}}}`,
			ignoreFields: []string{"a.b.c.d"},
			shouldMatch:  true,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			resp1 := &JsonRpcResponse{Result: []byte(tc.result1)}
			resp2 := &JsonRpcResponse{Result: []byte(tc.result2)}

			hash1, err1 := resp1.CanonicalHashWithIgnoredFields(tc.ignoreFields)
			hash2, err2 := resp2.CanonicalHashWithIgnoredFields(tc.ignoreFields)

			require.NoError(t, err1)
			require.NoError(t, err2)

			if tc.shouldMatch {
				assert.Equal(t, hash1, hash2, "Hashes should match when fields are ignored")
			} else {
				assert.NotEqual(t, hash1, hash2, "Hashes should not match")
			}
		})
	}
}

func TestRemoveFieldsByPaths(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		input    interface{}
		paths    []string
		expected interface{}
	}{
		{
			name: "remove simple field",
			input: map[string]interface{}{
				"keep":   "this",
				"remove": "that",
			},
			paths: []string{"remove"},
			expected: map[string]interface{}{
				"keep": "this",
			},
		},
		{
			name: "remove nested field",
			input: map[string]interface{}{
				"outer": map[string]interface{}{
					"keep": "this",
					"inner": map[string]interface{}{
						"remove": "that",
						"stay":   "here",
					},
				},
			},
			paths: []string{"outer.inner.remove"},
			expected: map[string]interface{}{
				"outer": map[string]interface{}{
					"keep": "this",
					"inner": map[string]interface{}{
						"stay": "here",
					},
				},
			},
		},
		{
			name: "remove from array with wildcard",
			input: map[string]interface{}{
				"items": []interface{}{
					map[string]interface{}{"id": 1, "timestamp": "2024-01-01", "value": "a"},
					map[string]interface{}{"id": 2, "timestamp": "2024-01-02", "value": "b"},
				},
			},
			paths: []string{"items.*.timestamp"},
			expected: map[string]interface{}{
				"items": []interface{}{
					map[string]interface{}{"id": 1, "value": "a"},
					map[string]interface{}{"id": 2, "value": "b"},
				},
			},
		},
		{
			name: "remove multiple paths",
			input: map[string]interface{}{
				"a": "keep",
				"b": "remove",
				"c": map[string]interface{}{
					"d": "remove",
					"e": "keep",
				},
			},
			paths: []string{"b", "c.d"},
			expected: map[string]interface{}{
				"a": "keep",
				"c": map[string]interface{}{
					"e": "keep",
				},
			},
		},
		{
			name: "handle non-existent path gracefully",
			input: map[string]interface{}{
				"a": "value",
			},
			paths: []string{"b", "c.d.e"},
			expected: map[string]interface{}{
				"a": "value",
			},
		},
		{
			name: "nested arrays with wildcards",
			input: map[string]interface{}{
				"blocks": []interface{}{
					map[string]interface{}{
						"number": 1,
						"transactions": []interface{}{
							map[string]interface{}{"hash": "0x1", "v": "0x1c", "from": "0xabc"},
							map[string]interface{}{"hash": "0x2", "v": "0x1b", "from": "0xdef"},
						},
					},
				},
			},
			paths: []string{"blocks.*.transactions.*.v"},
			expected: map[string]interface{}{
				"blocks": []interface{}{
					map[string]interface{}{
						"number": 1,
						"transactions": []interface{}{
							map[string]interface{}{"hash": "0x1", "from": "0xabc"},
							map[string]interface{}{"hash": "0x2", "from": "0xdef"},
						},
					},
				},
			},
		},
		{
			name: "nested arrays with wildcards",
			input: []interface{}{
				map[string]interface{}{
					"number": 1,
					"transactions": []interface{}{
						map[string]interface{}{"hash": "0x1", "v": "0x1c", "from": "0xabc"},
						map[string]interface{}{"hash": "0x2", "v": "0x1b", "from": "0xdef"},
					},
				},
			},
			paths: []string{"*.transactions.*.v"},
			expected: []interface{}{
				map[string]interface{}{
					"number": 1,
					"transactions": []interface{}{
						map[string]interface{}{"hash": "0x1", "from": "0xabc"},
						map[string]interface{}{"hash": "0x2", "from": "0xdef"},
					},
				},
			},
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			result := removeFieldsByPaths(tc.input, tc.paths)
			assert.Equal(t, tc.expected, result)
		})
	}
}
