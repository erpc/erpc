package thirdparty

import (
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
)

func init() {
	util.ConfigureTestLogger()
}

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
	chainIds := SqdSupportedChainIds()

	if len(chainIds) != len(sqdChainToDataset) {
		t.Errorf("SqdSupportedChainIds() returned %d chains, want %d", len(chainIds), len(sqdChainToDataset))
	}

	// Verify all returned chain IDs exist in the map
	for _, chainId := range chainIds {
		if _, ok := sqdChainToDataset[chainId]; !ok {
			t.Errorf("SqdSupportedChainIds() returned chainId %d which is not in sqdChainToDataset", chainId)
		}
	}

	// Verify all map keys are in the returned slice
	chainIdSet := make(map[int64]bool)
	for _, id := range chainIds {
		chainIdSet[id] = true
	}
	for chainId := range sqdChainToDataset {
		if !chainIdSet[chainId] {
			t.Errorf("chainId %d from sqdChainToDataset is missing from SqdSupportedChainIds()", chainId)
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

func TestSqdDatasetFromSettings_MapInterfaceInterface(t *testing.T) {
	settings := common.VendorSettings{
		"datasetByChainId": map[interface{}]interface{}{
			int64(1): "ethereum-mainnet",
			"10":     "optimism-mainnet",
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
}
