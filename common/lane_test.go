package common

import "testing"

func TestLaneName(t *testing.T) {
	tests := []struct {
		name string
		ids  []string
		want string
	}{
		// (1) Shared token = the group prefix.
		{"shared first token", []string{"systx-chainstack-hyperevm", "systx-quicknode-hyperevm", "systx-dwellir-hyperevm"}, "systx"},
		{"shared token not first", []string{"chainstack-systx", "quicknode-systx", "dwellir-systx"}, "systx"},
		{"flashblocks group", []string{"flashblocks-base-alchemy", "flashblocks-quicknode-base"}, "flashblocks"},
		// Prefer the EARLIER shared token when several are shared by all.
		// Both "a" and "shared" are in every id; "a" appears earlier -> "a".
		{"prefer earlier shared token", []string{"a-shared-x", "a-shared-y", "a-shared-z"}, "a"},
		// Order-independent + earliest preference (sorted first id drives order).
		{"order independent", []string{"quicknode-systx", "systx-quicknode"}, "quicknode"}, // sorted: ["quicknode-systx","systx-quicknode"]; first id walked -> quicknode shared? yes
		// Single id -> its first token.
		{"single id", []string{"standard-quicknode-hyperevm"}, "standard"},
		{"single id no hyphen", []string{"solo"}, "solo"},

		// (2) Combo fallback when nothing is shared by all.
		{"no shared token -> combo", []string{"alchemy-evm:8453", "blockpi-base"}, "albl"},
		{"combo three", []string{"dwellir-x", "blockpi-y", "alchemy-z"}, "albldw"}, // sorted: alchemy,blockpi,dwellir -> al+bl+dw
		{"combo caps at five", []string{"aa-1", "bb-2", "cc-3", "dd-4", "ee-5", "ff-6", "gg-7"}, "aabbccddee"},

		{"empty", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := LaneName(tt.ids); got != tt.want {
				t.Errorf("LaneName(%v) = %q, want %q", tt.ids, got, tt.want)
			}
		})
	}
}

func TestLaneName_Deterministic(t *testing.T) {
	a := LaneName([]string{"quicknode-systx", "systx-chainstack", "dwellir-systx"})
	b := LaneName([]string{"dwellir-systx", "systx-chainstack", "quicknode-systx"})
	if a != b {
		t.Fatalf("LaneName not order-independent: %q vs %q", a, b)
	}
	if a != "systx" {
		t.Fatalf("expected shared token systx, got %q", a)
	}
}
