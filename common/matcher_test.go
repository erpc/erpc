package common

import (
	"testing"
)

func TestWildcardMatch(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		value   string
		want    bool
		wantErr bool
	}{
		// Simple matching
		{
			name:    "exact address match",
			pattern: "0x1234567890123456789012345678901234567890",
			value:   "0x1234567890123456789012345678901234567890",
			want:    true,
		},
		{
			name:    "wildcard address match",
			pattern: "0x123456*",
			value:   "0x1234567890123456789012345678901234567890",
			want:    true,
		},

		// Method signatures
		{
			name:    "exact method match",
			pattern: "0x095ea7b3",
			value:   "0x095ea7b3",
			want:    true,
		},
		{
			name:    "empty value match",
			pattern: "<empty>",
			value:   "",
			want:    true,
		},

		// Numeric comparisons
		{
			name:    "greater than hex",
			pattern: ">0xff",
			value:   "0x100",
			want:    true,
		},
		{
			name:    "less than hex",
			pattern: "<0x100",
			value:   "0xff",
			want:    true,
		},
		{
			name:    "equal hex",
			pattern: "=0x100",
			value:   "0x100",
			want:    true,
		},

		// OR matching
		{
			name:    "simple OR with methods",
			pattern: "0x095ea7b3 | 0x23b872dd",
			value:   "0x095ea7b3",
			want:    true,
		},
		{
			name:    "OR with wildcards",
			pattern: "0x1234* | 0x5678*",
			value:   "0x12345678",
			want:    true,
		},

		// AND matching
		{
			name:    "simple AND with hex range",
			pattern: ">0x100 & <0x200",
			value:   "0x150",
			want:    true,
		},
		{
			name:    "AND with wildcards",
			pattern: "0x1234* & *5678",
			value:   "0x12345678",
			want:    true,
		},

		// Nested matching
		{
			name:    "nested parentheses",
			pattern: "(0x1234* | 0x5678*) & !0x12345678",
			value:   "0x12349999",
			want:    true,
		},

		// Complex nested AND/OR
		{
			name:    "complex nested expression",
			pattern: "(0x095ea7b3 | 0x23b872dd) & !(0xdeadbeef | 0xbeefdead)",
			value:   "0x095ea7b3",
			want:    true,
		},

		// Negation
		{
			name:    "simple negation",
			pattern: "!0x1234*",
			value:   "0x5678abcd",
			want:    true,
		},
		{
			name:    "negated range",
			pattern: "!(>=0x100 & <=0x200)",
			value:   "0x99",
			want:    true,
		},

		// Complex EVM patterns
		{
			name:    "complex topic pattern",
			pattern: "(0xddf252ad* & !0x00000000*) | 0x8c5be1e5*",
			value:   "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef",
			want:    true,
		},
		{
			name:    "block tag or numeric range with number within range",
			pattern: "!(latest|safe|finalized) & >=0x1111",
			value:   "0x2222",
			want:    true,
		},
		{
			name:    "block tag or numeric range with skipped block tag",
			pattern: "!(latest|safe|finalized) & >=0x1111",
			value:   "latest",
			want:    false,
		},
		{
			name:    "block tag not matching",
			pattern: "!(latest|safe|finalized)",
			value:   "pending",
			want:    true,
		},
		{
			name:    "numeric range with negating",
			pattern: "!>=0x2222",
			value:   "0x1111",
			want:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := WildcardMatch(tt.pattern, tt.value)
			if (err != nil) != tt.wantErr {
				t.Errorf("WildcardMatch() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("WildcardMatch() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestValidatePattern(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		wantErr bool
	}{
		{"empty pattern", "", false},
		{"valid simple pattern", "0x1234*", false},
		{"valid OR pattern", "0x1234 | 0x5678", false},
		{"valid AND pattern", "0x1234 & 0x5678", false},
		{"valid complex pattern", "(0x1234 | 0x5678) & !0x9abc", false},
		{"valid hex comparison", ">=0x100", false},

		{"invalid operator placement", "| 0x1234", true},
		{"missing operand", "0x1234 &", true},
		{"unmatched parenthesis", "(0x1234", true},
		{"invalid hex number", ">0xZZZ", true},
		{"double operator", "0x1234 && 0x5678", true},
		{"invalid NOT usage", "0x1234 ! 0x5678", true},
		{"invalid AND pattern syntax", "0x1234 & & 0x5678", true},
		{"unmatched parenthesis with AND pattern syntax 1", "0x1234 & (0x5678", true},
		{"unmatched parenthesis with AND pattern syntax 2", "(0x1234 & 0x5678", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidatePattern(tt.pattern)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidatePattern() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
