package integrity

// Chain profiles: checks that are PROTOCOL-INVALID on specific chains, learned
// from production shadow testing and shipped as defaults so every operator
// doesn't rediscover them as reject floods. The mechanism is the same on each
// chain: the protocol injects synthetic/system transactions into the block's
// cryptographic commitments (transactionsRoot / receiptsRoot) but omits them
// from the RPC response's transactions/receipts lists, so recomputing (or
// consistency-checking) the root from the response data can never reproduce
// the header's value — a mismatch is the protocol, not corruption.
//
// Diagnostic rule (how these were found): a check rejecting across ALL
// upstreams of one chain is a chain quirk; scattered per-upstream rejects are
// real catches.
//
// Precedence: a profile is applied AFTER the level preset and BEFORE the
// operator's per-check overrides — an explicit `checks.<id>.enabled: true`
// wins over the profile (and `enabled: false` is a no-op agreement with it).
var chainProfiles = map[int64][]string{
	// HyperEVM: HyperCore system transactions are committed in the header
	// roots but omitted from eth_getBlock*'s transactions list — the whole
	// tx/receipt-root family is unreproducible, including the structural
	// has-txs⟺non-empty-root consistency check.
	999: {"transactionsRootRecompute", "receiptsRootRecompute", "transactionsRootConsistency", "baseFeeDerivation"},
	// Polygon PoS: bor's state-sync transactions are committed but not listed.
	// Its EIP-1559 fee parameters are also NOT mainnet's: with the mainnet
	// elasticity/denominator the derivation rejected across three independent
	// vendors (alchemy, chainstack, quicknode) within minutes of going live on
	// the shadow — all-upstream rejects on one chain are the protocol, not
	// corruption.
	137: {"transactionsRootRecompute", "receiptsRootRecompute", "baseFeeDerivation"},
	// Arbitrum One: ArbOS internal transactions are committed but not listed.
	// Its base fee is also set by ArbOS rather than derived from the parent by
	// the EIP-1559 formula, so that derivation is protocol-invalid here.
	42161: {"transactionsRootRecompute", "receiptsRootRecompute", "baseFeeDerivation"},
	// Base (OP Stack): EIP-1559 is implemented with DIFFERENT parameters than
	// mainnet's (elasticity/denominator), so the mainnet derivation would
	// mismatch on every block. Excluded until the constants are verified per
	// chain rather than assumed — a wrong constant here rejects honest data on
	// every single block, which is the worst failure this module can have.
	8453: {"baseFeeDerivation"},
}

// ApplyChainProfile removes the checks that are protocol-invalid on the given
// chain from the set. Unknown chains are untouched.
func ApplyChainProfile(cs CheckSet, chainId int64) {
	for _, id := range chainProfiles[chainId] {
		delete(cs, id)
	}
}

// ChainProfileDisables returns the check ids a chain's profile disables (for
// introspection/docs/tests). nil for chains without a profile.
func ChainProfileDisables(chainId int64) []string {
	return chainProfiles[chainId]
}
