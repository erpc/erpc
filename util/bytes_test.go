package util

import (
	"testing"
)

func TestIsBytesEmptyish(t *testing.T) {
	tests := []struct {
		name     string
		input    []byte
		expected bool
	}{
		// Empty cases
		{
			name:     "empty byte slice",
			input:    []byte{},
			expected: true,
		},
		{
			name:     "nil byte slice",
			input:    nil,
			expected: true,
		},

		// Single zero
		{
			name:     "single zero character",
			input:    []byte("0"),
			expected: true,
		},

		// Raw hex values with 0x prefix
		{
			name:     "raw hex 0x",
			input:    []byte("0x"),
			expected: true,
		},
		{
			name:     "raw hex 0X",
			input:    []byte("0X"),
			expected: true,
		},
		{
			name:     "raw hex 0x0 with single zero",
			input:    []byte("0x0"),
			expected: true,
		},
		{
			name:     "raw hex 0x00 with double zeros",
			input:    []byte("0x00"),
			expected: true,
		},
		{
			name:     "raw hex 0x0000 with multiple zeros",
			input:    []byte("0x0000"),
			expected: true,
		},
		{
			name:     "raw hex 0x00000000 with many zeros",
			input:    []byte("0x00000000"),
			expected: true,
		},
		{
			name:     "raw hex 0x0001 with leading zeros but non-zero",
			input:    []byte("0x0001"),
			expected: false,
		},
		{
			name:     "raw hex 0x000a with leading zeros but non-zero",
			input:    []byte("0x000a"),
			expected: false,
		},
		{
			name:     "raw hex 0x1 without leading zeros",
			input:    []byte("0x1"),
			expected: false,
		},
		{
			name:     "raw hex 0x123456",
			input:    []byte("0x123456"),
			expected: false,
		},

		// String-quoted hex values
		{
			name:     "quoted hex \"0x\"",
			input:    []byte("\"0x\""),
			expected: true,
		},
		{
			name:     "quoted hex \"0x0\" with single zero",
			input:    []byte("\"0x0\""),
			expected: true,
		},
		{
			name:     "quoted hex \"0x00\" with double zeros",
			input:    []byte("\"0x00\""),
			expected: true,
		},
		{
			name:     "quoted hex \"0x000\" with triple zeros",
			input:    []byte("\"0x000\""),
			expected: true,
		},
		{
			name:     "quoted hex \"0x0000\" with multiple zeros",
			input:    []byte("\"0x0000\""),
			expected: true,
		},
		{
			name:     "quoted hex \"0x00000000\" with many zeros",
			input:    []byte("\"0x00000000\""),
			expected: true,
		},
		{
			name:     "quoted hex \"0x0001\" with leading zeros but non-zero",
			input:    []byte("\"0x0001\""),
			expected: false,
		},
		{
			name:     "quoted hex \"0x000a\" with leading zeros but non-zero",
			input:    []byte("\"0x000a\""),
			expected: false,
		},
		{
			name:     "quoted hex \"0x000abc\" with leading zeros but non-zero",
			input:    []byte("\"0x000abc\""),
			expected: false,
		},
		{
			name:     "quoted hex \"0x1\" without leading zeros",
			input:    []byte("\"0x1\""),
			expected: false,
		},
		{
			name:     "quoted hex \"0x123456\"",
			input:    []byte("\"0x123456\""),
			expected: false,
		},

		// Empty string
		{
			name:     "empty string \"\"",
			input:    []byte("\"\""),
			expected: true,
		},

		// Null
		{
			name:     "null",
			input:    []byte("null"),
			expected: true,
		},

		// Empty array
		{
			name:     "empty array []",
			input:    []byte("[]"),
			expected: true,
		},

		// Empty object
		{
			name:     "empty object {}",
			input:    []byte("{}"),
			expected: true,
		},

		// Non-empty cases
		{
			name:     "non-empty string",
			input:    []byte("hello"),
			expected: false,
		},
		{
			name:     "non-empty number",
			input:    []byte("123"),
			expected: false,
		},
		{
			name:     "non-empty array",
			input:    []byte("[1]"),
			expected: false,
		},
		{
			name:     "non-empty object",
			input:    []byte("{\"a\":1}"),
			expected: false,
		},
		{
			name:     "string with null",
			input:    []byte("\"null\""),
			expected: false,
		},
		{
			name:     "string with zero",
			input:    []byte("\"0\""),
			expected: false,
		},

		// Edge cases for length checking
		{
			name:     "three characters abc",
			input:    []byte("abc"),
			expected: false,
		},
		{
			name:     "four characters abcd",
			input:    []byte("abcd"),
			expected: false,
		},
		{
			name:     "five characters abcde",
			input:    []byte("abcde"),
			expected: false,
		},

		// Mixed case hex values
		{
			name:     "mixed case 0X00",
			input:    []byte("0X00"),
			expected: true,
		},
		{
			name:     "mixed case 0X0001",
			input:    []byte("0X0001"),
			expected: false,
		},

		// Invalid hex-like strings that aren't actually hex
		{
			name:     "invalid hex 0xg",
			input:    []byte("0xg"),
			expected: false,
		},
		{
			name:     "partial hex 0",
			input:    []byte("0"),
			expected: true,
		},
		{
			name:     "just x",
			input:    []byte("x"),
			expected: false,
		},

		// More edge cases with quoted strings
		{
			name:     "quoted string with only opening quote",
			input:    []byte("\""),
			expected: false,
		},
		{
			name:     "quoted hex missing closing quote",
			input:    []byte("\"0x0"),
			expected: false,
		},
		{
			name:     "quoted hex \"0x0\" but with extra trailing zero after quote",
			input:    []byte("\"0x0\"0"),
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsBytesEmptyish(tt.input)
			if result != tt.expected {
				t.Errorf("IsBytesEmptyish(%q) = %v, want %v", tt.input, result, tt.expected)
			}
		})
	}
}

