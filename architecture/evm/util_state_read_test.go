package evm

import "testing"

func TestIsStateReadMethod(t *testing.T) {
	stateReads := []string{
		"eth_getBalance",
		"eth_getCode",
		"eth_getStorageAt",
		"eth_getTransactionCount",
		"eth_getProof",
		"eth_call",
		"eth_estimateGas",
		"eth_simulateV1",
		"eth_createAccessList",
	}
	for _, m := range stateReads {
		if !IsStateReadMethod(m) {
			t.Errorf("IsStateReadMethod(%q) = false, want true", m)
		}
	}

	notStateReads := []string{
		"eth_blockNumber",
		"eth_getBlockByNumber",
		"eth_getBlockByHash",
		"eth_getBlockTransactionCountByHash",
		"eth_getBlockTransactionCountByNumber",
		"eth_getLogs",
		"eth_getTransactionByHash",
		"eth_getTransactionByBlockNumberAndIndex",
		"eth_getTransactionReceipt",
		"eth_getBlockReceipts",
		"eth_chainId",
		"eth_syncing",
		"eth_sendRawTransaction",
		"eth_sendTransaction",
		"eth_newFilter",
		"net_version",
		"web3_clientVersion",
		"trace_call",
		"trace_block",
		"trace_filter",
		"trace_transaction",
		"debug_traceTransaction",
		"debug_traceCall",
		"debug_traceBlockByHash",
		"arbtrace_call",
		"",
	}
	for _, m := range notStateReads {
		if IsStateReadMethod(m) {
			t.Errorf("IsStateReadMethod(%q) = true, want false", m)
		}
	}
}

// TestIsStateReadMethod_CaseSensitive confirms exact-string match — alternate
// casings like "eth_getbalance" or "ETH_GETBALANCE" must NOT classify as
// state-read, because JSON-RPC method names are case-sensitive on the wire
// and the upstream-selection / retry logic must mirror that.
func TestIsStateReadMethod_CaseSensitive(t *testing.T) {
	casings := []string{
		"eth_getbalance",   // lowercase b
		"ETH_GETBALANCE",   // uppercase
		"Eth_GetBalance",   // mixed
		"eth_GetBalance",   // PascalCase
		" eth_getBalance",  // leading whitespace
		"eth_getBalance ",  // trailing whitespace
		"eth_getBalance\t", // trailing tab
	}
	for _, m := range casings {
		if IsStateReadMethod(m) {
			t.Errorf("IsStateReadMethod(%q) = true, want false (must be exact match)", m)
		}
	}
}

// TestIsStateReadMethod_NotPrefixMatch confirms that method names that are
// prefixed by a state-read method but aren't equal don't match. Defensive
// against accidental strings.HasPrefix-style logic.
func TestIsStateReadMethod_NotPrefixMatch(t *testing.T) {
	prefixed := []string{
		"eth_getBalanceAt",     // hypothetical method with state-read name as prefix
		"eth_getCodeHash",      // ditto
		"eth_call_internal",    // ditto
		"eth_estimateGasPrice", // ditto
	}
	for _, m := range prefixed {
		if IsStateReadMethod(m) {
			t.Errorf("IsStateReadMethod(%q) = true, want false (not exact match)", m)
		}
	}
}
