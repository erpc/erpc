package thirdparty

import (
	"testing"

	"github.com/erpc/erpc/common"
)

func TestSqdChainToDataset(t *testing.T) {
	tests := []struct {
		name     string
		chainId  int64
		expected string
	}{
		{"ethereum mainnet", 1, "ethereum-mainnet"},
		{"polygon mainnet", 137, "polygon-mainnet"},
		{"arbitrum one", 42161, "arbitrum-one"},
		{"base mainnet", 8453, "base-mainnet"},
		{"optimism mainnet", 10, "optimism-mainnet"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dataset, ok := sqdChainToDataset[tt.chainId]
			if !ok {
				t.Fatalf("chain %d not found in sqdChainToDataset", tt.chainId)
			}
			if dataset != tt.expected {
				t.Errorf("sqdChainToDataset[%d] = %q, want %q", tt.chainId, dataset, tt.expected)
			}
		})
	}
}

func TestSqdSupportedChainIds(t *testing.T) {
	chainIds := sqdSupportedChainIds()

	if len(chainIds) != len(sqdChainToDataset) {
		t.Errorf("sqdSupportedChainIds() returned %d chains, want %d", len(chainIds), len(sqdChainToDataset))
	}

	chainIdSet := make(map[int64]bool)
	for _, id := range chainIds {
		chainIdSet[id] = true
	}
	for chainId := range sqdChainToDataset {
		if !chainIdSet[chainId] {
			t.Errorf("chainId %d from sqdChainToDataset is missing from sqdSupportedChainIds()", chainId)
		}
	}
}

func TestSqdChainToDataset_UnknownChain(t *testing.T) {
	unknownChainIds := []int64{999999, 0, -1, 123456789}

	for _, chainId := range unknownChainIds {
		if dataset, ok := sqdChainToDataset[chainId]; ok {
			t.Errorf("unexpected mapping for unknown chain %d: %q", chainId, dataset)
		}
	}
}

// TestSqdChainToDataset_NotRequiredForChainIdPlaceholder verifies that when using
// the {chainId} placeholder, chains don't need to be in the mapping.
func TestSqdChainToDataset_NotRequiredForChainIdPlaceholder(t *testing.T) {
	// Chain 999999 is not in the mapping
	_, inMapping := sqdChainToDataset[999999]
	if inMapping {
		t.Skip("chain 999999 is unexpectedly in the mapping")
	}

	// But with {chainId} placeholder, it should work (tested in sqd_test.go)
	// This test just documents that the mapping is not required for {chainId} usage
}

func TestSqdDatasetFromSettings_MapInterfaceInterface(t *testing.T) {
	settings := common.VendorSettings{
		"datasetByChainId": map[interface{}]interface{}{
			int(1):        "ethereum-mainnet",
			int64(10):     "optimism-mainnet",
			float64(137):  "polygon-mainnet",
			"42161":       "arbitrum-one",
			"not-a-chain": "ignore-me",
		},
	}

	dataset, ok := sqdDatasetFromSettings(settings, 1)
	if !ok {
		t.Fatalf("expected dataset for chain 1")
	}
	if dataset != "ethereum-mainnet" {
		t.Errorf("dataset = %q, want ethereum-mainnet", dataset)
	}

	dataset, ok = sqdDatasetFromSettings(settings, 10)
	if !ok {
		t.Fatalf("expected dataset for chain 10")
	}
	if dataset != "optimism-mainnet" {
		t.Errorf("dataset = %q, want optimism-mainnet", dataset)
	}

	dataset, ok = sqdDatasetFromSettings(settings, 137)
	if !ok {
		t.Fatalf("expected dataset for chain 137")
	}
	if dataset != "polygon-mainnet" {
		t.Errorf("dataset = %q, want polygon-mainnet", dataset)
	}

	dataset, ok = sqdDatasetFromSettings(settings, 42161)
	if !ok {
		t.Fatalf("expected dataset for chain 42161")
	}
	if dataset != "arbitrum-one" {
		t.Errorf("dataset = %q, want arbitrum-one", dataset)
	}
}

func TestSqdDatasetFromSettings_NonStringValue(t *testing.T) {
	settings := common.VendorSettings{
		"datasetByChainId": map[string]interface{}{
			"1": 42, // non-string value
		},
	}

	_, ok := sqdDatasetFromSettings(settings, 1)
	if ok {
		t.Error("expected false for non-string dataset value")
	}
}

func TestSqdUseDefaultDatasets_Coercion(t *testing.T) {
	tests := []struct {
		name     string
		value    interface{}
		expected bool
	}{
		{"bool true", true, true},
		{"bool false", false, false},
		{"string true", "true", true},
		{"string false", "false", false},
		{"string yes", "yes", true},
		{"string no", "no", false},
		{"string on", "on", true},
		{"string off", "off", false},
		{"string one", "1", true},
		{"string zero", "0", false},
		{"int one", 1, true},
		{"int zero", 0, false},
		{"int64 one", int64(1), true},
		{"int64 zero", int64(0), false},
		{"float one", float64(1), true},
		{"float zero", float64(0), false},
		{"unknown string defaults false", "maybe", false},
		{"unknown type defaults false", []string{"bad"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sqdUseDefaultDatasets(common.VendorSettings{"useDefaultDatasets": tt.value})
			if got != tt.expected {
				t.Fatalf("expected %v, got %v", tt.expected, got)
			}
		})
	}
}

func TestSqdUseDefaultDatasets_NilSettings(t *testing.T) {
	got := sqdUseDefaultDatasets(nil)
	if !got {
		t.Fatal("expected true for nil settings")
	}
}

func TestSqdUseDefaultDatasets_NotSet(t *testing.T) {
	got := sqdUseDefaultDatasets(common.VendorSettings{})
	if !got {
		t.Fatal("expected true when useDefaultDatasets not set")
	}
}