// TestIsBytesEmptyishHexLeadingZeros specifically tests the hex leading zero handling
func TestIsBytesEmptyishHexLeadingZeros(t *testing.T) {
	// Test that leading zeros are properly trimmed and evaluated
	tests := []struct {
		name     string
		input    string
		expected bool
	}{
		// Raw hex values
		{"raw hex all zeros", "0x00000000000000000000000000000000", true},
		{"raw hex zeros then 1", "0x00000000000000000000000000000001", false},
		{"raw hex zeros then a", "0x0000000000000000000000000000000a", false},
		{"raw hex zeros then f", "0x0000000000000000000000000000000f", false},
		{"raw hex zeros then 10", "0x00000000000000000000000000000010", false},

		// String-quoted hex values
		{"quoted hex all zeros", "\"0x00000000000000000000000000000000\"", true},
		{"quoted hex zeros then 1", "\"0x00000000000000000000000000000001\"", false},
		{"quoted hex zeros then a", "\"0x0000000000000000000000000000000a\"", false},
		{"quoted hex zeros then f", "\"0x0000000000000000000000000000000f\"", false},
		{"quoted hex zeros then 10", "\"0x00000000000000000000000000000010\"", false},

		// Edge case: very long string of zeros
		{"very long zeros raw", "0x" + string(make([]byte, 1000, 1000)), true}, // This creates 1000 null bytes which will appear as zeros

		// Edge case: zeros with different hex digits
		{"zeros then abc", "0x000000abc", false},
		{"zeros then 123", "0x000000123", false},
		{"zeros then deadbeef", "0x00000000deadbeef", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Special handling for the very long zeros case
			input := tt.input
			if tt.name == "very long zeros raw" {
				zeros := make([]byte, 1000)
				for i := range zeros {
					zeros[i] = '0'
				}
				input = "0x" + string(zeros)
			}

			result := IsBytesEmptyish([]byte(input))
			if result != tt.expected {
				t.Errorf("IsBytesEmptyish(%q) = %v, want %v", input, result, tt.expected)
			}
		})
	}
}

// BenchmarkIsBytesEmptyish benchmarks the function with various inputs
func BenchmarkIsBytesEmptyish(b *testing.B) {
	benchmarks := []struct {
		name  string
		input []byte
	}{
		{"empty", []byte{}},
		{"null", []byte("null")},
		{"0x", []byte("0x")},
		{"0x0000", []byte("0x0000")},
		{"0x0001", []byte("0x0001")},
		{"quoted_0x0000", []byte("\"0x0000\"")},
		{"quoted_0x0001", []byte("\"0x0001\"")},
		{"non_empty", []byte("hello world")},
		{"long_hex_zeros", []byte("0x00000000000000000000000000000000")},
		{"long_hex_value", []byte("0x00000000000000000000000000000001")},
	}

	for _, bm := range benchmarks {
		b.Run(bm.name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				_ = IsBytesEmptyish(bm.input)
			}
		})
	}
}
