package thirdparty

import (
	"testing"

	"github.com/erpc/erpc/common"
)

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

func TestSqdDatasetFromSettings_DirectDataset(t *testing.T) {
	settings := common.VendorSettings{
		"dataset": "custom-dataset",
	}

	dataset, ok := sqdDatasetFromSettings(settings, 999999)
	if !ok {
		t.Fatal("expected dataset to be found")
	}
	if dataset != "custom-dataset" {
		t.Errorf("dataset = %q, want custom-dataset", dataset)
	}
}

func TestSqdDatasetFromSettings_NilSettings(t *testing.T) {
	_, ok := sqdDatasetFromSettings(nil, 1)
	if ok {
		t.Error("expected false for nil settings")
	}
}

func TestSqdDatasetFromSettings_EmptySettings(t *testing.T) {
	_, ok := sqdDatasetFromSettings(common.VendorSettings{}, 1)
	if ok {
		t.Error("expected false for empty settings")
	}
}

func TestSqdDatasetFromSettings_UnknownChain(t *testing.T) {
	settings := common.VendorSettings{
		"datasetByChainId": map[string]string{
			"1": "ethereum-mainnet",
		},
	}

	_, ok := sqdDatasetFromSettings(settings, 999999)
	if ok {
		t.Error("expected false for unknown chain")
	}
}
