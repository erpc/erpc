package evm

import (
	"fmt"
	"testing"
)

func TestIsMissingDataError(t *testing.T) {
	tests := []struct {
		msg  string
		want bool
	}{
		{"missing trie node abc123", true},
		{"header not found", true},
		{"could not find block 100", true},
		{"unknown block", true},
		{"block not found", true},
		{"No state available for block", true},
		{"genesis is not traceable", true},
		{"transaction not found", true},
		{"trie does not contain key", true},
		{"requested data is not available", true},

		// The pattern: "historical state" + "is not available"
		{"required historical state is not available", true},
		// The bug fix: "historical state" + "unavailable" (without "is not available")
		{"required historical state unavailable", true},

		{"some unrelated error", false},
		{"rate limit exceeded", false},
		{"execution reverted", false},
	}

	for _, tt := range tests {
		t.Run(tt.msg, func(t *testing.T) {
			err := fmt.Errorf("%s", tt.msg)
			got := IsMissingDataError(err)
			if got != tt.want {
				t.Errorf("IsMissingDataError(%q) = %v, want %v", tt.msg, got, tt.want)
			}
		})
	}
}
