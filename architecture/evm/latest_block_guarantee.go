package evm

import (
	"fmt"

	"github.com/erpc/erpc/common"
)

// EvmBuiltinLatestBlockGuarantees contains hardcoded preset profiles of EVM methods
// that must be available on at least one upstream before a block number is reported as "latest".
var EvmBuiltinLatestBlockGuarantees = map[string][]string{
	"frontend-dapps": {
		"eth_getBlockByNumber",
		"eth_call",
		"eth_getBalance",
		"eth_estimateGas",
		"eth_gasPrice",
		"eth_getTransactionReceipt",
		"eth_getLogs",
	},
	"basic-indexing": {
		"eth_getBlockByNumber",
		"eth_getTransactionReceipt",
		"eth_getLogs",
		"eth_getBlockReceipts",
	},
	"complete-indexing": {
		"eth_getBlockByNumber",
		"eth_getTransactionReceipt",
		"eth_getLogs",
		"eth_getBlockReceipts",
		"*trace*|*debug*",
	},
	"full-archive": {
		"eth_getBlockByNumber",
		"eth_getTransactionReceipt",
		"eth_getLogs",
		"eth_getBlockReceipts",
		"*trace*|*debug*",
		"eth_getProof",
		"eth_call",
		"eth_getBalance",
		"eth_getStorageAt",
	},
}

// ResolveEvmLatestBlockGuarantee resolves a profile ID to its methods list.
// Checks custom profiles first, then falls back to built-in presets.
// Returns an error if the profile ID is not found.
func ResolveEvmLatestBlockGuarantee(customProfiles []*common.EvmLatestBlockGuaranteeConfig, profileId string) ([]string, error) {
	if profileId == "" {
		return nil, nil
	}
	for _, p := range customProfiles {
		if p.Id == profileId {
			return p.Methods, nil
		}
	}
	if methods, ok := EvmBuiltinLatestBlockGuarantees[profileId]; ok {
		return methods, nil
	}
	return nil, fmt.Errorf("evmLatestBlockGuarantee profile '%s' not found", profileId)
}
