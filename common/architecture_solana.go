package common

import (
	"context"
)

// SlotSharedVariable is a subset of data.CounterInt64SharedVariable that the
// SolanaStatePoller needs. Defined here in common so architecture/solana can
// use it without importing the data package (which would create an import cycle
// via data → clients → architecture/solana).
type SlotSharedVariable interface {
	GetValue() int64
	TryUpdate(ctx context.Context, newValue int64) int64
	OnValue(callback func(int64))
	OnLargeRollback(callback func(currentVal, newVal int64))
}

const (
	UpstreamTypeSolana UpstreamType        = "solana"
	ArchitectureSolana NetworkArchitecture = "solana"
)

// SolanaCluster represents known Solana cluster names.
type SolanaCluster string

const (
	SolanaClusterMainnetBeta SolanaCluster = "mainnet-beta"
	SolanaClusterDevnet      SolanaCluster = "devnet"
	SolanaClusterTestnet     SolanaCluster = "testnet"
)

// SolanaGenesisHashes maps each known cluster to its canonical genesis hash.
// Used during upstream bootstrap to verify the node serves the correct cluster,
// mirroring the eth_chainId check for EVM upstreams.
var SolanaGenesisHashes = map[SolanaCluster]string{
	SolanaClusterMainnetBeta: "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d",
	SolanaClusterDevnet:      "EtWTRABZaYq6iMfeYKouRu166VU2xqa1wcaWoxPkrZBG",
	SolanaClusterTestnet:     "4uhcVJyU9pJkvQyS88uRDiswHXSCkY3zQawwpjk2NsNY",
}

// SolanaCommitment represents Solana commitment levels, ordered from weakest to strongest.
type SolanaCommitment string

const (
	SolanaCommitmentProcessed SolanaCommitment = "processed"
	SolanaCommitmentConfirmed SolanaCommitment = "confirmed"
	SolanaCommitmentFinalized SolanaCommitment = "finalized"
)

// SolanaUpstream extends Upstream with Solana-specific capabilities.
type SolanaUpstream interface {
	Upstream
	SolanaCluster() string
	SolanaStatePoller() SolanaStatePoller
}

// SolanaStatePoller tracks Solana slot state for upstream health and cache decisions.
// Mirrors the EvmStatePoller interface for EVM block tracking.
type SolanaStatePoller interface {
	Bootstrap(ctx context.Context) error
	Poll(ctx context.Context) error
	PollProcessedSlot(ctx context.Context) (int64, error)
	PollFinalizedSlot(ctx context.Context) (int64, error)
	PollMaxShredInsertSlot(ctx context.Context) (int64, error)
	PollHealth(ctx context.Context) error
	LatestSlot() int64
	FinalizedSlot() int64
	// MaxShredInsertSlotLag returns the difference between the node's highest
	// shred-insert slot and its processed slot. A large lag (>100 slots) means
	// the node is receiving block shreds but failing to process them — it may
	// still pass getHealth but serves stale state.
	MaxShredInsertSlotLag() int64
	IsHealthy() bool
	SuggestLatestSlot(slot int64)
	SuggestFinalizedSlot(slot int64)
	IsObjectNull() bool
}
