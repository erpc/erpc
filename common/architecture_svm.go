package common

import "context"

const (
	UpstreamTypeSvm UpstreamType = "svm"
)

// SlotSharedVariable mirrors the EVM SharedStateVariable pattern for int64 slot numbers.
// It lets the state poller publish slot progress to a shared-state backend (memory or Redis)
// while keeping callers synchronous via the cached local value.
type SlotSharedVariable interface {
	GetValue() int64
	TryUpdate(ctx context.Context, newValue int64) int64
	OnValue(callback func(int64))
	OnLargeRollback(callback func(currentVal, newVal int64))
}

// SvmUpstream narrows common.Upstream to the surface SVM hooks need to reach
// into per-upstream state (currently just the state poller). Keeping this a
// tiny sub-interface avoids committing the full Upstream struct to common/ and
// parallels the existing EvmUpstream pattern.
type SvmUpstream interface {
	Upstream
	SvmStatePoller() SvmStatePoller
}

// SvmStatePoller is the per-upstream slot/health tracker. Concrete type lives in
// architecture/svm to avoid pulling the full implementation into common.
type SvmStatePoller interface {
	Bootstrap(ctx context.Context) error
	IsObjectNull() bool

	Poll(ctx context.Context) error

	LatestSlot() int64
	FinalizedSlot() int64
	MaxShredInsertSlotLag() int64
	IsHealthy() bool

	SuggestLatestSlot(slot int64)
	SuggestFinalizedSlot(slot int64)
}

// MaxShredInsertSlotLagThreshold is the cutoff beyond which an upstream is treated
// as degraded for shred-insert lag. Solana nodes with high `latest - maxShredInsertSlot`
// are receiving shreds but not processing them; their reads go stale silently.
const MaxShredInsertSlotLagThreshold int64 = 100

// SvmChainSolana is the canonical Solana chain identifier. Empty Chain values
// in SvmNetworkConfig normalize to this constant at NetworkId derivation time
// so pre-multi-chain configs keep producing "svm:<cluster>" IDs.
const SvmChainSolana = "solana"

// knownSvmGenesisHashes maps (chain, cluster) → immutable genesis hash.
// Genesis hashes are the hash of block 0 and never change, so we can validate
// upstream cluster membership at bootstrap via a single RPC compared against
// this table. Nested by chain so forks with overlapping cluster names (e.g.
// "mainnet" on Fogo vs "mainnet-beta" on Solana) don't collide.
//
// Onboarding a new (chain, cluster):
//   - Obtain the genesis hash once via `curl -X POST -d '{"method":"getGenesisHash"}'`
//     against any trusted node of that chain's cluster.
//   - Add or extend the chain's entry below.
//
// Do NOT add a cluster with an empty hash — that would cost an RPC per upstream
// bootstrap without any comparison happening. Better to leave the cluster
// absent; operators can still run it via CheckGenesisHash:true, which opts in
// to runtime fetch+compare against another of their own upstreams' responses.
//
// Non-Solana SVM-compatible chains (Fogo, Eclipse, custom forks) slot in as
// new top-level entries when their genesis hash has been verified.
var knownSvmGenesisHashes = map[string]map[string]string{
	SvmChainSolana: {
		"mainnet-beta": "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d",
		"devnet":       "EtWTRABZaYq6iMfeYKouRu166VU2xqa1wcaWoxPkrZBG",
		"testnet":      "4uhcVJyU9pJkvQyS88uRDiswHXSCkY3zQawwpjk2NsNY",
	},
}

// ResolveSvmChain normalizes an SvmNetworkConfig.Chain value: empty defaults to
// SvmChainSolana. Everything in this package should go through this helper so
// the backward-compat rule lives in one place.
func ResolveSvmChain(chain string) string {
	if chain == "" {
		return SvmChainSolana
	}
	return chain
}

// IsValidSvmCluster returns true when the (chain, cluster) pair is recognized.
// Unknown pairs (e.g. localnet under solana, or any cluster under a chain
// that's not in the table) must set CheckGenesisHash:true on the upstream
// config to opt in to runtime validation.
func IsValidSvmCluster(chain, cluster string) bool {
	clusters, ok := knownSvmGenesisHashes[ResolveSvmChain(chain)]
	if !ok {
		return false
	}
	_, ok = clusters[cluster]
	return ok
}

// KnownGenesisHash returns the hardcoded genesis hash for (chain, cluster), or
// "" if unknown. Callers can tell "unknown pair" (ok=false, skip check) from
// "known but empty" (ok=true, use the returned hash).
func KnownGenesisHash(chain, cluster string) (string, bool) {
	clusters, ok := knownSvmGenesisHashes[ResolveSvmChain(chain)]
	if !ok {
		return "", false
	}
	h, ok := clusters[cluster]
	return h, ok
}
