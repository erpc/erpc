package common

import "context"

const (
	UpstreamTypeSolana  UpstreamType        = "solana"
	ArchitectureSolana  NetworkArchitecture = "solana"
)

// SolanaCluster represents known Solana cluster names.
type SolanaCluster string

const (
	SolanaClusterMainnetBeta SolanaCluster = "mainnet-beta"
	SolanaClusterDevnet      SolanaCluster = "devnet"
	SolanaClusterTestnet     SolanaCluster = "testnet"
)

// SolanaCommitment represents Solana commitment levels, ordered from weakest to strongest.
type SolanaCommitment string

const (
	SolanaCommitmentProcessed  SolanaCommitment = "processed"
	SolanaCommitmentConfirmed  SolanaCommitment = "confirmed"
	SolanaCommitmentFinalized  SolanaCommitment = "finalized"
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
	LatestSlot() int64
	FinalizedSlot() int64
	SuggestLatestSlot(slot int64)
	SuggestFinalizedSlot(slot int64)
	IsObjectNull() bool
}
